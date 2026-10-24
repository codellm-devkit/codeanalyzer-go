package v2emit

// L3 conformance gate for the v2 emitter.
//
// L3 completes each callable's body (statement / branch / loop / switch /
// return / entry / exit nodes around the L1 call nodes) and adds the
// intra-callable cfg / cdg / ddg edge lists. These gates build a fixture
// through the real symbol-table builder, resolver and dataflow driver (the
// -a 3 wiring) and check:
//
//   - every callable with a body has @entry / @exit and a non-empty cfg
//   - no dangling endpoint: every cfg / cdg / ddg endpoint and every parent
//     is a key of the callable's own body; ddg prov is ["ssa"]
//   - every body node id is v2.GlobalOrdinal(callable id, key)
//   - json(-a 2) ⊆ json(-a 3), modulo the sanctioned additions
//   - spans slice to the statement text, cross-file methods included
//   - every side-table entry lands on an emitted callable

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
)

type l3Fixture struct {
	st     map[string]schema.GoFile
	l2, l3 *v2.Analysis
	res    map[dataflow.L3Key]dataflow.Result
}

var (
	l3Mu    sync.Mutex
	l3Cache = map[string]*l3Fixture{}
)

// buildL3 runs the -a 3 pipeline over testdata/<name> once and emits both the
// L2 and the L3 payload from the same resolved symbol table.
func buildL3(t *testing.T, name string) *l3Fixture {
	t.Helper()
	l3Mu.Lock()
	defer l3Mu.Unlock()
	if fx, ok := l3Cache[name]; ok {
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
	fx := &l3Fixture{
		st:  st,
		l2:  Emit(app, name, dir, 2, "gate-test", 3, nil),
		l3:  Emit(app, name, dir, 3, "gate-test", 3, res),
		res: res,
	}
	l3Cache[name] = fx
	return fx
}

// emitted is one callable of the v2 tree with its v1 source and the file its
// spans index.
type emitted struct {
	v1       schema.GoCallable
	v2       v2.Callable
	declFile string
}

// pairCallables walks the v1 symbol table and the v2 tree in parallel.
func pairCallables(st map[string]schema.GoFile, out *v2.Analysis) []emitted {
	var all []emitted
	var walk func(c1 schema.GoCallable, c2 v2.Callable, encl string)
	walk = func(c1 schema.GoCallable, c2 v2.Callable, encl string) {
		file := encl
		if c2.SourceFile != "" {
			file = c2.SourceFile
		}
		all = append(all, emitted{c1, c2, file})
		for name, inner := range c1.InnerCallables {
			walk(inner, c2.Callables[name], file)
		}
	}
	for path, f := range st {
		mod := out.Application.SymbolTable[path]
		for sig, fn := range f.Functions {
			walk(fn, mod.Functions[sig], path)
		}
		for tn, ty := range f.Types {
			for sig, m := range ty.Methods {
				walk(m, mod.Types[tn].Callables[sig], path)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v2.ID < all[j].v2.ID })
	return all
}

func TestGateL3_EveryCallableHasEntryExit(t *testing.T) {
	for name, floor := range map[string]int{"dataflow": 50, "multipackage": 10, "chi": 200} {
		gateEntryExit(t, name, floor)
	}
}

func gateEntryExit(t *testing.T, name string, floor int) {
	fx := buildL3(t, name)
	if fx.l3.MaxLevel != 3 || fx.l3.KLimit != 3 {
		t.Errorf("%s: envelope max_level=%d k_limit=%d, want 3, 3", name, fx.l3.MaxLevel, fx.l3.KLimit)
	}
	withBody := 0
	for _, e := range pairCallables(fx.st, fx.l3) {
		if e.v1.Code == "" {
			continue // bodyless (interface method)
		}
		withBody++
		for _, k := range []string{"@entry", "@exit"} {
			if _, ok := e.v2.Body[k]; !ok {
				t.Errorf("%s: no %s body node", e.v2.ID, k)
			}
		}
		if len(e.v2.Cfg) == 0 {
			t.Errorf("%s: empty cfg", e.v2.ID)
		}
	}
	if withBody < floor {
		t.Errorf("%s: only %d callables with a body", name, withBody)
	}
}

func TestGateL3_EveryEdgeEndpointIsABodyKey(t *testing.T) {
	for name, floor := range map[string]int{"dataflow": 500, "chi": 5000} {
		fx := buildL3(t, name)
		edges := 0
		for _, e := range pairCallables(fx.st, fx.l3) {
			c := e.v2
			in := func(list, k string) {
				if _, ok := c.Body[k]; !ok {
					t.Errorf("%s: %s endpoint %q is not a body key", c.ID, list, k)
				}
			}
			for _, g := range c.Cfg {
				in("cfg", g.Src)
				in("cfg", g.Dst)
			}
			for _, g := range c.Cdg {
				in("cdg", g.Src)
				in("cdg", g.Dst)
			}
			for _, g := range c.Ddg {
				in("ddg", g.Src)
				in("ddg", g.Dst)
				if len(g.Prov) != 1 || g.Prov[0] != "ssa" {
					t.Errorf("%s: ddg %v prov = %v, want [ssa]", c.ID, g, g.Prov)
				}
			}
			for k, n := range c.Body {
				if n.Parent != "" {
					in("parent of "+k, n.Parent)
				}
			}
			edges += len(c.Cfg) + len(c.Cdg) + len(c.Ddg)
		}
		if edges < floor {
			t.Errorf("%s: only %d edges", name, edges)
		}
		t.Logf("%s: %d cfg+cdg+ddg edges", name, edges)
	}
}

func TestGateL3_BodyNodeIDsAreGlobalOrdinals(t *testing.T) {
	fx := buildL3(t, "dataflow")
	for _, e := range pairCallables(fx.st, fx.l3) {
		for k, n := range e.v2.Body {
			if want := v2.GlobalOrdinal(e.v2.ID, k); n.ID != want {
				t.Errorf("%s body %s id = %q, want %q", e.v2.ID, k, n.ID, want)
			}
		}
	}
	// Below L3 the body nodes carry no id (additive at L3).
	for _, e := range pairCallables(fx.st, fx.l2) {
		for k, n := range e.v2.Body {
			if n.ID != "" || n.Parent != "" {
				t.Errorf("L2 %s body %s carries id/parent %q/%q", e.v2.ID, k, n.ID, n.Parent)
			}
		}
	}
	if fx.l2.KLimit != 0 {
		t.Errorf("L2 k_limit = %d, want absent", fx.l2.KLimit)
	}
}

// TestGateL3_SupersetOfL2 is the monotonicity gate: everything in the -a 2
// JSON is present and equal in the -a 3 JSON. The only sanctioned additions
// are new body nodes, cfg/cdg/ddg on a callable, id/parent on a body node and
// the envelope's k_limit; max_level is the level marker itself.
func TestGateL3_SupersetOfL2(t *testing.T) {
	for _, name := range []string{"dataflow", "multipackage"} {
		fx := buildL3(t, name)
		a, b := toAny(t, fx.l2), toAny(t, fx.l3)
		a["max_level"], b["max_level"] = nil, nil
		var errs []string
		subset("$", "", a, b, &errs)
		for i, e := range errs {
			if i == 20 {
				t.Errorf("%s: … %d more", name, len(errs)-20)
				break
			}
			t.Errorf("%s: %s", name, e)
		}
	}
}

func toAny(t *testing.T, a *v2.Analysis) map[string]any {
	t.Helper()
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// subset records where a (the lower level) is not contained in b. parentKey
// is the map key a and b sit under, which tells the sanctioned additions
// apart: any new key in a "body" map, id/parent in a body node (an object
// under a body map), cfg/cdg/ddg in a callable (an object with a body),
// k_limit at the root.
func subset(path, parentKey string, a, b any, errs *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*errs = append(*errs, fmt.Sprintf("%s: object became %T", path, b))
			return
		}
		for k, x := range av {
			y, ok := bv[k]
			if !ok {
				*errs = append(*errs, fmt.Sprintf("%s.%s missing at L3", path, k))
				continue
			}
			subset(path+"."+k, k, x, y, errs)
		}
		_, isCallable := av["body"]
		for k := range bv {
			if _, ok := av[k]; ok {
				continue
			}
			switch {
			case parentKey == "body":
			case path == "$" && k == "k_limit":
			case isCallable && (k == "cfg" || k == "cdg" || k == "ddg"):
			case (k == "id" || k == "parent") && strings.Contains(path, ".body."):
			default:
				*errs = append(*errs, fmt.Sprintf("%s.%s: unsanctioned addition", path, k))
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			*errs = append(*errs, fmt.Sprintf("%s: array changed", path))
			return
		}
		for i := range av {
			subset(fmt.Sprintf("%s[%d]", path, i), parentKey, av[i], bv[i], errs)
		}
	default:
		if a != b {
			*errs = append(*errs, fmt.Sprintf("%s: %v -> %v", path, a, b))
		}
	}
}

// TestGateL3_SpansSliceToStatementText: every L3 node's span slices its
// declaring module's source to text that starts with the construct's keyword,
// and every call node keeps its L1 span (R7). multipackage carries a method
// declared apart from its receiver type (Server.Describe in middleware.go).
func TestGateL3_SpansSliceToStatementText(t *testing.T) {
	prefixes := map[string][]string{
		"branch": {"if"},
		"loop":   {"for"},
		"switch": {"switch", "select"},
		"return": {"return"},
	}
	for _, name := range []string{"dataflow", "multipackage"} {
		fx := buildL3(t, name)
		l2 := map[string]v2.Callable{}
		for _, e := range pairCallables(fx.st, fx.l2) {
			l2[e.v2.ID] = e.v2
		}
		checked, crossFile := 0, 0
		for _, e := range pairCallables(fx.st, fx.l3) {
			src := fx.l3.Application.SymbolTable[e.declFile].Source
			for k, n := range e.v2.Body {
				text := n.Span.Slice(src)
				switch n.Kind {
				case "entry", "exit":
					continue
				case "call":
					if l1 := l2[e.v2.ID].Body[k]; l1.Span != n.Span {
						t.Errorf("%s call %s span %v, L1 %v", e.v2.ID, k, n.Span, l1.Span)
					}
				case "statement":
					if strings.TrimSpace(text) == "" || text != strings.TrimSpace(text) {
						t.Errorf("%s %s statement span slices to %q", e.v2.ID, k, text)
					}
				default:
					ok := false
					for _, p := range prefixes[n.Kind] {
						ok = ok || strings.HasPrefix(text, p)
					}
					if !ok {
						t.Errorf("%s %s (%s) span slices to %q", e.v2.ID, k, n.Kind, text)
					}
				}
				if want := v2.LocalID(n.Span.Start[0], n.Span.Start[1]); n.Kind != "call" && !strings.HasPrefix(k, want) {
					t.Errorf("%s %s span starts at %s", e.v2.ID, k, want)
				}
				checked++
				if e.v2.SourceFile != "" {
					crossFile++
				}
			}
		}
		if checked < 10 {
			t.Errorf("%s: only %d nodes checked", name, checked)
		}
		if name == "multipackage" && crossFile == 0 {
			t.Error("multipackage: no cross-file method body node checked")
		}
	}
}

func TestEmitL3_NoOrphanSideTableEntries(t *testing.T) {
	fx := buildL3(t, "dataflow")
	seen := map[dataflow.L3Key]bool{}
	for _, e := range pairCallables(fx.st, fx.l3) {
		seen[dataflow.L3Key{Path: e.declFile, Signature: e.v2.Signature}] = true
	}
	for k := range fx.res {
		if !seen[k] {
			t.Errorf("side-table entry %v matches no emitted callable", k)
		}
	}
	if len(fx.res) < 50 {
		t.Errorf("only %d side-table entries", len(fx.res))
	}
}
