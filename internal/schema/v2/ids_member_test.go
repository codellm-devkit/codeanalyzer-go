package v2

import "testing"

// The member segment is the signature's last dot-separated part, so the package
// import path (which may itself contain dots) never reaches the id.
func TestMemberIDs_DropPackagePath(t *testing.T) {
	mod := ModuleID("can://ns/app", "go/w.go")
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"type", TypeID(mod, "github.com/acme/app/worker.Worker"), mod + "/Worker"},
		{"function", CallableID(mod, "github.com/acme/app/worker.Run"), mod + "/Run"},
		{"method", CallableID(TypeID(mod, "github.com/acme/app/worker.Worker"), "github.com/acme/app/worker.Worker.Run"), mod + "/Worker/Run"},
		{"closure", CallableID(mod+"/Run", "github.com/acme/app/worker.Run.closure_2"), mod + "/Run/closure_2"},
		{"dotted import path", CallableID(mod, "gopkg.in/yaml.v2.Marshal"), mod + "/Marshal"},
		{"init", CallableID(mod, "github.com/acme/app/worker.init"), mod + "/init"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// Library callees: the import path stays whole, and the member's dots become
// segments, so Type and Member are separate segments.
func TestExternalID_PathForm(t *testing.T) {
	const app = "can://ns/app"
	cases := []struct {
		importPath, sig, want string
	}{
		{"strings", "Builder.WriteString", app + "/@external/strings/Builder/WriteString"},
		{"fmt", "Println", app + "/@external/fmt/Println"},
		{"database/sql", "DB.Query", app + "/@external/database/sql/DB/Query"},
		{"cloud.google.com/go/compute/metadata", "Client.Zone", app + "/@external/cloud.google.com/go/compute/metadata/Client/Zone"},
		{"net/http", "", app + "/@external/net/http"},
	}
	for _, c := range cases {
		if got := ExternalID(app, c.importPath, c.sig); got != c.want {
			t.Errorf("ExternalID(%q, %q) = %q, want %q", c.importPath, c.sig, got, c.want)
		}
	}
}

// Two library symbols with the same member name in different packages must keep
// distinct ids: the import path is the disambiguator.
func TestExternalID_SameMemberDifferentPackagesDistinct(t *testing.T) {
	const app = "can://ns/app"
	a := ExternalID(app, "encoding/json", "Decoder.Decode")
	b := ExternalID(app, "encoding/xml", "Decoder.Decode")
	if a == b {
		t.Fatalf("encoding/json and encoding/xml Decoder.Decode collide at %s", a)
	}
}

// Within one module, every member name is unique: Go forbids duplicate package-
// scope identifiers and duplicate method names per type, so the bare names are
// safe as keys under their parent.
func TestMemberIDs_UniqueWithinModule(t *testing.T) {
	mod := ModuleID("can://ns/app", "go/a.go")
	ids := map[string]bool{}
	for _, id := range []string{
		TypeID(mod, "pkg.T"),
		CallableID(mod, "pkg.F"),
		CallableID(TypeID(mod, "pkg.T"), "pkg.T.M"),
		CallableID(TypeID(mod, "pkg.T"), "pkg.T.N"),
	} {
		if ids[id] {
			t.Errorf("duplicate id %s", id)
		}
		ids[id] = true
	}
}
