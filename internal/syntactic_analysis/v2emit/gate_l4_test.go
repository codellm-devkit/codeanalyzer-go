package v2emit

// L4 conformance harness for the v2 emitter.
//
// At this stage -a 4 reuses the -a 3 pipeline unchanged: the tree equals -a 3
// except for max_level. buildL4 runs that pipeline with maxLevel 4 and an
// empty sdg result; later tasks fill the L4 content in, and the monotonicity
// gate below keeps holding as they do (-a 3 ⊆ -a 4).

import (
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
)

type l4Fixture struct {
	st     map[string]schema.GoFile
	l3, l4 *v2.Analysis
	res    map[dataflow.L3Key]dataflow.Result
}

var (
	l4Mu    sync.Mutex
	l4Cache = map[string]*l4Fixture{}
)

// buildL4 runs the -a 4 pipeline over testdata/<name> once and emits both the
// L3 and the L4 payload from the same resolved symbol table.
func buildL4(t *testing.T, name string) *l4Fixture {
	t.Helper()
	l4Mu.Lock()
	defer l4Mu.Unlock()
	if fx, ok := l4Cache[name]; ok {
		return fx
	}
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	b := syntactic_analysis.NewSymbolTableBuilder(dir)
	st, err := b.Build(nil, true)
	if err != nil {
		t.Fatalf("symbol table build failed: %v", err)
	}
	cg := semantic_analysis.NewCallGraphBuilder(dir, b.Fset(), b.Pkgs())
	edges := cg.Build(st)
	app := &schema.GoApplication{SymbolTable: st, CallGraph: edges, Externals: cg.Externals()}
	res := dataflow.Run(b.Fset(), b.L3Units(st), 3, 4)
	fx := &l4Fixture{
		st:  st,
		l3:  Emit(app, name, dir, 3, "gate-test", 3, res),
		l4:  Emit(app, name, dir, 4, "gate-test", 3, res),
		res: res,
	}
	l4Cache[name] = fx
	return fx
}

// supersetL4 records where a (-a 3) is not contained in b (-a 4): every object
// key and value, and every list entry. Nothing may be added yet either, so the
// check is exact; later tasks widen it with the sanctioned L4 additions.
func supersetL4(path string, a, b any, errs *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*errs = append(*errs, path+": object changed type")
			return
		}
		for k, x := range av {
			y, ok := bv[k]
			if !ok {
				*errs = append(*errs, path+"."+k+" missing at L4")
				continue
			}
			supersetL4(path+"."+k, x, y, errs)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) > len(bv) {
			*errs = append(*errs, path+": list shrank or changed type")
			return
		}
		for i := range av {
			supersetL4(path+"[]", av[i], bv[i], errs)
		}
	default:
		if a != b {
			*errs = append(*errs, path+": value changed")
		}
	}
}

// TestGateL4_SupersetOfL3 is the monotonicity gate: everything in the -a 3 JSON
// is present and equal in the -a 4 JSON. Only max_level, the level marker
// itself, may differ.
func TestGateL4_SupersetOfL3(t *testing.T) {
	for _, name := range []string{"dataflow", "chi", "generics"} {
		fx := buildL4(t, name)
		a, b := toAny(t, fx.l3), toAny(t, fx.l4)
		if b["max_level"] != float64(4) {
			t.Errorf("%s: max_level = %v, want 4", name, b["max_level"])
		}
		a["max_level"], b["max_level"] = nil, nil
		var errs []string
		supersetL4("$", a, b, &errs)
		for i, e := range errs {
			if i == 20 {
				t.Errorf("%s: … %d more", name, len(errs)-20)
				break
			}
			t.Errorf("%s: %s", name, e)
		}
	}
}
