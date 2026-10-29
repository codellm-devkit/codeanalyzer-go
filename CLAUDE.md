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
- **`application.id`** = `can://<app>` (app outermost, so a polyglot app shares one prefix),
  where `<app>` is the **`--app-name`** value, defaulting to the input directory's base name
  (per the CLI contract; the SDK's Neo4j backend must use the same anchor). Module ids are
  `can://<app>/go/<file>`; externals are language-neutral `can://<app>/@external/<import-path>/<sig>`.
  L3/L4 build ids on this unchanged.
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
  nesting module's `source`. Emitted only when it differs (absent = same file). It applies
  to **every callable** whose declaring file differs from its containing module, so
  **closures and nested closures** inside a cross-file method carry `source_file` too (no
  implicit inheritance; text recovery holds as written for them and their body nodes). This is
  the leaf-level fix for the cross-file-method empty-span collision the real-app eval
  surfaced. Cross-language parity (C# partial classes, Rust `impl`, Ruby reopened
  classes, TS declaration merging) is **deferred** — promote `source_file` into the
  shared keystone vocabulary when a second language needs it.

### `call` body node
- Typed **`is_goroutine`** and **`is_deferred`** boolean fields (`defer` is net-new vs
  v1). `is_constructor_call` **dropped** — Go has no constructors.
- `callee` is the sanctioned `null → id` refinement slot (null at L1, backfilled at L2).

### `external_symbols` (L2 library calls)
- **L2 keeps library call edges.** A call to a function outside the project (`fmt.Println`,
  `(*strings.Builder).WriteString`) is no longer dropped: the call node's `callee` and the
  `call_graph` edge `dst` are the callee's `@external` id. Builtins, type conversions
  (no longer minted as `<Type>.__new__`) and calls through function values stay `null`.
- **Id** = `can://<app>/@external/<import-path>/<sig>` where `<sig>` is the callee signature
  minus the `<import-path>.` prefix (`Println`, `Builder.WriteString`); it may also name a
  library *interface* method (`@external/io/Writer.Write`). Methods on generic library types
  record the declared (origin) signature, not the instance. `@external` is
  language-neutral (not `go/`), so a polyglot app shares one external namespace.
- **Home:** `application.external_symbols{<id>: {id, kind:"external", name, module,
  signature, parameters, return_type}}`, omitempty; parameters come from the callee's
  `*types.Signature` (receiver excluded, unnamed params `_<i>`).
- Mirrors the Python/Java precedent (library callees are leaf nodes at application scope);
  on by default at L2, no flag.

### L3 body and edges (`-a 3`; spec `docs/design/specs/v2-l3-l4-dataflow.md`)
- **Body node `id`** = `<callable-id>@<key>` (`v2.GlobalOrdinal`) on **every** body node,
  **from `-a 1` on** (not L3+ only) — Python parity (codeanalyzer-python stamps body ids at
  L1). **`parent`** (L3 only) = the local key of the statement a satellite call sits in.
- **Body kinds: shared vocabulary only, no Go-specific kinds** — `if` → `branch`;
  `for`/`range` → `loop`; `switch`/type switch/`select` → `switch`; `return`; every other
  statement and each case/comm-clause head → `statement`; L1 `call`; synthetic `entry`/`exit`.
  `go`/`defer`/`select` facts ride `is_goroutine`/`is_deferred` and the `switch` kind.
- **`@entry` / `@exit` are span-less** (keystone: synthetic CFG endpoints have no span) and
  carry no `callee` (a call-node field).
- **Defer two-node rule:** `defer f(x)` is a registration `statement` node (at the `defer`
  keyword; evaluates `f`, `x` and weak-defines `<defer>:<call-key>`) **plus** the L1 deferred
  `call` node (`is_deferred`), placed only in the exit chain (reverse source order before
  `@exit`; every defer assumed registered). `go f(x)` is NOT split — the statement is its call
  node.
- **`cfg` kind `defer_resume`** — the one Go add to the shared `cfg` kinds: chain-internal
  edges and the chain tail → `@exit`. The edge into the chain keeps its own kind.
- **`k_limit`** (envelope, L3+ only) = `--graph-field-depth` (default 3), the access-path
  bound of the syntactic `ddg` (`x.f.g.h` → `x.f.g.*`); `ddg.prov` is `["ssa"]` (parity).

### L4 SDG (`-a 4`; spec `docs/design/specs/v2-l3-l4-dataflow.md` §4.6–§4.16, §6)
Reuses Group A ids/spans and the L3 vocabulary unchanged; every L4 fact is an addition
(`-a 3` ⊆ `-a 4`, gated). Leaves:
- **Vertices** (body nodes, span-less, no `callee`): `@formal_in:<i>`, `@formal_out[:<i>]`,
  `<call-key>/actual_in:<i>`, `<call-key>/actual_out[:<i>]` (Python's `IdentityMap` indexing:
  `formal_in`/`actual_in` always indexed; an out is bare iff its group has one vertex). Actuals
  carry `parent` = the call key (a non-call capture site: the literal's host). **`of`** (D6,
  Python spelling, which the SDK's resolver reads): the param/receiver name; unnamed or `_` param
  `_<i>` (declared index, receiver excluded); unnamed or `_` receiver **`_recv`**; results
  `<return>` / **`<return>:<i>`** (never user result names; `:<i>` is a Go leaf);
  `<capture>:<name>`; `<global>:<pkgpath>::<Name>`. The SDK reads `of`, never
  `parameters[]` (`formal_in:0` is a method's receiver).
- **Edges:** application `param_in[]` / `param_out[]` `{src, dst, var}` (global ids, `var` = the
  callee formal's `of`); callable `summary[] {src, dst}` (local keys, actual_in → actual_out);
  port wiring in `ddg` with `prov: ["reaching-defs"]` (Python precedent; coarse host wiring:
  host → every actual_in, every actual_out → host); may-alias `ddg` with `prov: ["points-to"]`.
- **Dispatch:** VTA over the project's functions (library targets in the set); `call_graph`
  gains VTA-only edges `prov: ["vta"]`, weight 1 (an L2 edge is never changed). **D7:** an
  interface call binds the interface method (pass-through summary) *and* every VTA target.
- **Stand-ins:** `external_symbols[].receiver {name, type}` and `.body` (formal vertices only,
  ids `<external-id>@formal_in:<i>`); summary all-to-all unless `--library-models` overrides it
  (keys `<import-path>/<sig>`, values `[in, out]` `of` pairs). A model changes summaries, not
  statement-level reachability; sanitizer semantics are the SDK's [user, 2026-10-27;
  codellm-devkit/.github#103].
- **`degraded[]`** envelope leaf `{code, detail?, packages[]?, callables[]?}`, absent when
  nothing degraded; codes `no_ssa | no_vta | no_cha | partial_ssa | partial_types | unmapped_ssa |
  l3_skipped | range_func` (open vocabulary). `--strict` exits non-zero on any.
- **Rulings:** global → global summary edges are omitted [user, 2026-10-26; #30]; memory is
  measured and reported per app, not gated (D3) [user, 2026-10-26]; range-over-func is not
  modelled, degraded `range_func` (D8); `-a 4 -t` is an error and `--emit neo4j -t` projects
  at L3 with a warning (D9).
- A Go-specific `select` cfg kind was not added (`select` is a `switch` with `switch_case`
  edges).

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
- **`:GoBodyNode.id`** = the JSON body node's `id` (`<callable-id>@<key>`), which the JSON
  carries **from `-a 1` on** (not L3+ only) — Python parity: codeanalyzer-python stamps body
  ids at L1 (`stamp_body_ids`), so JSON and graph join on the same id at every level.
- **`call_node`** on a satellite call `:GoBodyNode` = the enclosing statement's **local** key
  (JSON `parent` verbatim), resolved against the owning callable (the `GO_HAS_BODY_NODE`
  source) — not a global id. Python parity: codeanalyzer-python projects `node.parent`
  unchanged and python-sdk's `reconstruct` maps `call_node` back to `parent`.
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
- `GO_CFG_NEXT` per `kind`; `GO_DDG` per `(var, prov)` as the composite
  `_k = var + "|" + join(prov, ",")` (declared as `Discriminant: "var|prov"`);
  `DECLARES_DEPENDENCY` per `kind` — same discriminant mechanism as the siblings.

### L3 overlay (live at `-a 3`)
- `GO_CFG_NEXT {kind}`, `GO_CDG`, `GO_DDG {var, prov}` are no longer reserved; the projector
  emits one row per JSON `cfg[]`/`cdg[]`/`ddg[]` entry (cross-projection count gate).
  Endpoints are `:GoBodyNode` **global ids** (`v2.GlobalOrdinal(callable-id, local-key)`),
  never recomposed by hand. `prov` is a string-array prop.
- `:GoBodyNode` covers every body node incl. synthetic `@entry`/`@exit` (id
  `<callable-id>@entry`, no `span_json`), each with `GO_HAS_BODY_NODE` from its callable.
  It carries `call_node` (JSON `parent` verbatim — the **local** key, see § Flattened
  properties) and `var` (JSON `of`; L4 — absent at L3). `GO_HAS_BODY_NODE` may originate at `:GoExternal` (L4 stand-ins).

### L4 overlay (live; `--emit neo4j` runs at L4, D4)
- `GO_PARAM_IN {var}`, `GO_PARAM_OUT {var}` and `GO_SUMMARY` are live (no longer `Reserved`;
  same names as declared, no contract change). One row per JSON `param_in[]` / `param_out[]` /
  `summary[]` entry; endpoints are `:GoBodyNode` global ids. Port and points-to ddg ride
  `GO_DDG {var, prov}` (`_k = var|prov`).
- Vertices are `:GoBodyNode`s with `var` (= JSON `of`) and `call_node` (actuals); stand-in
  vertices hang off their `:GoExternal` via `GO_HAS_BODY_NODE`. `:GoApplication.degraded_json`
  carries the envelope's `degraded[]`.
- **Lazy push (D5):** every L4 row is upserted on every push (L4 facts are non-local); stale L4
  rows need `--eager`. A stored `max_level` below the push's (or unreadable) forces a full
  upsert; a lower-level push leaves old L4 rows. `--emit neo4j -t` projects at L3 (D9).

### SDK facade (`GoAnalysisBackend`, on the `python-sdk` fork — frontend rung)
- **Type-centric surface** (Go is the SDK's first class-less facade; no C precedent exists
  yet): `get_types`/`get_type`, `get_functions` (package-level), `get_methods_of`/
  `get_method` (receiver methods), `get_fields` — **not** `get_classes` (Go has no class),
  and functions are kept distinct from methods (no "module-name-as-class" hack).
- `get_callers`/`get_callees` keyed on the callable id / `(type_id, sig)` (C-facade style:
  take the callable, not class+method strings).
- **Scope this train:** Tier-A lifecycle + L1/L2 leaf accessors + repository-artifact
  getters + `get_source`. Slicing / CFG / CDG / DDG / reachability are **omitted** (they
  need L3/L4 the analyzer does not produce) per the ABC contract. (2026-10-24: L3 is now
  emitted at `-a 3` — per-callable `cfg`/`cdg`/`ddg` and `GO_CFG_NEXT`/`GO_CDG`/`GO_DDG` —
  so L3 accessors are supportable now. 2026-10-27: L4 is emitted at `-a 4` and by
  `--emit neo4j` — `param_in`/`param_out`/`summary` and the points-to `ddg` — so
  interprocedural slicing and taint are supportable too.)

### Deferred (not this train)
- SDK consumer slicing/taint surface and L3/L4 facade methods — the analyzer now emits L4;
  the SDK side reuses this graph vocabulary unchanged.
- Struct-tag modeling. (`defer_resume` landed with L3; see § L3 body and edges.)
- `GO_EMBEDS` / `GO_SATISFIES` resolution: `base_types[]` holds raw type strings and
  `interfaces[]` is never computed (#31, pre-existing).
