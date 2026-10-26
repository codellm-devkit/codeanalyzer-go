package sdg

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"

	"github.com/codellm-devkit/codeanalyzer-go/internal/astwalk"
	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// The `of` markers of the L4 vertices (spec §4.6, D6): Python's spelling,
// which the SDK's resolve_value and value_candidate read.
const (
	ofReturn  = "<return>"
	ofCapture = "<capture>:"
	ofGlobal  = "<global>:"
)

// ports builds the L4 port vertices (spec §4.6): the formals of every call
// target and the actuals of every resolved call site and capture site. It
// reads the L3 results, the dispatch, the footprints and go/types, never the
// SSA program, so it is safe after a no_vta. Formal sets are cached: it is
// not safe for concurrent use.
type ports struct {
	in    Input
	d     *dispatch
	fps   map[Target]Footprint
	fx    *formalIndex
	units map[dataflow.L3Key]dataflow.Unit
	byFn  map[ast.Node]dataflow.L3Key
	sets  map[Target]*formalSet
	calls map[dataflow.L3Key]map[string]*ast.CallExpr // unit → call key → call
	kept  map[dataflow.L3Key]map[string]bool          // unit → its kept L3 node keys
	names map[string]string                           // interned formal keys and `of`s (intern)
}

func newPorts(in Input, d *dispatch) *ports {
	p := &ports{
		in:    in,
		d:     d,
		fps:   footprints(in, d),
		fx:    newFormalIndex(in, d),
		units: make(map[dataflow.L3Key]dataflow.Unit, len(in.Units)),
		byFn:  make(map[ast.Node]dataflow.L3Key, len(in.Units)),
		sets:  map[Target]*formalSet{},
		calls: map[dataflow.L3Key]map[string]*ast.CallExpr{},
		kept:  map[dataflow.L3Key]map[string]bool{},
		names: map[string]string{},
	}
	for _, u := range in.Units {
		p.units[u.Key] = u
		p.byFn[u.Fn] = u.Key
	}
	return p
}

// intern returns the one shared copy of s (a memory lever, spec §4.13):
// every target in a large SCC has the same few hundred `<global>:` formals,
// and every target the same `@formal_in:<i>` keys, so building each anew
// would hold one copy per vertex.
func (p *ports) intern(s string) string {
	if !memLevers {
		return s
	}
	if c, ok := p.names[s]; ok {
		return c
	}
	p.names[s] = s
	return s
}

// formalSet is the formals of one target, with how a call site binds them:
// by position (receiver, declared params, results) or by `of` (captures,
// globals).
type formalSet struct {
	in, out  []Vertex
	recv     bool     // inPos[0] is the receiver
	params   int      // declared params
	inPos    []string // formal_in key of the receiver (when recv), then of each declared param
	posOf    []string // the `of` of each inPos entry
	outPos   []string // formal_out key of each inPos entry that is a by-ref out, else ""
	results  []string // formal_out key of each result
	captures []string // `of` of each capture, in formal order
	capOut   []string // `of` of each written capture
	globIn   []string // `of` of each global formal_in, sorted
	globOut  []string // `of` of each global formal_out, sorted
	inByOf   map[string]string
	outByOf  map[string]string
}

// formals returns t's formal_in and formal_out vertices (spec §4.6, A9, D6),
// in index order:
//
//   - formal_in (`@formal_in:<i>`, always indexed): the receiver (its name,
//     `_recv` when unnamed or `_`), the declared params (`_<i>` when unnamed
//     or `_`, i the declared index), the captures of a closure
//     (`<capture>:<name>`, dataflow.FreeVars order), then the globals the
//     footprint reads or writes (`<global>:<pkgpath>::<Name>`, sorted);
//   - formal_out (`@formal_out` when there is exactly one, else
//     `@formal_out:<i>`): the results by position (`<return>` for one,
//     `<return>:<i>` for several, never the user's names), the by-ref
//     params the footprint mutates in declaration order (receiver first,
//     `of` = the formal's), the captures the closure writes
//     (dataflow.CaptureWrites), then the global writes.
//
// t may be any target: a unit, an interface method or bodiless declaration
// (its *types.Func), or an external (its GoExternalCallee, typed through the
// load). The slices are shared: callers must not modify them.
func (p *ports) formals(t Target) (in, out []Vertex) {
	fs := p.formalSet(t)
	return fs.in, fs.out
}

func (p *ports) formalSet(t Target) *formalSet {
	if fs, ok := p.sets[t]; ok {
		return fs
	}
	fs := &formalSet{inByOf: map[string]string{}, outByOf: map[string]string{}}
	p.sets[t] = fs
	formals := p.fx.of(t)
	fp, ok := p.fps[t]
	if !ok {
		fp = Footprint{Mutated: byRefFormals(formals)}
	}
	mutated := map[string]bool{}
	for _, m := range fp.Mutated {
		mutated[m] = true
	}
	var ins, outs []string // `of`s
	var outFrom []int      // per out: the inPos entry it mirrors, or -1
	for i, f := range formals {
		if i == 0 && f.recv {
			fs.recv = true
		} else {
			fs.params++
		}
		ins = append(ins, f.name)
		fs.posOf = append(fs.posOf, f.name)
	}
	nRes := p.resultCount(t)
	for i := range nRes {
		outs = append(outs, p.intern(resultOf(i, nRes)))
		outFrom = append(outFrom, -1)
	}
	for i, f := range formals {
		if mutated[f.name] {
			outs = append(outs, f.name)
			outFrom = append(outFrom, i)
		}
	}
	if u, ok := p.units[t.Callable]; ok && t.External == "" && u.Info != nil {
		if lit, ok := u.Fn.(*ast.FuncLit); ok {
			fs.captures, fs.capOut = captureOfs(lit, u.Info, p.in.K)
		}
	}
	ins = append(ins, fs.captures...)
	outs = append(outs, fs.capOut...)
	for range fs.capOut {
		outFrom = append(outFrom, -1)
	}
	for _, g := range unionSorted([][]string{fp.GlobalReads, fp.GlobalWrites}) {
		fs.globIn = append(fs.globIn, p.intern(ofGlobal+g))
	}
	for _, g := range fp.GlobalWrites {
		fs.globOut = append(fs.globOut, p.intern(ofGlobal+g))
		outFrom = append(outFrom, -1)
	}
	ins = append(ins, fs.globIn...)
	outs = append(outs, fs.globOut...)

	fs.outPos = make([]string, len(fs.posOf))
	for i, of := range ins {
		key := p.intern("@formal_in:" + strconv.Itoa(i))
		fs.in = append(fs.in, Vertex{Key: key, Of: of, Kind: KindFormalIn})
		if i < len(fs.posOf) {
			fs.inPos = append(fs.inPos, key)
		} else {
			fs.inByOf[of] = key
		}
	}
	for i, of := range outs {
		key := "@formal_out"
		if len(outs) > 1 {
			key = p.intern(key + ":" + strconv.Itoa(i))
		}
		fs.out = append(fs.out, Vertex{Key: key, Of: of, Kind: KindFormalOut})
		switch {
		case i < nRes:
			fs.results = append(fs.results, key)
		case outFrom[i] >= 0:
			fs.outPos[outFrom[i]] = key
		default:
			fs.outByOf[of] = key
		}
	}
	return fs
}

// resultOf is the D6 `of` of result i of n.
func resultOf(i, n int) string {
	if n == 1 {
		return ofReturn
	}
	return ofReturn + ":" + strconv.Itoa(i)
}

// captureOfs returns the `of` of every variable lit captures (dataflow.FreeVars
// order) and of those it writes (dataflow.CaptureWrites bases), in that order.
// A capture's name is unique: every variable a literal names from outside it
// resolves in the one scope chain the literal sits in.
func captureOfs(lit *ast.FuncLit, info *types.Info, k int) (all, written []string) {
	w := map[types.Object]bool{}
	for _, path := range dataflow.CaptureWrites(lit, info, k) {
		w[path.Base] = true
	}
	for _, v := range dataflow.FreeVars(lit, info) {
		all = append(all, ofCapture+v.Name())
		if w[v] {
			written = append(written, ofCapture+v.Name())
		}
	}
	return all, written
}

// resultCount is the number of results of t: from its go/types signature
// where the load has one, else from the symbol table's or the external's
// type strings.
func (p *ports) resultCount(t Target) int {
	x := p.fx
	if t.External != "" {
		e, ok := x.ext[t.External]
		if !ok {
			return 0
		}
		if fn := x.libraryFunc(e); fn != nil {
			return fn.Signature().Results().Len()
		}
		return countResults(e.ReturnType)
	}
	if u, ok := x.units[t.Callable]; ok && u.Info != nil {
		if sig := unitSignature(u); sig != nil {
			return sig.Results().Len()
		}
	}
	if fn, ok := x.decls[t.Callable]; ok {
		return fn.Signature().Results().Len()
	}
	return len(x.strs[t.Callable].ReturnTypes)
}

// countResults counts the results of a GoExternalCallee.ReturnType: "" is
// none, "(A, B)" (types.TypeString's tuple) is the top-level commas plus one,
// anything else is one.
func countResults(s string) int {
	if s == "" {
		return 0
	}
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return 1
	}
	n, depth := 1, 0
	for _, r := range s[1 : len(s)-1] {
		switch r {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				n++
			}
		}
	}
	return n
}

// slotRecv is the call-side slot of the receiver: x of a method call x.M(…),
// or the function-value operand f of f(…) for a bound-method target (A8).
// Slots 0, 1, … are the call's function-type params.
const slotRecv = -1

// portBind binds one actual of a call or capture site to one target's
// formal: the param_in / param_out endpoints.
type portBind struct {
	Actual string
	Target Target
	Formal string
}

// site is the actuals of one call or capture site, and how each binds to
// each target's formals (in: actual_in → formal_in; out: formal_out →
// actual_out).
type site struct {
	In, Out         []Vertex
	BindIn, BindOut []portBind
}

// actuals returns the actual_in and actual_out vertices of call site
// callKey of unit (spec §4.6, A8, A11), whose dispatch targets are ts
// (dispatch.targets, or siteTargets). Keys are `<call-key>/actual_in:<i>`
// and `<call-key>/actual_out` (exactly one) or `<call-key>/actual_out:<i>`;
// Parent is callKey. A call with no target, or whose node the CFG pruned,
// has none.
func (p *ports) actuals(unit dataflow.L3Key, callKey string, ts []Target) (in, out []Vertex) {
	s := p.callSite(unit, callKey, ts)
	return s.In, s.Out
}

// callSite is actuals with the bindings. The actuals are the union over ts:
//
//   - by position: the receiver slot (x of x.M(…); a method expression's
//     first argument; for a bound-method target, the function-value
//     operand), then the call's function-type params in order. A variadic
//     call's excess values collapse onto its last param, and f(g()) has one
//     value per g result. Each slot's `of` is that of the formal it binds
//     in the static callee (L2's), else in the first target (ts order) that
//     binds it.
//   - then, at a directly called literal (`func(…){…}(…)`, deferred or
//     launched), the literal's captures after the declared args;
//   - then the globals by `of`: the static callee's (sorted), then any
//     other target's extras in target order.
//
// actual_out: the results by position, then the by-ref slots (a slot whose
// bound formal some target mutates), then the written captures of a
// directly called literal, then the global writes (by `of`, as for in).
func (p *ports) callSite(unit dataflow.L3Key, callKey string, ts []Target) site {
	if len(ts) == 0 || p.pruned(unit, callKey) {
		return site{}
	}
	u := p.units[unit]
	call := p.callExpr(unit, callKey)
	if call == nil {
		return site{}
	}
	static, hasStatic := p.d.static[siteKey{unit, callKey}]
	if !hasStatic || !slices.Contains(ts, static) {
		static, hasStatic = Target{}, false
	}
	order := ts
	if hasStatic {
		order = append([]Target{static}, slices.DeleteFunc(slices.Clone(ts), func(t Target) bool { return t == static })...)
	}

	shape := callShapeOf(call, u.Info)
	type slotBind struct {
		t   Target
		pos int // index into the target's inPos
	}
	binds := map[int][]slotBind{}
	for _, t := range order {
		fs := p.formalSet(t)
		for slot, pos := range shape.bind(fs) {
			binds[slot] = append(binds[slot], slotBind{t, pos})
		}
	}
	slots := make([]int, 0, len(binds))
	for slot := range binds {
		slots = append(slots, slot)
	}
	slices.Sort(slots)

	var s site
	addIn := func(of string) string {
		key := callKey + "/actual_in:" + strconv.Itoa(len(s.In))
		s.In = append(s.In, Vertex{Key: key, Of: of, Parent: callKey, Kind: KindActualIn})
		return key
	}
	slotOf := map[int]string{}
	for _, slot := range slots {
		b := binds[slot][0]
		fs := p.formalSet(b.t)
		slotOf[slot] = fs.posOf[b.pos]
		key := addIn(slotOf[slot])
		for _, b := range binds[slot] {
			s.BindIn = append(s.BindIn, portBind{key, b.t, p.formalSet(b.t).inPos[b.pos]})
		}
	}

	// A directly called literal binds its captures here (A6).
	var lit Target
	litOK := false
	if fl, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
		if k, ok := p.byFn[fl]; ok && slices.Contains(ts, Target{Callable: k}) {
			lit, litOK = Target{Callable: k}, true
		}
	}
	if litOK {
		fs := p.formalSet(lit)
		for _, of := range fs.captures {
			s.BindIn = append(s.BindIn, portBind{addIn(of), lit, fs.inByOf[of]})
		}
	}
	for _, of := range unionByOf(order, p, func(fs *formalSet) []string { return fs.globIn }) {
		key := addIn(of)
		for _, t := range order {
			if f, ok := p.formalSet(t).inByOf[of]; ok {
				s.BindIn = append(s.BindIn, portBind{key, t, f})
			}
		}
	}

	// actual_out: collect (of, bindings), then key once the count is known.
	type pending struct {
		of    string
		binds []portBind // Actual filled in below
	}
	var outs []pending
	nRes := shape.results
	if nRes < 0 {
		nRes = len(p.formalSet(order[0]).results)
	}
	for i := range nRes {
		o := pending{of: p.intern(resultOf(i, nRes))}
		for _, t := range order {
			if fs := p.formalSet(t); i < len(fs.results) {
				o.binds = append(o.binds, portBind{Target: t, Formal: fs.results[i]})
			}
		}
		outs = append(outs, o)
	}
	for _, slot := range slots {
		o := pending{of: slotOf[slot]}
		for _, b := range binds[slot] {
			if f := p.formalSet(b.t).outPos[b.pos]; f != "" {
				o.binds = append(o.binds, portBind{Target: b.t, Formal: f})
			}
		}
		if len(o.binds) > 0 {
			outs = append(outs, o)
		}
	}
	if litOK {
		fs := p.formalSet(lit)
		for _, of := range fs.capOut {
			outs = append(outs, pending{of: of, binds: []portBind{{Target: lit, Formal: fs.outByOf[of]}}})
		}
	}
	for _, of := range unionByOf(order, p, func(fs *formalSet) []string { return fs.globOut }) {
		o := pending{of: of}
		for _, t := range order {
			if f, ok := p.formalSet(t).outByOf[of]; ok {
				o.binds = append(o.binds, portBind{Target: t, Formal: f})
			}
		}
		outs = append(outs, o)
	}
	for i, o := range outs {
		key := callKey + "/actual_out"
		if len(outs) > 1 {
			key += ":" + strconv.Itoa(i)
		}
		s.Out = append(s.Out, Vertex{Key: key, Of: o.of, Parent: callKey, Kind: KindActualOut})
		for _, b := range o.binds {
			b.Actual = key
			s.BindOut = append(s.BindOut, b)
		}
	}
	return s
}

// unionByOf is the union of list(formalSet(t)) over order: the first
// target's list, then each later target's `of`s not seen yet, in order.
func unionByOf(order []Target, p *ports, list func(*formalSet) []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range order {
		for _, of := range list(p.formalSet(t)) {
			if !seen[of] {
				seen[of] = true
				out = append(out, of)
			}
		}
	}
	return out
}

// callShape is what a call expression says about how its values bind: is
// it a method call (x.M(…), the receiver is x), how many params its
// function type has, how many values it passes (f(g()) passes each g
// result), and how many results it has (-1 when its type is unknown).
type callShape struct {
	method  bool
	nParams int
	values  int
	results int
}

func callShapeOf(call *ast.CallExpr, info *types.Info) callShape {
	s := callShape{values: len(call.Args), results: -1}
	var sig *types.Signature
	if info != nil {
		if len(call.Args) == 1 {
			if tup, ok := info.TypeOf(call.Args[0]).(*types.Tuple); ok {
				s.values = tup.Len()
			}
		}
		if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
			if x := info.Selections[sel]; x != nil && x.Kind() == types.MethodVal {
				s.method = true
			}
		}
		if t := info.TypeOf(call.Fun); t != nil {
			sig, _ = t.Underlying().(*types.Signature)
		}
	}
	if sig == nil {
		s.nParams = s.values
		return s
	}
	s.nParams = sig.Params().Len()
	s.results = sig.Results().Len()
	return s
}

// bind maps the call's slots to fs's positional formals (indices into
// fs.inPos):
//
//   - no receiver: call param j binds declared param j;
//   - a method call x.M(…): x (slotRecv) binds the receiver, param j
//     declared param j;
//   - a call with one more param than fs declares — a method expression
//     T.M(s, x), or its thunk reached through a function value: param 0
//     binds the receiver, param j declared param j-1;
//   - otherwise a bound method value (f := s.M; f(x), A8): the
//     function-value operand (slotRecv) binds the receiver, param j declared
//     param j.
//
// Values bind in order, one actual per bound param. Only a variadic call
// passes more values than it has params: the excess collapse onto the last
// param (weak, as Java), which value nParams-1 already binds, so they add
// no actual. A param no value reaches (a variadic call with no excess)
// binds nothing.
func (s callShape) bind(fs *formalSet) map[int]int {
	out := map[int]int{}
	base, off := 0, 0 // declared param d is fs.inPos[base+d] and binds call param d+off
	if fs.recv {
		base = 1
		if !s.method && s.nParams == fs.params+1 {
			off = 1
			out[0] = 0 // a method expression always passes its receiver
		} else {
			out[slotRecv] = 0
		}
	}
	for j := range min(s.values, s.nParams) {
		if d := j - off; d >= 0 && d < fs.params {
			out[j] = base + d
		}
	}
	return out
}

// pruned reports whether unit's call node callKey is not among its kept L3
// nodes. A unit with no L3 result (l3_skipped) has no CFG to prune by: its
// L1 call nodes all stand.
func (p *ports) pruned(unit dataflow.L3Key, callKey string) bool {
	if _, ok := p.in.L3[unit]; !ok {
		return false
	}
	return !p.keptKeys(unit)[callKey]
}

// keptKeys is the set of unit's kept L3 node keys (empty with no result).
func (p *ports) keptKeys(unit dataflow.L3Key) map[string]bool {
	keep, ok := p.kept[unit]
	if !ok {
		keep = map[string]bool{}
		for _, n := range p.in.L3[unit].Nodes {
			keep[n.Key] = true
		}
		p.kept[unit] = keep
	}
	return keep
}

// callExpr returns the call expression of unit's call key, paired by Lparen
// as BuildNodes pairs it.
func (p *ports) callExpr(unit dataflow.L3Key, callKey string) *ast.CallExpr {
	m, ok := p.calls[unit]
	if !ok {
		u := p.units[unit]
		byLparen := make(map[token.Pos]string, len(u.Calls))
		for _, c := range u.Calls {
			byLparen[c.Lparen] = c.Key
		}
		m = map[string]*ast.CallExpr{}
		for _, c := range astwalk.Calls(fnBody(u.Fn)) {
			if k, ok := byLparen[c.Expr.Lparen]; ok {
				m[k] = c.Expr
			}
		}
		p.calls[unit] = m
	}
	return m[callKey]
}

// siteTargets is the dispatch targets of unit's call callKey, plus, for a
// literal called in place, that literal's callable: L2 resolves no FuncLit
// callee, so without VTA (no_vta) only the syntax names it, and its
// captures must still bind at the call.
func (p *ports) siteTargets(unit dataflow.L3Key, callKey string) []Target {
	ts := p.d.targets(unit, callKey)
	if call := p.callExpr(unit, callKey); call != nil {
		if fl, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
			if k, ok := p.byFn[fl]; ok {
				t := Target{Callable: k}
				if i, found := slices.BinarySearchFunc(ts, t, compareTarget); !found {
					ts = slices.Insert(slices.Clone(ts), i, t)
				}
			}
		}
	}
	return ts
}

// captureSites returns the capture-site actuals of unit u (A6): for every
// outermost function literal of its body that is not called in place and
// captures a variable, `<lit-key>/actual_in:<i>` per capture and
// `<lit-key>/actual_out[:<i>]` per written capture (`of`
// `<capture>:<name>`), with Parent the host: the innermost CFG node holding
// the literal (hostOf). <lit-key> is the literal's line:col, taking the next
// free "/n" among taken (dataflow.NextFreeKey, as BodyNodes keys a
// statement), so it never equals a body key. A literal whose host the CFG
// pruned is dead and has none.
//
// A unit with no L3 result (l3_skipped) has no CFG to host a literal: its
// host is the innermost L1 call node whose call holds the literal (`once.Do(
// func(){…})`), else @entry (ruling, Task 12 review). Its port wiring is
// pass-through (wire).
func (p *ports) captureSites(u dataflow.Unit, taken map[string]bool) []Vertex {
	var out []Vertex
	for _, cs := range p.capSites(u, taken) {
		out = append(out, cs.vertices()...)
	}
	return out
}

// capSite is one capture site: its key, its host, its literal, and each
// capture (in actual_in order) with its actual_in key and, when the literal writes it,
// its actual_out key.
type capSite struct {
	key, host string
	lit       *ast.FuncLit
	caps      []types.Object
	ofs       []string
	in, out   []string // per capture; out[i] == "" when not written
}

func (cs capSite) vertices() []Vertex {
	var vs []Vertex
	for i, of := range cs.ofs {
		vs = append(vs, Vertex{Key: cs.in[i], Of: of, Parent: cs.host, Kind: KindActualIn})
	}
	for i, of := range cs.ofs {
		if cs.out[i] != "" {
			vs = append(vs, Vertex{Key: cs.out[i], Of: of, Parent: cs.host, Kind: KindActualOut})
		}
	}
	return vs
}

// capSites is captureSites with each site's capture objects.
func (p *ports) capSites(u dataflow.Unit, taken map[string]bool) []capSite {
	_, analysed := p.in.L3[u.Key]
	body := fnBody(u.Fn)
	if body == nil || u.Info == nil {
		return nil
	}
	direct := map[*ast.FuncLit]bool{}
	for _, c := range astwalk.Calls(body) {
		if fl, ok := ast.Unparen(c.Expr.Fun).(*ast.FuncLit); ok {
			direct[fl] = true
		}
	}
	var all []dataflow.Node // every body node, pruned ones included; built once
	kept := p.keptKeys(u.Key)
	var out []capSite
	for _, l := range astwalk.Literals(body) {
		if direct[l.Lit] {
			continue
		}
		caps := dataflow.FreeVars(l.Lit, u.Info)
		if len(caps) == 0 {
			continue
		}
		var host string
		if analysed {
			if all == nil {
				all, _ = dataflow.BuildNodes(p.in.Fset, body, u.Calls)
			}
			h, ok := hostOf(all, l.Lit)
			if !ok || !kept[h] {
				continue
			}
			host = h
		} else {
			host = p.skippedHost(u, l.Lit)
		}
		written := map[types.Object]bool{}
		for _, path := range dataflow.CaptureWrites(l.Lit, u.Info, p.in.K) {
			written[path.Base] = true
		}
		nOut := 0
		for _, v := range caps {
			if written[v] {
				nOut++
			}
		}
		pos := p.in.Fset.PositionFor(l.Lit.Pos(), false)
		cs := capSite{key: dataflow.NextFreeKey(v2.LocalID(pos.Line, pos.Column), taken), host: host, lit: l.Lit, caps: caps}
		j := 0
		for i, v := range caps {
			cs.ofs = append(cs.ofs, ofCapture+v.Name())
			cs.in = append(cs.in, cs.key+"/actual_in:"+strconv.Itoa(i))
			o := ""
			if written[v] {
				o = cs.key + "/actual_out"
				if nOut > 1 {
					o += ":" + strconv.Itoa(j)
				}
				j++
			}
			cs.out = append(cs.out, o)
		}
		out = append(out, cs)
	}
	return out
}

// skippedHost is the host of a capture site in a unit with no L3 result:
// the innermost of its L1 call nodes whose call expression holds lit, else
// @entry.
func (p *ports) skippedHost(u dataflow.Unit, lit *ast.FuncLit) string {
	host, best := dataflow.EntryKey, token.Pos(-1)
	for _, c := range u.Calls {
		call := p.callExpr(u.Key, c.Key)
		if call == nil || call.Pos() > lit.Pos() || lit.End() > call.End() {
			continue
		}
		if span := call.End() - call.Pos(); best < 0 || span < best {
			host, best = c.Key, span
		}
	}
	return host
}

// hostOf is the key of the innermost CFG node of nodes (BuildNodes' output,
// before pruning) whose span holds lit. A satellite call node is no CFG
// node; nor is a deferred call node the host of a literal in its operands:
// they are evaluated at the defer statement, whose DefUse uses the
// literal's captures.
func hostOf(nodes []dataflow.Node, lit *ast.FuncLit) (string, bool) {
	best, found := dataflow.Node{}, false
	for _, n := range nodes {
		if n.Parent != "" || n.Deferred || !n.Pos.IsValid() || n.Pos > lit.Pos() || lit.End() > n.End {
			continue
		}
		if !found || n.End-n.Pos < best.End-best.Pos {
			best, found = n, true
		}
	}
	return best.Key, found
}

// vertices returns every L4 vertex of unit k, sorted by key: its formals,
// the actuals of each resolved, unpruned call site (siteTargets), and its
// capture sites.
func (p *ports) vertices(k dataflow.L3Key) []Vertex {
	u, ok := p.units[k]
	if !ok {
		return nil
	}
	in, out := p.formals(Target{Callable: k})
	vs := slices.Concat(in, out)
	taken := make(map[string]bool, len(u.Calls))
	for _, c := range u.Calls {
		taken[c.Key] = true
		ai, ao := p.actuals(k, c.Key, p.siteTargets(k, c.Key))
		vs = append(vs, ai...)
		vs = append(vs, ao...)
	}
	for key := range p.keptKeys(k) {
		taken[key] = true
	}
	vs = append(vs, p.captureSites(u, taken)...)
	slices.SortFunc(vs, func(a, b Vertex) int { return strings.Compare(a.Key, b.Key) })
	return vs
}
