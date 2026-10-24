package dataflow_test

import (
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

// refCDG is control dependence computed by definition, independent of
// BuildCDG's Cooper-Harvey-Kennedy / Ferrante-Ottenstein-Warren code:
//
//   - x post-dominates y iff every CFG path from y to @exit goes through x,
//     i.e. x == y, or y cannot reach @exit once x is removed;
//   - x is control-dependent on a iff x post-dominates some CFG successor of
//     a and x does not strictly post-dominate a;
//   - x→x is never emitted; every non-synthetic node with no controller gets
//     @entry→x; @entry→@exit is never emitted.
//
// The CFG nodes are the non-satellite nodes; edge kinds and parallel edges
// are ignored. ok is false when a node cannot reach @exit (BuildCDG's
// precondition), where "post-dominates" would hold vacuously.
func refCDG(nodes []dataflow.Node, cfg []dataflow.CfgEdge) (out []dataflow.CdgEdge, ok bool) {
	var keys []string
	in := map[string]bool{}
	for _, n := range nodes {
		if n.Parent == "" && !in[n.Key] {
			in[n.Key] = true
			keys = append(keys, n.Key)
		}
	}
	sort.Strings(keys)
	succ := map[string][]string{}
	pred := map[string][]string{}
	for _, e := range cfg {
		if in[e.Src] && in[e.Dst] {
			succ[e.Src] = append(succ[e.Src], e.Dst)
			pred[e.Dst] = append(pred[e.Dst], e.Src)
		}
	}

	// reachesExit[x] is the set of nodes that reach @exit with x removed
	// ("" = nothing removed), by backward search from @exit.
	reachesExit := func(removed string) map[string]bool {
		seen := map[string]bool{}
		if removed == exit || !in[exit] {
			return seen
		}
		seen[exit] = true
		work := []string{exit}
		for len(work) > 0 {
			n := work[len(work)-1]
			work = work[:len(work)-1]
			for _, p := range pred[n] {
				if p != removed && !seen[p] {
					seen[p] = true
					work = append(work, p)
				}
			}
		}
		return seen
	}
	all := reachesExit("")
	for _, k := range keys {
		if !all[k] {
			return nil, false
		}
	}
	without := map[string]map[string]bool{}
	for _, x := range keys {
		without[x] = reachesExit(x)
	}
	pdom := func(x, y string) bool { return x == y || !without[x][y] }

	deps := map[dataflow.CdgEdge]bool{}
	controlled := map[string]bool{}
	for _, a := range keys {
		for _, x := range keys {
			if x == a || pdom(x, a) { // x == a is a self-loop; pdom(x, a) with x ≠ a is strict
				continue
			}
			for _, b := range succ[a] {
				if pdom(x, b) {
					deps[dataflow.CdgEdge{Src: a, Dst: x}] = true
					controlled[x] = true
					break
				}
			}
		}
	}
	if in[entry] {
		for _, x := range keys {
			if x != entry && x != exit && !controlled[x] {
				deps[dataflow.CdgEdge{Src: entry, Dst: x}] = true
			}
		}
	}
	out = []dataflow.CdgEdge{}
	for d := range deps {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Src != out[j].Src {
			return out[i].Src < out[j].Src
		}
		return out[i].Dst < out[j].Dst
	})
	return out, true
}

// TestCDG_BruteForceReference (R12) asserts BuildCDG equals refCDG on every
// function and function literal body in testdata/dataflow and testdata/chi.
func TestCDG_BruteForceReference(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata")
	total, edges := 0, 0
	for _, dir := range []string{"dataflow", "chi"} {
		fset, cases := loadDiffCases(t, filepath.Join(root, dir))
		for _, c := range cases {
			total++
			nodes, keyOf := dataflow.BuildNodes(fset, c.body, gateCallKeys(fset, c.body))
			kept, es := dataflow.BuildCFG(nodes, keyOf, c.body, c.info)
			want, ok := refCDG(kept, es)
			if !ok {
				t.Errorf("%s: a CFG node cannot reach @exit (BuildCDG precondition)", c.name)
				continue
			}
			got := dataflow.BuildCDG(kept, es)
			edges += len(got)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: BuildCDG differs from the by-definition reference\n got %v\nwant %v", c.name, got, want)
			}
		}
	}
	if total < 100 {
		t.Errorf("reference covered only %d bodies", total)
	}
	t.Logf("cdg reference: %d bodies, %d edges equal", total, edges)
}
