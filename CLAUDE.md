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
- **`application.id`** = `can://go/<app>`, where `<app>` is a slug derived from the
  **go.mod module path** (deterministic, no flag). L3/L4 build ids on this unchanged.
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

### `call` body node
- Typed **`is_goroutine`** and **`is_deferred`** boolean fields (`defer` is net-new vs
  v1). `is_constructor_call` **dropped** — Go has no constructors.
- `callee` is the sanctioned `null → id` refinement slot (null at L1, backfilled at L2).

### Deferred (not this train)
- L3 (CFG/CDG/DDG) and L4 (SDG param edges) — reuse Group A ids/spans unchanged.
- Go-specific `cfg` edge kinds (e.g. `defer_resume`, `select`) are recorded when L3
  is designed, not now.
