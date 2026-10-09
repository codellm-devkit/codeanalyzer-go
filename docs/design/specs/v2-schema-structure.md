# v2 `analysis.json` — structure reference (keys only)

The key skeleton of the canonical CLDK v2 output (`--analysis-schema 2`). This is a
quick map of the node tree and every field; the *decisions* behind the shape live in
[`v2-l1-emission.md`](./v2-l1-emission.md) and `CLAUDE.md` § Schema decisions, and the
authoritative Go definitions are `internal/schema/v2/schema.go`. Field names here are
the JSON tags from that file.

## Tree

```
├─ schema_version                     # "2.0.0"
├─ language                           # "go"
├─ max_level                          # 1 (symbol table) | 2 (+ call graph)
├─ analyzer
│  ├─ name                            # "codeanalyzer-go"
│  └─ version
└─ application
   ├─ id                              # "can://go/<app>"
   ├─ kind                            # "application"
   ├─ symbol_table                    # { "<rel/path.go>": module }
   │  └─ <module>
   │     ├─ id
   │     ├─ kind                      # "module"
   │     ├─ span { start, end, bytes }
   │     ├─ package
   │     ├─ source                    # whole file text, once per module
   │     ├─ content_hash              # optional
   │     ├─ imports[]  { name, path, alias?, span }
   │     ├─ types                     # { "<TypeName>": type }
   │     │  └─ <type>
   │     │     ├─ id
   │     │     ├─ kind                # struct | interface | alias | defined
   │     │     ├─ span { start, end, bytes }
   │     │     ├─ base_types[]        # optional: embedded type ids (explicit)
   │     │     ├─ interfaces[]        # optional: interfaces computed-satisfied
   │     │     ├─ fields              # optional: { "<FieldName>": field }
   │     │     │  └─ <field> { id, kind, type, span }
   │     │     └─ callables           # { "<signature>": callable }
   │     │        └─ <callable>       # ← callable node, defined below
   │     └─ functions                 # { "<signature>": callable }
   │        └─ <callable>             # ← callable node, defined below
   └─ call_graph[]                    # L2+; empty [] at L1
      └─ <edge>
         ├─ src                       # can:// callable id
         ├─ dst                       # can:// callable id
         ├─ prov[]                    # optional: e.g. ["go/types"]
         └─ weight                    # optional
```

## `<callable>` node (recursive)

The same shape whether it sits under a type's `callables` (methods), a module's
`functions` (package-level functions), or another callable's `callables` (closures).

```
<callable>
├─ id
├─ kind                               # function | method | lambda
├─ signature
├─ source_file                        # optional: set only for a method declared in a
│                                     #   different file than its receiver type; its
│                                     #   span.bytes then index symbol_table[source_file].source
├─ span { start, end, bytes }
├─ parameters[]  { name, type, span, is_variadic? }
├─ return_type                        # optional
├─ error_channel[]                    # optional: error-typed returns
├─ metrics                            # optional: { cyclomatic: N }
├─ callables                          # optional: nested closures (same shape)
└─ body                               # { "<line:col>": call node }
   └─ <call>
      ├─ kind                         # "call"
      ├─ span { start, end, bytes }
      ├─ callee                       # null | can:// id  (key always present)
      ├─ is_goroutine                 # optional: present only when true
      └─ is_deferred                  # optional: present only when true
```

## `span`

```
span
├─ start   [line, col]
├─ end     [line, col]
└─ bytes   [from, to]                 # UTF-8 byte offsets into the owning module.source
```

## Presence rules

- **Always present:** every `id`, `kind`, `span`; the envelope fields; `symbol_table`,
  `types`, `callables`, `functions`, `body` maps (may be empty `{}`); `call_graph` (empty
  `[]` at L1); a body node's `callee` key (value `null` when unresolved/external).
- **`omitempty` — emitted only when non-empty:** `content_hash`, `imports[].alias`,
  `base_types`, `interfaces`, `fields`, `source_file`, `return_type`, `error_channel`,
  `metrics`, a callable's nested `callables`, `parameters[].is_variadic`, edge `prov` /
  `weight`.
- **Emitted only when `true`:** `is_goroutine`, `is_deferred`.

## `id` shape

```
application   can://go/<app>
module        can://go/<app>/<rel/path.go>
type          can://go/<app>/<rel/path.go>/<type-signature>
callable      can://go/<app>/<rel/path.go>/<type-signature>/<callable-signature>
              (functions omit the <type-signature> segment)
body call     keyed by "<line>:<col>" within the callable's body map
```
