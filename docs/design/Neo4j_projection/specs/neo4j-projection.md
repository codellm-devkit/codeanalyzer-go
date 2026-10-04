# Spec: `codeanalyzer-go` Neo4j projection (+ `python-sdk` `GoAnalysisBackend`)

> **Status:** design spec, 2026-10-04. Produced by the `designing-cldk-changes` design
> loops (analyzer-side schema loop + SDK-facade loop), each divergence decided with the user.
> **Canonical home:** `codellm-devkit/.github → docs/design/specs/2026-10-04-codeanalyzer-go-neo4j-projection.md`
> — staged here on the fork because `lamwassi` has pull-only access upstream; a maintainer
> ports it. **Tracked under epic** `codellm-devkit/.github#94` (`codeanalyzer-go → canonical
> schema v2`), whose summary already scopes "both projections (analysis.json + Neo4j)" and
> lists the Neo4j backend rung + SDK frontend rung.
>
> This is the *what* (the contract + decisions). The sequenced *how* is the sibling
> `../roadmap.md`. The deep-internals references are `../Python_neo4j_design_and_dataflow.md`
> and `../Typescript_neo4j_design_and_dataflow.md`.

---

## 1. Contract-Impact Triage

**Does this change the schema v2 JSON output?** No. The Neo4j backend is a **second
projection of the frozen v2 envelope** the JSON path already emits, keyed on the identical
`can://` ids and `@line:col` ordinal ids — JSON and graph join on one string, built once
upstream, never recomposed. No new JSON node/field/id shape.

**Does it introduce a new contract?** Yes — two:
1. the **graph schema** (`schema.neo4j.json`: node labels, relationship types, merge keys,
   derived DDL, `SCHEMA_VERSION`), under the cross-language **parity clause**; and
2. the **SDK facade surface** — a net-new `GoAnalysisBackend` (the `cldk/analysis/go`
   package does not exist today).

Both are cross-language shared vocabulary → structural, designed here before any rung.

**Affected repos** (matches epic #94's affected-repos list verbatim):

| Repo | Rung | Change |
| --- | --- | --- |
| `codeanalyzer-go` | backend | `internal/neo4j/` projection package + CLI wiring + conformance/scope tests |
| `python-sdk` (fork `lamwassi/python-sdk`) | frontend | `cldk/analysis/go/{codeanalyzer,neo4j}/`, `GoAnalysisBackend`, `GoBackend` union, Go Pydantic models |
| docs | finishing | README `--emit neo4j` section + graph-vocabulary reference |

No other SDK and no `taint_flows` section: the analyzer is a **pure graph provider** of the
SDG substrate; labeled reachability / slicing / source-sink are SDK Cypher queries, a later
train.

---

## 2. Three invariants (inherited verbatim from the siblings — not re-litigated)

1. **Two-projection agreement.** Graph nodes carry the identical `can://` ids as the JSON
   tree; body-level nodes carry the identical global-ordinal ids (`<callable-id>@<local>`).
   No id recomposition in the projector — it reads ids off the stamped tree.
2. **Monotone / additive-safe writes.** The default `--lazy` Bolt push is pure MERGE-upsert
   and never deletes. Destructive reconciliation (purge/prune) is opt-in under `--eager`.
3. **Prefix-scoped destruction.** Every destructive statement is scoped on the
   `can://<app>/` id prefix, anchored on the `:GoCanNode` index label — one application can
   never corrupt another's subgraph in a shared, multi-language database.

Architecture inherited verbatim: **pure projection, dumb writers.** `project.go → rows.go`
is a deterministic, deduped `GraphRows` bag with no I/O and no driver; two writers
(`cypher.go` snapshot, `bolt.go` incremental) consume the identical rows. `schema.go` is the
single declared contract a conformance test holds the emitter to.

---

## 3. Analyzer-side design decisions (the graph-vocabulary loop)

Decided node-by-node with the user; each is a `GO_`/`Go` **leaf** under the parity clause,
or a verbatim reuse of shared vocabulary. "Ref" names the sibling precedent.

| # | Decision | Rationale / divergence from refs |
| --- | --- | --- |
| **N1** | Node-label prefix **`Go`**, relationship prefix **`GO_`** | Parity with `Py`/`PY_`, `TS`/`TS_`. The SDK `AnalysisBackend` ABC reads these as `N`/`P` ClassVars → `GoAnalysisBackend` sets `N="Go"`, `P="GO_"`. Must agree letter-for-letter. |
| **N2** | `:GoCanNode` marker on every `can://` node; `:GoSymbol` shared merge-label on `:GoType`/`:GoCallable`/`:GoExternal` | Index anchor for prefix seeks (Ref: `PyCanNode`, `CanNode`); one uniqueness constraint + one MATCH-by-id for the symbol family (Ref: `PySymbol`). |
| **T1** | **One `:GoType`** label, `kind ∈ struct\|interface\|alias\|defined` as a **property** | Mirrors the JSON `type.kind` collapse (CLAUDE.md D4). Python has one `:PyClass`; TS has **per-kind labels**. Go deliberately does **not** re-introduce a per-kind split the JSON schema folded away — keeps the two projections symmetric. |
| **T2** | `base_types[]` → **`GO_EMBEDS`** (deferred, edge-only-when-resolved); `interfaces[]` → **`GO_SATISFIES`** (computed) | Go's two mechanisms are genuinely different (explicit embedding = composition; structural satisfaction = implicit). Refs have only `*_EXTENDS`/`*_IMPLEMENTS` for nominal inheritance; reusing those names would misdescribe both. New `GO_` leaves, not renames. |
| **C1** | `GO_DECLARES` (module→func, callable→closure) + `GO_HAS_METHOD` (type→receiver-method) | Parity with `PY_DECLARES`/`PY_HAS_METHOD`. `GO_HAS_METHOD` marks the receiver-type containment that defines a Go method. |
| **C2** | `source_file` → **scalar property** on `:GoCallable` (present only when ≠ nesting module) | The cross-file-method case (CLAUDE.md D8) is Go-only; no ref precedent. Text recovery joins to the declaring `:GoModule` by that path. No new edge/label for a value-shaped fact. |
| **B1** | `is_goroutine` / `is_deferred` → **boolean props** on the call `:GoBodyNode` (pruned when false) | Net-new vs refs (CLAUDE.md D7). Modeled as the JSON models them. Not labels, not edge discriminants — the fact lives on the call site even when the callee is unresolved and no `GO_RESOLVES_TO` edge exists. |
| **F1** | `:GoField` node, id `<type-id>/<name>`, via `GO_HAS_FIELD` | Ref: TS `:TSField`/`TS_HAS_FIELD`, Python `:PyAttribute`. Id minted from the owner type's `can://` id so prefix-purge reaches it. |
| **X1** | **No** `:GoDecorator` / `:GoAttribute` / `:GoVariable` (and their edges) | Go's JSON tree has none. Parity adds at the leaves; it does not force a node a language lacks. The Go label set is legitimately narrower. (Struct tags deferred.) |
| **CG1** | `call_graph` → **`GO_CALLS`** (weight, prov); `error_channel[]` → **string-array prop** on `:GoCallable` | `GO_CALLS` parity with `PY_CALLS` (endpoints = callable id or `:GoExternal` ghost; no dangling). Go errors are returned values, not thrown types — a value-shaped fact rides the node, not a `GO_RAISES` edge. |
| **I1** | `GO_IMPORTS` (aggregated per target) → `:GoModule` (in-project) or `:GoExternal` ghost (stdlib/external, keyed by import path) | Parity with aggregated `PY_IMPORTS` + edge-only-when-resolved. Go imports are overwhelmingly external → ghost targets, never dangling, never minted as `:Package` (packages come from dependencies, not imports). |
| **A1** | Declare neutral `:Artifact`/`:Package`/`:ConfigKey` + `HAS_ARTIFACT`/`DECLARES_DEPENDENCY`(`_k=kind`)/`LOCKS`/`DEFINES_CONFIG` **verbatim**; project only facts the analyzer produces | These are the cross-language **merge targets** — reused unchanged so a sibling over the same repo converges. go.mod deps → `:Package` `pkg:golang/…`; absent facts → **zero rows** (declared contract, empty projection). `GO_PROVIDES`/`GO_UNRESOLVED_IMPORT` stay `GO_`-prefixed (this analyzer's claims). |
| **L1** | Declare the **full L3/L4 overlay vocabulary now**; projector emits **zero rows** until the levels land | `GO_HAS_BODY_NODE`, `GO_CFG_NEXT`(`_k=kind`), `GO_CDG`, `GO_DDG`(`_k=var,prov`), `GO_PARAM_IN`, `GO_PARAM_OUT`, `GO_SUMMARY` + body-node statement kinds. `--emit neo4j` = "full-depth = max implemented" (L2 today). The contract is declared once; L3/L4 fill it with **no schema change**. |
| **K1** | `_k` discriminants: `GO_CFG_NEXT` per `kind`; `GO_DDG` per `(var,prov)`; `DECLARES_DEPENDENCY` per `kind` | Same mechanism as the siblings — parallel edges survive MERGE instead of collapsing. |
| **V1** | `SCHEMA_VERSION = "2.0.0"`, stamped on `:GoApplication`; additive-MINOR suspended for the 2.0.0 line | Parity with the sibling versioning ruling; consumers gate on the analyzer-version floor. |

### Node-label catalog (derived from the decisions above)

| Label | Merge label | Key | Notes |
| --- | --- | --- | --- |
| `:GoApplication` | `GoApplication` | `id` | root `can://go/<app>`; schema_version, analyzer name/version, max_level |
| `:GoModule` | `GoModule` | `id` | per file: package, **source** (whole file), content_hash, span |
| `:GoType` | `GoSymbol` | `id` | `kind ∈ struct\|interface\|alias\|defined` (property), signature, span |
| `:GoCallable` | `GoSymbol` | `id` | signature, return_type, `error_channel[]`, `source_file?`, metrics, span |
| `:GoField` | `GoField` | `id` (`<type-id>/<name>`) | type, span |
| `:GoBodyNode` | `GoBodyNode` | `id` (global ordinal) | `kind` (`call` at L1), `is_goroutine?`, `is_deferred?`, span |
| `:GoExternal` | `GoSymbol` | `id` | library/stdlib ghost; name, import path |
| `:Artifact` | `Artifact` | `id` | **neutral** |
| `:Package` | `Package` | `id` (purl) | **neutral** |
| `:ConfigKey` | `ConfigKey` | `id` | **neutral** |

Every `can://`-keyed node also carries `:GoCanNode`.

### Relationship catalog

- **Containment:** `GO_HAS_MODULE`, `GO_DECLARES`, `GO_HAS_METHOD`, `GO_HAS_FIELD`, `GO_HAS_BODY_NODE`
- **Call graph:** `GO_CALLS` (weight, prov), `GO_RESOLVES_TO` (body-call → callee)
- **Inheritance:** `GO_EMBEDS` (deferred/resolved-only), `GO_SATISFIES`
- **Imports:** `GO_IMPORTS` (aggregated)
- **Repository (neutral):** `HAS_ARTIFACT`, `DECLARES_DEPENDENCY` (`_k`), `LOCKS`, `DEFINES_CONFIG`; (`GO_`) `GO_PROVIDES`, `GO_UNRESOLVED_IMPORT`
- **Reserved L3/L4 overlay (zero rows until the level lands):** `GO_CFG_NEXT` (`_k`), `GO_CDG`, `GO_DDG` (`_k`), `GO_PARAM_IN`, `GO_PARAM_OUT`, `GO_SUMMARY`

### CLI surface (parity with TS/Python)

`--emit json|neo4j|schema` · `--neo4j-uri/-user/-password/-database` · `--eager`/`--lazy`
(lazy default). `--emit neo4j` is **always full-depth** (= max implemented level); combining
it with `-a`/`--graphs` is a **flag error** (exit 2). `--emit schema` needs no `--input`.
The options surface and flags already exist (`internal/options/options.go`,
`cmd/codeanalyzer/main.go`) with env-var precedence `explicit > env > default`; the
dispatch currently returns "not yet implemented" and is wired up in Milestone 6.

---

## 4. SDK-facade design decisions (the `GoAnalysisBackend` loop)

Go is the SDK's **first class-less facade** (no C/procedural precedent exists in `python-sdk`
yet; the live anchors are the class-centric Java + Python facades). Tier A (lifecycle:
`get_application_view`, `get_symbol_table`, `get_call_graph` → `nx.DiGraph`,
`get_call_graph_json`, `get_callers`, `get_callees`, `get_class_hierarchy`-equivalent) is
**invariant — reproduced verbatim**, not re-litigated.

| # | Decision | Rationale |
| --- | --- | --- |
| **SDK1** | **Type-centric** leaf surface: `get_types`/`get_type`, `get_functions` (package-level), `get_methods_of`/`get_method` (receiver methods), `get_fields` | Go has no class → **not** `get_classes`. Functions are kept distinct from methods (no "module-name-as-class" hack Python uses for module funcs). Establishes the SDK's first type-centric precedent for future languages. |
| **SDK2** | `get_callers`/`get_callees` keyed on **callable id / `(type_id, sig)`** | Matches Go's identity model and the C-facade style (take the callable, not class+method strings). Clean for package-level funcs and methods alike. |
| **SDK3** | Scope: **Tier A + L1/L2 leaf accessors + repository-artifact getters + `get_source`**; omit slicing/CFG/CDG/DDG/reachability | Those need L3/L4 the analyzer does not produce. The generic `AnalysisBackend` ABC's abstractmethods are all implemented; L3/L4 methods are absent/`NotImplementedError` per the ABC contract — no misleading always-erroring surface. |
| **SDK4** | `GoAnalysisBackend` sets `N="Go"`, `P="GO_"`; `GoBackend = Union[GoCodeAnalyzerConfig, Neo4jConnectionConfig]` | `Neo4jConnectionConfig` already exists in `commons/backend_config.py` and in each `*Backend` union → the consumer side is **additive**. The graph it reads is "populated out of band" by the analyzer's `--emit neo4j`. |

One facade vocabulary, two encodings when the TS SDK catches up: `cldk/analysis/go/GoAnalysis`
(Python) and `src/analysis/go/GoAnalysis` (TS) mirror method-for-method. This train does the
Python encoding only.

---

## 5. Package layout

Analyzer (new, sibling to `internal/syntactic_analysis/v2emit/`, off the JSON hot path):

```
internal/neo4j/
  schema.go    declarative contract: labels + keys + typed props, rel types + endpoints,
               derived DDL (one uniqueness constraint per (mergeLabel,key)), curated
               indexes incl. the :GoCanNode id range index, SCHEMA_VERSION, --emit schema
  rows.go      output-agnostic row IR: GraphRows/NodeRow/EdgeRow/NodeRef, RowBuilder
               (in-memory MERGE, deferred edges), applicationPrefix/descendantPrefix,
               prune(), Cypher-literal rendering, in-memory-only `module` grouping field
  project.go   pure projector: v2.Analysis tree → GraphRows. No I/O, no driver
  cypher.go    snapshot writer → self-contained idempotent graph.cypher
  bolt.go      incremental writer → content-hash-diffed per-module Bolt push, schema-version
               gate, --eager purge/prune (lazy driver dependency)
  emit.go      facade: emit_schema() (static) and emit_neo4j() (project + write)
```

SDK (on the `lamwassi/python-sdk` fork), mirroring `cldk/analysis/python/{codeanalyzer,neo4j}/`:

```
cldk/analysis/go/
  __init__.py
  backend.py          GoAnalysisBackend(AnalysisBackend[...]) — generic ABC impl, N/P set
  go_analysis.py      the GoAnalysis facade (type-centric surface, SDK1–SDK3)
  codeanalyzer/       in-process backend: runs cango, reads analysis.json
  neo4j/              read-only Neo4j backend: Cypher queries over the GO_-prefixed graph
cldk/models/go/       Go Pydantic models (GoApplication/GoModule/GoType/GoCallable/...)
```

Plus `GoBackend` union + `cache_subdir` "go" key in `cldk/analysis/commons/backend_config.py`.

---

## 6. Release plan (lockstep, under epic #94)

Epic #94's release plan already states: **"Neo4j projection and the JSON schema move in
lockstep."** Concretely:

- **Analyzer** ships the `internal/neo4j/` projection as part of the v2 line. `--emit neo4j`
  is independently landable behind the existing flag stubs; it produces a graph at
  full-depth = L2 today and grows additively when L3/L4 arrive (no contract change — L1
  reserve).
- **SDK** (`GoAnalysisBackend`) pins the analyzer version **only after** the analyzer v2
  release is cut — same ordering constraint as the JSON wiring (the SDK's Go models can't
  parse v2 until then, and there is no Go wiring yet, so this is net-new, not a migration).
- **Version lockstep:** `SCHEMA_VERSION = "2.0.0"` on both sides; the SDK gates on the
  analyzer-version floor (additive-MINOR suspended for the 2.0.0 line).

---

## 7. Tracking (just-in-time, under epic #94)

Shape: **parent + sub-issue stack** (the Neo4j projection is a heavy rung whose units land
as separate reviewable PRs — the roadmap's milestones). Children filed **as each is picked
up**, not up front; the committed spec + roadmap already hold the full plan.

- **Analyzer children** on `codeanalyzer-go` (one PR each): schema contract + conformance
  test → row IR → pure projector → cypher writer → bolt writer → CLI wiring. May be grouped
  (see roadmap) if a reviewer prefers fewer, larger PRs.
- **SDK child** on `lamwassi/python-sdk`: `GoAnalysisBackend` + models + `GoBackend` union.
- Attach each as a native GitHub **sub-issue** of #94 by id (not a hand-kept checklist).
  Filing on the upstream org repo may need a maintainer given pull-only access
  ([upstream-access-readonly]); children on repos the user controls (the fork) attach directly.

---

## 8. Definition of done

- `--emit schema` produces a `schema.neo4j.json` that parses and lists every node family /
  relationship / property the projector can emit.
- `--emit neo4j` with no `--neo4j-uri` writes a `graph.cypher` that runs clean against an
  empty Neo4j and is **idempotent** (second run yields the same graph).
- Conformance test: the real projector never emits a label / relationship / property
  undeclared in `schema.go`.
- **No dangling endpoints:** every `GO_CALLS` / `GO_RESOLVES_TO` / `GO_EMBEDS` / `GO_SATISFIES`
  / `GO_IMPORTS` endpoint resolves to an emitted node (declared or external ghost).
- **Two-projection agreement:** graph node ids == JSON `can://` ids / global ordinals,
  one-to-one at full depth (modulo the explicit containment edges).
- **Determinism:** byte-identical `graph.cypher` across runs and `-j` values.
- **Prefix-scope safety:** a hand-built `GraphRows` with no `:GoApplication` root emits **no**
  destructive statement; a wipe never reaches a second app or a sibling-language subgraph.
- **Parity:** no renamed/repurposed shared vocabulary; `GoAnalysisBackend` `N`/`P` match the
  graph's prefixes exactly; SDK output validates against the Go Pydantic models.
