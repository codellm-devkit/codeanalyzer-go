// Package sdg builds the L4 overlay (-a 4): the interprocedural system
// dependence graph over the L3 results. It builds go/ssa over the analyzer's
// go/packages load (bodies for project packages only), resolves dynamic
// calls with VTA, and derives formal/actual vertices, param edges and
// summaries.
//
// The IR here holds no can:// ids: vertices are keyed by (L3Key, local key)
// and cross-callable edges by L3Key pairs and external signatures. The v2
// emitter mints the global ids. This package must not import v2emit or core.
package sdg

import (
	"go/token"

	"golang.org/x/tools/go/packages"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
)

// Target is a call target: an in-project callable or a library stand-in.
type Target struct {
	Callable dataflow.L3Key // in-project callable; interface methods use the interface's declaring file
	External string         // full callee signature (key of app.Externals) for a stand-in; else ""
}

// VertexKind is an L4 port vertex kind.
type VertexKind string

// The L4 port vertex kinds (shared vocabulary).
const (
	KindFormalIn  VertexKind = "formal_in"
	KindFormalOut VertexKind = "formal_out"
	KindActualIn  VertexKind = "actual_in"
	KindActualOut VertexKind = "actual_out"
)

// Vertex is one L4 port vertex of a callable, keyed by its local key.
type Vertex struct {
	Key, Of, Parent string
	Kind            VertexKind
}

// Ref names a vertex of a target by its local key.
type Ref struct {
	Owner Target
	Key   string
}

// ParamEdge is a param_in / param_out edge between two targets' vertices.
type ParamEdge struct {
	Src, Dst Ref
	Var      string
}

// PortEdge is a callable-local ddg edge added at L4.
type PortEdge struct{ Src, Dst, Var, Prov string } // callable-local ddg addition; Prov "reaching-defs" | "points-to"

// SummaryEdge is a call-site summary edge, actual_in to actual_out (local keys).
type SummaryEdge struct{ Src, Dst string }

// CallableSDG is the L4 overlay of one callable.
type CallableSDG struct {
	Vertices []Vertex
	Ddg      []PortEdge
	Summary  []SummaryEdge
}

// Degraded is one degradation marker entry (spec §4.15).
type Degraded struct {
	Code, Detail string
	Packages     []string
	Callables    []dataflow.L3Key
}

// Result is the L4 overlay of a run.
type Result struct {
	Callables         map[dataflow.L3Key]CallableSDG
	Externals         map[string][]Vertex                // stand-in formal vertices by full callee sig
	NewExternals      map[string]schema.GoExternalCallee // externals only VTA reached
	ParamIn, ParamOut []ParamEdge
	CallEdges         []schema.GoCallEdge // prov ["vta"]
	Degraded          []Degraded
}

// Input is what Run needs from the earlier phases.
type Input struct {
	Fset        *token.FileSet
	Pkgs        map[string]*packages.Package
	Variants    []*packages.Package // every loaded package, test variants included (SymbolTableBuilder.Loaded); nil = Pkgs' values
	ProjectDir  string
	SymbolTable map[string]schema.GoFile
	Externals   map[string]schema.GoExternalCallee
	CallGraph   []schema.GoCallEdge // L2 edges (app.CallGraph)
	Units       []dataflow.Unit
	L3          map[dataflow.L3Key]dataflow.Result
	K, Jobs     int
	Models      Models // --library-models (LoadModels); nil = the defaults
}

// Models overrides library stand-in summaries (spec §6), keyed by the
// external id's <import-path>/<sig> tail (`strconv/Atoi`): each pair is a
// flow [in, out] from the stand-in's formal_in of `of` in to its formal_out
// of `of` out. An empty list is no flow; an external with no entry keeps
// every input → every output.
type Models map[string][][2]string

// Degraded codes (spec §4.15; an open vocabulary of plain strings).
const (
	CodeNoSSA        = "no_ssa"
	CodePartialSSA   = "partial_ssa"
	CodePartialTypes = "partial_types"
	CodeNoVTA        = "no_vta"
	CodeUnmappedSSA  = "unmapped_ssa"
	CodeL3Skipped    = "l3_skipped"
	CodeRangeFunc    = "range_func"
)
