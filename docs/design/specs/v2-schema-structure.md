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
├─ max_level                          # 1 (symbol table) | 2 (+ call graph) | 3 (+ cfg/cdg/ddg)
│                                     #   | 4 (+ SDG; L4, Phase 2)
├─ k_limit                            # L3+ only: access-path bound (--graph-field-depth, default 3)
├─ degraded[]                         # L4, Phase 2; optional: absent when nothing degraded
│  └─ { code, detail?, packages[]?, callables[]? }
│                                     #   code: no_ssa | no_vta | partial_ssa | partial_types |
│                                     #   unmapped_ssa | l3_skipped | range_func (open vocabulary)
├─ analyzer
│  ├─ name                            # "codeanalyzer-go"
│  └─ version
└─ application
   ├─ id                              # "can://<app>"
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
   ├─ call_graph[]                   # L2+; empty [] at L1
   │  └─ <edge>
   │     ├─ src                       # can:// callable id
   │     ├─ dst                       # can:// callable id, or an @external id
   │     ├─ prov[]                    # optional: e.g. ["go/types"]; L4 adds ["vta"] (merged)
   │     └─ weight                    # optional
   ├─ param_in[]                     # L4, Phase 2; optional: { src, dst, var } — global ids,
   │                                 #   actual_in → formal_in; var = the callee formal's `of`
   ├─ param_out[]                    # L4, Phase 2; optional: { src, dst, var } — global ids,
   │                                 #   formal_out → actual_out; sorted (src, dst)
   └─ external_symbols               # L2+; optional: { "<@external id>": external }
      └─ <external>                  # library callee: `can://<app>/@external/<import-path>/<sig>`
         ├─ id
         ├─ kind                     # "external"
         ├─ name                     # e.g. "Println"
         ├─ module                   # import path, e.g. "fmt"
         ├─ signature                # [Type.]Name, e.g. "Builder.WriteString"; may name a
         │                           #   library interface method (e.g. "Writer.Write")
         ├─ parameters[]             # { name, type, is_variadic? } (no span: not in-project);
         │                           #   receiver excluded; unnamed params are "_<i>"
         ├─ return_type              # optional
         ├─ receiver                 # L4, Phase 2; optional: { name, type } of a method's
         │                           #   receiver (gives formal_in:0's `of`; "_recv" if unnamed)
         └─ body                     # L4, Phase 2; optional: stand-in formal vertices only,
                                     #   { "@formal_in:<i>" | "@formal_out[:<i>]": vertex }
```

## `<callable>` node (recursive)

The same shape whether it sits under a type's `callables` (methods), a module's
`functions` (package-level functions), or another callable's `callables` (closures).

```
<callable>
├─ id
├─ kind                               # function | method | lambda
├─ signature
├─ source_file                        # optional: set only when the declaring file differs
│                                     #   from the containing module — a method declared in
│                                     #   a different file than its receiver type, and every
│                                     #   closure (nested ones too) inside such a method; its
│                                     #   span.bytes, and its body nodes' spans, then index
│                                     #   symbol_table[source_file].source
├─ span { start, end, bytes }
├─ parameters[]  { name, type, span, is_variadic? }
├─ return_type                        # optional
├─ error_channel[]                    # optional: error-typed returns
├─ metrics                            # optional: { cyclomatic: N }
├─ callables                          # optional: nested closures (same shape)
├─ body                               # { "<key>": body node }; key = "<line:col>[/n]" | "@entry" | "@exit"
│  ├─ <call>                          # L1+
│  │  ├─ kind                         # "call"
│  │  ├─ id                           # L1+: <callable-id>@<key>
│  │  ├─ span { start, end, bytes }   # the call expression (unchanged at L3)
│  │  ├─ parent                       # L3+, optional: key of the statement a nested call sits in
│  │  ├─ callee                       # null | can:// id  (key always present)
│  │  ├─ is_goroutine                 # optional: present only when true
│  │  └─ is_deferred                  # optional: present only when true
│  ├─ <statement node>                # L3+
│  │  ├─ kind                         # statement | branch | loop | switch | return | entry | exit
│  │  ├─ id                           # <callable-id>@<key>  (@entry → <callable-id>@entry)
│  │  └─ span { start, end, bytes }   # the statement; absent on entry / exit
│  └─ <vertex>                        # L4, Phase 2: SDG port, span-less
│     ├─ kind                         # formal_in | formal_out | actual_in | actual_out
│     ├─ id                           # <callable-id>@<key>
│     ├─ of                           # L4: the bound name — param / receiver name, "_<i>",
│     │                               #   "_recv", "<return>", "<return>:<i>",
│     │                               #   "<capture>:<name>", "<global>:<pkgpath>::<Name>"
│     └─ parent                       # actual_* only: the call key; for a non-call closure
│                                     #   capture site, the key of the node containing the literal
├─ cfg[]                              # L3+, optional: { src, dst, kind } — local body keys
├─ cdg[]                              # L3+, optional: { src, dst }
├─ ddg[]                              # L3+, optional: { src, dst, var, prov }  (prov = ["ssa"];
│                                     #   L4 adds ["reaching-defs"] port wiring, ["points-to"])
└─ summary[]                          # L4, Phase 2; optional: { src, dst } — local keys,
                                      #   <call>/actual_in:<i> → <call>/actual_out[:<j>]
```

### L3 body nodes and edges

- **Kinds** (shared vocabulary, no Go-specific kinds): `if` → `branch`; `for` / `for … range` →
  `loop`; `switch` / type `switch` / `select` → `switch`; `return` → `return`; every other
  statement (assignment, `:=`, `var`, inc/dec, send, `go`, `defer`, `break` / `continue` /
  `goto` / `fallthrough`) and each `case` / `default` / comm-clause head → `statement`;
  `@entry` / `@exit` → `entry` / `exit`.
- **Call nodes.** A bare call statement and a `go` statement *are* their call node (kind
  `call`, the L1 key, the CFG step). A call nested in a statement keeps its L1 key and span
  and carries `parent` = that statement's key. It is not a CFG step and has no edges.
  `defer f(x)` is two nodes: a `statement` at the `defer` keyword, and the `call` node
  (`is_deferred`) placed in the exit chain. An L1 call node in unreachable code is still
  emitted, but it has no edges.
- **Keys.** A statement that starts at the same position as an L1 call (`f().x = v`) takes
  the next free `/n` suffix, so the L1 call key never moves.
- **`cfg` kinds:** `fallthrough`, `true`, `false`, `loop_back`, `switch_case`, `break`,
  `continue`, `return`, `exception`, and the Go add `defer_resume`. Sorted by
  `(src, dst, kind)`.
- **`cdg`** is control dependence from post-dominators. A node with no controller gets an
  edge from `@entry`. Sorted by `(src, dst)`.
- **`ddg`** comes from syntactic reaching definitions over k-limited access paths
  (`base(.field|[*])*`, past `k` → `.*`). `var` is the source spelling of the path, and
  `prov` is always `["ssa"]` at L3. Sorted by `(src, dst, var)`. It is **not
  refutation-complete**: writes to captured variables inside closures and writes through
  reference-typed fields of value-typed struct arguments/receivers are not modelled at L3
  (no edge ≠ no flow). The 2026-10-25 re-check decided to close both at L3, plus the deferred-operand
  reassignment miss (D1, Phase 2 prerequisite). A deferred call's receiver/argument paths are keyed
  on the variable at exit, so a reassignment after the `defer` can add a spurious edge, and (until
  the D1 fix lands) kill an earlier def. See `v2-l3-l4-dataflow.md` §4.5 "L3 precision".
- **L4 vertex keys (L4, Phase 2).** Formals `@formal_in:<i>`, `@formal_out` / `@formal_out:<i>`;
  actuals `<call-key>/actual_in:<i>`, `<call-key>/actual_out` / `…/actual_out:<i>`. `formal_in` and
  `actual_in` are always indexed; `formal_out`/`actual_out` are bare iff the group has exactly one
  vertex. The SDK reads `of`, never an index into `parameters[]` (`formal_in:0` is a method's
  receiver). See `v2-l3-l4-dataflow.md` §4.6–§4.7.
- **Spans.** A statement node's span is its AST node's range, and a clause head ends at its
  colon. For a callable with `source_file` (a method declared apart from its receiver type,
  or a closure inside one), body spans index `symbol_table[source_file].source`, as the
  callable's own span does. A closure never inherits `source_file` implicitly: each callable
  carries its own.

## `span`

```
span
├─ start   [line, col]
├─ end     [line, col]
└─ bytes   [from, to]                 # UTF-8 byte offsets into the owning module.source
```

## Presence rules

- **Always present:** every `id` (body nodes included, from L1 on — parity with
  codeanalyzer-python, which stamps body ids at L1), `kind`, `span` (except: `@entry` /
  `@exit` have no `span`); the envelope fields; `symbol_table`, `types`,
  `callables`, `functions`, `body` maps (may be empty `{}`); `call_graph` (empty `[]` at L1);
  a call node's `callee` key (value `null` when unresolved/external). Non-call body nodes
  have no `callee`.
- **L3+ only:** `k_limit`; body node `parent`; non-call body nodes; a callable's
  `cfg` / `cdg` / `ddg`. These are omitted for a callable with no body. Each is a pure
  addition: the `-a 2` document is a subset of the `-a 3` one.
- **L4 only (Phase 2):** `degraded` (and only when something degraded), `formal_*` / `actual_*`
  vertices and their `of`, a callable's `summary`, `param_in` / `param_out`,
  `external_symbols[].receiver` / `.body`, `ddg` edges with `prov` `reaching-defs` / `points-to`,
  `call_graph` `prov` `vta`. Bodiless interface methods and stand-ins get formals too. Each is a
  pure addition: `-a 3` ⊆ `-a 4`.
- **`omitempty` — emitted only when non-empty:** `content_hash`, `imports[].alias`,
  `base_types`, `interfaces`, `fields`, `source_file`, `return_type`, `error_channel`,
  `metrics`, a callable's nested `callables`, `parameters[].is_variadic`, edge `prov` /
  `weight`, `external_symbols` (absent when no library call was resolved),
  `external_symbols[].return_type`.
- A call node's `callee` is an in-tree callable id, an `external_symbols` key (library
  callee), or `null` (conversion, builtin, call through a function value, unresolved).
- **Emitted only when `true`:** `is_goroutine`, `is_deferred`.

## `id` shape

```
application   can://<app>
module        can://<app>/go/<rel/path.go>
type          can://<app>/go/<rel/path.go>/<type-signature>
callable      can://<app>/go/<rel/path.go>/<type-signature>/<callable-signature>
              (functions omit the <type-signature> segment)
external      can://<app>/@external/<import-path>/<sig>   (language-neutral)
body node     keyed by "<line>:<col>" (co-positioned: "<line>:<col>/<n>") or "@entry" / "@exit"
              within the callable's body map; its id (every level) is <callable-id>@<key>
              (a synthetic key already carries the "@": <callable-id>@entry)
```
