package sdg

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

// The go/ssa def-use differential (#22, spec §4.5 "L3 precision"): an
// independent answer key for the syntactic L3 ddg. It does not gate: L3 and
// SSA model different things (SSA is value flow over registers, L3 is
// access-path flow between statements), so mismatches are reported, and the
// test asserts only that the mapping layer works (ruling R-P6).

// ssaPair is one SSA def-use pair of a callable mapped to its L3 body keys.
type ssaPair struct {
	Unit     dataflow.L3Key
	Def, Use string // local body keys
	What     string // the SSA value, for the report
}

// bodyIndex maps a raw position to the innermost CFG body node of one L3
// result containing it. Satellite call nodes are skipped (their def/use
// belongs to the host statement, DefUseOf), and so are deferred call nodes:
// SSA evaluates a deferred call's function value and arguments at the
// registration, the `defer` statement node. A position in the signature
// (a parameter spilled to the heap) is @entry; one at the closing brace (the
// implicit return) is @exit.
type bodyIndex struct {
	nodes          []dataflow.Node
	lbrace, rbrace token.Pos
}

func newBodyIndex(r dataflow.Result, fn ast.Node) bodyIndex {
	var ns []dataflow.Node
	for _, n := range r.Nodes {
		if n.Pos.IsValid() && n.Parent == "" && !n.Deferred {
			ns = append(ns, n)
		}
	}
	b := bodyIndex{nodes: ns}
	switch fn := fn.(type) {
	case *ast.FuncDecl:
		b.lbrace, b.rbrace = fn.Body.Lbrace, fn.Body.Rbrace
	case *ast.FuncLit:
		b.lbrace, b.rbrace = fn.Body.Lbrace, fn.Body.Rbrace
	}
	return b
}

func (b bodyIndex) at(pos token.Pos) (string, bool) {
	if b.lbrace.IsValid() {
		if pos < b.lbrace {
			return dataflow.EntryKey, true
		}
		if pos >= b.rbrace {
			return dataflow.ExitKey, true
		}
	}
	best := -1
	for i, n := range b.nodes {
		if n.Pos <= pos && pos < n.End {
			if best < 0 || n.End-n.Pos < b.nodes[best].End-b.nodes[best].Pos {
				best = i
			}
		}
	}
	if best < 0 {
		return "", false
	}
	return b.nodes[best].Key, true
}

// ssaPairs returns the def-use pairs of the SSA functions of funcs that join
// a unit by body, mapped to body keys: for each SSA value with a definition
// site (a Parameter or FreeVar: @entry; an instruction: the body node at its
// position) and each of its referrers, the referrer's body node. Phis and
// position-less value instructions (Extract, implicit conversions,
// MakeClosure) are looked through on both sides; a Store through a value is
// a write, not a use. Pairs within one node are dropped (L3 edges are
// between nodes).
func ssaPairs(j *join, funcs map[*ssa.Function]bool) (pairs []ssaPair, instrs, unmappedInstrs int) {
	idx := map[dataflow.L3Key]bodyIndex{}
	for fn := range funcs {
		unit, kind := j.resolve(fn)
		if kind != joinBody {
			continue
		}
		r, ok := j.results[unit]
		if !ok {
			continue
		}
		bi, ok := idx[unit]
		if !ok {
			bi = newBodyIndex(r, j.units[unit].Fn)
			idx[unit] = bi
		}
		keyOf := func(instr ssa.Instruction) (string, bool) {
			p := instr.Pos()
			if !p.IsValid() {
				return "", false
			}
			instrs++
			k, ok := bi.at(p)
			if !ok {
				unmappedInstrs++
			}
			return k, ok
		}
		seen := map[ssaPair]bool{}
		add := func(def, use, what string) {
			if def == use {
				return
			}
			p := ssaPair{Unit: unit, Def: def, Use: use, What: what}
			if !seen[p] {
				seen[p] = true
				pairs = append(pairs, p)
			}
		}
		var values []ssa.Value
		for _, p := range fn.Params {
			values = append(values, p)
		}
		for _, fv := range fn.FreeVars {
			values = append(values, fv)
		}
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if v, ok := instr.(ssa.Value); ok {
					values = append(values, v)
				}
			}
		}
		for _, v := range values {
			var def string
			switch v := v.(type) {
			case *ssa.Parameter, *ssa.FreeVar:
				def = dataflow.EntryKey
			case *ssa.Phi:
				continue // looked through from its operands' side
			case ssa.Instruction:
				k, ok := keyOf(v)
				if !ok {
					continue // position-less: looked through from its operands
				}
				def = k
			}
			for _, use := range useSites(v, keyOf) {
				add(def, use, v.Name()+" = "+v.String())
			}
		}
	}
	sort.Slice(pairs, func(a, b int) bool {
		x, y := pairs[a], pairs[b]
		if x.Unit != y.Unit {
			return x.Unit.Path+x.Unit.Signature < y.Unit.Path+y.Unit.Signature
		}
		if x.Def != y.Def {
			return x.Def < y.Def
		}
		if x.Use != y.Use {
			return x.Use < y.Use
		}
		return x.What < y.What
	})
	return pairs, instrs, unmappedInstrs
}

// useSites returns the body keys of v's referrers, looking through phis and
// position-less value instructions.
func useSites(v ssa.Value, keyOf func(ssa.Instruction) (string, bool)) []string {
	var out []string
	seen := map[ssa.Value]bool{}
	var walk func(ssa.Value)
	walk = func(v ssa.Value) {
		if seen[v] {
			return
		}
		seen[v] = true
		refs := v.Referrers()
		if refs == nil {
			return
		}
		for _, r := range *refs {
			if phi, ok := r.(*ssa.Phi); ok {
				walk(phi)
				continue
			}
			if st, ok := r.(*ssa.Store); ok && st.Addr == v && st.Val != v {
				continue // a write through v, not a read of it
			}
			if writeOnlyAddr(r, v) {
				continue // &v.f / &v[i] taken only to store through: a write
			}
			if k, ok := keyOf(r); ok {
				out = append(out, k)
				continue
			}
			if rv, ok := r.(ssa.Value); ok {
				walk(rv)
			}
		}
	}
	walk(v)
	return out
}

// writeOnlyAddr reports whether r is a field or element address of v whose
// every referrer stores through it.
func writeOnlyAddr(r ssa.Instruction, v ssa.Value) bool {
	var refs *[]ssa.Instruction
	switch a := r.(type) {
	case *ssa.FieldAddr:
		if a.X != v {
			return false
		}
		refs = a.Referrers()
	case *ssa.IndexAddr:
		if a.X != v {
			return false
		}
		refs = a.Referrers()
	default:
		return false
	}
	if refs == nil || len(*refs) == 0 {
		return false
	}
	for _, x := range *refs {
		if st, ok := x.(*ssa.Store); !ok || st.Addr != r.(ssa.Value) {
			return false
		}
	}
	return true
}

// Triage classes of an SSA pair with no direct L3 ddg edge. All but
// classGap are known differences of representation: the flow is in L3.
const (
	classTransitive = "transitive in L3 (SSA copy-propagates through copies, closure values and captured cells)"
	classClause     = "bound at a different node (switch tag / select comm / type-switch binding: header in one, clause in the other)"
	classDefer      = "deferred call: SSA evaluates at the registration, L3 reads at the deferred call node (two-node rule)"
	classSlot       = "SSA bookkeeping (anonymous result slot)"
	classGap        = "candidate gap: no L3 ddg path"
)

// classify triages a mapped SSA pair p of result r that has no direct L3
// ddg edge.
func classify(r dataflow.Result, p ssaPair) string {
	succ := map[string][]string{}
	for _, e := range r.Ddg {
		succ[e.Src] = append(succ[e.Src], e.Dst)
	}
	reach := func(src, dst string) bool {
		seen := map[string]bool{src: true}
		for work := []string{src}; len(work) > 0; work = work[1:] {
			for _, m := range succ[work[0]] {
				if m == dst {
					return true
				}
				if !seen[m] {
					seen[m] = true
					work = append(work, m)
				}
			}
		}
		return false
	}
	node := map[string]dataflow.Node{}
	for _, n := range r.Nodes {
		node[n.Key] = n
	}
	// header returns the innermost switch/select node holding node n.
	header := func(n dataflow.Node) (dataflow.Node, bool) {
		var best dataflow.Node
		found := false
		for _, h := range r.Nodes {
			if h.Kind == dataflow.KindSwitch && h.Pos < n.Pos && n.End <= h.End && (!found || h.End-h.Pos < best.End-best.Pos) {
				best, found = h, true
			}
		}
		return best, found
	}
	clauses := func(h dataflow.Node) []dataflow.Node {
		var out []dataflow.Node
		for _, n := range r.Nodes {
			switch n.Stmt.(type) {
			case *ast.CaseClause, *ast.CommClause:
				if hh, ok := header(n); ok && hh.Key == h.Key {
					out = append(out, n)
				}
			}
		}
		return out
	}
	d, u := node[p.Def], node[p.Use]
	switch s := u.Stmt.(type) {
	case *ast.DeferStmt:
		for _, n := range r.Nodes {
			if n.Deferred && n.Pos == s.Call.Pos() && (p.Def == n.Key || reach(p.Def, n.Key)) {
				return classDefer
			}
		}
	case *ast.CaseClause, *ast.CommClause, *ast.SelectStmt:
		h := u
		if _, ok := u.Stmt.(*ast.SelectStmt); !ok {
			h, _ = header(u)
		}
		if p.Def == h.Key || reach(p.Def, h.Key) {
			return classClause
		}
		for _, c := range clauses(h) {
			if p.Def == c.Key || reach(p.Def, c.Key) {
				return classClause
			}
		}
	}
	// A value SSA binds at a header (a select's received tuple) or at a
	// clause (a type switch's per-clause assertion) that L3 binds at the
	// other.
	if h, ok := header(d); ok && reach(h.Key, p.Use) {
		return classClause
	}
	if d.Kind == dataflow.KindSwitch {
		for _, c := range clauses(d) {
			if reach(c.Key, p.Use) {
				return classClause
			}
		}
	}
	if strings.Contains(p.What, " = local ") && strings.HasSuffix(p.What, " ()") {
		return classSlot
	}
	if reach(p.Def, p.Use) {
		return classTransitive
	}
	return classGap
}

// TestSSADifferential_Report: on the fixtures, every project SSA function
// and call joins, and at least one SSA def-use pair maps to body keys; then
// it logs every mapped pair with no L3 ddg edge between the same two nodes
// (any variable), triaged (classify). Mismatches never fail the test. Set
// CANGO_SSA_DIFF_OUT=<file> to also write the report there.
func TestSSADifferential_Report(t *testing.T) {
	var report strings.Builder
	for _, name := range []string{"dataflow", "chi"} {
		in := loadInput(t, name)
		_, j, funcs := joined(t, in)
		// The mapping layer is complete: every function joins (but those of
		// declarations the symbol table dropped, pinned by
		// TestJoin_EveryProjectFunctionMaps) and no positioned call of a
		// joined function fails to join.
		dropped := droppedDecls(in)
		for fn := range funcs {
			if _, kind := j.resolve(fn); kind == joinNone && fn.Synthetic != synthPackageInit && !inDropped(fn, dropped) {
				t.Errorf("%s: %s joins nothing", name, fn)
			}
		}
		for _, b := range describeUnmapped(j, funcs) {
			if strings.HasPrefix(b, "call ") {
				t.Errorf("%s: unmapped %s", name, b)
			}
		}
		pairs, instrs, unmapped := ssaPairs(j, funcs)
		if len(pairs) == 0 {
			t.Errorf("%s: no SSA def-use pair mapped to body keys", name)
		}
		if unmapped > 0 {
			t.Errorf("%s: %d positioned SSA instructions of joined functions lie in no body node", name, unmapped)
		}
		has := map[[2]string]map[dataflow.L3Key]bool{}
		for k, r := range in.L3 {
			for _, e := range r.Ddg {
				p := [2]string{e.Src, e.Dst}
				if has[p] == nil {
					has[p] = map[dataflow.L3Key]bool{}
				}
				has[p][k] = true
			}
		}
		missing := 0
		classes := map[string]int{}
		for _, p := range pairs {
			if has[[2]string{p.Def, p.Use}][p.Unit] {
				continue
			}
			missing++
			class := classify(in.L3[p.Unit], p)
			classes[class]++
			fmt.Fprintf(&report, "%s\t%s\t%s\t%s -> %s\t%s\t%s\n", name, p.Unit.Path, p.Unit.Signature, p.Def, p.Use, p.What, class)
		}
		t.Logf("%s: %d SSA def-use pairs mapped (%d positioned instructions, %d outside every body node); %d with no L3 ddg edge",
			name, len(pairs), instrs, unmapped, missing)
		for _, c := range []string{classTransitive, classClause, classDefer, classSlot, classGap} {
			if classes[c] > 0 {
				t.Logf("%s:   %4d  %s", name, classes[c], c)
			}
		}
	}
	if report.Len() > 0 {
		t.Logf("SSA def-use pairs with no L3 ddg edge (fixture, file, callable, def -> use, SSA value, class):\n%s", report.String())
	}
	if out := os.Getenv("CANGO_SSA_DIFF_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(report.String()), 0o644); err != nil {
			t.Error(err)
		}
	}
}
