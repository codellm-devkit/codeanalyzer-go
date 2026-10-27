package dataflow_test

import (
	"go/ast"
	"go/types"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

const kTest = 3

// spells is the sorted spellings of ps.
func spells(ps []dataflow.Path) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Spell)
	}
	sort.Strings(out)
	return out
}

func wantSet(t *testing.T, what string, got []dataflow.Path, want ...string) {
	t.Helper()
	sort.Strings(want)
	if want == nil {
		want = []string{}
	}
	if g := spells(got); !reflect.DeepEqual(g, want) {
		t.Errorf("%s = %v, want %v", what, g, want)
	}
}

func wantDU(t *testing.T, what string, du dataflow.DefUse, defs, weak, uses []string) {
	t.Helper()
	wantSet(t, what+" Defs", du.Defs, defs...)
	wantSet(t, what+" WeakDefs", du.WeakDefs, weak...)
	wantSet(t, what+" Uses", du.Uses, uses...)
}

func ss(s ...string) []string { return s }

// nodes builds the body nodes of hermetic function fn with its call keys.
func (fx *duFixture) nodes(t *testing.T, fn string) (map[string]dataflow.Node, map[ast.Node]string, []types.Object) {
	t.Helper()
	d, ok := fx.decls[fn]
	if !ok {
		t.Fatalf("no function %s", fn)
	}
	nodes, keyOf := dataflow.BuildNodes(fx.fset, d.Body, gateCallKeys(fx.fset, d.Body))
	byKey := map[string]dataflow.Node{}
	for _, n := range nodes {
		byKey[n.Key] = n
	}
	return byKey, keyOf, dataflow.EntryBases(d, fx.info)
}

// du is DefUseOf the node of fn keyed at the first occurrence of sub (plus
// delta bytes) in duSrc.
func (fx *duFixture) du(t *testing.T, fn, sub string, delta int) (dataflow.DefUse, string) {
	t.Helper()
	byKey, keyOf, bases := fx.nodes(t, fn)
	key := fx.keyAt(t, sub, delta)
	n, ok := byKey[key]
	if !ok {
		t.Fatalf("%s: no node %s (%q)", fn, key, sub)
	}
	return dataflow.DefUseOf(n, fx.info, kTest, bases, fx.decls[fn], keyOf), key
}

// flowDU is DefUseOf the node keyed key of flow-fixture function fn.
func flowDU(t *testing.T, fn, key string) dataflow.DefUse {
	t.Helper()
	fx := loadFlow(t)
	_, byKey, keyOf := build(t, fn)
	n, ok := byKey[key]
	if !ok {
		t.Fatalf("%s: no node %s; have %v", fn, key, keysOf(byKey))
	}
	d := fx.decl(t, fn)
	return dataflow.DefUseOf(n, fx.info, kTest, dataflow.EntryBases(d, fx.info), d, keyOf)
}

func TestDefUse_AssignStrongDef(t *testing.T) {
	// Bump: y = y + 1.
	wantDU(t, "Bump 148:3", flowDU(t, "Bump", "148:3"), ss("y"), nil, ss("y"))
	// ForLoop header: init and post fold in.
	wantDU(t, "ForLoop 27:2", flowDU(t, "ForLoop", "27:2"), ss("i"), nil, ss("i", "n"))
	// s += i: compound assignment defines and uses.
	wantDU(t, "ForLoop 28:3", flowDU(t, "ForLoop", "28:3"), ss("s"), nil, ss("i", "s"))
	wantDU(t, "IfElse 16:2", flowDU(t, "IfElse", "16:2"), ss("b"), nil, ss("a", "b"))
	wantDU(t, "Range 46:2", flowDU(t, "Range", "46:2"), ss("x"), nil, ss("xs"))
	wantDU(t, "Switch 55:2", flowDU(t, "Switch", "55:2"), nil, nil, ss("a"))
	wantDU(t, "Switch 56:2", flowDU(t, "Switch", "56:2"), nil, nil, nil)
	wantDU(t, "GotoBack 112:2", flowDU(t, "GotoBack", "112:2"), ss("i"), nil, ss("i"))
	wantDU(t, "Bump 150:2", flowDU(t, "Bump", "150:2"), nil, nil, ss("y"))
	wantDU(t, "Labeled 97:5", flowDU(t, "Labeled", "97:5"), nil, nil, nil)
	wantDU(t, "Bump @exit", flowDU(t, "Bump", dataflow.ExitKey), nil, nil, nil)

	fx := loadDU(t)
	du, _ := fx.du(t, "Assign", "k++", 0)
	wantDU(t, "k++", du, ss("k"), nil, ss("k"))
	du, _ = fx.du(t, "Assign", "var z, w", 0)
	wantDU(t, "var z, w", du, ss("w", "z"), nil, ss("k", "v"))

	// Mixed := : b is new, a is the parameter, re-assigned.
	du, _ = fx.du(t, "Mixed", "b, a := 1, 2", 0)
	wantDU(t, "b, a :=", du, ss("a", "b"), nil, nil)
	param := fx.info.Defs[fx.decls["Mixed"].Type.Params.List[0].Names[0]]
	for _, d := range du.Defs {
		if d.Spell == "a" && d.Base != param {
			t.Errorf("b, a := : def a Base = %v, want the parameter", d.Base)
		}
	}
}

func TestDefUse_ShadowedBasesDiffer(t *testing.T) {
	outer := flowDU(t, "Shadow", "155:2").Defs
	inner := flowDU(t, "Shadow", "157:3").Defs
	ret := flowDU(t, "Shadow", "160:2").Uses
	if len(outer) != 1 || len(inner) != 1 || len(ret) != 1 {
		t.Fatalf("Shadow: outer=%v inner=%v ret=%v", spells(outer), spells(inner), spells(ret))
	}
	if outer[0].Spell != "x" || inner[0].Spell != "x" || ret[0].Spell != "x" {
		t.Fatalf("Shadow: spellings %v %v %v", spells(outer), spells(inner), spells(ret))
	}
	if outer[0].Base == inner[0].Base {
		t.Errorf("Shadow: inner x has the outer x's base")
	}
	if ret[0].Base != outer[0].Base {
		t.Errorf("Shadow: return x is not the outer x")
	}
	if inner[0].Covers(ret[0]) || ret[0].Covers(inner[0]) {
		t.Errorf("Shadow: inner x interferes with the outer x")
	}
}

func TestDefUse_FieldDefIsPrefixDef(t *testing.T) {
	fx := loadDU(t)
	du, _ := fx.du(t, "Assign", "x.f = v", 0)
	wantDU(t, "x.f = v", du, ss("x.f"), nil, ss("v"))
	du, _ = fx.du(t, "Assign", "m[k] = v", 0)
	wantDU(t, "m[k] = v", du, nil, ss("m[*]"), ss("k", "m", "v"))
	du, _ = fx.du(t, "Assign", "*q = v", 0)
	wantDU(t, "*q = v", du, nil, ss("q.*"), ss("q", "v"))
	du, _ = fx.du(t, "Assign", "x.f += 1", 0)
	wantDU(t, "x.f += 1", du, ss("x.f"), nil, ss("x.f"))
	du, _ = fx.du(t, "Assign", "m[k]++", 0)
	wantDU(t, "m[k]++", du, nil, ss("m[*]"), ss("k", "m", "m[*]"))

	// Fields: a write deeper than k is a weak def of the truncated path; the
	// write goes through the pointer p (p.f is (*p).f), so it reads p.
	wantDU(t, "Fields 176:2", flowDU(t, "Fields", "176:2"), nil, ss("p.f.g.*"), ss("p", "v"))
	wantDU(t, "Fields 177:2", flowDU(t, "Fields", "177:2"), nil, nil, ss("p.f.g.x"))
	// A statement whose LHS starts with a call defines no path; the call is
	// its satellite, so the host carries the call's uses and weak defs.
	wantDU(t, "CallLHS 211:2/2", flowDU(t, "CallLHS", "211:2/2"), nil, nil, ss("v"))
}

func TestDefUse_CallWeakDefsRefArgs(t *testing.T) {
	fx := loadDU(t)
	du, _ := fx.du(t, "Calls", "mut(&req", 0)
	// t is a value T, whose field s is a slice: it may hold a reference, so
	// the callee may write through it (D1 gap 2).
	wantDU(t, "mut(...)", du, nil, ss("m.*", "p.*", "req.*", "s.*", "t.*"), ss("m", "p", "req", "s", "t"))
	du, _ = fx.du(t, "Calls", "t.Ptr()", 0)
	wantDU(t, "t.Ptr()", du, nil, ss("t.*"), ss("t"))
	du, _ = fx.du(t, "Calls", "p.Ptr()", 0)
	wantDU(t, "p.Ptr()", du, nil, ss("p.*"), ss("p"))
	du, _ = fx.du(t, "Calls", "t.Val()", 0)
	wantDU(t, "t.Val()", du, nil, ss("t.*"), ss("t")) // value receiver holding T.s (D1 gap 2)
	du, _ = fx.du(t, "Calls", "n := len(s)", 0)
	wantDU(t, "n := len(s)", du, ss("n"), nil, ss("s"))
	du, _ = fx.du(t, "Calls", "s = append(s, n)", 0)
	// append may write into s's spare capacity: s[*] weak, besides the s def.
	wantDU(t, "s = append(s, n)", du, ss("s"), ss("s[*]"), ss("n", "s"))
	du, _ = fx.du(t, "Calls", "copy(s, s)", 0)
	wantDU(t, "copy(s, s)", du, nil, ss("s[*]"), ss("s"))
	du, _ = fx.du(t, "Calls", "_ = T(t)", 0)
	wantDU(t, "_ = T(t)", du, nil, nil, ss("t"))
	du, _ = fx.du(t, "Calls", "r := pid(&req)", 0)
	wantDU(t, "r := pid(&req)", du, ss("r"), ss("req.*"), ss("req"))

	// Satellites carry nothing; the host has the nested calls' flow.
	for _, k := range []string{"196:7", "196:9"} {
		wantDU(t, "Nested "+k, flowDU(t, "Nested", k), nil, nil, nil)
	}
	wantDU(t, "Nested 196:2", flowDU(t, "Nested", "196:2"), ss("x"), nil, ss("y"))
	wantDU(t, "Chain 234:2/2", flowDU(t, "Chain", "234:2/2"), nil, nil, nil)
	wantDU(t, "Chain 234:2", flowDU(t, "Chain", "234:2"), nil, ss("s.*"), ss("s"))
	// A bare panic(i) is a call node of its own.
	wantDU(t, "PanicLast 413:2", flowDU(t, "PanicLast", "413:2"), nil, nil, ss("i"))
	wantDU(t, "PanicLast 412:2", flowDU(t, "PanicLast", "412:2"), nil, ss("xs[*]"), ss("i", "xs"))
	// A closure expression uses what it captures, not its body's locals.
	wantDU(t, "Closure 216:2", flowDU(t, "Closure", "216:2"), ss("h"), nil, ss("x"))
	wantDU(t, "Closure 219:2", flowDU(t, "Closure", "219:2"), nil, nil, ss("h"))

	// go: the call's receiver, arguments and calls nested in them.
	du, _ = fx.du(t, "Goes", "mut2(s, id(x))", 0)
	wantDU(t, "go mut2(s, id(x))", du, nil, ss("s.*"), ss("s", "x"))
	du, _ = fx.du(t, "Goes", "func() { sink(x) }()", 0)
	wantDU(t, "go func(){sink(x)}()", du, nil, nil, ss("x"))
	wantDU(t, "Spawn 202:5", flowDU(t, "Spawn", "202:5"), nil, nil, ss("x"))
	// The calls in a go statement's arguments are satellites now (L1 records
	// them): their uses stay on the go node, the satellites carry nothing.
	wantDU(t, "Spawn 202:10", flowDU(t, "Spawn", "202:10"), nil, nil, nil)
	wantDU(t, "GoArgs 534:5", flowDU(t, "GoArgs", "534:5"), nil, nil, ss("x"))
	wantDU(t, "GoArgs 534:7", flowDU(t, "GoArgs", "534:7"), nil, nil, nil)
}

func TestDefUse_ChanSendRecv(t *testing.T) {
	wantDU(t, "Chan 182:2", flowDU(t, "Chan", "182:2"), nil, ss("ch[*]"), ss("ch", "v"))
	wantDU(t, "Chan 183:2", flowDU(t, "Chan", "183:2"), ss("w"), nil, ss("ch", "ch[*]"))
	wantDU(t, "Select2 82:2", flowDU(t, "Select2", "82:2"), nil, nil, nil)
	wantDU(t, "Select2 83:2", flowDU(t, "Select2", "83:2"), ss("x"), nil, ss("a", "a[*]"))
	wantDU(t, "Select2 85:2", flowDU(t, "Select2", "85:2"), nil, ss("b[*]"), ss("b"))

	fx := loadDU(t)
	du, _ := fx.du(t, "Chans", "for x := range ch", 0)
	wantDU(t, "range ch", du, ss("x"), nil, ss("ch", "ch[*]"))
	du, _ = fx.du(t, "Chans", "out <- x", 0)
	wantDU(t, "out <- x", du, nil, ss("out[*]"), ss("out", "x"))
	du, _ = fx.du(t, "Chans", "case y, ok", 0)
	wantDU(t, "case y, ok := <-ch", du, ss("ok", "y"), nil, ss("ch", "ch[*]"))
	du, _ = fx.du(t, "Chans", "case <-ch", 0)
	wantDU(t, "case <-ch", du, nil, nil, ss("ch", "ch[*]"))
	du, _ = fx.du(t, "Chans", "sink(<-ch)", 0)
	wantDU(t, "sink(<-ch)", du, nil, nil, ss("ch", "ch[*]"))
}

func TestDefUse_TypeSwitchGuardDefinesClauseVars(t *testing.T) {
	hdr := flowDU(t, "TypeSwitch", "69:2")
	wantSet(t, "TypeSwitch 69:2 Uses", hdr.Uses, "v")
	use := flowDU(t, "TypeSwitch", "71:3").Uses
	if len(use) != 1 || use[0].Spell != "t" {
		t.Fatalf("TypeSwitch 71:3 Uses = %v, want [t]", spells(use))
	}
	found := false
	for _, d := range hdr.Defs {
		if d.Spell != "t" {
			t.Errorf("TypeSwitch header def %q, want only t", d.Spell)
		}
		found = found || d.Base == use[0].Base
	}
	if !found {
		t.Errorf("TypeSwitch: the case-int clause's t is not defined by the guard; defs %v", spells(hdr.Defs))
	}
}

func TestDefUse_DeferRegistrationCarriesArgs(t *testing.T) {
	// EarlyReturnDefer: defer work(g(a)).
	wantDU(t, "EarlyReturnDefer 127:2", flowDU(t, "EarlyReturnDefer", "127:2"), nil, ss("<defer>:127:8"), ss("a"))
	wantDU(t, "EarlyReturnDefer 127:8", flowDU(t, "EarlyReturnDefer", "127:8"), nil, nil, ss("<defer>:127:8"))
	wantDU(t, "EarlyReturnDefer 127:13", flowDU(t, "EarlyReturnDefer", "127:13"), nil, nil, nil)
	wantDU(t, "EarlyReturnDefer 126:2", flowDU(t, "EarlyReturnDefer", "126:2"), nil, ss("<defer>:126:8"), nil)
	wantDU(t, "EarlyReturnDefer 126:8", flowDU(t, "EarlyReturnDefer", "126:8"), nil, nil, ss("<defer>:126:8"))

	fx := loadDU(t)
	// defer mu.Unlock(): the receiver is evaluated at registration; the
	// deferred call writes through it, reads the named results and (I7)
	// reads the state behind the receiver at exit (mu.*).
	reg, _ := fx.du(t, "Defers", "defer mu.Unlock()", 0)
	call, ck := fx.du(t, "Defers", "mu.Unlock()", 0)
	d := "<defer>:" + ck
	wantDU(t, "defer mu.Unlock()", reg, nil, ss(d), ss("mu"))
	wantDU(t, "mu.Unlock() (deferred)", call, nil, ss("mu.*"), ss(d, "err", "mu.*", "n"))

	// defer func(){…}(): nothing is evaluated at registration but the func
	// value; the deferred call uses what the closure captures and (D1 gap 1)
	// weak-defines what it writes of them: err = nil, and err.* (an
	// interface may hold a reference: hostWrites).
	reg, _ = fx.du(t, "Defers", "defer func() {", 0)
	call, ck = fx.du(t, "Defers", "defer func() {", len("defer "))
	d = "<defer>:" + ck
	wantDU(t, "defer func(){…}", reg, nil, ss(d), nil)
	wantDU(t, "func(){…}() (deferred)", call, nil, ss("err", "err.*"), ss(d, "err", "n", "x"))

	reg, _ = fx.du(t, "Defers", "defer mut2(s, x)", 0)
	call, ck = fx.du(t, "Defers", "defer mut2(s, x)", len("defer "))
	d = "<defer>:" + ck
	wantDU(t, "defer mut2(s, x)", reg, nil, ss(d), ss("s", "x"))
	wantDU(t, "mut2(s, x) (deferred)", call, nil, ss("s.*"), ss(d, "err", "n", "s.*"))

	// A bare return reads the named results; a return with operands assigns
	// them (the deferred calls run after that assignment).
	ret, _ := fx.du(t, "Defers", "return\n", 0)
	wantDU(t, "return", ret, nil, nil, ss("err", "n"))
	ret, _ = fx.du(t, "NamedRet", "\treturn x\n", 1)
	wantDU(t, "return x", ret, ss("n"), nil, ss("x"))
}

func TestDefUse_EntryDefinesParamsReceiverNamedResults(t *testing.T) {
	fx := loadDU(t)
	entry := fx.decls["Entry"]
	bases := dataflow.EntryBases(entry, fx.info)
	var names []string
	for _, b := range bases {
		names = append(names, b.Name())
	}
	// The signature by position (one file), then package-level vars by
	// (pkgpath, name) — FileSet bases of other files are load-order dependent.
	if want := ss("r", "a", "b", "n", "err", "V", "W", "G"); !reflect.DeepEqual(names, want) {
		t.Errorf("EntryBases(Entry) = %v, want %v", names, want)
	}
	byKey, keyOf, _ := fx.nodes(t, "Entry")
	du := dataflow.DefUseOf(byKey[dataflow.EntryKey], fx.info, kTest, bases, entry, keyOf)
	wantDU(t, "Entry @entry", du,
		ss("example.com/lib::V", "example.com/lib::W", "example.com/p::G", "r", "a", "b", "n", "err"), nil, nil)

	// G = a defines the package-level path.
	g, _ := fx.du(t, "Entry", "G = a", 0)
	wantDU(t, "G = a", g, ss("example.com/p::G"), nil, ss("a"))
	l, _ := fx.du(t, "Entry", "_ = lib.V.F + lib.W", 0)
	wantDU(t, "_ = lib.V.F + lib.W", l, nil, nil, ss("example.com/lib::V.F", "example.com/lib::W"))

	// The closure's captures are its free variables: a and r, not its own k
	// (which the inner closure captures).
	var lit *ast.FuncLit
	ast.Inspect(entry.Body, func(n ast.Node) bool {
		if f, ok := n.(*ast.FuncLit); ok && lit == nil {
			lit = f
		}
		return lit == nil
	})
	names = nil
	for _, b := range dataflow.EntryBases(lit, fx.info) {
		names = append(names, b.Name())
	}
	if want := ss("r", "a"); !reflect.DeepEqual(names, want) {
		t.Errorf("EntryBases(closure) = %v, want %v", names, want)
	}
	h, _ := fx.du(t, "Entry", "h := func", 0)
	wantDU(t, "h := func…", h, ss("h"), ss("r.*"), ss("a", "r"))

	// A flow-fixture function with no package vars: params only.
	wantDU(t, "Bump @entry", flowDU(t, "Bump", dataflow.EntryKey), ss("n"), nil, nil)
}

// TestDefUse_Invariants runs DefUseOf over every node of every flow-fixture
// function: no panic, satellites and @exit are empty, strong defs carry no
// wildcard, and @entry has only defs.
func TestDefUse_Invariants(t *testing.T) {
	fx := loadFlow(t)
	for fn, d := range fx.decls {
		nodes, keyOf := dataflow.BuildNodes(fx.fset, d.Body, fx.callKeys(t, fn))
		bases := dataflow.EntryBases(d, fx.info)
		for _, n := range nodes {
			du := dataflow.DefUseOf(n, fx.info, kTest, bases, d, keyOf)
			empty := len(du.Defs)+len(du.WeakDefs)+len(du.Uses) == 0
			if (n.Parent != "" || n.Kind == dataflow.KindExit) && !empty {
				t.Errorf("%s %s: satellite / @exit has def/use %+v", fn, n.Key, du)
			}
			if n.Kind == dataflow.KindEntry && len(du.WeakDefs)+len(du.Uses) != 0 {
				t.Errorf("%s @entry: weak defs / uses %+v", fn, du)
			}
			if n.Deferred && len(du.Defs) != 0 {
				t.Errorf("%s %s: deferred call strong-defines %v (R11)", fn, n.Key, spells(du.Defs))
			}
			for what, ps := range map[string][]dataflow.Path{"Defs": du.Defs, "WeakDefs": du.WeakDefs, "Uses": du.Uses} {
				if !sort.SliceIsSorted(ps, func(i, j int) bool { return ps[i].Spell < ps[j].Spell }) {
					t.Errorf("%s %s: %s not sorted by spelling: %v", fn, n.Key, what, ps)
				}
			}
			for _, p := range du.Defs {
				if strings.Contains(p.Spell, "*") {
					t.Errorf("%s %s: strong def %q has a wildcard", fn, n.Key, p.Spell)
				}
			}
		}
	}
}

// TestDefUse_FoldedLoopDefsAreWeak locks the for/range header rule: the
// header node runs on loop entry and on every back edge, so only a variable
// defined by both Init and Post is a strong def there; a range with `=` may
// run zero times, so its Key/Value are weak.
func TestDefUse_FoldedLoopDefsAreWeak(t *testing.T) {
	fx := loadDU(t)
	du, _ := fx.du(t, "ForPost", "for i2 := 0", 0)
	wantDU(t, "ForPost header", du, ss("i2"), ss("x"), ss("i2"))
	du, _ = fx.du(t, "ForInit", "for i, v := 0, 0", 0)
	wantDU(t, "ForInit header", du, ss("i"), ss("v"), ss("i", "n"))
	du, _ = fx.du(t, "RangeAssign", "for _, last = range a", 0)
	wantDU(t, "RangeAssign header", du, nil, ss("last"), ss("a"))
	// Both Init and Post define i: strong (flow fixture).
	wantDU(t, "Bump 147:2", flowDU(t, "Bump", "147:2"), ss("i"), nil, ss("i", "n"))
}

func TestDefUse_ReferenceTypedReceiverIsWeakDefined(t *testing.T) {
	fx := loadDU(t)
	du, _ := fx.du(t, "Recv", "w.Write(data)", 0)
	wantDU(t, "w.Write(data)", du, nil, ss("data.*", "w.*"), ss("data", "w"))
	du, _ = fx.du(t, "Recv", "h.Set(", 0)
	wantDU(t, "h.Set(...)", du, nil, ss("h.*"), ss("h"))
	du, _ = fx.du(t, "Recv", "\tt.Val()\n}\n\nfunc IfShadow", 1)
	wantDU(t, "t.Val() value struct", du, nil, ss("t.*"), ss("t")) // T.s is a slice (D1 gap 2)
}

func TestDefUse_IfInitShadows(t *testing.T) {
	fx := loadDU(t)
	du, _ := fx.du(t, "IfShadow", "if x := id(x)", 0)
	wantDU(t, "IfShadow header", du, ss("x"), nil, ss("x", "x"))
	param := fx.info.Defs[fx.decls["IfShadow"].Type.Params.List[0].Names[0]]
	if du.Defs[0].Base == param {
		t.Errorf("if-init x has the parameter's base")
	}
	// Uses are sorted by (spelling, base position): the parameter first.
	if du.Uses[0].Base != param || du.Uses[1].Base != du.Defs[0].Base {
		t.Errorf("IfShadow header uses: want [param x, init x], got bases %v, %v", du.Uses[0].Base, du.Uses[1].Base)
	}
}

// TestDefUse_SubsliceArgs (C1): a slice expression passed to a callee (or as
// copy's/clear's destination) weak-defines its base's elements, base[*]; the
// call still reads the base and the slice bounds.
func TestDefUse_SubsliceArgs(t *testing.T) {
	wantDU(t, "r.Read(buf[:n])", flowDU(t, "SubsliceRead", "487:2"), nil, ss("buf[*]", "r.*"), ss("buf", "n", "r"))
	wantDU(t, "readFull(r, arr[:])", flowDU(t, "ArraySliceRead", "494:2"), nil, ss("arr[*]", "r.*"), ss("arr", "r"))
	wantDU(t, "copy(dst[i:], src)", flowDU(t, "CopyInto", "501:2"), nil, ss("dst[*]"), ss("dst", "i", "src"))
	wantDU(t, "clear(m)", flowDU(t, "ClearAll", "507:2"), nil, ss("m[*]"), ss("m"))
	wantDU(t, "clear(s[:n])", flowDU(t, "ClearAll", "508:2"), nil, ss("s[*]"), ss("n", "s"))
}

// firstLit is the first function literal in fn's body (outermost first).
func firstLit(t *testing.T, fn string) *ast.FuncLit {
	t.Helper()
	var lit *ast.FuncLit
	ast.Inspect(loadFlow(t).decl(t, fn).Body, func(n ast.Node) bool {
		if f, ok := n.(*ast.FuncLit); ok && lit == nil {
			lit = f
		}
		return lit == nil
	})
	if lit == nil {
		t.Fatalf("%s: no function literal", fn)
	}
	return lit
}

// TestDefUse_CaptureWrites (D1 gap 1): CaptureWrites is what a literal or a
// literal nested in it may write of the literal's free variables; the host
// weak-defines it, and a strong def of a captured-and-written base is weak.
func TestDefUse_CaptureWrites(t *testing.T) {
	fx := loadFlow(t)
	// CaptureNested's f: the nested literal's x = y (y is f's own), and the
	// fill(buf) write through buf; z is only read.
	wantSet(t, "CaptureWrites(CaptureNested f)", dataflow.CaptureWrites(firstLit(t, "CaptureNested"), fx.info, kTest), "buf.*", "x")
	wantSet(t, "CaptureWrites(Closure h)", dataflow.CaptureWrites(firstLit(t, "Closure"), fx.info, kTest))

	wantDU(t, "CaptureNested 613:2", flowDU(t, "CaptureNested", "613:2"), ss("f"), ss("buf.*", "x"), ss("buf", "x", "z"))
	// x, z := 0, 0: x is written by the literal, so its def is weak; z stays.
	wantDU(t, "CaptureNested 612:2", flowDU(t, "CaptureNested", "612:2"), ss("z"), ss("x"), nil)
	wantDU(t, "CaptureWrite 564:2", flowDU(t, "CaptureWrite", "564:2"), nil, ss("x"), ss("x"))
	wantDU(t, "CaptureThenReassign 573:2", flowDU(t, "CaptureThenReassign", "573:2"), nil, ss("x"), nil)
	wantDU(t, "OnceDo 588:2", flowDU(t, "OnceDo", "588:2"), nil, ss("cfg", "once.*"), ss("cfg", "once"))
	// The deferred closure call node is the host, not the registration:
	// err = wrapErr(err) writes err, and err.* through the interface argument.
	wantDU(t, "DeferWrap 557:2", flowDU(t, "DeferWrap", "557:2"), nil, ss("<defer>:557:8"), nil)
	wantDU(t, "DeferWrap 557:8", flowDU(t, "DeferWrap", "557:8"), nil, ss("err", "err.*"), ss("<defer>:557:8", "err"))
	wantDU(t, "DeferWrap 555:2", flowDU(t, "DeferWrap", "555:2"), nil, ss("err"), nil)
	// go func(){ x = <-ch }(): the go node is the host; ch, a channel, may
	// hold a reference, so it weak-defines ch.* too (hostWrites).
	wantDU(t, "CaptureGo 627:5", flowDU(t, "CaptureGo", "627:5"), nil, ss("ch.*", "x"), ss("ch", "x"))
	wantDU(t, "CaptureGo 626:2", flowDU(t, "CaptureGo", "626:2"), nil, ss("x"), nil)
}

// TestDefUse_AppendOntoIsShallow (#28): s = append(s, n) strong-defines s
// Shallow (it kills s, not s[*]) besides append's s[*] weak def.
func TestDefUse_AppendOntoIsShallow(t *testing.T) {
	du := flowDU(t, "AppendKeepsElems", "604:2")
	wantDU(t, "AppendKeepsElems 604:2", du, ss("s"), ss("s[*]"), ss("n", "s"))
	wantSet(t, "AppendKeepsElems 604:2 Shallow", du.Shallow, "s")
	// out := append(dst[:0], x): the LHS is not the destination.
	wantSet(t, "AppendReuse 541:2 Shallow", flowDU(t, "AppendReuse", "541:2").Shallow)
}

// TestDefUse_DeferOperandWrites (D1 gap 3): a node writing a base the
// registered defer fill(b) takes as an operand also weak-defines its pseudo
// path; the registration and the deferred call node are unchanged.
func TestDefUse_DeferOperandWrites(t *testing.T) {
	wantDU(t, "DeferReassign 596:2", flowDU(t, "DeferReassign", "596:2"), nil, ss("<defer>:595:8", "b[*]"), ss("b", "s"))
	wantDU(t, "DeferReassign 597:2", flowDU(t, "DeferReassign", "597:2"), ss("b"), ss("<defer>:595:8"), ss("o"))
	wantDU(t, "DeferReassign 595:2", flowDU(t, "DeferReassign", "595:2"), nil, ss("<defer>:595:8"), ss("b"))
	wantDU(t, "DeferReassign 595:8", flowDU(t, "DeferReassign", "595:8"), nil, ss("b.*"), ss("<defer>:595:8", "b.*"))
	wantDU(t, "DeferReassign @entry", flowDU(t, "DeferReassign", dataflow.EntryKey), ss("b", "o", "s"), nil, nil)
}

// TestMayHoldRef (D1 gap 2): the reference kinds, and structs and arrays
// whose fields or elements transitively may hold one, through named types,
// cycle-safe; a reference-free struct or array, a string and a basic do not.
func TestMayHoldRef(t *testing.T) {
	i := types.Typ[types.Int]
	sl := types.NewSlice(i)
	st := func(ts ...types.Type) *types.Struct {
		var fs []*types.Var
		for n, ft := range ts {
			fs = append(fs, types.NewField(0, nil, "f"+string(rune('a'+n)), ft, false))
		}
		return types.NewStruct(fs, nil)
	}
	named := func(name string, u types.Type) *types.Named {
		n := types.NewNamed(types.NewTypeName(0, nil, name, nil), nil, nil)
		if u != nil {
			n.SetUnderlying(u)
		}
		return n
	}
	empty := types.NewInterfaceType(nil, nil)
	tp := types.NewTypeParam(types.NewTypeName(0, nil, "P", nil), empty)

	// A cycle through value types only (invalid Go, but go/types builds it in
	// a package with type errors): A{b B}, B{a A, n int}.
	a, b := named("A", nil), named("B", nil)
	a.SetUnderlying(st(b))
	b.SetUnderlying(st(a, i))
	// The same cycle with a slice two levels down: C{d D}, D{c C, s []int}.
	c, d := named("C", nil), named("D", nil)
	c.SetUnderlying(st(d))
	d.SetUnderlying(st(c, sl))

	for _, tc := range []struct {
		name string
		t    types.Type
		want bool
	}{
		{"pointer", types.NewPointer(i), true},
		{"slice", sl, true},
		{"map", types.NewMap(i, i), true},
		{"chan", types.NewChan(types.SendRecv, i), true},
		{"func", types.NewSignatureType(nil, nil, nil, nil, nil, false), true},
		{"interface", empty, true},
		{"type parameter", tp, true},
		{"int", i, false},
		{"string", types.Typ[types.String], false},
		{"nil", nil, false},
		{"plain struct", st(i, types.Typ[types.String]), false},
		{"struct with slice", st(i, sl), true},
		{"named struct with slice", named("Cfg", st(sl)), true},
		{"struct nesting a struct with a map", st(st(i, types.NewMap(i, i))), true},
		{"array of int", types.NewArray(i, 2), false},
		{"array of slices", types.NewArray(sl, 2), true},
		{"array of structs with a pointer", types.NewArray(st(types.NewPointer(i)), 3), true},
		{"struct of arrays of int", st(types.NewArray(i, 4)), false},
		{"value cycle, no reference", a, false},
		{"value cycle, reference below", c, true},
	} {
		if got := dataflow.MayHoldRef(tc.t); got != tc.want {
			t.Errorf("MayHoldRef(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDefUse_ValueStructMayHoldRef (D1 gap 2): a value argument or receiver
// that may hold a reference weak-defines a.* (whole base, never per field);
// a reference-free one is only read.
func TestDefUse_ValueStructMayHoldRef(t *testing.T) {
	wantDU(t, "FillCfg 651:2", flowDU(t, "FillCfg", "651:2"), nil, ss("c.*"), ss("c"))
	wantDU(t, "ValueRefs 659:2", flowDU(t, "ValueRefs", "659:2"), nil, ss("c.*"), ss("c"))
	wantDU(t, "ValueRefs 660:2", flowDU(t, "ValueRefs", "660:2"), nil, nil, ss("p"))
	wantDU(t, "ValueRefs 661:2", flowDU(t, "ValueRefs", "661:2"), nil, ss("a.*", "n.*"), ss("a", "b", "n"))
	// A value receiver holding no reference is a copy: read only.
	wantDU(t, "PlainRecv 698:2", flowDU(t, "PlainRecv", "698:2"), nil, nil, ss("p"))
}

// TestDefUse_SatelliteParents (#28): a call in a folded for init, cond or
// post, a case list, a select comm clause (send value, receive operand) or a
// range key or expression is a satellite of the header or clause head that
// expression folds into, and carries an empty DefUse.
func TestDefUse_SatelliteParents(t *testing.T) {
	_, byKey, _ := build(t, "SatParents")
	want := map[string]string{
		"674:11": "674:2", "674:21": "674:2", "674:31": "674:2", // for init, cond, post
		"678:7": "678:2", "678:13": "678:2", // case f(1), g(2):
		"682:13": "682:2",                    // case ch <- f(s):
		"683:14": "683:2",                    // case v := <-chanOf(ch):
		"687:9":  "687:2", "687:26": "687:2", // for xs[f(2)], x = range ids(xs)
	}
	calls := 0
	for k, n := range byKey {
		if n.Kind != dataflow.KindCall {
			continue
		}
		calls++
		p, ok := want[k]
		if !ok {
			t.Errorf("SatParents: unexpected call node %s (parent %q)", k, n.Parent)
			continue
		}
		if n.Parent != p {
			t.Errorf("SatParents: call %s Parent = %q, want %q", k, n.Parent, p)
		}
		wantDU(t, "SatParents "+k, flowDU(t, "SatParents", k), nil, nil, nil)
	}
	if calls != len(want) {
		t.Errorf("SatParents: %d call nodes, want %d", calls, len(want))
	}
	// The hosts carry their satellites' flow.
	wantKind(t, "SatParents", byKey, "674:2", dataflow.KindLoop)
	wantKind(t, "SatParents", byKey, "678:2", dataflow.KindStatement)
	wantKind(t, "SatParents", byKey, "682:2", dataflow.KindStatement)
	wantKind(t, "SatParents", byKey, "683:2", dataflow.KindStatement)
	wantKind(t, "SatParents", byKey, "687:2", dataflow.KindLoop)
	wantDU(t, "SatParents 682:2", flowDU(t, "SatParents", "682:2"), nil, ss("ch[*]"), ss("ch", "s"))
	// <-chanOf(ch) receives from a call, not a path: the call reads ch and
	// writes through it.
	wantDU(t, "SatParents 683:2", flowDU(t, "SatParents", "683:2"), ss("v"), ss("ch.*"), ss("ch"))
	// `=` range: weak key and value; ids(xs) writes through xs.
	wantDU(t, "SatParents 687:2", flowDU(t, "SatParents", "687:2"), nil, ss("x", "xs.*", "xs[*]"), ss("xs"))
}
