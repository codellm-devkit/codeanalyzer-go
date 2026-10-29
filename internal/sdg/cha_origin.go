package sdg

import (
	"go/token"
	"go/types"
	"slices"
	"sort"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
)

// origins is the interprocedural "may carry a library-originated dynamic
// type" analysis behind the CHA complement (spec §4.8). VTA propagates
// types through project bodies only (its graph is field-based for struct
// fields and type-based for pointer, slice, map and channel contents), so
// what it cannot see is a value that came out of library code: no flow
// reaches it. The analysis tracks where such values go, through project
// code, to a monotone fixpoint over the whole program (the project
// functions VTA runs over):
//
//   - Seeds (per value): a TypeAssert; the result of a library function, a
//     project function with no body, a library interface's method, recover();
//     a load of a library global or through a library struct's field.
//   - Local steps: Phi, Extract, ChangeInterface, ChangeType, Convert, Slice,
//     Field and FieldAddr / IndexAddr of an LO base; a map Lookup, an Index,
//     a channel receive and a range Next over an LO container (N2).
//   - Interprocedural facts (finite, so the fixpoint terminates):
//     ret(F, i)   result i of project function F (any Return operand i is LO);
//     param(p)    a parameter (receiver included) some call passes an LO actual;
//     free(v)     a closure capture bound to an LO value (MakeClosure);
//     global(G)   a project global an LO value is stored to;
//     field(S, i) field i of struct type S (by type: every instance);
//     alloc(a)    an address-taken local;
//     seq(T)      an element of any slice or array of element type T;
//     mapkey(T), mapval(T), chan(T)  map keys, map values and channel
//     elements of type T (any container of that element type);
//     unknown(T)  a T stored through a pointer of unknown provenance (a
//     parameter, a call result); every precise load of a T reads it too;
//     any(T)      any store of a T; a load through such a pointer reads it.
//     A pointer, slice, map or channel of interface elements passed to
//     library code (errors.As(err, &target)) marks unknown(T) / any(T) and
//     the container's element cell: the library may write into it.
//   - Call targets: the static callee; for an interface call, the method
//     each project implementer's method set selects (CHA), and the result is
//     LO too when the receiver is LO or a library type the project converts
//     to an interface implements it; for a call through a function value,
//     every address-taken project function of an identical signature, and
//     the result is LO when the value is LO or a library function of that
//     signature is used as a value. VTA's targets are not used: the analysis
//     runs before VTA (the program must not be read after a no_vta), and
//     these sets are supersets of VTA's.
//
// Each fact only goes from false to true. A function is re-evaluated when a
// fact it read changes (a reader-tracked worklist); a function's values are
// computed by a local fixpoint (Phi cycles). The least fixpoint is unique,
// so the result is independent of the visiting order; the order is
// deterministic anyway (functions sorted by package path and name, readers
// in registration order). It reads SSA instructions and go/types only and
// builds no code.
type origins struct {
	c     *cha
	prog  *ssa.Program
	funcs map[*ssa.Function]bool
	order []*ssa.Function

	facts   map[fact]bool
	readers map[fact][]*ssa.Function
	readBy  map[fact]map[*ssa.Function]bool
	cur     *ssa.Function // the function being evaluated (reader registration); nil = none

	work   []*ssa.Function
	queued map[*ssa.Function]bool

	typeIDs   typeutil.Map // type → int id (fact keys)
	nextID    int
	byFuncSig typeutil.Map // signature → []*ssa.Function: address-taken project functions
	libFuncs  typeutil.Map // signature → true: address-taken library functions
	libConv   []types.Type // library types project code converts to an interface
	// Memos of pure functions of the program (fixed before the worklist;
	// never facts): each entry is computed whole the first time it is read.
	libImpl typeutil.Map                // interface → bool: a libConv type implements it
	implFns map[implKey][]*ssa.Function // filled with implLib, by implementers
	implLib map[implKey]bool            // an implementer's selected method is library code
}

type factKind uint8

const (
	fRet factKind = iota
	fParam
	fFree
	fGlobal
	fField
	fAlloc
	fSeq
	fMapKey
	fMapVal
	fChan
	fUnknown
	fAny
)

// fact is one interprocedural fact: kind, an SSA object (ret, param, free,
// global, alloc) and up to two ints (a result index; a type id and a field
// index).
type fact struct {
	k    factKind
	p    any
	a, b int
}

type implKey struct {
	iface int // type id of the interface
	pkg   *types.Package
	name  string
}

// originSeedOrder, when set, permutes the order the origin worklist is
// seeded in (a test seam: the least fixpoint must not depend on it).
var originSeedOrder func([]*ssa.Function)

// chaHook, when set, runs on each function the CHA complement scans (a test
// seam for the recovered-panic path, N1).
var chaHook func(*ssa.Function)

func newOrigins(c *cha, funcs map[*ssa.Function]bool) *origins {
	o := &origins{
		c:       c,
		funcs:   funcs,
		facts:   map[fact]bool{},
		readers: map[fact][]*ssa.Function{},
		readBy:  map[fact]map[*ssa.Function]bool{},
		queued:  map[*ssa.Function]bool{},
		implFns: map[implKey][]*ssa.Function{},
		implLib: map[implKey]bool{},
	}
	for fn := range funcs {
		o.order = append(o.order, fn)
		o.prog = fn.Prog
	}
	sort.Slice(o.order, func(i, j int) bool {
		a, b := o.order[i], o.order[j]
		if pa, pb := homePath(a), homePath(b); pa != pb {
			return pa < pb
		}
		return a.String() < b.String()
	})
	var conv typeutil.Map
	var buf [10]*ssa.Value
	for _, fn := range o.order {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				var callee *ssa.Value
				if call, ok := instr.(ssa.CallInstruction); ok && !call.Common().IsInvoke() {
					callee = &call.Common().Value
				}
				for _, op := range instr.Operands(buf[:0]) {
					if g, ok := (*op).(*ssa.Function); ok && op != callee {
						o.addressTaken(g)
					}
				}
				if mc, ok := instr.(*ssa.MakeClosure); ok {
					o.addressTaken(mc.Fn.(*ssa.Function))
				}
				if mi, ok := instr.(*ssa.MakeInterface); ok && c.libraryType(derefType(mi.X.Type())) && conv.At(mi.X.Type()) == nil {
					conv.Set(mi.X.Type(), true)
					o.libConv = append(o.libConv, mi.X.Type())
				}
			}
		}
	}
	return o
}

func derefType(t types.Type) types.Type {
	if p, ok := types.Unalias(t).Underlying().(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

func (o *origins) addressTaken(g *ssa.Function) {
	if o.funcs[g] {
		list, _ := o.byFuncSig.At(g.Signature).([]*ssa.Function)
		for _, f := range list {
			if f == g {
				return
			}
		}
		o.byFuncSig.Set(g.Signature, append(list, g))
		return
	}
	o.libFuncs.Set(g.Signature, true)
}

func (o *origins) typeID(t types.Type) int {
	if v := o.typeIDs.At(t); v != nil {
		return v.(int)
	}
	o.nextID++
	o.typeIDs.Set(t, o.nextID)
	return o.nextID
}

// get reads fact f, registering the current function as its reader.
func (o *origins) get(f fact) bool {
	if o.cur != nil {
		set := o.readBy[f]
		if set == nil {
			set = map[*ssa.Function]bool{}
			o.readBy[f] = set
		}
		if !set[o.cur] {
			set[o.cur] = true
			o.readers[f] = append(o.readers[f], o.cur)
		}
	}
	return o.facts[f]
}

// set makes fact f true and re-queues its readers.
func (o *origins) set(f fact) {
	if o.facts[f] {
		return
	}
	o.facts[f] = true
	for _, r := range o.readers[f] {
		o.enqueue(r)
	}
}

func (o *origins) enqueue(fn *ssa.Function) {
	if !o.queued[fn] {
		o.queued[fn] = true
		o.work = append(o.work, fn)
	}
}

// solve runs the worklist to the fixpoint.
func (o *origins) solve() {
	seed := o.order
	if originSeedOrder != nil {
		seed = slices.Clone(o.order)
		originSeedOrder(seed)
	}
	for _, fn := range seed {
		o.enqueue(fn)
	}
	for len(o.work) > 0 {
		fn := o.work[0]
		o.work = o.work[1:]
		o.queued[fn] = false
		o.cur = fn
		lo := o.eval(fn)
		o.effects(fn, lo)
		o.cur = nil
	}
}

// eval is fn's local fixpoint: the set of its values (parameters, captures
// and instruction values) that may carry a library-originated dynamic type,
// under the current facts.
func (o *origins) eval(fn *ssa.Function) map[ssa.Value]bool {
	lo := map[ssa.Value]bool{}
	for _, p := range fn.Params {
		if o.get(fact{k: fParam, p: p}) {
			lo[p] = true
		}
	}
	for _, fv := range fn.FreeVars {
		if o.get(fact{k: fFree, p: fv}) {
			lo[fv] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				v, ok := instr.(ssa.Value)
				if !ok || lo[v] {
					continue
				}
				if o.valueLO(v, lo) {
					lo[v] = true
					changed = true
				}
			}
		}
	}
	return lo
}

// valueLO is the rule for one instruction value.
func (o *origins) valueLO(v ssa.Value, lo map[ssa.Value]bool) bool {
	switch v := v.(type) {
	case *ssa.Phi:
		for _, e := range v.Edges {
			if lo[e] {
				return true
			}
		}
	case *ssa.Extract:
		return o.tupleLO(v.Tuple, v.Index, lo)
	case *ssa.ChangeInterface:
		return lo[v.X]
	case *ssa.ChangeType:
		return lo[v.X]
	case *ssa.Convert:
		return lo[v.X]
	case *ssa.MultiConvert:
		return lo[v.X]
	case *ssa.SliceToArrayPointer:
		return lo[v.X]
	case *ssa.Slice:
		return lo[v.X]
	case *ssa.Range:
		return lo[v.X]
	case *ssa.TypeAssert:
		return true
	case *ssa.Call:
		return o.callLO(v.Common(), 0, lo)
	case *ssa.UnOp:
		switch v.Op {
		case token.MUL:
			return o.loadLO(v.X, lo)
		case token.ARROW:
			return o.recvLO(v.X, lo)
		}
	case *ssa.Field:
		st := v.X.Type()
		return o.c.libraryType(st) || lo[v.X] || o.get(o.fieldFact(st, v.Field))
	case *ssa.FieldAddr, *ssa.IndexAddr:
		var x ssa.Value
		if fa, ok := v.(*ssa.FieldAddr); ok {
			x = fa.X
		} else {
			x = v.(*ssa.IndexAddr).X
		}
		return lo[x]
	case *ssa.Lookup:
		return o.lookupLO(v, lo)
	case *ssa.Index:
		return lo[v.X] || o.get(fact{k: fSeq, a: o.typeID(v.Type())})
	}
	return false
}

// tupleLO is the rule for component i of a tuple-valued instruction.
func (o *origins) tupleLO(t ssa.Value, i int, lo map[ssa.Value]bool) bool {
	switch t := t.(type) {
	case *ssa.Call:
		return o.callLO(t.Common(), i, lo)
	case *ssa.TypeAssert:
		return i == 0
	case *ssa.Lookup:
		return i == 0 && o.lookupLO(t, lo)
	case *ssa.UnOp:
		return i == 0 && t.Op == token.ARROW && o.recvLO(t.X, lo)
	case *ssa.Next:
		if t.IsString || i == 0 {
			return false
		}
		r, ok := t.Iter.(*ssa.Range)
		if !ok {
			return true
		}
		if lo[r.X] {
			return true
		}
		m, ok := types.Unalias(r.X.Type()).Underlying().(*types.Map)
		if !ok {
			return false
		}
		if i == 1 {
			return o.get(fact{k: fMapKey, a: o.typeID(m.Key())})
		}
		return o.get(fact{k: fMapVal, a: o.typeID(m.Elem())})
	case *ssa.Select:
		k := i - 2 // results: index, recvOk, then one per receive state
		for _, st := range t.States {
			if st.Dir != types.RecvOnly {
				continue
			}
			if k == 0 {
				return o.recvLO(st.Chan, lo)
			}
			k--
		}
	}
	return false
}

func (o *origins) lookupLO(l *ssa.Lookup, lo map[ssa.Value]bool) bool {
	if lo[l.X] {
		return true
	}
	if m, ok := types.Unalias(l.X.Type()).Underlying().(*types.Map); ok {
		return o.get(fact{k: fMapVal, a: o.typeID(m.Elem())})
	}
	return false
}

func (o *origins) recvLO(ch ssa.Value, lo map[ssa.Value]bool) bool {
	if lo[ch] {
		return true
	}
	if c, ok := types.Unalias(ch.Type()).Underlying().(*types.Chan); ok {
		return o.get(fact{k: fChan, a: o.typeID(c.Elem())})
	}
	return false
}

func (o *origins) fieldFact(structType types.Type, i int) fact {
	return fact{k: fField, a: o.typeID(types.Unalias(structType).Underlying()), b: i}
}

// loadLO is the rule for a load *x.
func (o *origins) loadLO(x ssa.Value, lo map[ssa.Value]bool) bool {
	t := o.typeID(derefType(x.Type()))
	unknown := func() bool { return o.get(fact{k: fUnknown, a: t}) }
	switch x := x.(type) {
	case *ssa.Global:
		if x.Pkg != nil && !o.c.j.projPaths[x.Pkg.Pkg.Path()] {
			return true
		}
		return o.get(fact{k: fGlobal, p: x}) || unknown()
	case *ssa.FieldAddr:
		st := derefType(x.X.Type())
		return o.c.libraryType(st) || lo[x.X] || o.get(o.fieldFact(st, x.Field)) || unknown()
	case *ssa.IndexAddr:
		return lo[x.X] || o.get(fact{k: fSeq, a: t}) || unknown()
	case *ssa.Alloc:
		return o.get(fact{k: fAlloc, p: x}) || unknown()
	}
	return lo[x] || o.get(fact{k: fAny, a: t})
}

// callLO is the rule for result i of the call cc.
func (o *origins) callLO(cc *ssa.CallCommon, i int, lo map[ssa.Value]bool) bool {
	if b, ok := cc.Value.(*ssa.Builtin); ok {
		switch b.Name() {
		case "recover":
			return true
		case "append":
			for _, a := range cc.Args {
				if lo[a] {
					return true
				}
			}
		}
		return false
	}
	if cc.IsInvoke() {
		if cc.Method == nil || cc.Method.Pkg() == nil {
			return lo[cc.Value]
		}
		if o.c.j.isLibrary(cc.Method) || lo[cc.Value] || o.libImplements(cc.Value.Type()) {
			return true
		}
		fns, lib := o.implementers(cc)
		if lib {
			return true
		}
		for _, g := range fns {
			if o.get(fact{k: fRet, p: g, a: i}) {
				return true
			}
		}
		return false
	}
	if g := cc.StaticCallee(); g != nil {
		if !o.funcs[g] || len(g.Blocks) == 0 {
			return true // library code, or a function with no SSA body (assembly)
		}
		return o.get(fact{k: fRet, p: g, a: i})
	}
	sig := cc.Signature()
	if lo[cc.Value] || o.libFuncs.At(sig) != nil {
		return true
	}
	list, _ := o.byFuncSig.At(sig).([]*ssa.Function)
	for _, g := range list {
		if o.get(fact{k: fRet, p: g, a: i}) {
			return true
		}
	}
	return false
}

// libImplements reports whether a library type the project converts to an
// interface implements iface: a call on such a value may run library code.
func (o *origins) libImplements(iface types.Type) bool {
	if v := o.libImpl.At(iface); v != nil {
		return v.(bool)
	}
	it, _ := types.Unalias(iface).Underlying().(*types.Interface)
	found := false
	for _, t := range o.libConv {
		if it != nil && types.Implements(t, it) {
			found = true
			break
		}
	}
	o.libImpl.Set(iface, found)
	return found
}

func (o *origins) implKey(cc *ssa.CallCommon) implKey {
	return implKey{o.typeID(cc.Value.Type()), cc.Method.Pkg(), cc.Method.Name()}
}

// implementers returns the project functions an interface call cc may run
// — the method each project implementer's method set selects, when it has a
// body — and whether some implementer's selected method is library code (a
// promoted library method: its result may carry a library-originated
// type). Both are a pure function of the program (the project types and
// their method sets, fixed before the worklist), computed together and
// memoized together: neither is ever read before it is filled, so no
// evaluation depends on which call was evaluated first.
func (o *origins) implementers(cc *ssa.CallCommon) ([]*ssa.Function, bool) {
	k := o.implKey(cc)
	if fns, ok := o.implFns[k]; ok {
		return fns, o.implLib[k]
	}
	var fns []*ssa.Function
	seen := map[*ssa.Function]bool{}
	for _, t := range o.c.implementers(cc.Value.Type()) {
		sel := o.c.msets.MethodSet(t).Lookup(k.pkg, k.name)
		if sel == nil {
			continue
		}
		obj := sel.Obj().(*types.Func)
		if obj.Pkg() == nil {
			continue
		}
		if o.c.j.isLibrary(obj) {
			o.implLib[k] = true
			continue
		}
		if g := o.prog.FuncValue(obj); g != nil && o.funcs[g] && !seen[g] {
			seen[g] = true
			fns = append(fns, g)
		}
	}
	o.implFns[k] = fns
	return fns, o.implLib[k]
}

// effects applies fn's instructions to the facts, under its values lo.
func (o *origins) effects(fn *ssa.Function, lo map[ssa.Value]bool) {
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			switch instr := instr.(type) {
			case *ssa.Return:
				for i, r := range instr.Results {
					if lo[r] {
						o.set(fact{k: fRet, p: fn, a: i})
					}
				}
			case ssa.CallInstruction:
				o.callEffects(instr.Common(), lo)
			case *ssa.MakeClosure:
				g := instr.Fn.(*ssa.Function)
				for i, v := range instr.Bindings {
					if lo[v] && i < len(g.FreeVars) {
						o.set(fact{k: fFree, p: g.FreeVars[i]})
					}
				}
			case *ssa.Store:
				if lo[instr.Val] {
					o.store(instr.Addr)
				}
			case *ssa.MapUpdate:
				m, ok := types.Unalias(instr.Map.Type()).Underlying().(*types.Map)
				if !ok {
					continue
				}
				if lo[instr.Value] {
					o.set(fact{k: fMapVal, a: o.typeID(m.Elem())})
				}
				if lo[instr.Key] {
					o.set(fact{k: fMapKey, a: o.typeID(m.Key())})
				}
			case *ssa.Send:
				if lo[instr.X] {
					if c, ok := types.Unalias(instr.Chan.Type()).Underlying().(*types.Chan); ok {
						o.set(fact{k: fChan, a: o.typeID(c.Elem())})
					}
				}
			}
		}
	}
}

// store records an LO value stored through addr.
func (o *origins) store(addr ssa.Value) {
	t := o.typeID(derefType(addr.Type()))
	o.set(fact{k: fAny, a: t})
	switch a := addr.(type) {
	case *ssa.Global:
		o.set(fact{k: fGlobal, p: a})
	case *ssa.FieldAddr:
		o.set(o.fieldFact(derefType(a.X.Type()), a.Field))
	case *ssa.IndexAddr:
		o.set(fact{k: fSeq, a: t})
	case *ssa.Alloc:
		o.set(fact{k: fAlloc, p: a})
	default:
		o.set(fact{k: fUnknown, a: t})
	}
}

// callEffects passes cc's LO actuals to its targets' parameters, and marks
// what an interface container passed to library code may receive.
func (o *origins) callEffects(cc *ssa.CallCommon, lo map[ssa.Value]bool) {
	if _, ok := cc.Value.(*ssa.Builtin); ok {
		if b := cc.Value.(*ssa.Builtin); b.Name() == "copy" && len(cc.Args) == 2 && lo[cc.Args[1]] {
			if s, ok := types.Unalias(cc.Args[0].Type()).Underlying().(*types.Slice); ok {
				o.set(fact{k: fSeq, a: o.typeID(s.Elem())})
			}
		}
		return
	}
	pass := func(g *ssa.Function, args []ssa.Value) {
		for i, a := range args {
			if lo[a] && i < len(g.Params) {
				o.set(fact{k: fParam, p: g.Params[i]})
			}
		}
	}
	if cc.IsInvoke() {
		if cc.Method == nil || cc.Method.Pkg() == nil {
			return
		}
		if o.c.j.isLibrary(cc.Method) {
			o.escape(cc.Args)
		}
		args := append([]ssa.Value{cc.Value}, cc.Args...)
		fns, _ := o.implementers(cc)
		for _, g := range fns {
			pass(g, args)
		}
		return
	}
	if g := cc.StaticCallee(); g != nil {
		if !o.funcs[g] || len(g.Blocks) == 0 {
			o.escape(cc.Args)
			return
		}
		pass(g, cc.Args)
		return
	}
	list, _ := o.byFuncSig.At(cc.Signature()).([]*ssa.Function)
	for _, g := range list {
		pass(g, cc.Args)
	}
	if o.libFuncs.At(cc.Signature()) != nil {
		o.escape(cc.Args)
	}
}

// escape marks what library code may write into the interface containers
// args hand it (through a MakeInterface too): a pointer to an interface
// (errors.As), a slice, map or channel of interface elements.
func (o *origins) escape(args []ssa.Value) {
	for _, a := range args {
		if mi, ok := a.(*ssa.MakeInterface); ok {
			a = mi.X
		}
		switch t := types.Unalias(a.Type()).Underlying().(type) {
		case *types.Pointer:
			if types.IsInterface(t.Elem()) {
				id := o.typeID(t.Elem())
				o.set(fact{k: fUnknown, a: id})
				o.set(fact{k: fAny, a: id})
			}
		case *types.Slice:
			if types.IsInterface(t.Elem()) {
				o.set(fact{k: fSeq, a: o.typeID(t.Elem())})
			}
		case *types.Map:
			if types.IsInterface(t.Elem()) {
				o.set(fact{k: fMapVal, a: o.typeID(t.Elem())})
			}
			if types.IsInterface(t.Key()) {
				o.set(fact{k: fMapKey, a: o.typeID(t.Key())})
			}
		case *types.Chan:
			if types.IsInterface(t.Elem()) {
				o.set(fact{k: fChan, a: o.typeID(t.Elem())})
			}
		}
	}
}
