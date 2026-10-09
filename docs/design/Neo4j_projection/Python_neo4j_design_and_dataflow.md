# Neo4j design — the graph projection of `codeanalyzer-python`

> **Status:** design/architecture reference, written 2026-10-02 against `codeanalyzer/neo4j/`.
> For the *authoritative vocabulary* (every label, every property, every `PY_*` relationship),
> read `docs/skills/analyzing-canpy-graphs/references/vocabulary.md` and the machine-readable
> `schema.neo4j.json` (regenerate with `uv run canpy --emit schema`). This document explains
> **purpose, dataflow, module responsibilities, and the projection algorithm** — the *why* and
> *how*, not a second copy of the vocabulary table.

---
## Repo
https://github.com/codellm-devkit/codeanalyzer-python

## 1. Purpose

`codeanalyzer-python` emits **canonical schema v2** — one additive Code Property Graph (CPG)
tree — in two **projections** of the *same facts* keyed on the *same ids*:

1. **`analysis.json`** — the tree itself (the `Analysis` envelope), the single wire format.
2. **Neo4j** — a **near-identity projection** of that tree into a labeled property graph.

The Neo4j backend exists so the CPG can be **queried as a graph**: entrypoint reachability,
call-graph and taint traversals, data-/control-dependence slicing, config-key reads — all the
things that are natural as Cypher path queries and awkward as JSON tree walks. It is the
substrate the cross-language CLDK SDK's Neo4j backends attach to.

**Core invariant — JSON == Graph.** The projection is *lossless* up to re-expression:
> re-expressing a JSON field as an edge or a flattened property is fine; silently dropping one is not.

(This is the `2026-09-10-neo4j-json-parity.md` contract, #202/#203.) A JSON-side node and its
graph node **join on one string** — the same `can://` id or global ordinal id — with no id
recomposition. That identity equality is what makes the two projections interchangeable and is
built once, upstream, by `codeanalyzer/dataflow/identity.py`.

**What it is *not*.** The analyzer is the **provider**: it emits the SDG *substrate* only. There
is **no `taint_flows` section** by design — labeled reachability + source/sink model packs are the
SDK's job. The backward slicer is internal (an L3/L4 validation gate), not a product surface.

---

## 2. Where it sits (the module map)

Everything lives in `codeanalyzer/neo4j/` and runs **only** when `--emit neo4j` (or
`--emit schema`) is selected — it is off the default JSON path entirely, and the `neo4j` Bolt
driver is an **optional** dependency imported lazily.

| File | Lines | Responsibility |
| --- | --- | --- |
| `emit.py` | ~80 | **Facade** between the CLI and the backend. `emit_schema()` (static contract) and `emit_neo4j()` (project + write). |
| `schema.py` | ~430 | **Declarative schema contract** — the single in-repo source of truth: node labels + keys + typed props, relationship types + endpoints, derived DDL (constraints + indexes), `SCHEMA_VERSION`. |
| `project.py` | ~940 | **The projector** — walks the `PyApplication` IR and emits graph rows. This is where JSON → row mapping happens. |
| `rows.py` | ~230 | **Output-agnostic row IR** (`GraphRows`, `NodeRow`, `EdgeRow`, `NodeRef`) + the `RowBuilder` (MERGE-in-memory accumulator) + Cypher-literal rendering helpers. |
| `cypher.py` | ~160 | **Snapshot writer** — renders `GraphRows` to a self-contained `graph.cypher` script. |
| `bolt.py` | ~310 | **Incremental writer** — pushes `GraphRows` into a live Neo4j over Bolt, updating only what changed. |
| `__init__.py` | ~45 | Public surface re-exports. |

**Design principle: the projection is pure, the writers are dumb.** `project.py` → `rows.py`
produces a deterministic, deduped bag of nodes and edges with **no I/O and no driver**. Both
writers then consume that identical bag — one renders text, one talks Bolt. Nothing about the
graph's *content* lives in a writer.

---

## 3. End-to-end dataflow

```
                       codeanalyzer analysis pipeline (syntactic → semantic → dataflow)
                                              │
                                              ▼
                                   Analysis envelope  (in memory)
                                   └─ application: PyApplication
                                        ├─ symbol_table {<file>: PyModule}   ← the node tree
                                        ├─ call_graph  [PyCallEdge]
                                        ├─ external_symbols {<id>: …}
                                        ├─ param_in / param_out  [ParamEdge]
                                        ├─ artifacts / dependencies / unresolved_imports
                                        └─ entrypoint_report
                                              │
                             emit_neo4j(analysis, options)         ── emit.py (facade)
                                              │
                 ┌────────────────────────────┼─────────────────────────────┐
                 │ 1. assign_ids(app, app_name)                              │
                 │    → stamps every module/class/callable with its can://   │
                 │      id (idempotent); returns sig_to_id map               │
                 │ 2. project(app, app_name, sig_to_id, analyzer)            │
                 └────────────────────────────┼─────────────────────────────┘
                                              │
                                     project.py  (the projector)
                                     walks IR, calls RowBuilder.node()/.edge()/.edge_to_symbol()
                                              │
                                              ▼
                                     RowBuilder.finish()
                                     - MERGE-dedup nodes in memory
                                     - resolve deferred "edge-only-if-target-exists" edges
                                     - sort deterministically
                                              │
                                              ▼
                                        GraphRows  (rows.py)
                                     { nodes: [NodeRow], edges: [EdgeRow] }   ← pure data
                                              │
                        ┌─────────────────────┴──────────────────────┐
                        │  --neo4j-uri set?                           │
                 no ────┤                                             ├──── yes
                        ▼                                             ▼
             render_cypher(rows)  (cypher.py)              bolt_writer(rows, cfg, …)  (bolt.py)
             → graph.cypher file                           → live Neo4j over Bolt
             - constraints + indexes                       - ensure constraints + indexes
             - scoped wipe of prior subgraph               - diff content_hash per module
             - batched UNWIND … MERGE nodes                - purge/upsert only changed modules
             - batched UNWIND … MERGE edges                - optional orphan prune (full+eager)
             (full truth, non-incremental)                 (incremental, additive by default)
```

### The two-step facade (`emit.py`)

`emit_neo4j(analysis, options)` does exactly two things before handing off to a writer:

1. **`assign_ids(analysis.application, app_name)`** — idempotent. It stamps every
   module/class/callable with its canonical `can://` id and returns the `signature → id` map that
   the projection keys nodes on. *This is what keeps the JSON and Neo4j projections in agreement:*
   both are built from the same stamped tree and the same `sig_to_id`.
2. **`project(application, app_name, sig_to_id, analyzer)`** → `GraphRows`.

Then it branches on `options.neo4j_uri`: present → Bolt push; absent → `graph.cypher` snapshot in
the output directory (`GRAPH_CYPHER`).

`emit_schema(output)` is independent — it needs no analyzed project. It serializes
`build_schema_document()` to `schema.json`, the version-stamped contract.

---

## 4. The row IR and `RowBuilder` (`rows.py`)

`GraphRows` is a **deterministic, deduped bag** of `NodeRow`s and `EdgeRow`s. The data shapes:

- **`NodeRow`** — `labels` (`labels[0]` is the constrained MERGE label; the rest are SET as extra
  labels), `key_prop`, `value` (the key), `props`, and an **in-memory-only** `module` field (the
  owning module's id, used by the incremental writer's per-module diff — never emitted to the graph).
- **`EdgeRow`** — `type`, `from_ref`, `to_ref`, `props`, and an optional `key` **relationship
  discriminant**.
- **`NodeRef`** — how an edge addresses an endpoint: `(label, key_prop, value)` to MATCH on.

`RowBuilder` is the in-memory analog of Cypher MERGE semantics. Three design choices live here:

1. **Node MERGE-dedup.** `node(labels, key_prop, value, props)` upserts on `(labels[0], value)`:
   re-seeing the same identity merges props (last-write-wins) and unions labels. A hot external
   symbol or a canonical decorator collapses to one row. (Edges via `.edge()` are **append-only** —
   not deduped — so per-package facts need an explicit `seen` guard in the projector.)

2. **The `PyCanNode` marker.** Any node keyed by a `can://` id automatically gets the extra label
   `PyCanNode`. It is a pure **index anchor** (Neo4j property indexes are label-scoped) so that the
   prefix predicate `id STARTS WITH $p` can *seek* instead of scanning the store. It carries no
   safety claim — the test is the `can://` scheme, never a language segment.

3. **The "edge-only-when-resolved" rule.** `edge_to_symbol(...)` *defers* an edge whose target may
   be library/external code not present in the graph. At `finish()`, a deferred edge is kept **only
   if** its target `(label, value)` was actually emitted as a node — so it never dangles. (The
   string fallback for an unresolved base class survives on the source node's own props instead.) In
   the whole projector this is used in **exactly one** place: `PY_EXTENDS` (base classes, resolved by
   the per-module `_base_ref_resolver`: local class → import-table resolve → `@external` ghost).
   `PY_RESOLVES_TO` deliberately uses the plain `b.edge()` instead, because a call node's `callee` is
   *already* a resolved `can://` id — routing it through the symbol fallback would match a
   `signature` property against an id and emit an edge that matches nothing at load time.

`finish()` resolves deferred edges, sorts nodes and edges by a stable key, and returns the
`GraphRows`. Determinism matters: identical input → byte-identical `graph.cypher`.

> **Neo4j-legal values only.** Property values are primitives or homogeneous arrays of primitives.
> `None` is pruned (`prune()`) — in Neo4j a null property is simply absence, so "not carried" and
> "empty" never collide. `Optional[Any]` fields (variable `value`, call `arguments`) are
> JSON-encoded into a `*_json` string property.

---

## 5. The projection algorithm (`project.py`)

`project(app, app_name, sig_to_id, analyzer)` walks the IR in this order, emitting rows as it goes:

1. **Application root** → `:PyApplication` node, keyed on the `can://<app>` id (**not** on the
   free-text `--app-name`, so two apps analyzed under one name stay two roots). Carries analyzer
   identity, repo info, and the whole entrypoint report (`entrypoint_report_json` + always-present
   `entrypoint_frameworks`).

2. **Per module** (`symbol_table` walk) → `:PyModule` node (`id` key, carries whole-file `source`,
   `content_hash`), `PY_HAS_MODULE` edge from the app, then `_project_module_body` for its
   functions, classes, module-level variables, and imports.

3. **Call graph** (`app.call_graph`) → the aggregated `PY_CALLS` twin: one edge per `(src, dst)`
   with `weight` + `prov`, endpoints resolved by `_call_endpoint`.

4. **Program graphs** (`_project_program_graphs`) → the L3/L4 CPG overlay: `:PyBodyNode` nodes +
   `PY_HAS_BODY_NODE`, `PY_CFG_NEXT`, `PY_CDG`, `PY_DDG`, and (L4) `PY_PARAM_IN`/`PY_PARAM_OUT`/
   `PY_SUMMARY`. Idempotent — a no-op below the level that populates the body fields.

5. **Artifacts/dependencies** (`_project_artifacts`) → the language-neutral `Artifact`/`Package`/
   `ConfigKey` subgraph. **Always emitted** (L1 data, identical at every `-a`).

6. **Config uses** (`_project_config_uses`) → `PY_USES_CONFIG` (resolved reads: body node →
   `ConfigKey`) and `PY_READS_CONFIG_UNRESOLVED` (first-class unresolved reads).

### Identity: the three id forms used as merge keys

The projection never invents ids — it reads them off the stamped tree so both projections agree:

- **`can://` ids** for every node at/above a callable (`:PyApplication`, `:PyModule`, `:PyClass`,
  `:PyCallable`, `:PyExternal`, `Artifact`, `ConfigKey`). `can://<app>` is the outermost prefix of
  every id for that application.
- **GLOBAL ordinal ids** `"<callable-id>@<local>"` for `:PyBodyNode` and every cross-callable
  dataflow endpoint. `_global_ordinal()` **must** agree with `IdentityMap.global_id` — that is the
  two-projection agreement for body-level nodes.
- Package/decorator nodes merge on `name` (shared, cross-application) — never pruned, so
  per-application facts ride the *relationship*, not the node.

**The `:PySymbol` idiom.** Both declared symbols (`:PyClass`, `:PyCallable`) and external ghosts
(`:PyExternal`) carry the shared **`:PySymbol`** MERGE label. Because `RowBuilder` merges on
`(labels[0], value)`, a call to a bare imported name and its `PY_PROVIDES` ghost collapse onto one
node — correctly, since they name the same real-world symbol. Attribute and variable ids are minted
from the **owner's `can://` id** (`<class-id>/<name>`, `<owner-id>/<name>@<line>`), so they carry the
application segment and the module's prefix-purge reaches them (a signature-minted id would be
identical across two applications and MERGE them onto one node).

### How endpoints resolve (the resolved/external split)

`_call_endpoint` is **authoritative**, not heuristic: classification comes from
`app.external_symbols` (keyed by `can://…/@external/…` id), so an imported module name — which
exists only as a `:PyPackage` — can never shadow a real call target. A declared endpoint resolves
to its `can://` id; anything neither declared nor listed falls back to an **id-keyed `:PyExternal`
ghost** rather than raising. **No `PY_CALLS` endpoint ever dangles** — every target joins the id
space, either as a declared node or as an external ghost.

### JSON source → graph row mapping

| IR source (JSON) | Node label(s) | Relationship(s) | Projector |
| --- | --- | --- | --- |
| `application` + `entrypoint_report` + `analyzer` | `PyApplication` | — | `project` |
| `symbol_table[file]` (`PyModule`) | `PyModule` | `PY_HAS_MODULE` | `project` / `_project_module_body` |
| `PyModule.functions`, `PyClass.callables` | `PyCallable` (`:PySymbol`) | `PY_DECLARES`, `PY_HAS_METHOD` | `_project_callable` |
| `PyModule.types` / `PyClass` (nested) | `PyClass` (`:PySymbol`) | `PY_DECLARES`, `PY_EXTENDS` (base classes, deferred) | `_project_class` |
| `PyClass` attributes | `PyAttribute` | `PY_HAS_ATTRIBUTE` | `_project_attribute` |
| module/callable variables | `PyVariable` | `PY_DECLARES_VAR` | `_project_variable` |
| decorators | `PyDecorator` (merged on `qualified_name`) | `PY_DECORATED_BY` (span + args ride the edge) | `_project_decorator` |
| `PyModule.imports` (aggregated per target) | `PyModule`/`PyPackage` target | `PY_IMPORTS` (`spellings`, `positions_json`) | `_project_imports` |
| `application.call_graph` (`PyCallEdge`) | declared `PyCallable` or `PyExternal` ghost | `PY_CALLS` (`weight`, `prov`) | `project` / `_call_endpoint` |
| `external_symbols` endpoints | `PyExternal` (`:PySymbol`) ghost | (endpoint of `PY_CALLS`/`PY_RESOLVES_TO`) | `_external_ghost` / `_call_endpoint` |
| callable `body`/`cfg`/`cdg`/`ddg` (L3) | `PyBodyNode` | `PY_HAS_BODY_NODE`, `PY_CFG_NEXT` (`kind`), `PY_CDG`, `PY_DDG` (`var`,`prov`) | `_project_program_graphs` |
| call-site body nodes | `PyBodyNode` (`kind:"call"`, `callee_signature` joined from `call_sites`) | `PY_RESOLVES_TO` → callee | `_project_program_graphs` |
| param vertices + `param_in`/`param_out`/`summary` (L4) | `PyBodyNode` (`formal_in`/`_out`, `actual_in`/`_out`) | `PY_PARAM_IN`, `PY_PARAM_OUT`, `PY_SUMMARY` | `_project_program_graphs` |
| `artifacts` | `Artifact` (neutral) | `HAS_ARTIFACT`, `DEFINES_CONFIG` | `_project_artifacts` |
| `dependencies` | `Package` (neutral, purl) | `DECLARES_DEPENDENCY` (`kind`-disc.), `LOCKS`, `PY_PROVIDES` | `_project_artifacts` |
| config keys (per artifact) | `ConfigKey` (neutral) | `DEFINES_CONFIG` | `_project_artifacts` |
| `unresolved_imports` | `PyExternal` ghost | `PY_UNRESOLVED_IMPORT` | `_project_artifacts` |
| config reads (#162) | (bridges body node → `ConfigKey`) | `PY_USES_CONFIG`, `PY_READS_CONFIG_UNRESOLVED` | `_project_config_uses` |

---

## 6. The schema contract (`schema.py`)

`schema.py` is the **single declarative source of truth** for the graph contract. It is *not* just
documentation — three things are derived from it:

- **The DDL.** `uniqueness_constraints()` emits **one constraint per distinct `(merge_label, key)`**,
  so a new label automatically brings its own constraint — there is no second list to keep in sync.
  Plus hand-written `INDEXES` (name/code lookups, and the critical `py_can_node_id` range index that
  makes every scoped/destructive statement a prefix *seek*).
- **`--emit schema`** serializes the whole thing (`build_schema_document()`) to `schema.json`.
- **A conformance test** (`test/test_neo4j_schema.py`) asserts the real emitter in `project.py`
  **never** produces a label / relationship / property not declared here. So the contract cannot
  silently drift from the projector.

### Relationship identity: the `_k` discriminant

A plain endpoint-pair MERGE would **collapse legitimately-distinct parallel edges**. Several
relationships therefore carry an internal `_k` discriminant (set via `EdgeRow.key`), so the MERGE is
on `{_k: …}`:

- `PY_DDG` merges per `(var, prov)` — one dependence per variable, and the ssa/points-to split.
- `PY_CFG_NEXT` merges per `kind` — a conditional's true/false pair.
- `DECLARES_DEPENDENCY` merges per `kind`; `PY_READS_CONFIG_UNRESOLVED` per `(key, reason)`.

### Namespacing: `PY_` vs neutral

Code-shaped vocabulary is **`PY_`-prefixed by design** (`:PyModule`, `PY_CALLS`, …) so a shared,
multi-language Neo4j database never mingles one analyzer's facts with another's. The
`Artifact`/`Package`/`ConfigKey` subgraph is **deliberately un-prefixed** — these are cross-language
**merge targets**, so a sibling-language analyzer over the same repo lands on the *same* node
instead of a per-language duplicate. (`PY_PROVIDES` / `PY_UNRESOLVED_IMPORT` stay `PY_` — they are
*this* analyzer's claim about what an import resolves to.)

### Versioning

`SCHEMA_VERSION = "2.0.0"` is stamped onto every `:PyApplication` node so a consumer can detect a
producer/consumer mismatch at runtime. MAJOR bumps on a breaking change, MINOR on additive — but the
**additive-MINOR rule is suspended for the 2.0.0 line** (per the 2026-09-07 ruling): additive
properties ship without a bump and consumers gate on the **analyzer version** floor instead.

---

## 7. The two writers

Both consume the identical `GraphRows`; they differ only in liveness and incrementality.

### Snapshot writer — `cypher.py` → `graph.cypher`

Renders a **self-contained, non-incremental** `.cypher` script. Running it (e.g.
`cypher-shell < graph.cypher`) rebuilds this project's subgraph from scratch:

1. constraints + indexes;
2. a **scoped wipe** of the prior version — `MATCH (x:PyCanNode) WHERE x.id = <app-id> OR x.id
   STARTS WITH 'can://<app>/'` then `DETACH DELETE … IN TRANSACTIONS`. Scoped by id prefix, so it is
   **one application by construction** — a second Python app sharing a module path, and a sibling
   analyzer's `:Py*` graph, are outside it (shared `:Package` purls stay outside too);
3. batched `UNWIND … MERGE` for nodes (grouped by full label set + key prop);
4. batched `UNWIND … MERGE` for edges (grouped by `(type, endpoint labels + key props, discriminated?)`).

A static script has no view of the live DB, so it expresses the **full truth** — incremental updates
are the Bolt writer's job.

### Incremental writer — `bolt.py` → live Neo4j

Reads the DB's current state and updates **only what changed**, with the **module subgraph as the
unit of idempotent replacement**:

1. ensure constraints + indexes;
2. **diff** each module's `content_hash` against the DB (keyed by module `id` inside the app prefix,
   so a second app sharing a path is never mistaken for "unchanged") → the set of changed modules;
3. per changed module, in a transaction: purge the edges it owned + detach-delete the declarations it
   no longer emits, then upsert its current nodes;
4. upsert edges owned by changed modules + the shared edges;
5. on a **full run** only, prune modules whose source file vanished (batched, to avoid exhausting the
   transaction memory limit).

**A push never deletes by default (#171).** Steps 3 and 5 — the only destructive steps — run under
`--eager` only. A default `--lazy` push is **purely additive** (MERGE-upsert, nothing removed); the
cost is staleness (a removed declaration/edge lingers until an `--eager` push reconciles it). The
trade is deliberate: an incremental push into a *shared* database should not be able to destroy
anything, and the destructive rebuild is opt-in under the same flag that forces a clean analysis
rebuild.

**Every destructive statement is scoped on the `can://` id prefix (#173).** The id is a path
(`can://<app>/python/<file>/...`), so `id = <module-id> OR id STARTS WITH <module-id>/` is exact
containment — one application, one language, one module at once — which neither a label anchor nor the
retired `_module` property could give. `:PyCanNode` anchors the predicate so it *seeks* an index
instead of scanning the store.

Shared nodes (`:PyExternal` / `:PyPackage` / `:PyDecorator`) have no owning module and are MERGE-only,
so a declaration another (unchanged) module still references survives and its incoming edges stay
valid.

---

## 8. CLI surface and gating

```
canpy -i <project> --emit neo4j [--neo4j-uri bolt://… --neo4j-user … --neo4j-password … --neo4j-database …]
                                [--eager | --lazy]        # --lazy is the default (additive push)
canpy --emit schema [-o <dir>]                            # static contract → schema.json (no project needed)
```

- **Neo4j is always full-depth (#119).** `--emit neo4j` forces **level 4 with every graph section**;
  passing an explicit `-a`/`--graphs` alongside it is a **flag error** (code 2). The graph carries
  every level's facts, so depth/section selectors cannot be combined with it.
- With `--neo4j-uri` → live Bolt push; without → a `graph.cypher` snapshot in the output dir.
- `--eager`/`--lazy` is the single switch that gates **all** destructive behavior in a Bolt push.

(For the JSON path the level gating is the usual `-a 1|2|3|4`; `--graphs sdg` needs `-a 4`,
`cfg,dfg,pdg` need `-a 3`. Those don't apply once `--emit neo4j` has forced level 4.)

---

## 9. Querying the result (pointers)

- **Scope every query by id prefix, never by a label list:**
  `MATCH (x:PyCanNode) WHERE x.id STARTS WITH 'can://<app>/'` (append `python/<file>/` for one module).
- The full label/relationship/property vocabulary and worked query recipes (entrypoint, taint,
  exit-point, slicing) live in **`docs/skills/analyzing-canpy-graphs/`** — read it before writing
  Cypher.
- The machine-readable contract is `schema.neo4j.json` (`--emit schema`); `SCHEMA_VERSION` on the
  `:PyApplication` node lets a consumer detect a mismatch.

---

## 10. Design decisions worth remembering

| Decision | Why |
| --- | --- |
| **Pure projection, dumb writers** | Graph *content* has one definition (`project.py`); the two writers only differ in liveness. Testable without a DB. |
| **Same ids as JSON, built once upstream** | `analysis.json` and Neo4j join on one string. No recomposition, no drift (`identity.py`). |
| **`can://` prefix scoping + `PyCanNode` anchor** | Destructive statements are exact containment and index-seekable, safe in a shared multi-app / multi-language DB. |
| **Schema derives its own DDL + conformance test** | The contract (`schema.py`) cannot silently drift from the emitter (`project.py`). |
| **`PY_`-namespaced code, neutral artifact/package nodes** | Multi-language DB: never mingle analyzers' code facts; always converge on shared repo-level artifacts. |
| **`_k` relationship discriminant** | Parallel edges (per-var DDG, true/false CFG, kind-split deps) survive MERGE instead of collapsing. |
| **Additive push by default (`--lazy`)** | An incremental push into a shared DB must not be able to destroy anything; destruction is opt-in `--eager`. |
| **Edge-only-when-resolved** | `PY_EXTENDS`/`PY_RESOLVES_TO` never dangle; unresolved targets degrade to a string prop or an external ghost. |
| **No `taint_flows`** | Analyzer is the provider of the SDG substrate; labeled taint is the SDK's job. |
