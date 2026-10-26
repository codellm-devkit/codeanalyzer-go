package sdg

import (
	"reflect"
	"runtime/debug"
	"testing"
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
