package sdg

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

const libstorePkg = "example.com/dataflow/libstore"

// libstoreKey is the L3Key of a callable declared in
// testdata/dataflow/libstore/libstore.go.
func libstoreKey(name string) dataflow.L3Key {
	return dataflow.L3Key{Path: filepath.Join("libstore", "libstore.go"), Signature: libstorePkg + "." + name}
}

func libstoreTarget(name string) Target { return Target{Callable: libstoreKey(name)} }

// TestCHA_LibraryOriginatedReceivers (F3): HandleCtx's
// ctx.Value(k).(Store).Find(id) and HandleMap's v.(Store).Find(id) call Find
// on a value that came out of library storage (context.Context, sync.Map):
// VTA sees no concrete type, so the site's targets include every project
// type implementing Store — sqlStore.Find and memStore.Find, and the Find of
// any other package's type with the same method set — beside the interface
// method (D7). The callers' footprints see the global
// sqlStore.Find writes. HandleLocal's s.Find(id), on a value project code
// built, gets VTA's memStore.Find and no CHA target.
func TestCHA_LibraryOriginatedReceivers(t *testing.T) {
	in, res, d := runDispatch(t)
	all := []Target{libstoreTarget("Store.Find"), libstoreTarget("memStore.Find"), libstoreTarget("sqlStore.Find")}
	fps := footprints(in, d)
	for _, c := range []struct{ fn, site string }{
		{"HandleCtx", "46:9"},
		{"HandleMap", "59:9"},
	} {
		got := d.targets(libstoreKey(c.fn), c.site)
		for _, w := range all {
			if !slices.Contains(got, w) {
				t.Errorf("targets(%s, %s) = %v, want %v among them", c.fn, c.site, got, w)
			}
		}
		// Every project type with Find(string) string implements Store
		// structurally (wrap's too): each target is some type's Find.
		for _, tg := range got {
			if !strings.HasSuffix(tg.Callable.Signature, ".Find") {
				t.Errorf("targets(%s, %s) has %v, not a Find", c.fn, c.site, tg)
			}
		}
		if fp := fps[libstoreTarget(c.fn)]; !slices.Contains(fp.GlobalWrites, libstorePkg+"::lastQuery") {
			t.Errorf("%s writes %v, want %s::lastQuery", c.fn, fp.GlobalWrites, libstorePkg)
		}
	}
	want := []Target{libstoreTarget("Store.Find"), libstoreTarget("memStore.Find")}
	if got := d.targets(libstoreKey("HandleLocal"), "72:9"); !slices.Equal(got, want) {
		t.Errorf("targets(HandleLocal, 72:9) = %v, want %v (no CHA target for a project-built value)", got, want)
	}
	// CHA targets are not VTA's: no call_graph edge (F7).
	for _, e := range res.CallEdges {
		if (e.Source == libstorePkg+".HandleCtx" || e.Source == libstorePkg+".HandleMap") && e.Target != "" {
			t.Errorf("call_graph edge %s -> %s for a CHA target", e.Source, e.Target)
		}
	}
}

// TestCHA_UniverseMethodNoTarget: HandleErr's context.Cause(ctx).Error()
// is an interface call on a library call's result; errStore embeds the
// universe error, so its method set's Error has no package. The CHA
// complement adds no target for it (and does not panic): the site keeps
// what VTA and D7 give it.
func TestCHA_UniverseMethodNoTarget(t *testing.T) {
	_, _, d := runDispatch(t)
	if _, ok := d.sites[siteKey{libstoreKey("HandleErr"), "95:9/2"}]; !ok {
		t.Fatal("HandleErr's context.Cause call has no targets: the site keys moved")
	}
	for _, tg := range d.targets(libstoreKey("HandleErr"), "95:9") {
		if tg.Callable.Signature != "" {
			t.Errorf("targets(HandleErr, 95:9) has %v, want no in-project target", tg)
		}
	}
}
