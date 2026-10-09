<!--
CHILD issue (M7, SDK frontend rung) for the python-sdk Go wiring. File on the fork
lamwassi/python-sdk (Issues tab must be enabled first: `gh repo edit lamwassi/python-sdk
--enable-issues`), or upstream codellm-devkit/python-sdk via a maintainer. feature_request
form (label: enhancement). A fork-only issue cannot be attached as a sub-issue of upstream
#94 by a pull-only contributor — a maintainer attaches it, or it is re-filed upstream.
Title: python-sdk GoAnalysisBackend — type-centric Go facade (L1/L2) + Neo4j read backend
-->

**Is your feature request related to a problem? Please describe.**

`cldk/analysis/` has no `go` package, so `CLDK(language="go").analysis(...)` has nothing to
call — the SDK cannot read a Go `analysis.json` or query a Go Neo4j graph. This is the
frontend rung (milestone **M7**) of the Neo4j projection, moving in lockstep with the
analyzer's v2 release.

**Describe the solution you'd like**

A net-new Go analysis package mirroring `cldk/analysis/python/{codeanalyzer,neo4j}/`:

- [ ] `cldk/models/go/` — Go Pydantic models (`GoApplication`/`GoModule`/`GoType`/
  `GoCallable`/`GoField`/`GoBodyNode`/`GoExternal`) matching the v2 tree.
- [ ] `cldk/analysis/go/backend.py` — `GoAnalysisBackend(AnalysisBackend[...])`, the generic
  ABC implementation, with `N="Go"`, `P="GO_"` (the graph's node/relationship prefixes).
- [ ] `cldk/analysis/go/go_analysis.py` — the **type-centric** `GoAnalysis` facade:
  Tier-A lifecycle verbatim; `get_types`/`get_type`, `get_functions` (package-level),
  `get_methods_of`/`get_method` (receiver methods), `get_fields`; `get_callers`/`get_callees`
  keyed on the callable id / `(type_id, sig)`; repository-artifact getters + `get_source`.
  **No** `get_classes` (Go has no class); functions kept distinct from methods.
- [ ] `cldk/analysis/go/codeanalyzer/` (in-process: run `cango`, read `analysis.json`) and
  `cldk/analysis/go/neo4j/` (read-only Cypher over the `GO_`-prefixed graph).
- [ ] `GoBackend = Union[GoCodeAnalyzerConfig, Neo4jConnectionConfig]` + the `"go"`
  `cache_subdir` key in `cldk/analysis/commons/backend_config.py`.

**Scope this train (SDK3):** Tier A + L1/L2 leaf accessors + artifact getters + `get_source`.
**Omit** slicing / CFG / CDG / DDG / reachability — they need L3/L4 the analyzer does not
produce; absent per the ABC contract (no always-erroring surface).

Full facade decisions: `codeanalyzer-go/docs/design/Neo4j_projection/specs/neo4j-projection.md` §4.

**Gate (this PR must pass):**
- `GoAnalysisBackend` instantiates (all ABC abstractmethods implemented).
- `CLDK(language="go").analysis(project_path=…)` returns a populated type table + call graph
  from a real Go module (in-process backend).
- Analyzer output validates against the Go Pydantic models; `N`/`P` match the graph prefixes.
- Pins the analyzer version **only after** the analyzer v2 release is cut (lockstep).

**Describe alternatives you've considered**

- *Class-centric facade reusing `get_classes`/`get_method(class, name)`* — rejected: "class"
  is a lie for Go and conflates package-level functions with methods (the "module-as-class"
  hack). Type-centric honors Go's real structure and sets the SDK's first class-less precedent.
- *Full Python-parity surface with L3/L4 stubs* — rejected: ships methods that always error.

**Additional context**

Built on the fork `lamwassi/python-sdk`. `Neo4jConnectionConfig` already exists in
`commons/backend_config.py` and in each `*Backend` union, so the Neo4j consumer side is
additive — the graph it reads is "populated out of band" by the analyzer's `--emit neo4j`.
