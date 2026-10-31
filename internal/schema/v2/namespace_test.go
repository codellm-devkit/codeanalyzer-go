package v2

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestResolveIdentityFixtures(t *testing.T) {
	b, e := os.ReadFile("testdata/namespaces.json")
	if e != nil {
		t.Fatal(e)
	}
	var f struct {
		Invalid []struct {
			Value  string
			Reason string
		} `json:"invalid"`
		Roots []struct {
			ApplicationName string  `json:"application_name"`
			Namespace       *string `json:"namespace"`
			ExpectedRoot    string  `json:"expected_root"`
		}
	}
	if e = json.Unmarshal(b, &f); e != nil {
		t.Fatal(e)
	}
	for _, r := range f.Invalid {
		if strings.Contains(r.Reason, "surrogate") {
			continue
		}
		if _, err := ResolveIdentity(r.Value, nil); err == nil {
			t.Errorf("accepted fixture %q (%s)", r.Value, r.Reason)
		}
	}
	for _, r := range f.Roots {
		i, e := ResolveIdentity(r.ApplicationName, r.Namespace)
		if e != nil || i.ApplicationID != r.ExpectedRoot {
			t.Fatalf("%+v: %+v %v", r, i, e)
		}
	}
}
func TestNamespaceExplicitEmptyRejected(t *testing.T) {
	for _, s := range []string{"", " ", "\u00a0", "a/b", "a\\b", ".", "..", "a\n", string([]byte{255})} {
		if _, e := ResolveIdentity("app", &s); e == nil {
			t.Errorf("accepted %q", s)
		}
		if _, e := ResolveIdentity(s, nil); e == nil {
			t.Errorf("accepted app %q", s)
		}
	}
	s := "AZaz09-._~!()*'"
	i, e := ResolveIdentity(s, nil)
	if e != nil || i.ApplicationID != "can://AZaz09-._~%21%28%29%2A%27/AZaz09-._~%21%28%29%2A%27" {
		t.Fatalf("%+v %v", i, e)
	}
	s = "\u00a0x\u00a0"
	if _, e = ResolveIdentity(s, nil); e != nil {
		t.Fatal(e)
	}
}
