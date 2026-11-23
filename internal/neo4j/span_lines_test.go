package neo4j

import (
	"testing"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// The projector writes start_line and end_line beside span_json on every span-bearing
// node, and omits them on a span-less synthetic body node.
func TestProject_SpanLineProperties(t *testing.T) {
	const (
		app = "can://ns/app"
		mod = app + "/go/a.go"
		typ = mod + "/T"
		fld = typ + "/n"
		fn  = mod + "/T/M"
		bod = fn + "@12:3"
		syn = fn + "@entry"
	)
	span := func(sl, el int) v2.Span { return v2.Span{Start: [2]int{sl, 1}, End: [2]int{el, 2}} }
	a := &v2.Analysis{
		IdentityVersion: 2, SchemaVersion: v2.SchemaVersion, Language: v2.Language, MaxLevel: 1,
		Application: v2.Application{
			IdentityVersion: 2, ID: app, Kind: "application",
			SymbolTable: map[string]v2.Module{
				"a.go": {ID: mod, Kind: "module", Span: span(1, 20), Types: map[string]v2.Type{
					"T": {ID: typ, Kind: "struct", Span: span(5, 9), StartLine: 5, EndLine: 9,
						Fields: map[string]v2.Field{"n": {ID: fld, Kind: "field", Span: span(6, 6), StartLine: 6, EndLine: 6}},
						Callables: map[string]v2.Callable{
							"M": {ID: fn, Kind: "method", Signature: "example.com/a.T.M", Span: span(11, 14), StartLine: 11, EndLine: 14,
								Body: map[string]v2.BodyNode{
									"12:3":   {ID: bod, Kind: "call", Span: span(12, 12), StartLine: 12, EndLine: 12},
									"@entry": {ID: syn, Kind: "entry"},
								}},
						}},
				}},
			},
		},
	}
	props := map[string]Props{}
	for _, n := range Project(a).Nodes {
		props[n.Value] = n.Props
	}
	cases := []struct {
		id         string
		start, end any
	}{
		{mod, 1, 20},
		{typ, 5, 9},
		{fld, 6, 6},
		{fn, 11, 14},
		{bod, 12, 12},
	}
	for _, c := range cases {
		p, ok := props[c.id]
		if !ok {
			t.Errorf("no row for %s", c.id)
			continue
		}
		if p["start_line"] != c.start || p["end_line"] != c.end {
			t.Errorf("%s: start_line/end_line = %v/%v, want %v/%v", c.id, p["start_line"], p["end_line"], c.start, c.end)
		}
	}
	if p, ok := props[syn]; ok {
		if _, has := p["start_line"]; has {
			t.Errorf("synthetic body node %s carries start_line %v; want absent", syn, p["start_line"])
		}
	}
}
