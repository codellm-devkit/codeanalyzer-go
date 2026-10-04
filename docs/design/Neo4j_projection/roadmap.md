# CLDK Go analyzer — Neo4j projection roadmap

> Sequenced implementation roadmap for the Go Neo4j projection (collision group **D** of the
> v1→v2 migration, now in design). Companion to the spec `specs/neo4j-projection.md` (the
> *what* + decisions) — this is the *how*: ordered milestones → modules, commit boundaries,
> dependencies, and the test gate each milestone must pass.
>
> Tracked under epic `codellm-devkit/.github#94`. Cross-repo: analyzer in `codeanalyzer-go`,
> SDK facade on the `lamwassi/python-sdk` fork. Golden rule (inherited): **pure projection,
> dumb writers** — graph *content* has exactly one definition (`project.go`); writers differ
> only in liveness.

## Initiative: Neo4j projection (`codeanalyzer-go` + `python-sdk`)

A **second projection** of the frozen v2 envelope into a labeled property graph, keyed on
the identical `can://` ids — JSON and graph interchangeable, never divergent. Vocabulary is
`GO_`/`Go`-namespaced for a polyglot database, with the neutral `:Artifact`/`:Package`/
`:ConfigKey` layer reused verbatim as cross-language merge targets. **Full-depth = max
implemented** (L2 today); the L3/L4 overlay vocabulary is declared now and fills additively.

## Dependency order (DAG)

```
M1 schema.go (contract + conformance test + --emit schema)
     │  (the declared vocabulary everything below is held to)
     ▼
M2 rows.go (GraphRows + RowBuilder: MERGE-dedup, deferred edges, prune, scoping, literals)
     │  (the IR both writers consume)
     ▼
M3 project.go (pure projector: v2.Analysis tree → GraphRows)   ◄── the intellectual core
     │
     ├─────────────────────────────┬──────────────────────────────┐
     ▼                             ▼                              ▼
M4 cypher.go (snapshot writer)   M5 bolt.go (incremental writer)   │
     └──────────────┬──────────────┘                              │
                    ▼                                              │
M6 emit.go facade + CLI wiring (dispatch --emit neo4j|schema, flag-error gate)
                    │                                              │
                    ▼                                              ▼
M7 python-sdk fork: GoAnalysisBackend + Go models + GoBackend union
   (pins analyzer version only after the analyzer release is cut)
```

M4 and M5 both depend only on M3 (the rows), and are independent of each other — they can
land in either order or in parallel. M7 depends on a cut analyzer release (lockstep).

## Milestones, commit boundaries, and gates

Each milestone is **one coherent, independently reviewable PR** (per the prompt's commit
discipline). A milestone may be split into the listed commits within its PR, or filed as its
own sub-issue if a reviewer prefers finer PRs — the boundaries below are the natural seams.

### M1 — Schema contract + conformance test + `--emit schema`
**Module:** `internal/neo4j/schema.go` (+ `schema_test.go`).
**Builds:** the declarative single-source-of-truth — node labels + merge keys + typed
properties, relationship types + endpoints, **derived** DDL (one uniqueness constraint per
distinct `(mergeLabel, key)` + curated indexes incl. the `:GoCanNode` id range index),
`SCHEMA_VERSION = "2.0.0"`, and `buildSchemaDocument()` serialization. Declares the full
reserved L3/L4 overlay vocabulary (decision L1) even though nothing emits it yet.
**Depends on:** nothing (pure declaration).
**Commits:** (a) label/relationship/property declarations + `_k` discriminants; (b) derived
DDL + indexes; (c) `buildSchemaDocument()` + `emit_schema()` wiring to `schema.neo4j.json`.
**Gate:**
- `--emit schema` writes a `schema.neo4j.json` that parses and lists every declared family.
- A **conformance scaffold** test exists (asserts-nothing-yet placeholder that M3 fills) and
  the derived-DDL test: exactly one constraint per distinct `(mergeLabel, key)`, no dupes.
- `go build` + `go vet` clean; the checked-in `schema.neo4j.json` is regenerated.

### M2 — Row IR + `RowBuilder`
**Module:** `internal/neo4j/rows.go` (+ `rows_test.go`).
**Builds:** `GraphRows`/`NodeRow`/`EdgeRow`/`NodeRef`; the `RowBuilder` in-memory-MERGE
accumulator (`node()` dedup on `(labels[0], value)` with prop last-write-wins + label union;
append-only `edge()`; **deferred** `edgeToSymbol()` kept at `finish()` only if the target was
emitted); `prune()` (null → absence); the in-memory-only `module` grouping field;
`applicationPrefix`/`descendantPrefix` scoping helpers (refuse a non-bare app id); Cypher
literal/`*_json` rendering; deterministic `finish()` sort.
**Depends on:** M1 (references the declared labels/keys; no runtime dep).
**Commits:** (a) data types + `NodeRef`; (b) `RowBuilder` MERGE/dedup + prune; (c) deferred
edges + `finish()` sort; (d) scoping helpers + literal rendering.
**Gate:**
- Unit tests: node re-see merges props + unions labels; deferred edge dropped when target
  absent, kept when present; `prune()` drops nulls; `applicationPrefix` throws on a non-bare
  id (so no-root rows get no wipe); `finish()` output is order-stable.

### M3 — Pure projector  ◄ the intellectual core
**Module:** `internal/neo4j/project.go` (+ `project_test.go`), fills M1's conformance test.
**Builds:** `project(analysis) → GraphRows` walking the v2 tree in spine order: application
root → per module (`GO_HAS_MODULE`, source/content_hash) → types (`:GoType` + `kind` prop,
`GO_EMBEDS` deferred, `GO_SATISFIES`, fields via `GO_HAS_FIELD`) → callables (`GO_DECLARES`/
`GO_HAS_METHOD`, `error_channel[]`/`source_file` props, closures) → body call nodes
(`GO_HAS_BODY_NODE`, `is_goroutine`/`is_deferred` props, `GO_RESOLVES_TO`) → `call_graph`
(`GO_CALLS`) → imports (`GO_IMPORTS` → module or external ghost) → neutral artifact/package
layer (rows only for facts present) → reserved overlay sections (zero rows at L2). Endpoints
resolve to a declared id or an id-keyed `:GoExternal` ghost — never dangle.
**Depends on:** M1 + M2.
**Commits:** (a) app + module + type walk; (b) callable + body + closures; (c) call_graph +
imports + external ghosts; (d) neutral artifact/package layer + reserved-overlay no-ops.
**Gate (the heart of the suite):**
- **Conformance** (`TestNeo4jSchema_Conformance`): the projector never emits a label /
  relationship / property undeclared in `schema.go` — run over the `multipackage` + `chi`
  fixtures at L2.
- **No dangling** (`TestNeo4j_NoDangling`): every edge endpoint is an emitted node.
- **Two-projection agreement** (`TestNeo4j_IdParity`): every graph `can://`/ordinal id has a
  matching JSON-tree node and vice-versa (modulo containment edges) — cross-checks against
  `v2emit.Emit` output on the same fixture.
- **Go-specific** gates: `:GoType.kind` carries the four kinds; `GO_EMBEDS` vs `GO_SATISFIES`
  split correct; `source_file` prop present only on a cross-file method; `is_goroutine` set
  on a `go`-statement call node; reserved overlay sections emit zero rows at L2.

### M4 — Snapshot writer
**Module:** `internal/neo4j/cypher.go` (+ `cypher_test.go`).
**Builds:** `GraphRows` → self-contained idempotent `graph.cypher`: constraints + indexes
(`IF NOT EXISTS`) → **scoped wipe** (`MATCH (x:GoCanNode) WHERE x.id = <app-id> OR x.id
STARTS WITH 'can://go/<app>/' DETACH DELETE … IN TRANSACTIONS`) → batched `UNWIND … MERGE`
nodes (grouped by label-set + key) → batched `UNWIND … MERGE` edges (grouped by type +
endpoint labels/keys + `_k`?).
**Depends on:** M3.
**Commits:** (a) DDL + scoped-wipe block; (b) node batches; (c) edge batches + literal escaping.
**Gate:**
- `--emit neo4j` (no `--neo4j-uri`) writes `graph.cypher`; **determinism** test: byte-identical
  across runs and `-j` values.
- **Idempotency** (`TestNeo4j_CypherIdempotent`): running the script twice against an empty
  Neo4j (containerized, build-tag-gated) yields the same node/edge counts — or, DB-free, a
  golden-file assertion on the emitted script.
- **Prefix-scope** (`TestNeo4j_PrefixScope`): no-root rows emit no wipe; the wipe predicate is
  anchored on `:GoCanNode` and the `can://go/<app>/` prefix only.

### M5 — Incremental Bolt writer
**Module:** `internal/neo4j/bolt.go` (+ `bolt_test.go`).
**Builds:** content-hash-diffed per-module push over a live DB (lazy Bolt driver dependency,
off the JSON path): ensure DDL → **schema-version gate** (read `:GoApplication.schema_version`
scoped to this app; mismatch forces full re-upsert, never delete) → partition nodes by owning
`module` (shared nodes MERGE-only) → diff each module's `content_hash` → per changed module
upsert; **`--eager`** adds the destructive steps (purge module edges/stale decls, orphan-prune
vanished modules, batched `IN TRANSACTIONS`). Default `--lazy` is purely additive.
**Depends on:** M3 (consumes the identical rows M4 does).
**Commits:** (a) driver bootstrap + DDL ensure + version gate; (b) module diff + additive
upsert (lazy); (c) `--eager` purge/prune.
**Gate:**
- `go build` with and without the Bolt build tag; driver import stays off the JSON hot path.
- **Additive-by-default** test: a `--lazy` push issues no `DELETE`.
- **Scoped-destruction** test: every `--eager` destructive statement carries the
  `can://go/<app>/` prefix predicate.
- Live-DB integration (containerized, build-tag-gated): edit one file → second push touches
  only that module's subgraph.

### M6 — Facade + CLI wiring
**Modules:** `internal/neo4j/emit.go`; `cmd/codeanalyzer/main.go`; `internal/core`.
**Builds:** `emit_neo4j(analysis, opts)` = project → branch on `--neo4j-uri` (set → bolt;
absent → cypher); `emit_schema(opts)`. Replace the two "not yet implemented" error stubs in
`main.go` with real dispatch; enforce **`--emit neo4j` + `-a`/`--graphs` → flag error (exit
2)**; force full depth; keep the existing env-precedence flag plumbing.
**Depends on:** M4 + M5.
**Commits:** (a) facade; (b) CLI dispatch + flag-error gate + full-depth forcing.
**Gate:**
- CLI tests (mirror `cmd/codeanalyzer/main_test.go`): `--emit schema` needs no `--input`;
  `--emit neo4j -a 1` exits 2 with the parity error message; `--emit neo4j` without a URI
  writes `graph.cypher`.
- End-to-end: `cango -i testdata/multipackage --emit neo4j` produces a graph; full existing
  suite stays green.

### M7 — SDK `GoAnalysisBackend` (fork `lamwassi/python-sdk`)
**Modules:** `cldk/analysis/go/{__init__,backend,go_analysis}.py` + `codeanalyzer/` +
`neo4j/`; `cldk/models/go/`; `GoBackend` union + `cache_subdir` key in
`commons/backend_config.py`.
**Builds:** `GoAnalysisBackend(AnalysisBackend[...])` with `N="Go"`, `P="GO_"`; the
type-centric `GoAnalysis` facade (SDK1: `get_types`/`get_type`/`get_functions`/
`get_methods_of`/`get_method`/`get_fields`; SDK2: id-keyed `get_callers`/`get_callees`;
Tier A verbatim); Go Pydantic models mirroring the v2 tree; both the in-process (`analysis.json`)
and read-only Neo4j backends.
**Depends on:** a cut analyzer v2 release (lockstep — pin the analyzer version only after).
**Commits:** (a) Go models; (b) `GoAnalysisBackend` ABC impl + `GoBackend` union; (c)
`GoAnalysis` facade; (d) neo4j read backend.
**Gate:**
- `GoAnalysisBackend` instantiates (all ABC abstractmethods implemented).
- Round-trip: `CLDK(language="go").analysis(project_path=…)` returns a populated type table +
  call graph from a real Go module (in-process backend).
- Analyzer output validates against the Go Pydantic models; `N`/`P` match the graph prefixes.

## Release trains

- **Train 1 — analyzer `--emit neo4j` (part of the v2 line).** M1–M6. Lands behind the
  existing flag stubs; produces the graph at full-depth = L2, grows additively at L3/L4 with
  no contract change. Ships with the analyzer v2 major.
- **Train 1′ — SDK Go wiring (lockstep).** M7 on the fork; pins the analyzer version **only
  after** the analyzer release is cut (the SDK's Go models can't parse v2 until then, and
  there's no Go wiring yet — net-new, not a migration). Per epic #94: "Neo4j projection and
  the JSON schema move in lockstep."

## Not now (considered, deliberately excluded)

- **`taint_flows` / slicing / reachability / source-sink** — the analyzer is a pure graph
  **provider**; labeled traversal is the SDK's job as Cypher queries, a later train. No
  analyzer surface for it.
- **L3/L4 overlay population** — the vocabulary is declared (decision L1) but emits zero rows
  until the deferred L3/L4 analysis train lands. When it does, `project.go` fills the reserved
  sections with **no `schema.go` change**.
- **Go-specific `cfg` edge kinds** (`defer_resume`, `select`) — recorded when L3 is designed.
- **Struct-tag modeling** — Go has struct tags; v2 doesn't model them yet. Not forced in here.
- **TS-SDK `GoAnalysis` encoding** — the facade vocabulary is designed once (M7 Python
  encoding); the TS mirror follows when the TS SDK catches up.
