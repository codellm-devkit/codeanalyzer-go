package v2emit

// The L4 "done" gate (spec §8): on the HTTP → SQL fixture
// testdata/dataflow/httpsql, the emitted JSON graph carries a flow from an
// HTTP handler's request parameter to database/sql's DB.Query query
// argument through an interface call, and a handler that never queries
// reaches nothing there.

import (
	"slices"
	"testing"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
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
}

func newSDGGraph(a *v2.Analysis) *sdgGraph {
	g := &sdgGraph{succ: map[string][]sdgHop{}}
	add := func(src, dst, family string) { g.succ[src] = append(g.succ[src], sdgHop{dst, family}) }
	for _, c := range l4Callables(a) {
		id := func(k string) string { return v2.GlobalOrdinal(c.ID, k) }
		for _, e := range c.Ddg {
			add(id(e.Src), id(e.Dst), "ddg")
		}
		for _, e := range c.Cdg {
			add(id(e.Src), id(e.Dst), "cdg")
		}
		for _, e := range c.Summary {
			add(id(e.Src), id(e.Dst), "summary")
		}
	}
	for _, e := range a.Application.ParamIn {
		add(e.Src, e.Dst, "param_in")
	}
	for _, e := range a.Application.ParamOut {
		add(e.Src, e.Dst, "param_out")
	}
	return g
}

// path is a shortest path from src to dst by BFS, as the hops taken (nil if
// dst is unreachable). Successors are visited in edge order, so the path is
// deterministic.
func (g *sdgGraph) path(src, dst string) []sdgHop {
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
}
