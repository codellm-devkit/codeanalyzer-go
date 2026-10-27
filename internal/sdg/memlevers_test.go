package sdg

import (
	"reflect"
	"runtime/debug"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

// TestSDG_OutputUnchangedByMemoryLevers: the memory levers (early cache
// release, interned port names, the L4 GC target) change memory, never
// facts: Run's result on dataflow and chi is deeply equal with them off
// (memLevers = false) and on.
func TestSDG_OutputUnchangedByMemoryLevers(t *testing.T) {
	for _, name := range []string{"dataflow", "chi"} {
		t.Run(name, func(t *testing.T) {
			defer func(v bool) { memLevers = v }(memLevers)
			memLevers = false
			off := Run(loadInput(t, name))
			memLevers = true
			on := Run(loadInput(t, name))
			if len(on.Callables) == 0 || len(on.ParamIn) == 0 {
				t.Fatalf("empty overlay: %d callables, %d param_in", len(on.Callables), len(on.ParamIn))
			}
			if !reflect.DeepEqual(off, on) {
				for k, c := range off.Callables {
					if !reflect.DeepEqual(c, on.Callables[k]) {
						t.Errorf("callable %v differs:\n off %+v\n on  %+v", k, c, on.Callables[k])
						break
					}
				}
				t.Fatalf("Run(%s) differs with the memory levers on", name)
			}
		})
	}
}

// TestRun_RestoresGCPercent: Run lowers the GC target only while it runs:
// the caller's setting is back on return, and a lower one is never raised.
func TestRun_RestoresGCPercent(t *testing.T) {
	in := loadInput(t, "greeter")
	for _, pct := range []int{200, 20} {
		prev := debug.SetGCPercent(pct)
		Run(in)
		got := debug.SetGCPercent(prev)
		if got != pct {
			t.Errorf("GC percent after Run = %d, want the caller's %d", got, pct)
		}
	}
}

// TestSDG_ReleasedCachesAreNotRebuilt (Task 15b): with the memory levers on,
// a unit's flow graph, call sites and wiring are each built at most once —
// a cache released before its last reader is rebuilt silently, which costs
// time and memory but changes no fact, so only a miss count catches it —
// and fill leaves the per-unit caches empty.
func TestSDG_ReleasedCachesAreNotRebuilt(t *testing.T) {
	for _, name := range []string{"dataflow", "chi"} {
		t.Run(name, func(t *testing.T) {
			defer func(v bool) { memLevers = v }(memLevers)
			memLevers = true
			builds := map[string]map[dataflow.L3Key]int{}
			defer func(f func(string, dataflow.L3Key)) { onCacheMiss = f }(onCacheMiss)
			onCacheMiss = func(cache string, k dataflow.L3Key) {
				if builds[cache] == nil {
					builds[cache] = map[dataflow.L3Key]int{}
				}
				builds[cache][k]++
			}
			in := loadInput(t, name)
			res, d := dispatchRun(in)
			b := newSDGBuilder(in, d)
			b.fill(&res)
			for _, cache := range []string{"graph", "sites", "wired"} {
				if len(builds[cache]) == 0 {
					t.Errorf("no %s cache built", cache)
				}
				for k, n := range builds[cache] {
					if n > 1 {
						t.Errorf("%s of %s built %d times: released before its last reader", cache, k.Signature, n)
					}
				}
			}
			if len(b.graph)+len(b.sites)+len(b.wired)+len(b.summ) != 0 {
				t.Errorf("after fill: %d graphs, %d sites, %d wirings, %d summaries still cached",
					len(b.graph), len(b.sites), len(b.wired), len(b.summ))
			}
		})
	}
}
