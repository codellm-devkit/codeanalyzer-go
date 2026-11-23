package neo4j

import (
	"testing"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// A method declared in b.go but whose receiver type lives in a.go: GO_HAS_METHOD
// from the type, and GO_DECLARES from the declaring module b.go.
func TestProject_CrossFileMethodDeclaredByItsModule(t *testing.T) {
	const (
		app      = "can://xf/xf"
		typeMod  = app + "/go/a.go"
		declMod  = app + "/go/b.go"
		typeID   = typeMod + "/T"
		methodID = declMod + "/T/M"
	)
	a := &v2.Analysis{
		IdentityVersion: 2, SchemaVersion: v2.SchemaVersion, Language: v2.Language, MaxLevel: 1,
		Application: v2.Application{
			IdentityVersion: 2, ID: app, Kind: "application",
			SymbolTable: map[string]v2.Module{
				"a.go": {ID: typeMod, Kind: "module", Types: map[string]v2.Type{
					"T": {ID: typeID, Kind: "struct", Signature: "example.com/xf.T",
						Callables: map[string]v2.Callable{
							"example.com/xf.T.M": {ID: methodID, Kind: "method", Signature: "example.com/xf.T.M", SourceFile: "b.go"},
						}},
				}},
				"b.go": {ID: declMod, Kind: "module"},
			},
		},
	}
	rows := Project(a)

	hasMethod, declares := false, false
	for _, e := range rows.Edges {
		if e.To.Value != methodID {
			continue
		}
		switch e.Type {
		case RelHasMethod:
			hasMethod = e.From.Value == typeID
		case RelDeclares:
			declares = e.From.Label == LabelModule && e.From.Value == declMod
		}
	}
	if !hasMethod {
		t.Errorf("no GO_HAS_METHOD from receiver type %s to %s", typeID, methodID)
	}
	if !declares {
		t.Errorf("no GO_DECLARES from declaring module %s to %s", declMod, methodID)
	}
}
