package v2emit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/sdg"
)

// TestStream_EqualsEmit: two walks of the stream rebuild the Emit payload exactly.
func TestStream_EqualsEmit(t *testing.T) {
	for _, name := range append(slices.Clone(l4Fixtures), "rangefunc", "multipackage") {
		fx := buildL4(t, name)
		want := fx.l4
		s := Open(fx.app, testIdentity(name), fx.dir, 4, "gate-test", 3, fx.res, fx.sdg)
		for range 2 { // the second walk must give the same modules
			got := *s.Envelope()
			got.Application.SymbolTable = map[string]v2.Module{}
			for k, m := range s.Modules() {
				got.Application.SymbolTable[k] = m
			}
			got.Application.ParamIn = slices.Collect(s.ParamIn())
			got.Application.ParamOut = slices.Collect(s.ParamOut())
			got.Degraded = s.Degraded()
			a, _ := json.Marshal(want)
			b, _ := json.Marshal(&got)
			if !bytes.Equal(a, b) {
				t.Fatalf("%s: stream differs from Emit at byte %d", name, firstDiff(a, b))
			}
		}
	}
}

// TestStream_LateFieldsAreCached: the param lists are minted once. A second ParamIn
// walk does not resolve the targets again, so the resolve warning does not repeat.
// (This tests the late() cache, not setQuiet: resolve runs only inside late().
// TestL4Overlay_QuietSecondWalk is the test of the quiet flag.)
func TestStream_LateFieldsAreCached(t *testing.T) {
	fx := buildL4(t, "dataflow")
	res := *fx.sdg
	ghost := sdg.Target{Callable: dataflow.L3Key{Path: "nowhere.go", Signature: "example.com/nowhere.F"}}
	res.ParamIn = append(slices.Clone(res.ParamIn), sdg.ParamEdge{
		Src: sdg.Ref{Owner: ghost, Key: "1:1/actual_in:0"},
		Dst: sdg.Ref{Owner: ghost, Key: "@formal_in:0"}, Var: "x",
	})
	var warned []string
	prev := warnf
	warnf = func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }
	defer func() { warnf = prev }()
	s := Open(fx.app, testIdentity("dataflow"), fx.dir, 4, "t", 3, fx.res, &res)
	for range s.Modules() {
	}
	for range s.ParamIn() {
	}
	first := len(warned)
	for range s.Modules() {
	}
	for range s.ParamIn() {
	}
	if first == 0 || len(warned) != first {
		t.Errorf("warnings: %d after walk 1, %d after walk 2; want >0 and unchanged", first, len(warned))
	}
}
