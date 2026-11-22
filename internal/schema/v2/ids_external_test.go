package v2

import "testing"

const extApp = "can://ns/app"

// Unique external ids are returned unchanged: every id already emitted stays valid.
func TestDisambiguateExternalIDs_UniqueIDsUnchanged(t *testing.T) {
	plain := map[string]string{
		"fmt.Println":                 ExternalID(extApp, "fmt", "Println"),
		"strings.Builder.WriteString": ExternalID(extApp, "strings", "Builder.WriteString"),
	}
	got := DisambiguateExternalIDs(plain)
	for sig, id := range plain {
		if got[sig] != id {
			t.Errorf("%s: id changed from %s to %s", sig, id, got[sig])
		}
	}
}

// The motivating case: a method and a function that share a plain id both get
// distinct, suffixed ids, and neither keeps the plain one.
func TestDisambiguateExternalIDs_CollidingCalleesAreDistinct(t *testing.T) {
	const method = "example.com/dep.Box.Put"
	const function = "example.com/dep/Box.Put"
	plainID := ExternalID(extApp, "example.com/dep/Box", "Put") // both map here
	plain := map[string]string{
		method:   ExternalID(extApp, "example.com/dep", "Box.Put"),
		function: plainID,
	}
	if plain[method] != plain[function] {
		t.Fatalf("fixture does not collide: %s vs %s", plain[method], plain[function])
	}
	got := DisambiguateExternalIDs(plain)
	if got[method] == got[function] {
		t.Fatalf("colliding callees still share id %s", got[method])
	}
	for sig := range plain {
		if got[sig] == plainID {
			t.Errorf("%s kept the plain id %s", sig, plainID)
		}
	}
}

// The suffix depends only on the callee's own signature, so adding or removing an
// unrelated callee does not change an id, and the order of the set does not matter.
func TestDisambiguateExternalIDs_StableAcrossSets(t *testing.T) {
	plainID := ExternalID(extApp, "example.com/dep/Box", "Put")
	a := "example.com/dep.Box.Put"
	b := "example.com/dep/Box.Put"
	c := "example.com/other.Box.Put"

	pair := DisambiguateExternalIDs(map[string]string{a: plainID, b: plainID})
	triple := DisambiguateExternalIDs(map[string]string{
		a: plainID, b: plainID, c: ExternalID(extApp, "example.com/other", "Box.Put"),
	})
	if pair[a] != triple[a] || pair[b] != triple[b] {
		t.Errorf("an unrelated callee changed a colliding id: pair=%v triple=%v", pair, triple)
	}
	if triple[c] != ExternalID(extApp, "example.com/other", "Box.Put") {
		t.Errorf("a unique callee was renamed: %s", triple[c])
	}
}
