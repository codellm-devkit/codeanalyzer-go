# codeanalyzer-go — agent notes

The Go static-analysis backend for CLDK (`cango`). Emits the canonical CLDK
`analysis.json`. Parser/resolver/call-graph builder compute the facts; the emission
layer serializes them into the schema shape.

## Schema decisions

Decisions made with the user during the v1 → v2 migration design loop
(`designing-cldk-changes`), spine order. Each is a leaf-level addition to the shared
v2 vocabulary (parity clause: add at the leaves, never rename shared names). The
committed spec is the full transcript: `docs/design/specs/v2-l1-emission.md`.

### Identity & positioning (Group A — reused verbatim by L3/L4)
- **`application.id`** = `can://go/<app>`, where `<app>` is the **`--app-name`** value,
  defaulting to the input directory's base name (per the CLI contract; the SDK's Neo4j
  backend must use the same anchor). L3/L4 build ids on this unchanged.
- **Callable signature** = existing `signatureOf()` output as the last `can://` path
  segment; receiver folded in for methods. One canonicalizer, unchanged from v1.
- **`span.bytes`** = **UTF-8 byte offsets** into `module.source` (Go strings are
  natively UTF-8; avoids the UTF-16/rune slicing mismatch TS hit in cants#179).

### `type` node
- **`kind`** ∈ `struct | interface | alias | defined` — Go's four type-declaration
  shapes. Collapses v1's `is_interface` boolean. (`alias` = `type X = Y`;
  `defined` = `type X Y` over a non-struct, e.g. `type Celsius float64`.)
- **Methods** (declared outside the type in Go source) are resolved to their receiver
  type and placed in that `type.callables{}`; receiver recorded as a field. The tree
  reflects containment, not source location.
- **Embedding vs satisfaction:** `base_types[]` = embedded type ids (struct/interface
  embedding — the explicit spine); `interfaces[]` = interfaces the type is *computed*
  to satisfy via method-set matching (Go's implicit/structural implementation).

### `callable` node
- **`error_channel[]`** populated from `error`-typed return values (Go's error idiom);
  those returns remain in `return_type` too. Maps Go onto the shared field.
- **Closures / function literals** → nested `callables{}` on the enclosing callable
  (replaces v1 `InnerCallables`); each closure gets its own `can://` id.
- **`source_file`** (optional; added in the 2026-09-29 design loop): Go allows a method
  to be declared in a *different file* than its receiver type. The method node stays
  nested under its receiver type (containment-by-receiver, above), but its `span.bytes`
  then index the **declaring file's** `module.source`, not the nesting module's. When
  the declaring file ≠ the nesting module, the callable carries `source_file` = that
  file's path (a `symbol_table` key). **Text recovery:** a node's text =
  `symbol_table[source_file].source[span.bytes]` if `source_file` present, else the
  nesting module's `source`. Emitted only when it differs (absent = same file). This is
  the leaf-level fix for the cross-file-method empty-span collision the real-app eval
  surfaced. Cross-language parity (C# partial classes, Rust `impl`, Ruby reopened
  classes, TS declaration merging) is **deferred** — promote `source_file` into the
  shared keystone vocabulary when a second language needs it.

### `call` body node
- Typed **`is_goroutine`** and **`is_deferred`** boolean fields (`defer` is net-new vs
  v1). `is_constructor_call` **dropped** — Go has no constructors.
- `callee` is the sanctioned `null → id` refinement slot (null at L1, backfilled at L2).

### Deferred (not this train)
- L3 (CFG/CDG/DDG) and L4 (SDG param edges) — reuse Group A ids/spans unchanged.
- Go-specific `cfg` edge kinds (e.g. `defer_resume`, `select`) are recorded when L3
  is designed, not now.

## Neo4j-projection decisions

The graph projection (`--emit neo4j`) is a **second projection of the same v2 envelope**,
keyed on the identical `can://` ids — JSON and graph join on one string, never recomposed.
These are the Go leaf decisions, made with the user in the 2026-10-04 design loop
(`designing-cldk-changes`), under the parity clause (add `GO_`/`Go` leaves; reuse the
shared neutral vocabulary verbatim; never rename a sibling's shared name). Spec:
`docs/design/Neo4j_projection/specs/neo4j-projection.md`. Cross-repo (analyzer +
`python-sdk` fork); tracked under epic `codellm-devkit/.github#94`.

### Namespacing & anchor (parity with Python `PY_`/`Py`, TS `TS_`/`TS`)
- **Node-label prefix `Go`, relationship prefix `GO_`.** The SDK's `AnalysisBackend` ABC
  reads these as its `N`/`P` ClassVars, so `GoAnalysisBackend` sets `N="Go"`, `P="GO_"`;
  the graph vocabulary and the backend's `P`/`N` must agree letter-for-letter.
- **`:GoCanNode` marker label** rides every `can://`-id-keyed node (index anchor for the
  prefix seek; carries no safety claim). **`:GoSymbol` shared merge-label** on `:GoType` /
  `:GoCallable` / `:GoExternal` (one uniqueness constraint; one MATCH target by id).
- **Scoping:** every destructive statement is scoped on the `can://<app>/` id prefix,
  anchored on `:GoCanNode` — mirrors the sibling `can://`-prefix invariant (`.github`
  spec `2026-09-02-prune-scope-on-can-id-prefix.md`).

### Node labels (narrower than Python — Go has fewer kinds)
- **`:GoType`** (one label, `kind ∈ struct|interface|alias|defined` as a **property**),
  NOT per-kind labels — mirrors the JSON `type.kind` collapse (§ `type` node above),
  keeping the two projections symmetric.
- **`:GoField`** (struct fields), id `<type-id>/<name>`, via `GO_HAS_FIELD`.
- **No `:GoDecorator` / `:GoAttribute` / `:GoVariable`** — Go's JSON tree has none; the
  parity clause adds at the leaves, it does not force a node a language lacks. (Struct
  tags are not modeled yet — deferred, not forced in.)

### Relationships
- **Containment:** `GO_HAS_MODULE`, `GO_DECLARES` (module→func, callable→closure),
  `GO_HAS_METHOD` (type→receiver-method), `GO_HAS_FIELD`, `GO_HAS_BODY_NODE`.
- **Inheritance (Go's two distinct mechanisms, named honestly):** `GO_EMBEDS` =
  `base_types[]` (explicit struct/interface embedding; deferred/edge-only-when-resolved
  like `PY_EXTENDS`) vs **`GO_SATISFIES`** = `interfaces[]` (computed structural
  satisfaction via method-set matching). Deliberately **not** `GO_EXTENDS`/`GO_IMPLEMENTS`
  — embedding is composition not subtyping, and satisfaction is implicit not declared.
- **Call graph:** `GO_CALLS` (weight, prov; endpoints resolve to a callable id or an
  id-keyed `:GoExternal` ghost — no dangling endpoint), `GO_RESOLVES_TO` (call body node →
  callee).
- **Imports:** `GO_IMPORTS` (aggregated per target) → `:GoModule` (in-project) or a
  `:GoExternal` ghost (stdlib/external, keyed by import path).

### Flattened properties (Neo4j-legal scalars / homogeneous arrays)
- **`is_goroutine` / `is_deferred`** → boolean props on the call `:GoBodyNode` (pruned when
  false, matching JSON `omitempty`), not labels, not edge discriminants (the fact lives on
  the call site even when the callee is unresolved and no edge exists).
- **`source_file`** (cross-file method, § callable above) → scalar prop on `:GoCallable`;
  text recovery joins to the declaring `:GoModule` by that path. Present only when it
  differs from the nesting module.
- **`error_channel[]`** → string-array prop on `:GoCallable` (a value-shaped fact, not an
  edge — Go errors are returned values, not thrown types).

### Repository-artifact layer (neutral, reused verbatim — cross-language merge targets)
- Declare `:Artifact` / `:Package` / `:ConfigKey` and `HAS_ARTIFACT` /
  `DECLARES_DEPENDENCY` (`_k=kind`) / `LOCKS` / `DEFINES_CONFIG` **un-prefixed**, identical
  to the siblings, so a sibling analyzer over the same repo converges on the same node.
  The projector emits rows **only for facts the Go analyzer produces** (go.mod deps →
  `:Package` `pkg:golang/…`); absent facts emit **zero rows** (declared contract, empty
  projection). `GO_PROVIDES` / `GO_UNRESOLVED_IMPORT` stay `GO_`-prefixed (this analyzer's
  own resolution claims).

### `_k` relationship discriminants (parallel edges survive MERGE)
- `GO_CFG_NEXT` per `kind`; `GO_DDG` per `(var, prov)`; `DECLARES_DEPENDENCY` per `kind`
  — same discriminant mechanism as the siblings.

### Reserved L3/L4 overlay (declared now, emits nothing until the levels land)
- `schema.go` declares the full overlay vocabulary **now** — `GO_HAS_BODY_NODE`,
  `GO_CFG_NEXT`, `GO_CDG`, `GO_DDG`, `GO_PARAM_IN`, `GO_PARAM_OUT`, `GO_SUMMARY`, and the
  body-node statement kinds — with parity-correct names and `_k` discriminants. The
  analyzer caps at L2, so the projector walks these sections and emits **zero rows**;
  when L3/L4 land they fill with **no schema-contract change**. (`--emit neo4j` is
  "full-depth = max implemented" — L2 today; the graph grows additively.)

### SDK facade (`GoAnalysisBackend`, on the `python-sdk` fork — frontend rung)
- **Type-centric surface** (Go is the SDK's first class-less facade; no C precedent exists
  yet): `get_types`/`get_type`, `get_functions` (package-level), `get_methods_of`/
  `get_method` (receiver methods), `get_fields` — **not** `get_classes` (Go has no class),
  and functions are kept distinct from methods (no "module-name-as-class" hack).
- `get_callers`/`get_callees` keyed on the callable id / `(type_id, sig)` (C-facade style:
  take the callable, not class+method strings).
- **Scope this train:** Tier-A lifecycle + L1/L2 leaf accessors + repository-artifact
  getters + `get_source`. Slicing / CFG / CDG / DDG / reachability are **omitted** (they
  need L3/L4 the analyzer does not produce) per the ABC contract.

### Deferred (not this train)
- SDK consumer slicing/taint surface and L3/L4 facade methods — arrive with the L3/L4
  analyzer train, reusing this graph vocabulary unchanged.
- Struct-tag modeling; Go-specific `cfg` edge kinds (`defer_resume`, `select`) — with L3.
