// Package examples holds small functions written to be eligible sites of the
// metamorphic relations (internal/metamorphic): each one names the relation
// it targets and the effect the relation expects. conventions.go pins the
// oracle conventions those relations rely on.
package examples

// sinkInt is the sink the call sites pass values to: it has no by-ref or
// global output, so a flow into its argument ends at the call.
func sinkInt(v int) int { return v }

// sink2 is a two-argument sink.
func sink2(v, w int) int { return v + w }
