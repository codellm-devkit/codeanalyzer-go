package sdg

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

// stdlibKey is the L3Key of a callable declared in
// testdata/dataflow/sdg/stdlib.go.
func stdlibKey(name string) dataflow.L3Key {
	return dataflow.L3Key{Path: filepath.Join("sdg", "stdlib.go"), Signature: sdgPkg + "." + name}
}

func byKey(vs []Vertex) []Vertex {
	vs = slices.Clone(vs)
	slices.SortFunc(vs, func(a, b Vertex) int { return cmp.Compare(a.Key, b.Key) })
	return vs
}

// TestStandIn_VerticesFromSignature: fmt.Sprintf's stand-in has formal_in
// format and a (its declared params, named from go/types) and formal_out
// <return>; a ([]any, a slice) is also a by-ref out (the shared predicate).
func TestStandIn_VerticesFromSignature(t *testing.T) {
	_, res, _ := runDispatch(t)
	vs, ok := res.Externals["fmt.Sprintf"]
	if !ok {
		t.Fatalf("Externals lack fmt.Sprintf: %v", keysOf(res.Externals))
	}
	checkVertices(t, "stand-in fmt.Sprintf", vs,
		byKey([]Vertex{fin("0", "format"), fin("1", "a"), fout(":0", "<return>"), fout(":1", "a")}))
}

// TestStandIn_ByRefOut: json.Unmarshal(data []byte, v any) may write through
// v, so v is a formal_out after the result (data, a slice, is one too).
func TestStandIn_ByRefOut(t *testing.T) {
	_, res, _ := runDispatch(t)
	vs, ok := res.Externals["encoding/json.Unmarshal"]
	if !ok {
		t.Fatalf("Externals lack encoding/json.Unmarshal: %v", keysOf(res.Externals))
	}
	checkVertices(t, "stand-in encoding/json.Unmarshal", vs, byKey([]Vertex{
		fin("0", "data"), fin("1", "v"),
		fout(":0", "<return>"), fout(":1", "data"), fout(":2", "v"),
	}))
}

// TestStandIn_EveryTargetedExternal: Externals holds exactly the externals
// some call site targets, each with the formals ports gives it; one with no
// formals at all (runtime.Gosched) is listed with an empty vertex list.
func TestStandIn_EveryTargetedExternal(t *testing.T) {
	in, res, d := runDispatch(t)
	p := newPorts(in, d)
	want := map[string]bool{}
	for _, ts := range d.sites {
		for _, tg := range ts {
			if tg.External != "" {
				want[tg.External] = true
			}
		}
	}
	if got := keysOf(res.Externals); !slices.Equal(got, keysOf(want)) {
		t.Errorf("Externals = %v, want %v", got, keysOf(want))
	}
	for sig, vs := range res.Externals {
		fi, fo := p.formals(Target{External: sig})
		checkVertices(t, "stand-in "+sig, vs, byKey(slices.Concat(fi, fo)))
	}
	vs, ok := res.Externals["runtime.Gosched"]
	if !ok || vs == nil || len(vs) != 0 {
		t.Errorf("runtime.Gosched: listed %v, vertices %#v; want an empty, non-nil list", ok, vs)
	}
}

// fillWith fills the L4 overlay of the dataflow fixture under models, and
// returns it with the warnings it logged.
func fillWith(t *testing.T, models Models) (Result, []string) {
	t.Helper()
	in, d := runPortsInput(t)
	in.Models = models
	var warns []string
	defer func(f func(string, ...any)) { warnf = f }(warnf)
	warnf = func(format string, args ...any) { warns = append(warns, fmt.Sprintf(format, args...)) }
	var res Result
	newSDGBuilder(in, d).fill(&res)
	return res, warns
}

const atoiSite = "27:10"

// TestModels_OverrideCutsFlow: the model strconv/Atoi: [] (no flow) leaves
// atoi's call of it with no summary edge; by default it has one per result.
func TestModels_OverrideCutsFlow(t *testing.T) {
	res, _ := fillWith(t, nil)
	checkSummary(t, "default summary of atoi", siteSummaryOf(res, stdlibKey("atoi"), atoiSite), []SummaryEdge{
		se(atoiSite+"/actual_in:0", atoiSite+"/actual_out:0"), se(atoiSite+"/actual_in:0", atoiSite+"/actual_out:1"),
	})
	res, warns := fillWith(t, Models{"strconv/Atoi": {}})
	if s := siteSummaryOf(res, stdlibKey("atoi"), atoiSite); len(s) != 0 {
		t.Errorf("strconv/Atoi modelled with no flow; summary = %+v", s)
	}
	if len(warns) != 0 {
		t.Errorf("warnings: %v", warns)
	}
}

// TestModels_PairsByOf: a model's pairs name formals by `of`: s → <return>:0
// keeps that one edge and drops the error result's.
func TestModels_PairsByOf(t *testing.T) {
	res, warns := fillWith(t, Models{"strconv/Atoi": {{"s", "<return>:0"}}})
	checkSummary(t, "summary of atoi", siteSummaryOf(res, stdlibKey("atoi"), atoiSite),
		[]SummaryEdge{se(atoiSite+"/actual_in:0", atoiSite+"/actual_out:0")})
	if len(warns) != 0 {
		t.Errorf("warnings: %v", warns)
	}
}

// TestModels_UnknownOfFallsBackToAllToAll: a pair naming an `of` the
// external lacks is a warning, and the external keeps its all-to-all
// default: a typo never refutes a flow.
func TestModels_UnknownOfFallsBackToAllToAll(t *testing.T) {
	for _, c := range []struct {
		pair    [2]string
		unknown string
	}{{[2]string{"str", "<return>:0"}, "in str"}, {[2]string{"s", "<return>"}, "out <return>"}} {
		pair := c.pair
		res, warns := fillWith(t, Models{"strconv/Atoi": {{"s", "<return>:1"}, pair}})
		checkSummary(t, fmt.Sprint("summary of atoi under ", pair), siteSummaryOf(res, stdlibKey("atoi"), atoiSite), []SummaryEdge{
			se(atoiSite+"/actual_in:0", atoiSite+"/actual_out:0"), se(atoiSite+"/actual_in:0", atoiSite+"/actual_out:1"),
		})
		if len(warns) != 1 || !strings.Contains(warns[0], "strconv/Atoi") || !strings.Contains(warns[0], c.unknown) {
			t.Errorf("pair %v: warnings %q, want one naming strconv/Atoi and the unknown of", pair, warns)
		}
	}
}

// TestModels_UncalledEntryIgnored: an entry for an external no site targets
// is no warning and changes nothing.
func TestModels_UncalledEntryIgnored(t *testing.T) {
	res, warns := fillWith(t, Models{"example.com/nosuch/F": {}})
	if len(warns) != 0 {
		t.Errorf("warnings: %v", warns)
	}
	if s := siteSummaryOf(res, stdlibKey("atoi"), atoiSite); len(s) != 2 {
		t.Errorf("summary of atoi = %+v, want the default", s)
	}
}

func writeModels(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestModels_Load: a models file maps <import-path>/<sig> to [in, out]
// pairs; [] is no flow; an empty path is no models.
func TestModels_Load(t *testing.T) {
	m, err := LoadModels(writeModels(t, `{
		"strconv/Atoi": [],
		"strings/TrimSpace": [["s", "<return>"]],
		"database/sql/DB.Query": [["query", "<return>:0"], ["args", "<return>:0"]]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Models{
		"strconv/Atoi":          {},
		"strings/TrimSpace":     {{"s", "<return>"}},
		"database/sql/DB.Query": {{"query", "<return>:0"}, {"args", "<return>:0"}},
	}
	if len(m) != len(want) {
		t.Fatalf("models = %v, want %v", m, want)
	}
	for k, v := range want {
		got, ok := m[k]
		if !ok || !slices.Equal(got, v) || got == nil {
			t.Errorf("models[%s] = %#v, want %#v", k, got, v)
		}
	}
	m, err = LoadModels("")
	if err != nil || m == nil || len(m) != 0 {
		t.Errorf(`LoadModels("") = %v, %v; want Models{}, nil`, m, err)
	}
}

// TestModels_BadFileErrors: a missing file, malformed JSON and a value that
// is not a list of [in, out] string pairs are errors naming the path.
func TestModels_BadFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	paths := map[string]string{"missing": missing}
	for name, body := range map[string]string{
		"malformed":  `{"strconv/Atoi": [`,
		"not object": `[]`,
		"null":       `null`,
		"null value": `{"strconv/Atoi": null}`,
		"short pair": `{"strconv/Atoi": [["s"]]}`,
		"long pair":  `{"strconv/Atoi": [["s", "<return>:0", "x"]]}`,
		"number":     `{"strconv/Atoi": [[0, "<return>:0"]]}`,
		"trailing":   `{} {}`,
	} {
		paths[name] = writeModels(t, body)
	}
	for name, path := range paths {
		m, err := LoadModels(path)
		if err == nil {
			t.Errorf("%s: LoadModels = %v, want an error", name, m)
			continue
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("%s: error %q does not name %s", name, err, path)
		}
	}
}
