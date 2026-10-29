package v2emit

// The L4 "done" gate (spec §8): on the HTTP → SQL fixture
// testdata/dataflow/httpsql, the emitted JSON graph carries a flow from an
// HTTP handler's request parameter to database/sql's DB.Query query
// argument through an interface call, and unrelated pairs are unreachable.
// Sanitizers are SDK taint semantics (.github#103); the second test pins
// what L4 gives a sanitizer to work with.

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/sdg"
	"github.com/codellm-devkit/codeanalyzer-go/internal/semantic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
)

// sdgGraph is the emitted JSON's dependence graph over global body node ids:
// every callable's ddg, cdg and summary edges (local keys minted with
// v2.GlobalOrdinal) and the application's param_in and param_out — the
// edge families the SDK's slice and taint walks follow. Each edge records
// its family, so a path can be checked for the hops it takes.
type sdgGraph struct {
	succ map[string][]sdgHop
}

type sdgHop struct {
	to, family string
	v          string // the edge's var (ddg, param_in, param_out); "" for cdg and summary
}

func newSDGGraph(a *v2.Analysis) *sdgGraph {
	g := &sdgGraph{succ: map[string][]sdgHop{}}
	add := func(src, dst, family, v string) { g.succ[src] = append(g.succ[src], sdgHop{dst, family, v}) }
	for _, c := range l4Callables(a) {
		id := func(k string) string { return v2.GlobalOrdinal(c.ID, k) }
		for _, e := range c.Ddg {
			add(id(e.Src), id(e.Dst), "ddg", e.Var)
		}
		for _, e := range c.Cdg {
			add(id(e.Src), id(e.Dst), "cdg", "")
		}
		for _, e := range c.Summary {
			add(id(e.Src), id(e.Dst), "summary", "")
		}
	}
	for _, e := range a.Application.ParamIn {
		add(e.Src, e.Dst, "param_in", e.Var)
	}
	for _, e := range a.Application.ParamOut {
		add(e.Src, e.Dst, "param_out", e.Var)
	}
	return g
}

// path is a shortest path from src to dst by BFS, as the hops taken (nil if
// dst is unreachable). Successors are visited in edge order, so the path is
// deterministic.
func (g *sdgGraph) path(src, dst string) []sdgHop {
	return g.pathExcept(src, dst, nil)
}

// underCallable is python-sdk's under_callable for one id: the callable
// itself, a body node of it (q@…), or a node of a callable nested in it
// (q/…).
func underCallable(id, q string) bool {
	return id == q || strings.HasPrefix(id, q+"@") || strings.HasPrefix(id, q+"/")
}

// pathExcept is path over the graph without the hops skip rejects (skip
// gets the hop's source node and the hop); a nil skip keeps every hop.
func (g *sdgGraph) pathExcept(src, dst string, skip func(from string, h sdgHop) bool) []sdgHop {
	type pred struct {
		from string
		hop  sdgHop
	}
	prev := map[string]pred{src: {}}
	for q := []string{src}; len(q) > 0; q = q[1:] {
		n := q[0]
		if n == dst {
			var out []sdgHop
			for n != src {
				p := prev[n]
				out = append(out, p.hop)
				n = p.from
			}
			slices.Reverse(out)
			return out
		}
		for _, h := range g.succ[n] {
			if skip != nil && skip(n, h) {
				continue
			}
			if _, seen := prev[h.to]; !seen {
				prev[h.to] = pred{n, h}
				q = append(q, h.to)
			}
		}
	}
	return nil
}

// TestGateL4_HTTPToSQLPath: Handler's closure reads r.URL.Query().Get("id")
// and calls repo.Find(id) on the Repo interface; VTA binds sqlRepo.Find,
// which passes "…" + id to db.Query. A BFS over the emitted graph from the
// closure's r formal_in reaches the stand-in DB.Query's formal_in of
// "query", and the path enters sqlRepo.Find through a param_in. Health
// never queries: from its r, the sink is unreachable.
func TestGateL4_HTTPToSQLPath(t *testing.T) {
	fx := buildL4(t, "dataflow/httpsql")
	a := fx.l4
	if len(a.Degraded) != 0 {
		t.Fatalf("httpsql degraded: %+v", a.Degraded)
	}
	app := a.Application.ID
	if app != "can://httpsql" {
		t.Fatalf("application id %s, want can://httpsql", app)
	}
	idx := bodyIndex(a)
	vertex := func(owner, kind, of string) string {
		t.Helper()
		var found []string
		for id, on := range idx {
			if on.owner == owner && on.node.Kind == kind && on.node.Of == of {
				found = append(found, id)
			}
		}
		if len(found) != 1 {
			t.Fatalf("%s has %d %s vertices of %q, want 1", owner, len(found), kind, of)
		}
		return found[0]
	}
	mod := v2.ModuleID(app, "httpsql.go")
	pkg := "example.com/dataflow/httpsql."
	fn := func(name string) string { return v2.CallableID(mod, pkg+name) }
	closure := func(name string) string { return v2.CallableID(fn(name), pkg+name+".closure_1") }
	method := func(typ, m string) string { return v2.CallableID(v2.TypeID(mod, pkg+typ), pkg+typ+"."+m) }

	sink := vertex(v2.ExternalID(app, "database/sql", "DB.Query"), "formal_in", "query")
	if sink != "can://httpsql/@external/database/sql/DB.Query@formal_in:1" {
		t.Fatalf("sink vertex %s", sink)
	}
	find := method("sqlRepo", "Find")
	g := newSDGGraph(a)

	src := vertex(closure("Handler"), "formal_in", "r")
	p := g.path(src, sink)
	if p == nil {
		t.Fatalf("no path from Handler's r (%s) to %s", src, sink)
	}
	intoFind := slices.ContainsFunc(p, func(h sdgHop) bool {
		return h.family == "param_in" && idx[h.to].owner == find
	})
	if !intoFind {
		t.Errorf("path %v does not enter %s through a param_in", p, find)
	}
	// The interface call binds sqlRepo.Find by VTA.
	vta := false
	for _, e := range a.Application.CallGraph {
		vta = vta || e.Src == closure("Handler") && e.Dst == find && slices.Contains(e.Prov, "vta")
	}
	if !vta {
		t.Errorf("no VTA call_graph edge %s -> %s", closure("Handler"), find)
	}
	for i, h := range p {
		t.Logf("hop %d: %s -> %s", i, h.family, h.to)
	}
	// The shortest path may take the receiver (the coarse host wiring feeds
	// every actual_in of repo.Find(id)); the flow of id itself must exist
	// too: r → Find's id formal_in → the query actual_in.
	findID := vertex(find, "formal_in", "id")
	if g.path(src, findID) == nil || g.path(findID, sink) == nil {
		t.Errorf("no path %s → %s → %s", src, findID, sink)
	}

	// A pair with no flow: Health never touches the database.
	if p := g.path(vertex(fn("Health"), "formal_in", "r"), sink); p != nil {
		t.Errorf("Health's r reaches %s: %v", sink, p)
	}

	// A no-flow pair inside the DB plumbing, so the gate catches
	// over-connection and not only an uncalled callable. Audit is an entry
	// point of its own: it builds its own Repo inline and calls only Count, so
	// it shares no host variable and no in-project callee with the handlers.
	// Its user reaches the sink (through Count), but never Find's id, and
	// Handler's r never reaches Count's user. Either fails if an interface
	// site bound the wrong method or param edges were cross-wired between
	// sites. Reordering Routes' registrations or Audit's statements, or moving
	// Audit in the file, leaves both pairs unreachable (checked by mutation).
	count := method("sqlRepo", "Count")
	auditUser := vertex(fn("Audit"), "formal_in", "user")
	if g.path(auditUser, sink) == nil {
		t.Errorf("Audit's user does not reach %s (the pairs below would be vacuous)", sink)
	}
	if p := g.path(auditUser, findID); p != nil {
		t.Errorf("Audit's user reaches %s: %v", findID, p)
	}
	if p := g.path(src, vertex(count, "formal_in", "user")); p != nil {
		t.Errorf("Handler's r reaches %s's user: %v", count, p)
	}
}

// buildHTTPSQLWithModels is buildL4 over testdata/dataflow/httpsql with
// --library-models models, uncached.
func buildHTTPSQLWithModels(t *testing.T, models sdg.Models) *v2.Analysis {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "dataflow", "httpsql"))
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
	units := b.L3Units(st)
	res := dataflow.RunWithSideTables(b.Fset(), units, 3, 4)
	l4 := sdg.Run(sdg.Input{
		Fset: b.Fset(), Pkgs: b.Pkgs(), ProjectDir: dir, SymbolTable: st,
		Externals: app.Externals, CallGraph: app.CallGraph,
		Units: units, L3: res, K: 3, Jobs: 4, Models: models,
	})
	l4.ApplyTo(app)
	return Emit(app, "httpsql", dir, 4, "gate-test", 3, res, &l4)
}

// TestGateL4_HTTPToSQLSanitizerIsSDKSemantics pins what L4 does — and
// does not do — about SanitizedFind's strconv.Atoi [user, 2026-10-27]. L4 is
// reaching-definitions data flow, not taint: `n, err := strconv.Atoi(id)`
// does make n (and err) depend on id, and that edge is correct. Treating a
// call as a sanitizer is the SDK taint analysis's job (callable or
// variable cuts, including the bypasses below), tracked in
// codellm-devkit/.github#103.
//
//   - (a) The model strconv/Atoi: [] removes the Atoi call site's summary
//     edges and nothing else that matters here: SanitizedHandler's r still
//     reaches DB.Query's query, because the host statement links id → n.
//   - (b) A variable cut on n within sqlRepo.SanitizedFind — the SDK's pair
//     cut: drop the edges whose var is n and whose source node is under that
//     callable (python-sdk commons/graphs.py:380-383) — does NOT sever the
//     path either. Three bypasses remain, each correct data or control
//     dependence:
//     1. err → cdg → Query: the same statement defines err; `if err != nil`
//     controls the Query statement (implicit flow);
//     2. err → return → caller → receiver: `return nil, err` carries err to
//     the caller, whose host feeds the receiver actual_in back into
//     SanitizedFind's s, then s.db → Query;
//     3. caller host → receiver actual_in → s.db: the caller's host
//     statement uses id, and the coarse host wiring feeds every host use
//     into every actual_in, receiver included.
func TestGateL4_HTTPToSQLSanitizerIsSDKSemantics(t *testing.T) {
	plain := buildL4(t, "dataflow/httpsql").l4
	modelled := buildHTTPSQLWithModels(t, sdg.Models{"strconv/Atoi": {}})
	app := modelled.Application.ID
	mod := v2.ModuleID(app, "httpsql.go")
	pkg := "example.com/dataflow/httpsql."
	sanFind := v2.CallableID(v2.TypeID(mod, pkg+"sqlRepo"), pkg+"sqlRepo.SanitizedFind")
	handler := v2.CallableID(v2.CallableID(mod, pkg+"SanitizedHandler"), pkg+"SanitizedHandler.closure_1")
	sink := v2.ExternalID(app, "database/sql", "DB.Query") + "@formal_in:1"
	r := v2.GlobalOrdinal(handler, "@formal_in:1")
	id := v2.GlobalOrdinal(sanFind, "@formal_in:1")
	recv := v2.GlobalOrdinal(sanFind, "@formal_in:0")

	callable := func(a *v2.Analysis, cid string) v2.Callable {
		for _, c := range l4Callables(a) {
			if c.ID == cid {
				return c
			}
		}
		t.Fatalf("no callable %s", cid)
		return v2.Callable{}
	}
	// The Atoi call site: the actual_in whose param_in reaches strconv/Atoi.
	atoi := v2.ExternalID(app, "strconv", "Atoi")
	var atoiSite string
	for _, e := range modelled.Application.ParamIn {
		if strings.HasPrefix(e.Dst, atoi+"@") && strings.HasPrefix(e.Src, sanFind+"@") {
			atoiSite = strings.TrimPrefix(e.Src, sanFind+"@")
			atoiSite = atoiSite[:strings.Index(atoiSite, "/actual_in")]
		}
	}
	if atoiSite == "" {
		t.Fatal("no param_in from SanitizedFind to strconv/Atoi")
	}
	sitesSummary := func(a *v2.Analysis) int {
		n := 0
		for _, e := range callable(a, sanFind).Summary {
			if strings.HasPrefix(e.Src, atoiSite+"/") {
				n++
			}
		}
		return n
	}

	// (a) The model removes the Atoi summary edges; the flow stays.
	if n := sitesSummary(plain); n == 0 {
		t.Fatalf("without a model, the Atoi site %s has no summary edges (the model check would be vacuous)", atoiSite)
	}
	if n := sitesSummary(modelled); n != 0 {
		t.Errorf("strconv/Atoi: [] leaves %d summary edges at %s", n, atoiSite)
	}
	g := newSDGGraph(modelled)
	if g.path(r, sink) == nil {
		t.Errorf("with strconv/Atoi: [], SanitizedHandler's r no longer reaches %s; L4 must keep the id → n data flow (.github#103)", sink)
	}

	// (b) A variable cut on n within SanitizedFind does not sever it.
	cutN := func(src string, h sdgHop) bool { return h.v == "n" && underCallable(src, sanFind) }
	if g.pathExcept(r, sink, cutN) == nil {
		t.Errorf("a cut on n within SanitizedFind severs %s → %s: a bypass below is gone; update this test and .github#103", r, sink)
	}
	// Bypass 1: err → cdg → Query, within SanitizedFind alone.
	local := func(src string, h sdgHop) bool {
		return cutN(src, h) || !underCallable(h.to, sanFind) && h.to != sink
	}
	if p := g.pathExcept(id, sink, local); p == nil || !slices.ContainsFunc(p, func(h sdgHop) bool { return h.family == "cdg" }) {
		t.Errorf("bypass 1 (err → cdg → Query) gone: path %v", p)
	}
	// Bypass 2: err → return → the caller → the receiver, with cdg excluded.
	noCDG := func(src string, h sdgHop) bool { return cutN(src, h) || h.family == "cdg" }
	p2 := g.pathExcept(id, sink, noCDG)
	if p2 == nil || !slices.ContainsFunc(p2, func(h sdgHop) bool { return h.family == "param_out" }) ||
		!slices.ContainsFunc(p2, func(h sdgHop) bool { return h.family == "param_in" && h.to == recv }) {
		t.Errorf("bypass 2 (err → return → caller → receiver) gone: path %v", p2)
	}
	// Bypass 3: the caller's host feeds id into the receiver actual_in.
	// r reaches the receiver without entering SanitizedFind's body first.
	outside := func(_ string, h sdgHop) bool { return underCallable(h.to, sanFind) && h.to != recv }
	p3 := g.pathExcept(r, recv, outside)
	if p3 == nil || g.pathExcept(recv, sink, cutN) == nil {
		t.Errorf("bypass 3 (caller host → receiver actual_in → s.db) gone: %v", p3)
	}
}
