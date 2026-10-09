# Neo4j in `cants` — design & dataflow reference

> Deep-internals reference for the Neo4j output path in `codellm-devkit/codeanalyzer-typescript`.
> Covers purpose, where it sits in the pipeline, the project → rows → writer dataflow, the full
> label/relationship/property catalog, scoping & safety, schema versioning, and the conformance
> contract. Grounded in `src/build/neo4j/*` and `src/utils/serialize.ts` as of schema v2 `2.0.0`.

---
## Git repo

https://github.com/codellm-devkit/codeanalyzer-typescript


## 1. Purpose — what Neo4j *is* here

Neo4j is **one of two co-primary projections of the same canonical schema-v2 envelope**. The
analyzer builds a single additive Code Property Graph (the `TSAnalysis` envelope). `finalizeAnalysis`
produces that envelope once; then:

- the **JSON path** writes it verbatim to `analysis.json`, and
- the **Neo4j path** projects *the same envelope* into a property graph.

> "Second projection of the SAME v2 envelope the JSON path emits … so JSON and graph never diverge."
> — `src/build/neo4j/project.ts` header; `src/utils/serialize.ts::emitNeo4j`.

The point of the graph projection is to make the CPG **queryable with Cypher**: the containment tree,
call graph, CFG/CDG/DDG, interprocedural SDG, inheritance, decorators, imports/exports, and the
repository-artifact layer (dependencies, config keys) all become nodes and typed relationships you
can traverse. Slicing/taint are *not* materialized — the analyzer is a pure graph **provider**; those
are reachability queries that belong to the frontend SDK.

Crucially, the Neo4j vocabulary is **namespaced for a polyglot database**: TypeScript nodes/edges are
`TS`/`TS_`-prefixed so a shared multi-language Neo4j can hold TS, Python and Java graphs side by side
and attribute each element to its origin — while a deliberate set of **language-neutral** nodes
(`:Artifact`, `:Package`, `:ConfigKey`) let sibling analyzers `MERGE` onto the *same* repository-level
nodes.

---

## 2. Where it sits in the pipeline

The whole analyzer is one orchestration function, `analyze()` in `src/core.ts`. Neo4j is **not** a
pipeline stage — it is an *output target* chosen at `emit` time, downstream of everything:

```
                              analyze()  (src/core.ts)
   materialize → symbol table → call graph → dataflow(L3/L4) → cache → finalizeAnalysis
                                                                              │
                                                                              ▼
                                                              AnalysisResult.application
                                                              = TSAnalysis envelope (wire)
                                                                              │
                                                            emit(application, opts)  (serialize.ts)
                                                              │
                        ┌─────────────────────────────────────┼─────────────────────────────────┐
                        ▼                                       ▼                                 ▼
                  --emit json                            --emit neo4j                      --emit schema
             write analysis.json                   emitNeo4j(application, opts)        emitSchema(opts)
             (streamed, #112)                              │                           buildSchemaDocument()
                                                           ▼                           → schema.json (static,
                                            project(application) → GraphRows            no project needed)
                                                           │
                                            ┌──────────────┴───────────────┐
                                            ▼                              ▼
                                  --neo4j-uri set?  yes            --neo4j-uri absent
                                            ▼                              ▼
                                  boltWriter(rows, cfg, …)        writeCypherFile(graph.cypher, rows)
                                  (incremental, live Bolt)        (self-contained snapshot script)
```

**`--emit neo4j` is always full-depth.** `cli.ts` *rejects* combining it with `-a`/`--graphs`
(the level gate applies to the JSON path only). So the projected graph always carries the complete
L4 CPG — see `serialize.ts::emitNeo4j` ("the graph is always projected at full depth") and the
`cli.ts` errors on `--analysis-level` / `--graphs` with `neo4j`.

### Entry points (`src/utils/serialize.ts`)

| Function | Trigger | Output |
|---|---|---|
| `emit()` | always | dispatches on `opts.emit` |
| `emitNeo4j()` | `--emit neo4j` | `project()` then bolt **or** cypher |
| `emitSchema()` | `--emit schema` | `schema.json` via `buildSchemaDocument()` — no project required |

### CLI surface (`src/cli.ts`, `src/options/options.ts`)

- `--emit json|neo4j|schema` (default `json`)
- `--neo4j-uri <uri>` — present ⇒ live Bolt push; absent ⇒ write `graph.cypher`
- `--neo4j-user` / `--neo4j-password` / `--neo4j-database` (env: `NEO4J_USERNAME`, `NEO4J_PASSWORD`, `NEO4J_DATABASE`)
- `--eager` — purge this app's graph and rebuild (otherwise the push only ever adds/updates)
- full run vs. targeted run (`--only`/targetFiles) controls orphan pruning safety

---

## 3. The module map

| File | Lines | Responsibility |
|---|---|---|
| `src/build/neo4j/index.ts` | 8 | Barrel: re-exports `project`, the two writers, schema symbols, row types |
| `src/build/neo4j/schema.ts` | 346 | **The contract.** Node labels + keys + typed props, relationship types + endpoints, derived constraints, indexes, `SCHEMA_VERSION`, `buildSchemaDocument()` |
| `src/build/neo4j/project.ts` | 455 | **Pure projection**: `TSAnalysis` tree → `GraphRows`. No I/O |
| `src/build/neo4j/rows.ts` | 232 | `GraphRows`/`NodeRow`/`EdgeRow`, the `RowBuilder` (in-memory MERGE), scoping helpers, Cypher-literal rendering, `prune()` |
| `src/build/neo4j/cypher.ts` | 139 | **Snapshot writer**: `GraphRows` → self-contained `.cypher` script |
| `src/build/neo4j/bolt.ts` | 310 | **Incremental writer**: diff + push `GraphRows` to a live DB over Bolt |

The split is deliberate: `project.ts` is **pure data** (no driver, no fs), `rows.ts` is the
output-agnostic intermediate both writers consume **identically**, and the two writers differ only
in *how* they realize the same rows (literal script vs. parameterized Bolt).

---

## 4. Dataflow in detail: `project()` → `GraphRows`

`project(app: TSAnalysis)` walks the uniform v2 tree and emits **one graph node per tree/body node**,
keyed on its `can://` id, with containment and every typed overlay as relationships. It accumulates
into a `RowBuilder` and returns the deduped, sorted `GraphRows`.

### 4.1 The tree walk

```
project(app)
├─ Application node  (labels: ["Application","TSApplication"], key id)
├─ for each module in symbol_table:
│    ├─ TSModule node  →  TS_HAS_MODULE (app → module)
│    └─ projectScope(module):
│         ├─ types{}      → projectType     (TS_DECLARES)
│         ├─ functions{}  → projectCallable (TS_DECLARES)
│         └─ fields{}     → projectField     (TS_HAS_FIELD)
│
│  projectType(t):
│    ├─ TSClass/TSInterface/TSEnum/TSTypeAlias/TSNamespace node  →  TS_DECLARES (parent → t)
│    ├─ decorators      → TS_DECORATED_BY (→ shared :TSDecorator)
│    ├─ extends_ids     → TS_EXTENDS   (deferred, resolved-only)
│    ├─ implements_ids  → TS_IMPLEMENTS (deferred, resolved-only)
│    ├─ namespace? recurse projectScope
│    ├─ callables{}     → projectCallable (TS_HAS_METHOD)
│    └─ fields{}        → projectField     (TS_HAS_FIELD)
│
│  projectCallable(c):
│    ├─ TSCallable node (+ :TSAnonymousCallable label if anon signature)  →  owner-rel
│    ├─ decorators → TS_DECORATED_BY
│    ├─ body{}  → one TSBodyNode each  →  TS_HAS_BODY_NODE
│    │            call node with string callee  →  TS_RESOLVES_TO (body → callee)
│    ├─ cfg[]     → TS_CFG_NEXT  (kind-discriminated)
│    ├─ cdg[]     → TS_CDG
│    ├─ ddg[]     → TS_DDG       ((var,prov)-discriminated)
│    ├─ summary[] → TS_SUMMARY
│    ├─ nested callables (closures) → projectCallable (TS_DECLARES)
│    └─ local types                 → projectType
│
├─ repository-artifact layer:
│    ├─ artifacts{}  → :Artifact  → HAS_ARTIFACT; config_keys → :ConfigKey (DEFINES_CONFIG)
│    ├─ dependencies[] → :Package (purl) → DECLARES_DEPENDENCY, LOCKS, TS_PROVIDES → ghost :TSExternal
│    └─ unresolved_imports[] → TS_UNRESOLVED_IMPORT → ghost :TSExternal
│
├─ module bindings: imports/exports → TS_IMPORTS / TS_RE_EXPORTS  (one edge per module-pair, aggregated)
├─ config_uses[]  → TS_USES_CONFIG (body → :ConfigKey)
├─ config_reads[] → TS_READS_CONFIG_UNRESOLVED (app → callee/ghost, folded by key|reason)
├─ external_symbols{} → :TSExternal nodes
├─ synthesized_callables (residual) → :TSAnonymousCallable nodes
└─ application-scope overlays:
     ├─ call_graph[] → TS_CALLS
     ├─ param_in[]   → TS_PARAM_IN
     └─ param_out[]  → TS_PARAM_OUT
```

### 4.2 `RowBuilder` — in-memory MERGE semantics (`rows.ts`)

The builder is the "database" at projection time: it mirrors Neo4j's `MERGE … SET += props`
*before* any writer runs, so each writer just replays a clean, deduped bag.

- **`node(labels, keyProp, value, props)`** — upsert keyed on `labels[0] + "\0" + value`.
  Re-seeing the same node **merges props (last-write-wins)** and **unions labels**. Returns a
  `NodeRef` (`{label, keyProp, value}`) that edges address it by.
- **`edge(type, from, to, props, key?)`** — endpoints known to exist this run.
- **`edgeToSymbol(type, from, targetId, …)`** — a `can://` target that *might not* have materialized
  (e.g. a resolved supertype). **Deferred** and kept at `finish()` only if the target id was actually
  emitted as a node — the "edge-only-when-resolved" rule, so `TS_EXTENDS`/`TS_IMPLEMENTS` never dangle.
- **`finish()`** — resolves deferred edges, then **sorts nodes and edges deterministically** (so
  `graph.cypher` is byte-stable across runs).
- **`prune(props)`** — drops `null`/`undefined`: in Neo4j a null property *is* absence, so the graph
  never stores one. A present-but-empty string list is likewise treated as a non-fact.

### 4.3 `_module` — in memory, never on the graph (#140)

Every project-owned node carries `_module` (its owning file key) *into* the builder. `node()` lifts
it **off** the props and into `NodeRow.module`. It is **never emitted as a graph property** — it only
groups rows for the bolt writer's per-module diff. Scope on the graph comes entirely from the
`can://` id prefix (see §6).

### 4.4 Property flattening

Neo4j properties must be scalars or homogeneous primitive arrays — no nested maps. The projector
flattens accordingly:

- **`span`** → six integers: `start_line/end_line/start_column/end_column/start_byte/end_byte`
  (spellings adopted verbatim from `codeanalyzer-java` under the parity clause).
- **`code`** is *derived at projection time*: `spanCode(source, span.bytes)` slices the module's
  `source` by **UTF-8 byte offsets** via `sliceBytes` (a `Buffer` slice — string slicing would cut
  multi-byte chars). Source is stored once per module (`:TSModule.source`); every narrower node's
  `code` is a byte-slice of it (#179/#201).
- **maps → JSON strings** (sorted keys, to match Python and stay diffable): `keyword_arguments_json`,
  `parameters_json`, `exports_json`, `entrypoint_report_json`.

---

## 5. The graph schema (contract) — `schema.ts`

`schema.ts` is the **single in-repo source of truth**. `--emit schema` serializes it to
`schema.json`, and the conformance test asserts the emitter never produces an undeclared label /
relationship / property.

### 5.1 Merge labels & markers

- Every `can://`-id-keyed node shares the MERGE label **`CanNode`** (one uniqueness constraint,
  uniform edge endpoints) plus one **specific** `TS…` kind label.
- `:TSApplication` merges on bare **`Application`**.
- Neutral repo nodes merge on their own label: `Artifact`, `Package`, `ConfigKey`, `TSDecorator`.
- **Marker labels** `TSCanNode` / `JSCanNode` (`MARKER_LABELS`) ride every `can://` node as **index
  anchors** — property indexes are label-scoped, so `id STARTS WITH $prefix` needs a label to seek on.
  `TSCanNode` rides every node this analyzer writes; `JSCanNode` is a secondary consumer filter on the
  `javascript` namespace. They carry *no* safety claim of their own.

### 5.2 Node labels

| Label | Merge on | Notes |
|---|---|---|
| `TSApplication` | `Application` | root: id, name, schema_version, language, max_level, k_limit, analyzer_name/version, entrypoint report |
| `TSModule` | `CanNode` | per file: name(=fileKey), is_tsx, is_declaration_file, content_hash, exports_json, **source** (whole file), span |
| `TSClass` / `TSInterface` / `TSEnum` / `TSTypeAlias` / `TSNamespace` | `CanNode` | type decls; `signature`, `code`, span, is_exported/is_ambient; TSClass also carries entrypoint flags, base_classes, implements_types |
| `TSCallable` | `CanNode` | functions/methods/ctor/getter/setter/arrow/fn-expr; signature, return_type, cyclomatic_complexity, accessibility, is_async/generator/static/…, code, parameters_json, entrypoint flags |
| `TSAnonymousCallable` | `CanNode` | marker label **alongside** `:TSCallable` for unnamed arrows/fn-exprs (#92) — keeps old `MATCH (:TSAnonymousCallable)` queries working |
| `TSField` | `CanNode` | name, type, span |
| `TSBodyNode` | `CanNode` | statement/call/entry/exit/formal_in/out/actual_in/out; `callee` on calls, `of`/`parent` on synthetics |
| `TSExternal` | `CanNode` | library/ghost targets; name, module |
| `Artifact` | `Artifact` | **neutral** — repo file inventory: path, format, roles, size_bytes, sha256, extraction, source |
| `Package` | `Package` | **neutral** — purl id, ecosystem, name |
| `ConfigKey` | `ConfigKey` | **neutral** — key, namespace, value, references |
| `TSDecorator` | `TSDecorator` | shared decorator target, merged on `qualified_name` so `@Get` and `@Get(':id')` collapse |

### 5.3 Relationship types (grouped)

- **Containment:** `TS_HAS_MODULE`, `TS_DECLARES`, `TS_HAS_METHOD`, `TS_HAS_FIELD`, `TS_HAS_BODY_NODE`
- **Call graph:** `TS_CALLS` (weight, prov), `TS_RESOLVES_TO` (body-call → callee)
- **Intra-callable overlays:** `TS_CFG_NEXT` (kind + `_k`), `TS_CDG`, `TS_DDG` (var, prov, `_k`), `TS_SUMMARY` (var)
- **Interprocedural (SDG):** `TS_PARAM_IN` (var), `TS_PARAM_OUT` (var)
- **Inheritance:** `TS_EXTENDS`, `TS_IMPLEMENTS` (resolved-only)
- **Decorators:** `TS_DECORATED_BY` (positional_arguments, keyword_arguments_json)
- **Imports/exports:** `TS_IMPORTS`, `TS_RE_EXPORTS` (one per module-pair, aggregated name arrays, type_only_names)
- **Repository layer:** `HAS_ARTIFACT`, `DECLARES_DEPENDENCY`, `LOCKS`, `TS_PROVIDES`,
  `TS_UNRESOLVED_IMPORT`, `DEFINES_CONFIG`, `TS_USES_CONFIG`, `TS_READS_CONFIG_UNRESOLVED` (`_k=key|reason`)

### 5.4 The `_k` relationship discriminant (#70)

Some relationships legitimately need **multiple edges between the same endpoint pair**: a conditional's
true/false `TS_CFG_NEXT` pair, or several `TS_DDG` edges (one per `(var, prov)`). A plain
endpoint-pair `MERGE` would collapse them. `EdgeRow.key` sets a discriminant so the `MERGE` is on
`{_k: key}` and distinct edges coexist. Both writers honor it identically.

### 5.5 DDL — constraints & indexes

- **Constraints** are *derived* from node labels: one `IS UNIQUE` per distinct `(mergeLabel, key)` —
  so they can never drift from the labels.
- **Indexes** are curated: `callable_name`, a fulltext `ts_code_fts` on `TSCallable.code` (mirrors
  Python's `py_code_fts`), `cannode_kind`, and the two marker-anchored id range indexes
  (`tscannode_id`, `jscannode_id`) that back every scoped/destructive statement.

---

## 6. Scoping & safety (#140, #116)

Every destructive statement is scoped on the **`can://<app>/` id prefix**, never on a relationship
walk from the Application node. The prefix reaches *every* node the app owns (including ones a walk
would miss) and *nothing* another app owns — even one whose file keys collide. Helpers in `rows.ts`:

- `applicationPrefix(appId)` — refuses anything that is not a bare `can://<app>` id (an empty or
  deeper id would silently widen or narrow the blast radius; it `throw`s instead). This is why a
  hand-built `GraphRows` with no Application node gets **no** destructive statement.
- `descendantPrefix(canId)` — appends `/` so `foo.ts` is not a prefix of `foo.tsx`.
- The app is the **outermost** id segment, so one prefix spans both the `typescript` and `javascript`
  language namespaces → one scoped statement covers both.

Anchoring on `:TSCanNode` (an index) plus the `can://<app>/` prefix is what keeps the wipe from ever
touching a **sibling analyzer's** graph (Python/Java tag `_module`, never apply `:CanNode`).

---

## 7. The two writers

### 7.1 Snapshot — `cypher.ts` → `graph.cypher`

A self-contained, **non-incremental** script. Running it (`cypher-shell < graph.cypher`) rebuilds
this project's subgraph from scratch. Block order:

1. **constraints & indexes** (`IF NOT EXISTS`, idempotent, run first so `MERGE` seeks an index).
2. **scoped wipe** — `MATCH (x:TSCanNode) WHERE x.id STARTS WITH '<prefix>' DETACH DELETE x;` plus a
   second `MATCH` for the root `:Application` itself (the root id `can://<app>` is *outside* its own
   descendant prefix). Rows with no `can://` application id emit **no wipe** (refused visibly).
3. **nodes** — grouped by full label set + keyProp, batched into `UNWIND […] MERGE … SET n += row.p`
   (BATCH 500). Extra labels appended via `SET n:Extra`.
4. **relationships** — grouped by `(type, endpoint labels/keys, keyed?)`, `UNWIND … MATCH a … MATCH b
   … MERGE (a)-[r:TYPE {_k?}]->(b) SET r += row.p`.

Property values are rendered as Cypher **literals** (`cypherValue`/`cypherMap`, with string escaping).

### 7.2 Incremental — `bolt.ts` → live DB

Reads the DB's current state and updates **only what changed**, over Bolt. `neo4j-driver` is imported
**dynamically** so it stays off the JSON hot path. Algorithm:

1. ensure constraints + indexes.
2. **schema-version gate** (#68): read `:Application.schema_version` *scoped to this app's id*; if it
   differs from the producer's `SCHEMA_VERSION`, **force a full re-upsert** (`shouldForceFullUpsert`)
   — a version change forces re-UPSERT, it never deletes (that is `--eager`'s job).
3. partition nodes by owning module (`NodeRow.module`); shared nodes (externals, neutral repo nodes)
   have none and are **MERGE-only, always upserted**.
4. `--eager`? **`EAGER_PURGE`**: `DETACH DELETE` everything under `$prefix`, batched
   `IN TRANSACTIONS OF 5000 ROWS` (one transaction would exhaust `transaction.total.max`, measured at
   2.7 GiB, #116).
5. **diff `content_hash`** per module (`:TSModule.content_hash`, keyed by module id under the prefix)
   → the set of changed modules.
6. per changed module: on `--eager`, `PURGE_MODULE_EDGES` + `PURGE_MODULE_STALE` (delete owned edges
   & vanished decls, keep surviving `$keys`), then upsert its nodes. **Without `--eager` the push only
   adds/updates** — removing nodes is the operator's call.
7. upsert edges owned by a changed module (owner = source node's module) or shared.
8. **orphan prune** (`PRUNE_VANISHED`) — only on a **full run** *and* `--eager`: delete modules under
   the prefix that this run no longer emits, with their subtrees.

**Type fidelity:** `toParams` converts integer-valued numbers to `neo4j.int(...)` so the bolt and
snapshot writers agree on type (the JS driver otherwise stores every number as a float).

---

## 8. Schema versioning & the conformance contract

- `SCHEMA_VERSION = "2.0.0"`. **MAJOR** on a breaking change (renamed/removed label, relationship,
  key); **MINOR** on additive. v2 was a MAJOR bump from v1 (keys moved signature → `can://` id; labels
  reshaped; TS-namespaced vocabulary, #66).
- The repository-artifact layer is **additive within 2.0.0** — the neutral `:Artifact`/`:Package`/
  `:ConfigKey` labels and their edges were added without a version bump, because `SCHEMA_VERSION`
  moves only when *every* sibling analyzer re-baselines together (contract 2.1.0 is pending that).
- **`test/neo4j-schema.test.ts`** is the anti-drift guard: it projects the `dataflow-app` fixture at
  L4 and asserts the real emitter only ever produces labels / relationship types / properties that
  `schema.ts` declares, and that the checked-in `schema.neo4j.json` is regenerated (`bun gen:schema`).
- Other Neo4j tests pin specific behaviors: `neo4j-prefix-scope`, `neo4j-edge-identity` (the `_k`
  discriminant), `neo4j-bindings`, `neo4j-decorators`, `neo4j-bolt`, `bolt-version-gate`.

Keep `schema.ts` and the JSON schema in lockstep, and keep the vocabulary parity-aligned with the
Python/Java backends — both are **contracts**, per CLAUDE.md.

---

## 9. One-paragraph summary

The Neo4j path is a **pure, deterministic projection** of the single schema-v2 CPG envelope.
`project()` walks the uniform tree into a deduped `GraphRows` bag with in-memory MERGE semantics;
one of two writers then realizes those rows — a self-contained idempotent `.cypher` snapshot, or an
incremental content-hash-diffed Bolt push. The vocabulary is TS-namespaced for a polyglot database
with a deliberate neutral-node exception for the repository layer, every destructive statement is
scoped on the `can://<app>/` id prefix anchored to an index, and `schema.ts` is the single declared
contract that a conformance test holds the emitter to.
