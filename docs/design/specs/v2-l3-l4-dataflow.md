# Spec: Go L3/L4 dataflow (CFG/CDG/DDG + SDG) with SDK taint (codeanalyzer-go)

**Status:** Phases 0–1 implemented (PR #16, open, not yet merged); Phase 2 (L4) implemented on
`feat/l4-sdg` and evaluated (`docs/design/notes/l3-dataflow-metrics.md` § L4); L4 design amended by
the 2026-10-25 re-check (§4.16) and the 2026-10-26/27 rulings ·
**Date:** 2026-10-23, amended 2026-10-25, 2026-10-27 · **Repos:** `codeanalyzer-go`, `python-sdk` ·
**Supersedes:** the "L3/L4 deferred" entries in `docs/design/roadmap.md`.

## 1. Intent

`python-sdk` ships one shared, language-agnostic `taint()` / slicing / reachability surface that
already works on Python, Java and TypeScript analyses. It walks five edge families —
`DDG`, `CDG`, `PARAM_IN`, `PARAM_OUT`, `SUMMARY` (`cldk/analysis/commons/graphs.py:sdg_rels`) —
and nothing else. Go stops at L2, so none of it works on Go code.

This effort brings `codeanalyzer-go` to **L3 (intraprocedural CFG/CDG/DDG) and L4 (SDG)** in parity
with the sibling analyzers, shaped specifically so the SDK's existing `taint()`, `slice_backward`/
`slice_forward`, `paths_between`, `reaches`, `flows_to_call`, `flows_to_argument` answer correctly
on a Go application through **both** the JSON (local) and Neo4j backends — and builds the Go side of
the SDK that exposes them.

**Why now:** there is a concrete downstream need to run taint analysis on Go code; parity with the
siblings is required too; and the id grammar has to change for polyglot apps anyway (§4.1).

### Goals

1. `can://<app>/go/…` id grammar (polyglot parity), shipped first as its own breaking release.
2. L3: per-callable `body{}` completed with statement nodes; `cfg[]`, `cdg[]`, `ddg[]` per callable.
3. L4: synthetic `formal_in/out`, `actual_in/out` vertices; `param_in[]`/`param_out[]` at
   application scope; `summary[]` per callable; alias-aware `ddg` delta (`prov:["points-to"]`).
4. Taint-relevant Go flows are **not missed** (user-ranked):
   - **Must:** (1) through function calls; (3) through interface (dynamic) dispatch.
   - **Should:** (2) through struct fields and pointers (k-limited access paths); closure captures;
     goroutine launches; channels (coarse); library calls (pass-through).
5. Sinks and sources can name **library functions directly**, e.g. `("query", "database/sql.DB.Query")`,
   with the shared SDK taint code unchanged (§6).
6. Neo4j projection fills the already-reserved `GO_CFG_NEXT/GO_CDG/GO_DDG/GO_PARAM_IN/GO_PARAM_OUT/
   GO_SUMMARY` overlay.
7. python-sdk: a v2 Go model, a `GoAnalysis` facade, a local `GoCodeanalyzer` backend and a
   `GoNeo4jBackend`, answering the same L1/L2/L3/L4 surface the Python/Java/TS facades do.

### Non-goals

- No taint engine, sources/sinks catalogue, or `taint_flows` section in the analyzer — it is a graph
  provider; the SDK owns queries (keystone provider/client boundary).
- No goroutine interleaving / happens-before, no `sync` modelling, no `select` fairness.
- No analysis of the standard library's or dependencies' bodies (stand-ins only, §6).
- No `cgo`, `unsafe`, `reflect`-driven flows beyond what pass-through over-approximation gives.
- No context-sensitive summaries, no hammock-region decomposition (neither sibling built it).
- No change to the shared `commons/` taint/slice code in python-sdk (that is a later,
  cross-language revisit — §11).

### Anti-goals (would be failure even if it "works")

- **A false refutation.** Any missing edge that lets `taint()` put a real flow in `exhausted`
  ("searched to exhaustion, no flow") closes a live alert. Every precision choice here leans to
  **over-approximation** (extra edges ⇒ false positives are acceptable; dropped edges are not).
- Breaking the level-monotonicity superset `json(-a1) ⊆ … ⊆ json(-a4)`.
- Non-deterministic output (`-j 1` vs `-j N` must be byte-identical; two runs byte-identical).
- Renaming or repurposing any shared keystone name (parity clause).

## 2. Constraints and sources of truth

- **Keystone:** `cldk-devtools/skills/designing-cldk-changes/references/canonical-schema.md` (shapes,
  levels, edge families, parity clause). Construction guides:
  `codeanalyzer-backend/references/level-3-intraprocedural-dataflow.md`,
  `level-4-interprocedural-sdg.md`, `testing-and-validation.md`, `neo4j-projection.md`,
  `cli-contract.md`. Where the keystone and the **shipped siblings** disagree, this design follows
  the shipped siblings (Python 1.6.x, Java 3.3.x) because the SDK is written against them; each such
  case is called out.
- **Golden rule** (from the v2 migration): keep everything that computes facts; add L3/L4 as new
  computation; the v2 emitter stays the single serialization point.
- **Substrate:** `golang.org/x/tools v0.52.0` (`go.mod`, `go 1.26.0`) is already a dependency —
  `go/cfg`, `go/ssa`, `go/ssa/ssautil` and `go/callgraph/vta` are available with no new module.
  VTA is seeded with no initial call graph (its internal lazy CHA, §4.8); `go/callgraph/cha` is
  not used. VTA's package doc still says "experimental" and carries a TODO on generic
  instantiation wrappers, so every VTA call runs under `recover` (§4.8, §4.15).
  `x/tools/go/pointer` is deprecated: **do not adopt**.
- Level flag semantics (`cli-contract.md`): `-a 3` implies `-a 2`; `-a 4` implies `-a 3`; an `-a 3`
  run must not pay L4 cost; `--emit neo4j` runs at max implemented depth (now 4) and rejects `-a` and
  `--graph-field-depth`.
- python-sdk rules: the Iron Rule (no public accessor changes name/signature/return type); new
  capability only as new methods; models are `extra="ignore"` mirrors of v2 (PR #434's choice;
  the TypeScript and Java wire models are `extra="ignore"` since python-sdk #386, per their
  module docstrings on `main` — an additive analyzer release parses without an SDK edit; §7).

## 3. Current state (what exists)

As of 2026-10-25 (code at `3504445`, branch `feat/l3-dataflow` = `feat/l4-sdg`):

- **Phases 0 and 1 are implemented together on PR #16** (§9; open, not yet merged). L1/L2/L3 on
  v2, Neo4j projection live through L3.
- **Phase 0 (live):** `can://<app>/go/…` ids (§4.1, `internal/schema/v2/ids.go`); L2 library calls
  with `application.external_symbols` (§4.14); co-positioned call keys `l:c/n`; `is_deferred`;
  `typeKind` emits `alias`/`defined`; `GO_DDG` `_k = var|prov`; the per-package call-site index
  (`callsite_index.go`) replacing the per-site AST walk; body-node `id` from `-a 1`.
- **Phase 1 (live):** `-a 3` and `--graph-field-depth`; per-callable statement body nodes, `cfg`
  (defer chain, panic edges), `cdg`, syntactic `ddg` (`prov:["ssa"]`); `k_limit`; Neo4j
  `GO_CFG_NEXT`/`GO_CDG`/`GO_DDG`. Package `internal/dataflow`; join key to the tree is
  `dataflow.L3Key{Path, Signature}` (`run.go`); ids minted only in the emitter.
- **Load:** `go/packages` once, `NeedSyntax|NeedTypes|NeedTypesInfo|NeedDeps` — every dependency
  (stdlib included) is parsed and type-checked from source. That dominates RSS: the Kubernetes
  root peaks at 7.1 GB at `-a 3` (`docs/design/notes/l3-dataflow-metrics.md`).
- **L2 dispatch** is still `go/types`: an interface-method call's callee is the bodiless interface
  method (`collectInterfaceMethods`); no concrete targets. Insufficient for must-have flow (3); L4
  adds VTA (§4.8).
- **Not yet built (Phase 2):** `-a 4` is rejected by the CLI; no `LevelSDG` in `internal/options`;
  no `--strict`; `--emit neo4j` forces `LevelDataflow` (L3); `v2.Analysis` has no `degraded` and
  the package doc says L4 fields are not declared; `ExternalSymbol` has no receiver or body;
  `GO_PARAM_IN`/`GO_PARAM_OUT`/`GO_SUMMARY` stay `Reserved`.
- **python-sdk:** Go v2 L1/L2 support is on PR #434 (branch `pr-434-v3`; merged into python-sdk
  `main` on 2026-10-26, after this section was written); the SDK work builds on it (§7). It cannot yet parse `-a 3` output (§7 prerequisites). `add-go-support` is
  obsolete.

## 4. Decisions

Decisions marked **[user]** were made with the user; **[call]** are the designer's calls, made on
sibling precedent and taint needs, open to override on review.

### 4.1 Identity: `can://<app>/go/…` [user]

Switch from `can://go/<app>/…` to the app-outermost grammar Python (`codeanalyzer/schema/ids.py`)
and Java (`SCHEMA_DECISIONS.md` D8) ship:

```
application   can://<app>
module        can://<app>/go/<rel/path.go>
type          can://<app>/go/<rel/path.go>/<type-sig>
callable      can://<app>/go/<rel/path.go>[/<type-sig>]/<callable-sig>
body node     <callable-id>@<line>:<col> | <callable-id>@<tag>
external      can://<app>/@external/<import-path>/<name>      (language-neutral, as Python/TS)
stand-in      see §6
```

Reason: polyglot applications — one `<app>` prefix scopes every language's nodes, the SDK's
`under_callable` / Neo4j prune prefixes assume app-outermost, and `@external` homes merge across
languages. Ships as its own release **before** L3 (Phase 0), so L3/L4 are built on final ids.
Breaking change; acceptable now because no released SDK consumes Go output. Update CLAUDE.md
§ Identity, `v2-schema-structure.md`, Neo4j prune/scope Cypher (`cypher.go`, `bolt.go`), and the
`:GoApplication` id. Mirror Python's application-root handling (`id = can://<app>` matched
separately from the `can://<app>/` descendant prefix).

### 4.2 Body nodes (L3) [call]

Body node ids are positional `line:col` local keys, global id `<callable-id>@<local>`
(`global_ordinal` rule: synthetic `@…` keys concatenate as-is). Every body node carries its global
`id` field (Python #176 precedent, which the SDK reads) **at every level, from `-a 1` on** — the
L1 call nodes included. Python parity: codeanalyzer-python runs `stamp_body_ids` at L1, so a
consumer reads `id` off an L1 document the same way in both languages. (Supersedes the earlier
"id at L3+ only" ruling; additive at L1/L2.) `parent` stays an L3 refinement.

**Kinds — shared vocabulary only, no Go-specific kinds:**

| Go construct | `kind` |
|---|---|
| `if` (incl. init stmt folded in) | `branch` |
| `for` (3-clause, cond-only, infinite), `for … range` | `loop` |
| `switch`, type `switch`, `select` | `switch` |
| `return` | `return` |
| everything else (assign, `:=`, `var` decl, inc/dec, send `ch <- v`, expression stmt, `go`, `defer`, `break`/`continue`/`goto`/`fallthrough`, labeled stmt's inner stmt, `panic(...)` call stmt) | `statement` |
| each `case`/`default` clause head of switch/type switch/select | `statement` (the clause head — anchors its `switch_case` edge and holds the case expression / comm-clause defs/uses) |
| synthetic | `entry`, `exit` |
| existing L1 call | `call` |

(The `defer` statement is two nodes — registration `statement` + deferred `call`; §4.3.1.)

Rationale for not adding `select`/`go`/`defer` kinds: the keystone lets a language add kinds, but the
SDK's `body_node_kind` and Neo4j queries treat unknown kinds as opaque; `is_goroutine`/`is_deferred`
on the call node already carry those facts. Fewer new names = less parity debt.

**Call nodes stay satellites** (Python #115, Java `nodeIdFor`): a call nested inside a statement keeps
its own L1 `line:col` key, is not a CFG step, and gains `parent` = the enclosing statement's local
id at L3 (sanctioned `parent` refinement). A bare call statement (`foo(x)` as an `ExprStmt`) **shares
its key** with the statement node: the CFG node *is* the call node (kind stays `call`). Same for
`go f(x)`: the statement node is the call node, keyed at the call's position, carrying
`is_goroutine`. `defer f(x)` is the exception — two nodes (§4.3.1). Co-positioned calls (`f(g(x))` where both start at the same column, or
`a.b().c()`) disambiguate with `/2`, `/3` suffixes, outermost first, exactly as Python
`call_body_keys` (#215). Fix this in L1 too (today a collision silently drops a call).

Statement span = the statement's `ast.Node` span; ids anchor at `stmt.Pos()`.

### 4.3 CFG construction (L3) [call]

**Substrate: hand-built from `go/ast`** (statement-granular, positions are native), with a
**differential test against `x/tools/go/cfg`** for block reachability/successor structure (the
construction guide's "stdlib as answer key"). Rejected: building L3 from `go/ssa` — SSA instructions
lose statement granularity and need a position back-map (Java's WALA pain, `line:0` sentinels);
rejected `go/cfg` as the sole source because it does not model panics/defers/exits as edges we need.

Edge `kind`s — shared vocabulary, **plus one Go add**:

| Construct | Edges |
|---|---|
| straight-line | `fallthrough` |
| `if` | `true` → then, `false` → else/next |
| `for cond` / 3-clause | `true` into body, `false` out, `loop_back` tail → header; `post` stmt folded into header node |
| `for {}` (no cond) | body entry is `fallthrough`; **no** `false` edge; synthetic `exception` edge header → `@exit` to restore post-dominance (Python precedent, `cfg.py ~520`) |
| `for range` | header `true` into body, `false` out, `loop_back` |
| `switch` / type switch | `switch_case` header → each clause head; to next-stmt if no `default`; clause head → first body stmt `fallthrough`; Go `fallthrough` stmt → next clause **body** with kind `fallthrough` |
| `select` | `switch_case` header → each comm clause; no "no default" edge (select blocks); empty `select {}` → `exception` → `@exit` |
| `break`/`continue` (labeled or not) | `break` / `continue` to resolved target |
| `goto L` | `fallthrough` to labeled stmt (no new kind; rare) |
| `return` | `return` → `@exit` (via defer chain, below) |
| falling off end | `return` → `@exit` |
| panic-capable stmt | `exception` → `@exit` (via defer chain) |
| deferred-call resume | **`defer_resume`** (Go add, §4.3.1) |

**4.3.1 `defer` — two nodes, no cycle [call, from builder check].** A `defer f(x)` is split into
two body nodes with distinct keys:

- **Registration node:** the `defer` statement itself, `kind:"statement"`, keyed at the `defer`
  keyword (`stmt.Pos()`), on the straight-line path. It **uses** the call's receiver and arguments
  (evaluated here, per Go semantics) and weak-defines a pseudo access path `<defer>:<line>:<col>`
  (the call's key), carrying those evaluation-time values to the exit.
- **Deferred call node:** the L1 `call` node (keyed at the call expression's position — a different
  column from the `defer` keyword, so no collision), `is_deferred:true`, placed **only** in the exit
  chain. It uses `<defer>:<line>:<col>` (argument values from registration) plus, at the exit
  point, named results and (for `defer func(){…}()`) everything the closure captures (§4.6
  closures rule).

Every edge that reaches `@exit` (`return`, fall-off, `exception`) is routed through the **defer
chain** — deferred call nodes in reverse source order — then to `@exit`. The edge from an exit point
into the chain keeps its original kind (`return`/`exception`); chain-internal edges and the tail →
`@exit` are `defer_resume` (Go add, pre-registered in CLAUDE.md and the keystone rubric). A
deferred call that can panic gets an `exception` edge to the next chain member (remaining defers
still run). Over-approximation: every defer is assumed registered on every path (conditional and
in-loop defers included; one chain member per `defer` statement, not per execution). The CFG
stays acyclic through the chain, so post-dominance and the "reaches `@exit`" gate hold.
`go f(x)` is **not** split: the `go` statement shares its key with the call node (the call is the
step; arguments are evaluated there; the spawned body is not a CFG successor).

**4.3.2 panic / recover.** A statement *can panic* if it contains a call (any call — callee bodies
can panic), an index/slice expression, a type assertion without `ok`, a pointer dereference/selector
on a pointer, a channel send/close, a division, or a `panic(...)`. Go has no handlers, so `exception`
edges go to the defer chain then `@exit`. A `panic(...)` statement has **only** the `exception`
edge (no fall-through). `recover()` inside a deferred closure is not modelled as resuming normal
flow (over-approximate: the function still "exits"; data defined in the defer still reaches
`@formal_out` via the defer chain, which is what taint needs). `os.Exit`/`log.Fatal`-style no-return
calls are treated as ordinary calls (over-approximation; differs from `go/cfg`'s `mayReturn`, which
the differential test must account for).

**4.3.3 goroutines.** `go f(x)` is a CFG step whose successor is the next statement; the spawned
function is not a CFG successor (keystone). Its data effect is the call (§4.6).

**Gates:** single `@entry`/`@exit`; every node has a real span (synthetic excepted); every node
reachable from `@entry` and reaching `@exit`; edges sorted/deduped by `(src,dst,kind)`; node count
stable across runs; unreachable code after `return`/`panic`/`goto` pruned (Python precedent).

### 4.4 CDG (L3) [call]

Post-dominators via Cooper-Harvey-Kennedy on the reverse CFG rooted at `@exit`; control dependence
via Ferrante-Ottenstein-Warren. Nodes with no controller get an edge from `@entry` (Python
precedent — every statement anchored). Self-loops skipped. `cdg` edges `{src,dst}`, no attrs.
Gate: hand-computed control dependences on fixtures match exactly (incl. infinite loop, `select`,
defer chain).

### 4.5 DDG (L3, syntactic) [call]

Monotone forward may-reaching-definitions over **k-limited access paths**
`base(.field|[*])*`, `k` = `--graph-field-depth` (default 3; `x.f.g.h` → `x.f.g.*`; bases never
truncated). Match Java's `AccessPath` / Python's `access_paths` semantics:

- **Bases:** locals, parameters, named results, the receiver, captured variables, package-level
  variables (qualified `pkgpath.Name`, see §4.6). Shadowed variables are distinct bases — identity is
  the `types.Object`, not the spelling (fixes the "shadowed variables leak" gate for free via
  `TypesInfo.Uses/Defs`). The emitted `var` is the source spelling (`x.f`), as siblings.
- **Defs/uses per node:** from the node's own header expressions only (`if` cond + init, `for`
  cond/post/init, `range` key/value defs + range-expr use, switch tag / type-switch guard,
  case expressions, comm-clause send/recv, assignment LHS/RHS, inc/dec, `return` operands).
  `@entry` defines every parameter, the receiver, named results (zero value), captures and every
  package-level base the callable reads.
- **Writes:** plain `x = …` strong-kills `x` and its extensions (`x.f`, `x[*]`); `x.f = …` kills only
  `x.f…` (no kill of `x`); `x[i] = …` is a weak def of `x[*]` (kills nothing); `*p = …` is a weak def
  of `p.*` (deref collapses to `.*`, kills nothing). Compound assignment and `++`/`--` def+use.
- **Calls:** a call uses all its argument expressions and the receiver; it **weak-defines `a.*` for
  every argument `a` that is pointer-, slice-, map-, chan-, func- or interface-typed, and the
  receiver if it is a pointer receiver or reference-typed** (pointer, slice, map, chan, func or
  interface — e.g. `hs.Write(b)` on an interface, `h.Set(k, v)` on a map-typed `http.Header`; the
  callee may write through it; Python's suffixed-weak-def precedent). **Amended (D1, gap 2):** the
  predicate is widened to **"may hold a reference"** — the reference kinds above, or a struct or
  array whose fields/elements transitively contain one (cycle-safe). A value struct or array that
  may hold a reference (`fill(cfg)` with `cfg.items []T`) weak-defines `a.*` (whole base, not per
  field, to stay k-safe). A value struct or array that holds no reference is not weak-defined (copy
  semantics). The **same** predicate function is the L4 by-ref `formal_out` rule (§4.6), so caller
  weak defs and callee ports agree. A **slice expression** argument (`r.Read(buf[:n])`,
  `io.ReadFull(r, hdr[:])`, and the destination of `copy(dst[i:], src)` / `clear(s[:n])`) shares its
  elements with its base, so it weak-defines the base's elements **`base[*]`** — slice, array or
  pointer-to-array base alike (`&`, `*` and parentheses around the base are stripped). `[*]` rather
  than `.*` because it is the element step the builtin rule (`copy`/`clear` → `dst[*]`) and index
  reads (`buf[0]` uses `buf[*]`) already use; the slice bounds are still uses.
- **Channels (goal 4, coarse):** `ch <- v` weak-defines `ch[*]` and uses `v`, `ch`; `v := <-ch` /
  `v, ok := <-ch` / `range ch` / recv in a comm clause uses `ch[*]` and defines `v`. Cross-goroutine
  flow then rides parameter/capture/field paths — a channel passed to a goroutine is an argument, so
  its `[*]` contents flow via the port wiring (§4.7) and the "weak-def through reference arguments"
  rule.
- **Maps/slices:** `m[k] = v` weak-defines `m[*]`; `m[k]` reads `m[*]`; `append(s, v)` uses `s`, `v`
  and its result flows to the LHS; it also weak-defines `s[*]` (`append(dst[:0], v)` gives
  `dst[*]`), since it may write into the destination's backing array (spare capacity, or a
  re-sliced `dst[:0]`).
- **Type errors (partial types):** a package that does not type-check still gets its defs. A field
  selector with no recorded selection (its operand has an invalid type, e.g. `var req missing.Req`)
  is still a field step (`req.Body`); an identifier go/types recorded nothing for (undeclared, or a
  member of a package that failed to import) is an unresolved base identified by its spelling, and a
  write through it weak-defines its root `x.*`. An assignment target is never demoted to use-only
  while it has a variable root: a target `pathOf` cannot walk (`x.(*T).f = v`) weak-defines its
  root `x.*`.
- **Use matching:** a use links to a reaching def if the paths interfere — equal, either a prefix of
  the other, or a wildcard covers it.
- **Edge attrs:** `{src, dst, var, prov:["ssa"]}`. `prov` value is `"ssa"` for **parity with Python
  and Java**, whose L3 DDG is also syntactic reaching-defs labelled `ssa` (the SDK's prov
  precision ranking and the keystone's monotonicity rule both key on it). Rejected: TS's
  `reaching-defs` at L3 — the SDK ranks `reaching-defs` as a distinct, less-approximate token and
  using it here would mislabel. Sorted by `(src,dst,var,prov)`.
- **Gates:** loop-carried `x = x + 1` self/back edge; shadowed variable does not leak; k-limit
  contract example; backward intraprocedural slice over `cdg ∪ ddg` of a named var at a named line
  equals the hand-computed set exactly.

Differential check (non-gating, reported): compare scalar def-use against `go/ssa` (the
construction guide's answer key) on fixtures; mismatches are triaged, not asserted.

#### L3 precision (known gaps) — amended 2026-10-25 (D1) [user]

The L3 `ddg` is **syntactic and intraprocedural**. It over-approximates where it models a flow
(weak defs, bidirectional prefix matching). The re-check found that L4 as first designed closed
neither gap 2 (the §4.10 oracle aliases only reference-typed bases, and the by-ref predicate
excluded value structs) nor gap 3. **Decision (D1): all three gaps are closed at L3**, before L4
port wiring (§9 Phase 2 prerequisites), and L4 still adds capture ports on top:

- **Gap 1 — writes to captured variables inside closures.** `func(){ x = secret }()` or
  `once.Do(func(){ cfg = load() })` writes `x` / `cfg` in the closure's own body. **L3 fix:** the
  node containing the literal weak-defines each captured variable the literal (or a literal nested
  in it) may write; the write-set is computed syntactically per literal, and (amended 2026-10-27,
  from metamorphic relation MR5) also holds `x.*` for every captured `x` that may hold a reference
  (`MayHoldRef`): a write through a local alias, `q := p; q.n = v`, writes no path of `p`. In
  sequential code a
  closure cannot run before it exists, so this is complete apart from goroutine interleaving (a
  non-goal). Expect more `ddg` edges at capture sites (measured on Kubernetes). **L4** then adds a
  traceable source through capture ports (§4.6 closures rule).
- **Gap 2 — value structs or arrays with reference fields.** `fill(cfg)` with `cfg.items []T` may
  write `cfg.items[*]`. **L3 fix:** the "may hold a reference" predicate (§4.5 Calls) weak-defines
  `a.*`. Weak defs drive the `ddg` long tail (`l3-dataflow-metrics.md` takeaway 4), so the change
  is gated on a Kubernetes measurement. **L4** reuses the same predicate for by-ref `formal_out`
  (§4.6).
- **Gap 3 — deferred operands after reassignment.** The deferred call node already uses every path
  it may write through (`defer w.Flush()` uses `w.*`, `defer fill(b[1:])` uses `b[*]`). The real
  miss was `defer fill(b); b[0] = s; b = o`: `b = o` kills `b[*]` before the exit, losing the write
  `fill` reads. **L3 fix:** every write whose base is an operand base of a registered `defer` in the
  same callable also weak-defines `<defer>:<key>`, which closes the false negative. L4 does not
  touch gap 3.

**Remaining documented over-approximation:** the deferred call's exit-time uses stay keyed on the
*variable* at exit, not on the value bound at registration (they are the points-to hook), so a
reassignment after the `defer` can still add a **spurious** edge. That is extra edges, not a
dropped flow.

With these fixes the L3 `ddg` misses no flow it models, but L3 is still **intraprocedural**: a flow
through a callee is not an L3 fact, and the absence of an L3 `ddg` path does not prove the
absence of a flow. A `go/ssa` def-use differential (built with the L4 SSA join, §4.8) is
reported, non-gating.

### 4.6 L4 vertices [call; naming follows Python because the SDK reads it]

Per callable (formals), all with `kind` + `of`, no span:

| Vertex | `of` | When |
|---|---|---|
| `@formal_in:<i>` | real parameter name; receiver name for the receiver | receiver first (index 0) if method, then declared params in order (unnamed or `_` params: `_<i>`; unnamed or `_` receiver: `_recv` — D6) |
| `@formal_in:<i>` | `<capture>:<name>` | each variable a closure captures (after declared params) |
| `@formal_in:<i>` | `<global>:<pkgpath>::<Name>` | each package-level variable the callable (transitively, via summary) reads |
| `@formal_out` / `@formal_out:<i>` | `<return>` / `<return>:<i>` | **one per result position** — `<return>` for a single result; `<return>:<i>` for each position of a multi-result, **never the user's result names** (D6) |
| `@formal_out:<i>` | parameter name | each parameter (incl. the receiver, pointer or value) satisfying the **shared by-ref predicate** the callable may write through (by-ref out) |
| `@formal_out:<i>` | `<capture>:<name>` | each captured variable a closure writes (strong or weak def), or that satisfies the by-ref predicate (a write through a local alias, `q := p; q.n = v`, is no def of `p`) |
| `@formal_out:<i>` | `<global>:<pkgpath>::<Name>` | each package-level variable written (transitively, via summary) |

**Indexing and order (amended, A9) [call].** Python's `IdentityMap` rule verbatim
(`codeanalyzer-python dataflow/identity.py`): `formal_in` and `actual_in` are always indexed; a bare
`@formal_out` / `<call>/actual_out` **iff that group has exactly one vertex**, else every vertex in
the group is indexed — so a single-result function with a by-ref or global out has
`@formal_out:0` (`<return>`), `@formal_out:1`, …. `formal_out` order: results by position, then
by-ref params (receiver first) in declaration order, then captures, then globals sorted by `of`.
`formal_in` order: receiver, declared params, captures, globals sorted (matches `EntryBases`).

**`of` naming (D6) [call].**
- Unnamed or `_` param: `_<i>`, i = the **declared-param index, receiver excluded** — the same
  spelling `external_symbols[].parameters[]` uses. (In-project `parameters[].name` stays `_`; the
  vertex `of` is the authority.)
- Unnamed or `_` receiver: `_recv`.
- Captures `<capture>:<name>`; globals `<global>:<pkgpath>::<Name>` (matches the L3 `ddg` base
  spelling `pkgpath::Name`; the Go SDK matches on import-path segments, §7).
- **The SDK reads `of`, never an index into `parameters[]`**: `formal_in:0` is the receiver for a
  method, so Java's `formal_in:<n>` = `parameters[n]` convention does not hold for Go.

**By-ref predicate (amended, A3/A10) [call].** By-ref `formal_out` (callee-side `mutated_params`)
uses **the same function** as the L3 caller-side call rule (§4.5 Calls): reference-typed (pointer,
slice, map, chan, func, interface) **or "may hold a reference"** (struct/array transitively
containing one, D1 gap 2), for parameters and the receiver alike — a pointer receiver, and a value
receiver of reference kind (map-typed `http.Header`) or one that may hold a reference. Caller weak
defs and callee ports therefore agree.

Per call site with a resolved callee (in-project **or stand-in**, §6):

| Vertex | `of` | `parent` |
|---|---|---|
| `<call-key>/actual_in:<i>` | the bound callee formal's name (mirrors the callee's `formal_in:<i>` `of`) | the call key |
| `<call-key>/actual_out` / `…/actual_out:<i>` | mirrors callee's `formal_out` `of` | the call key |

Vertices are **per call node** (Java style), keyed on the L1 call key (A11). Python groups actuals
per statement; that is a recorded parity difference the SDK does not depend on. A call node the
CFG pruned (unreachable, kept in `body` with no edges) gets **no** vertices.

**Why Python's naming, not the keystone's `$ret`/`argN` [call, the one non-trivial parity choice]:**
the SDK's `resolve_value` resolves a source/sink `(name, within)` against `formal_in` `of`, and
`value_candidate` translates exactly the Python markers (`<global>:`, `<capture>:`, `<return>`).
With Python's spelling the shared resolver works unchanged; Java/TS's `$ret`/`argN` would require a
Go-specific translation layer. The `<return>:<i>` multi-result form is a Go-only leaf extension of
`<return>`. Sources/sinks address `formal_in` only, so `<return>:<i>` never reaches
`value_candidate`; it does reach `body_node_kind` (which labels path hops), where `RETURN_MARKER`
is compared by equality — so a multi-result hop would be labelled `return` with name
`<return>:1`. The Go backend normalises this locally before calling the shared helper; shared
`commons/` stays untouched.

**Closures — by-reference captures, flow-insensitive binding [call, from builder check].** Go
closures capture variables by reference, and the closure may run later (`defer func(){…}()`,
`go func(){…}()`, stored callbacks), so binding a capture to the defs reaching the `FuncLit` would
miss later assignments (false refutation). Rule:

- The closure callable gets `@formal_in:<i>` `of:"<capture>:<name>"` per captured variable and
  `@formal_out:<i>` `of:"<capture>:<name>"` per captured variable it **writes** (strong or weak
  def) **or that satisfies the shared by-ref predicate** (amended 2026-10-27, from metamorphic
  relation MR5): as for a by-ref param, a write through a local alias (`q := p; q.n = v`) is no
  def of the capture at L3, so a reference-holding capture always gets its `formal_out`.
- In the enclosing callable, the `FuncLit` has a **capture site** (amended, A6 — a call's key is
  `call.Pos()` = `Fun.Pos()` = `FuncLit.Pos()`, so keying the site at the `func` keyword collides
  with the call node of `func(x){…}(v)`, `defer func(){…}()`, `go func(){…}()`):
  - **Directly called literal:** the captures bind **at that call node**. Their
    `<call-key>/actual_in:<i>` / `actual_out:<i>` indices continue after the declared arguments'
    (matching the closure's `formal_in` order: params, then captures). For `defer func(){…}()` this
    is the deferred call node in the exit chain.
  - **Any other literal** (stored, passed, returned): the capture site is keyed at the literal's
    position, taking the next free `/n` suffix the way `BodyNodes.add` does, so it never moves an
    L1 call key (chains such as `func() T {…}().M()` already shift to `l:c/2`).
  - The site's vertices carry `parent` = the innermost CFG node that contains the literal (the
    host); at a directly called literal, `parent` = the call key, as for any call (§4.6 table).
  - `param_in`/`param_out` connect the site to the closure's capture formals. No `summary` edges at
    a non-call capture site (it is not a call; flow through the closure is via its own body).
- **Flow-insensitive wiring for captured variables** (port wiring, `prov:["reaching-defs"]`):
  *every* def of a captured variable anywhere in the enclosing callable → the capture site's
  `actual_in`; the capture site's `actual_out` → *every* use of that variable in the enclosing
  callable, **and** (amended, A7) the enclosing callable's own `@formal_out` port for that variable
  when it is a named result, a by-ref param base or a global. Without the latter,
  `defer func(){ err = wrap(err) }()` — whose deferred call node only *uses* named results, with
  nothing defining them after it — never reaches `@formal_out(err)`: a false refutation on the
  commonest Go error idiom. This over-approximates timing (covers defer, goroutines and callbacks
  alike) and is the same weak-update posture as the rest of the design.
- Calls *of* the closure value (`f := func(){…}; f()`) resolve to the closure callable via VTA at
  L4 and get ordinary call-site vertices for its declared parameters.

Go specifics:
- **Variadic** `f(xs ...T)`: excess actuals collapse onto the last formal (weak, as Java).
- **Multi-value call as args** `f(g())`: each `g` result binds to the corresponding `f` formal.
- **Method expression** (`T.M(s, x)`): resolved callee is `M`; `s` binds `formal_in:0`.
- **Method value** (amended, A8) (`f := s.M; f(x)`): the receiver is not an argument of the call,
  and VTA resolves the call to `M$bound` (unwrapped to `M`, §4.8). For a bound-method callee the
  call's **function-value operand** (`f`) feeds `actual_in:0`, and the arguments bind
  `formal_in:1…`. L3 already carries `s` into `f` (the `f := s.M` statement uses `s`).
- **Global state as extra parameters** (keystone L4 rule): globals read/written are computed in the
  summary pass and propagated up the call graph, so a caller's `formal_in`/`formal_out` set includes
  globals its callees touch (Python precedent).
- **Transitive globals at hosts (amended, A4) [call].** L3 `@entry` defines only the package vars a
  body touches **directly** (not those of nested literals), and a satellite call has an empty
  DefUse, so a global G only a callee reads has no def reaching `<call>/actual_in(G)`, and a global
  only a callee writes never defines G at the host. Adopt Python's `augmented_facts` + re-solve
  (`summaries.py`): at each host statement, add the callee's `global_reads` as uses and its
  `global_writes` as **weak** defs; `@formal_in(G)` is the def of G for transitively read globals.
  Re-run `BuildDDG` and emit only the **new** edges, `prov:["reaching-defs"]`. Weak defs kill
  nothing, so the L3 `ssa` set is untouched and L3 ⊆ L4 holds.
- **Global filter — only globals that can carry data [call, from builder check; amended A5].**
  Python emits 546k `<global>:` formals on one real app; transitive propagation is quadratic at
  Kubernetes scale. A **project** package-level variable is modelled as an extra parameter only if
  it is **mutable in a way that can carry a value**: it has at least one write in any project
  function body **including closures and `init`** — a strong def, a **weak def from the call rule**
  (`mu.Lock()` on a global with a pointer receiver), or `&v` — **or** its initializer contains a
  call (e.g. `os.Getenv`). A global initialized only from literals/constants and never written
  cannot carry taint, so dropping it removes no flow. **Non-project (library) package vars always
  pass the filter** (`os::Args`, `os::Stdin`, `flag` vars — the L3 `@entry` already defines them via
  `isPkgVar`); without that they would be unnameable as sources. Constants and functions are never
  globals. This filter is sound under the over-approximation rule; `<global>:` volume is measured in
  the eval (§8) and a size budget set from the first measurement.
- **Range-over-func (amended, A21; D8) [call].** `for v := range seq` has no `CallExpr` (no L1 call
  node), and SSA routes the loop body through a synthetic yield function, so data the iterator
  passes to `yield` (a `rows.Scan` result inside a DB iterator) never reaches `v`
  interprocedurally. **Not modelled this train:** a program that uses range-over-func gets degraded
  code `range_func` (§4.15), and a fixture pins the gap. Modelling `for … range f` as a call site
  of `f` is a follow-up (§11).

### 4.7 L4 edges [call]

- `param_in` (application scope, global ids): `<caller>@<call>/actual_in:<i>` →
  `<callee>@formal_in:<j>`, **`var` = callee formal `of`** on every edge (Python #195 / Java #250 —
  the SDK's sanitizer cuts match on `var`).
- `param_out`: `<callee>@formal_out[:<i>]` → `<caller>@<call>/actual_out[:<i>]`, `var` = `of`.
- **Port wiring** (`ddg`, `prov:["reaching-defs"]`, Python precedent; Java uses `ssa` — following
  Python because both projections of the SDK accept it and it keeps port edges distinguishable from
  statement def-use):
  `@formal_in:k → first-use statements of that base` (replaces the `@entry` def for that base for
  port purposes — keep both, the `@entry` edge stays for L3 monotonicity); `return stmt →
  @formal_out[:i]` (`var` = the formal's `of`, `<return>` / `<return>:<i>` — never the user's
  result name, D6), statement writing a by-ref param/global →
  corresponding `@formal_out`; `host stmt → <call>/actual_in:i` (`var` = arg spelling or `argN` if
  not an access path); `<call>/actual_out[:i] → host stmt` (`var` = the LHS base it defines). **This
  wiring is what the SDK's `_ports_carry_dependence` probe checks; without it taint certifies false
  refutations.**
- **Port host (amended, A11) [call].** Go records the host directly: host = the call node's
  `parent` if set, else the call node itself (a bare call, a `go` call, a deferred call — each is
  its own CFG node). For a deferred call the host is the deferred call node in the exit chain; its
  DefUse already carries `<defer>:<key>` plus the weak defs of §4.5. Wiring is Java's coarse
  host → `actual_in` (every host use feeds every `actual_in`; sound). Python's reaching-def-matched
  variant is optional. A pruned call node (no vertices, §4.6) gets no port wiring.
- `summary` (callable scope, local ids): `<call>/actual_in:i → <call>/actual_out[:j]` iff the
  callee's summary says formal `i` may flow to output `j`.
- Edge-only-when-resolved: a call site whose callee cannot be resolved at all (e.g. a call through a
  `func` value with no VTA target) gets no vertices — **but** to avoid a false refutation it gets
  conservative intra-callable `ddg` edges `arg host → host` (the existing L3 "call uses args, defines
  results" already provides this: L3 DDG treats the call statement as using every argument and
  defining its LHS). Taint therefore still flows *through* an unresolved call within the caller; it
  just cannot enter the unknown callee.
- No dangling endpoints; `param_in`/`param_out` sorted by `(src,dst)`.

### 4.8 Dispatch resolution: VTA for L2 and L4 [call]

Must-have flow (3) needs concrete targets for interface and function-value calls. Use
`x/tools/go/callgraph/vta` over `go/ssa` — the construction guide's recommended Go oracle — with SSA
built per D2 (amended, A12): `ssautil.Packages` over the **same** `go/packages` load, not
`AllPackages`.

- **L2 refinement (amended, Task 10):** `call_graph` gains VTA-derived edges for interface/func-value
  call sites, `prov` `["vta"]`, weight 1 — **only** for a `(src, dst)` pair the L2 resolver did not
  already find. An edge L2 found that VTA also finds is left exactly as L2 emitted it (its `prov`
  is not unioned, its weight not summed): the `-a 3` `call_graph` is a byte-for-byte subset of the
  `-a 4` one (the monotonicity gate). `Result.CallEdges` holds only the VTA-only pairs, so
  `Result.ApplyTo`'s `MergeEdges` appends them and never unions into an L2 edge. Body `callee` stays a single id (the static callee — the interface
  method for an interface call) — the schema has one slot. Multiple concrete targets are carried by
  `call_graph` and, at L4, by one `param_in`/`param_out` set **per target** from the same
  `actual_in` vertices (the actual vertices are per call site, not per target).
- **Interface methods are already callable nodes** (bodiless, in the interface type's
  `callables{}` — `collectInterfaceMethods`), so an interface call's static `callee` resolves today.
  At L4 a bodiless interface method gets `formal_in`/`formal_out` vertices (so the call site's
  `actual_*` have a target for the static edge) and a **pass-through summary** (like a stand-in, §6),
  so a call whose VTA target set is empty still does not drop flow. When VTA finds concrete targets,
  `param_in`/`param_out` go to each concrete target **and** to the interface method; the
  call-site `summary` edges are the union over targets. **D7:** an interface call *always* binds to
  the interface method (pass-through) and every VTA target, even when every target is in-project —
  per the anti-goal. Cost: every interface call (`io.Writer`) carries an all-to-all summary.
- **Interfaces with no symbol-table node (ruling, Task 10) [call].** `error.Error()` (the universe
  `error`), methods of interface literals (`interface{ M() }`), and methods promoted through an
  embedded interface whose origin is declared outside the symbol table have no callable id and no
  external id, so such a call gets **no** interface-method target. It keeps its VTA targets;
  when VTA finds none the call is unresolved, and the L3 host pass-through (§4.7: the call
  statement uses its receiver and arguments and defines its LHS) keeps the intra-caller flow. No
  false refutation; but a sink cannot be named as `error.Error`. A stand-in for the universe
  `error.Error` is a follow-up (§11).
- **VTA-only externals (amended, A19).** A VTA target outside the project that the `go/types`
  resolver never saw (e.g. `(*bytes.Buffer).Write` reached through `io.Writer`) is **added** to
  `external_symbols` at `-a 4`, with its stand-in body (§6). Monotone: `-a 3` ⊆ `-a 4`.
- **SSA construction (amended, A12; D2) [call].** The load is
  `NeedSyntax|NeedTypesInfo|NeedDeps`, so `AllPackages` would create, and `Build` would compile,
  **every dependency body** (stdlib included). Instead:
  - `ssautil.Packages(roots, ssa.InstantiateGenerics|ssa.BuildSerially)` over `builder.Pkgs()`:
    bodies only for project packages; dependencies come from types.
  - Build **per package** (`pkg.Build()`) in our own goroutine under `recover` — `Program.Build`
    and VTA panic inside their own goroutines, out of reach of the per-callable `recover`. A panic
    degrades (`partial_ssa` / `no_ssa`, §4.15).
  - Ill-typed packages (`pkg.IllTyped`) get no SSA and degrade to `partial_ssa`.
- **VTA scope [call, from builder check].** Give `vta.CallGraph(funcs, nil)` a function set of
  **project functions only** (the SSA functions of project packages, plus their anonymous
  functions). `initial = nil`: VTA uses its internal lazy CHA (no separate `callgraph/cha` seed).
  VTA needs no `main`/roots, so libraries work. Library callbacks are therefore not discovered (§6,
  as intended) and cost stays bounded by project size. VTA runs under `recover`; a panic degrades to
  `no_vta`. Targets are sorted by `can://` id (determinism: never key on SSA names or map order).
- **SSA → L3 join (amended, A12) [call].** By **AST pointer identity**, never by position:
  - Function: `fn.Syntax()` == the L3 unit's `Unit.Fn` (`l3units.go`); generic instances via
    `fn.Origin()`. This holds because SSA is built from the same `*ast.File`s; positions break under
    `//line`, and SSA's anonymous-function numbering (`f$1`) follows evaluation order, not
    `ast.Inspect` order (L1 closure naming `closure_N` stays).
  - Call instruction: `CallCommon.Pos()` == `CallExpr.Lparen` → the `*ast.CallExpr` → its L1 key.
    Never by `line:col`.
  - Synthetics unwrap: `$bound` / `$thunk` and promoted-method / pointer wrappers to the method
    they wrap; instantiation wrappers to the origin; range-over-func `yield` synthetics fold into
    `fn.Parent()`; `init#N` to its `init`; "from type information" synthetics have no syntax and map
    to no callable.
  - An SSA function or call that maps to nothing in-project is an external stand-in (§6) if it has a
    package path outside the project; otherwise it is counted and degrades with `unmapped_ssa`
    (§4.15).
- **Mapping to `can://` ids (amended, A13) [call].** The L4 IR keys vertices by `(L3Key, local key)`
  and cross-callable edges by `L3Key` pairs and external sigs — no ids in the IR. The emitter mints
  global ids from an `L3Key` → callable-id map built in the same walk as `flow.apply` (`emit.go`).
  Do not reuse `sigIndex.bySigPath`: it keys on the **nesting** module path, not the declaring file.
  (The per-site AST walk the old text tied to this is already fixed by `callsite_index.go`.)
- **VTA runs only at `-a 4`** (and `--emit neo4j`, which is `-a 4` per D4, §4.12): SSA construction is the
  dominant cost; `-a 2`/`-a 3` keep today's cheap `go/types` resolver (an `-a 3` run must not pay
  L4 cost). VTA edges therefore appear in `call_graph` only at `-a 4` (still monotone: L3 ⊆ L4).
- Unsound spots (reflection, `unsafe`, cgo, plugins): no targets ⇒ handled by the unresolved-call
  rule above (§4.7).
- SSA is also the substrate for the may-alias oracle (§4.10).

### 4.9 Summary pass [call]

Bottom-up over the call graph (L2 `go/types` edges ∪ VTA edges), condensed with iterative Tarjan
SCC (sorted for determinism), callees first; non-recursive singletons solved once, recursive SCCs
iterated to a monotone fixpoint over finite domains (formal indices × outputs; globals sets).
Context-insensitive. Per callable compute:

- `flows ⊆ inputs × outputs`, inputs = formals (incl. receiver, captures, globals), outputs =
  results, by-ref params, written globals — by forward reachability over the callable's
  `ddg ∪ cdg` from each input's `@entry`/`formal_in` def, bridging call sites via callees'
  summaries (`summary` edges). Including `cdg` (implicit flow) matches Python and over-approximates.
- **What summaries refine (amended, A15).** At statement granularity the host already links every
  use to every def (the L3 call rule), so a callee summary adds **no intra-caller precision**; it
  contributes only the callee's global footprint (A4, §4.6) and its by-ref writes. Summary edges
  will be close to all-to-all — a precision limit, not a bug (Python's stated posture,
  `summaries.py`).
- `global_reads`, `global_writes`, `mutated_params` (written through, from the shared by-ref
  predicate, §4.6), propagated up the SCC DAG and injected at hosts as uses / weak defs (A4).
- No hammock regions (neither sibling built them; recorded as a known simplification).
- Stand-ins (§6) contribute their modelled summary (default all-inputs → all-outputs).
- **Global → global summary edges are not emitted [user, 2026-10-26].** A `summary` edge whose
  `actual_in` and `actual_out` both have a `<global>:` `of` is omitted; every edge with at least
  one non-global end is kept, and the flows (and so the global formals, actuals and param edges)
  are computed in full. Under the coarse host wiring (A15) a summary edge only shortcuts
  host → `actual_in` → … → `actual_out` → host, and sources and sinks address `formal_in`, so a
  global → global summary adds no reachability for an HRB-unaware walk. Measured on nats-server:
  97.7% of 101M summary edges were global → global; the cut took `sdg.Run`'s peak RSS from
  11.1 GB to ~4.4 GB (2.34M summary edges left). They return with the HRB two-phase walk (§11).
  Tracked as **codeanalyzer-go#30**. Caveat: the SDK's slices and taint walks are
  **depth-bounded** (python-sdk `DEFAULT_DEPTH = 5` for slices); a dropped global → global
  shortcut makes the remaining route longer (into the callee's body and back), so a depth-bounded
  slice may stop short of a node it would have reached through the shortcut. Reachability with an
  unbounded depth is unchanged.
- **Unanalysed callables (amended, A16).** A callable whose L3 result was dropped (a contained
  panic or a call-count mismatch, `run.go`) has no `ddg`, so taint entering through `param_in`
  would stop at its `formal_in`. Such callables get formals and a **pass-through summary** (like an
  interface method) and are listed under degraded code `l3_skipped` (§4.15). The same applies to SSA
  functions that map to no callable (`unmapped_ssa`).
- Caching of summaries in `cache_dir` and SCC-wavefront parallelism: **builder's choice** whether
  to implement in this train; determinism is mandatory either way.

### 4.10 Alias oracle and the L4 semantic DDG delta [call]

L4 adds `ddg` edges with `prov:["points-to"]` **in addition** to L3's `ssa` edges (strictly
additive). Oracle: **type-based may-alias MVP** — two access paths with different bases may alias iff
the bases' types are reference types (pointer/slice/map/chan/interface/func) with compatible
pointee types (identical, or one is an interface the other's dynamic type may satisfy), and their
field suffixes are prefix-compatible. The oracle answers `may_alias(path1, path2)`; the DDG solver
re-runs with it consulted only when a suffix exists (Python precedent). This catches
`p := &req; p.UserID = id; … q.UserID` (flow 2) without SSA points-to. Rejected for this train:
an Andersen solve over `go/ssa` (precision upgrade, own PR later, keystone's "MVP may stub the
oracle"); `go/pointer` (deprecated).

The oracle's API is frozen as an interface so an Andersen implementation can replace it later
without schema change (`prov` stays `points-to`).

**As built (Task 17 and its fix round 1) [call].** `AliasOracle.MayAlias(d, u)` asks whether a write
to `d` may change memory a read of `u` observes. The oracle is built per callable
(`newAliasOracle(unit, root)`). The solver re-runs the same reaching-definitions fixpoint over the
port wiring's augmented `du'`. It never adds a kill across bases. It consults the oracle only for a
def and a use of different bases inside a **one-step offset window**: after at most one leading step
is dropped from each, the suffixes are prefix-compatible.
- **Anchors.** The oracle matches each path at offset 0 (its base) or offset 1 (its base and first
  step). The anchored prefix is one of two things:
  - a *reference*: the object it points to. A bare def of a reference rebinds it and writes
    nothing that another base observes.
  - an *address-taken value*: its own storage.
- **Compatible references.**
  - Pointee types are compared structurally on the underlying types, with `types.Identical` on the
    element types. This covers pointers, slices, maps (key and element), chans of either direction,
    and identical signatures. So `type Bytes []byte` aliases `[]byte`, and `http.Header` aliases
    `map[string][]string`.
  - Any two interface (or type-parameter) types are compatible.
  - An interface is compatible with a type that implements it.
- **Address-taken values.** A value x is address-taken when the callable's top-level declaration
  (closures included) contains any of:
  - `&x`, `&x.f…` or `&x[i]…`;
  - `x[:]` or `x[i:j]` on an array value;
  - a call or method value of a pointer-receiver method on x or `x.f…`.

  Package-level variables are always address-taken.
- **Address-taken matching.** An address-taken value of type T may alias:
  - a `*T`;
  - an interface that `*T` implements;
  - for an array `[N]E`, a `[]E`.

  A field whose address is taken, or a field of reference type, matches at offset 1. So `p := &o.f;
  p.g = v` reaches `o.f.g`, and `m := s.cache; m[k] = v` reaches `s.cache[k]`.
- **Never alias.** Two distinct value variables never share storage. Neither do value copies that are
  not address-taken.
- **Known gaps (false negatives).**
  - Offsets deeper than one step (`&x.f.g` against a `*G`).
  - Two prefixes of one base that alias each other.
  - An interface holding a value struct whose reference field is written through the interface.

  The Andersen upgrade closes them.

### 4.11 Neo4j projection [call]

Fill the reserved overlay, no contract renames:

- `:GoBodyNode` for every body node incl. synthetic vertices; props add `of` → **`var`** and
  `parent` → **`call_node`** (the **local** parent key, JSON `parent` verbatim, resolved against
  the owning callable — Python parity: codeanalyzer-python's `project.py` stores `node.parent` and
  python-sdk's `reconstruct.py` maps `call_node` straight back to `parent`; a global id would not
  round-trip) — Python's projection names, which the SDK's Cypher reads
  (`projection` includes `n.var`). Keep `kind`, `is_goroutine`, `is_deferred`, `span_json`; add the
  flat span props siblings use if the SDK backend needs them (builder confirms against
  `PyNeo4jBackend`).
- `GO_HAS_BODY_NODE` callable → every body node (incl. synthetic).
- `GO_CFG_NEXT {kind}` `_k=kind`; `GO_CDG`; `GO_DDG {var, prov}` `_k=var|prov` (live since
  Phase 0/1); `GO_SUMMARY`; `GO_PARAM_IN {var}` / `GO_PARAM_OUT {var}`. **Amended (A17):** today
  `schema.go` declares the param rels with no props; Phase 2 adds `var` to both (Python parity,
  `codeanalyzer-python neo4j/schema.py`).
- Stand-ins (§6) project onto the existing `:GoExternal:GoSymbol:GoCanNode` ghost (same
  `can://<app>/@external/…` id, so today's call-graph ghosts and stand-ins are one node; it already
  carries `signature`/`module`). Their vertex ids are `<external-id>@formal_in:<i>`, so the SDK's
  `under_callable` id-prefix rule covers them. `GO_HAS_BODY_NODE` already allows a `GoExternal`
  source. **Amended (A17):** the projector today creates `:GoExternal` only as call-endpoint ghosts
  and never walks an external's body; Phase 2 adds a **stand-in pass** that emits each external's
  `body` vertices as `:GoBodyNode` with `GO_HAS_BODY_NODE` from the `:GoExternal`.
- **`degraded_json`** (amended, A17) — a new `:GoApplication` property carrying the envelope's
  `degraded[]` (§4.15).
- Flip `Reserved:false`; conformance test must see every declared relationship emitted on the
  fixture; cross-projection gate: Neo4j counts == JSON counts modulo `HAS_*`.
- `--emit neo4j` now runs `-a 4` (D4, §4.12).
- **Lazy Bolt push (amended, A17; D5) [call].** The lazy push diffs per module by `content_hash`,
  but L4 facts are non-local: editing a callee changes callers' `summary` edges, actual vertices and
  even `formal_in` indices (a transitive global set grows). Policy: at L4 a lazy push **upserts
  every L4 row on every push** (additive — adds, never removes). Removing stale L4 rows needs
  `--eager`. The README documents this under Neo4j and in Upgrading. App-wide L4 purge-and-rewrite
  on a lazy push is deferred (§11).

### 4.12 CLI and envelope [call]

- `-a 3`, `-a 4` accepted; `--graph-field-depth <k>` (default 3, error below `-a 3`, error with
  `--emit neo4j`).
- **Phase 2 CLI tasks (amended, A14).** Today `-a 4` is rejected, there is no `LevelSDG` in
  `internal/options`, no `--strict`, `--emit neo4j` forces `LevelDataflow`, and the
  `--graph-field-depth` help says "at -a 3 only". Phase 2 adds `LevelSDG`, accepts `-a 4`, adds
  `--strict`, sets the neo4j level per D4, handles `-t` per D9, and updates the help to "L3+".
- **`--emit neo4j` level (D4) [user].** `--emit neo4j` runs at max implemented depth = **L4**, and
  `-a` / `--graph-field-depth` with it stay an error — the same as Python and Java v2. (Letting
  `--emit neo4j` accept `-a ≤ max` would be a CLI-contract change shared with the siblings; not
  decided here.)
- **`--target-files` with L4 (D9) [user].** L4 is whole-program, so a targeted run cannot produce
  correct summaries or `param_in` edges. Incremental L4 is deferred to the CLDK-wide
  incremental-analysis design (§11).
  - `-a 1..3 -t` (JSON): unchanged, still works.
  - `-a 4 -t` (JSON): an error, exact text:
    `--target-files is not supported at analysis level 4 (whole-program); run without -t, or use -a 3`
  - `--emit neo4j -t`: runs at **L3**, not L4, and pushes the targeted modules' L1–L3 rows as today.
    It writes no L4 rows: the targeted modules' L4 rows stay as the last full run left them until
    the next full run (`--eager` removes stale ones, D5). It logs one warning on stderr, exact text:
    `--target-files: projecting the graph at level 3 (level 4 is whole-program); L4 rows keep the last full run's facts until a full run`
  - The README documents both rules under Neo4j and in Upgrading.
- Envelope: emit `k_limit` at L3+ (Python precedent; harmless superset of the keystone's "at L4").
- `max_level` = requested level.
- **Envelope and tree fields (amended, A18).** `v2.Analysis` gains `degraded` (§4.15); `BodyNode`
  gains `of`; `Callable` gains `summary`; `Application` gains `param_in`/`param_out`;
  `ExternalSymbol` gains `receiver` and `body` (§6). All absent below `-a 4` (the `schema.go`
  package doc's "L4 fields … are not declared here" is retired).
- `--strict`: fail non-zero on any degradation at `-a 4` (§4.15) (default: degrade — keep L3,
  derived vertices and param edges from `go/types` dispatch, log a warning to stderr, and record
  the `degraded` envelope marker), Java precedent.
- `-j N`: parallelize L3 per callable and L4 per SCC wavefront if implemented; collect then sort by
  id; never mint ids or emit inside a worker. Byte-identical to `-j 1`.

### 4.13 Caps [call]

No hard node/time caps (neither sibling has them; a cap silently drops edges ⇒ false refutation).
Termination comes from k-limiting and finite summary domains.

**Memory (amended, A20; D3) [user, 2026-10-26: measured, not gated].** Today L3 nodes keep their AST (`ir.go` `Stmt`), the
whole envelope is marshalled into one `[]byte` (`core/analyzer.go`), and most RSS is `go/packages`
type info (Kubernetes root: 7.1 GB at `-a 3`). L4 adds SSA bodies for project packages, VTA,
summaries and roughly 1.5–2.5× the body nodes (the ports).
- **Prerequisites:** #27 streaming JSON output and AST release (before the real-app L4 eval).
- L4 needs the ASTs to bind actuals, so each callable's L4 vertices are built **before** its AST
  references are dropped. The SSA program is shared and freed after L4.
- **Memory is measured, not gated (D3, amended) [user, 2026-10-26].** L4 is whole-program
  analysis and needs more memory than L3; the former "≤ 1.5× `-a 3`" budget is dropped. The
  real-app eval (§8) reports, per app, the absolute `-a 4` peak RSS and its ratio to `-a 3`.
  Loading dependencies from export data (dropping `NeedDeps` syntax) is a separate follow-up, not
  this train (§11).
- **Output-neutral levers (Task 15b).** `sdg.Run` releases each unit's flow graph and call-site
  bindings once its call-graph SCC is solved and its summary edges are produced, and the cached
  port wiring once `fill` has copied it into the result; it interns the formal keys and `of`
  strings (every `<global>:` spelling) so identical spellings share one string; and it lowers
  the process GC target to 50 while it runs (`debug.SetGCPercent`, restored on return, never
  raised above the caller's), so L1–L3 and emission keep the default. None changes a fact: the
  result is deeply equal with them off (`TestSDG_OutputUnchangedByMemoryLevers`).
- **Where the `-a 4` peak sits (measured, Task 16/18).** With the levers on, the end-to-end
  peak is no longer in `sdg.Run` but in the emission: the streaming writer still builds one
  module's subtree whole before writing it, and an L4 module of a large file is big
  (nats-server's `server/server.go` module is 344 MB of JSON). The next lever, if one is needed,
  is to stream per callable within a module; measured numbers per app are in
  `l3-dataflow-metrics.md` § L4.

### 4.14 L2 external calls (Phase 0, implemented on PR #16) [user: fix it; details are calls]

Before Phase 0 the L2 resolver resolved library callees (`internal/semantic_analysis/call_graph.go`)
and then dropped them: no `call_graph` edge, and the emitter left the call node's `callee` `null`.
Python and TS emit these; Java emits them behind `--external-calls` (always under
`--emit neo4j`). Go now emits them at L2:

- **`application.external_symbols{}`** (the Python/Java name) — one entry per distinct external
  callee: `id` (`can://<app>/@external/<import-path>/<sig>`, §6), `kind:"external"`, `name`,
  `module` (import path), `signature`. Parameters/return type are added here too (cheap from
  `go/types`; span-less `{name, type, is_variadic?}`, unnamed → `_<i>`, receiver excluded). The
  receiver is not recorded today (`recordExternal` drops it); Phase 2 adds the optional
  `receiver {name, type}` leaf and the L4 `body{}` vertices (§6).
  An external id may name a library *interface* method (e.g. `@external/io/Writer.Write`), and a
  method on a generic library type records the declared (origin) signature.
- **`call_graph`** keeps edges to external targets (`dst` = external id, `prov:["go/types"]`).
- **Body `callee`** is backfilled to the external id (Python precedent: a callee is "a symbol-table
  callable or an `@external` home"). Still a monotone `null → id` refinement.
- **On by default, no flag** (Python precedent; Java's opt-in flag exists only for its v1 parity,
  which Go does not need — Go is already breaking ids in this release).
- Still `null`: builtins (`len`, `append`, `make`, …, which `go/types` does not model as
  `*types.Func`), calls through function values, and anything unresolvable. Type conversions
  (today minted as `<Type>.__new__`) stop being minted and stay `null` — they are not calls. Taint
  still flows through a conversion (`string(b)`) because L3 DDG treats it as an expression: the
  statement defining the LHS uses `b`. (Builtins likewise: L3 "call uses every argument, defines its
  LHS".)
- This is an intended **L2 output change shipped in Phase 0** (more `call_graph` edges, non-null
  `callee` on library calls, a new `external_symbols` map; README Upgrading).
- Neo4j: the `:GoExternal` ghost path in `project.go` (`callEndpoint`) receives these rows;
  `GO_RESOLVES_TO` extends to external callees; `GO_CALLS` endpoints include `GoExternal`.
- Gate: the L2 fixture gate (`gate_l2_test.go`) asserts the external id plus a matching
  `external_symbols` entry, and no dangling endpoint.

### 4.15 Degradation marker [user]

When `-a 4` cannot build what L4 needs — SSA/VTA construction fails, some packages failed to
type-check and got no SSA, or some callables or constructs were not analysed — the analyzer still
emits (L3 + derived vertices + `go/types` dispatch, with pass-through summaries for unanalysed
callables) but marks the envelope:

```json
"degraded": [{"code": "no_vta", "detail": "...", "packages": ["example.com/x/pkg"]}]
```

- Envelope leaf, sibling of `max_level`; **absent** when nothing degraded (absence = no fact).
  Entry shape `{code, detail?, packages[]?, callables[]?}` (`callables[]` = `can://` ids).
  `max_level` still states the requested level.
- Codes (open vocabulary, plain strings):
  - `no_ssa` — SSA build failed entirely.
  - `no_vta` — VTA failed (incl. a recovered VTA panic).
  - `partial_ssa` — listed packages had type errors (`pkg.IllTyped`) or a recovered per-package
    build panic, so no SSA.
  - `partial_types` — packages with `go/packages` errors at any level.
  - `unmapped_ssa` — SSA functions or call instructions that map to no callable or call node
    (§4.8); counted, listed where nameable (A16).
  - `l3_skipped` — callables whose L3 result was dropped (contained panic, call-count mismatch);
    listed in `callables[]`; they get formals and a pass-through summary (§4.9, A16).
  - `range_func` — the program uses range-over-func, which is not modelled (§4.6, D8); listed
    in `callables[]`.
- Projected to Neo4j as a `degraded_json` property on `:GoApplication` (§4.11).
- `--strict` turns any degradation into a non-zero exit.
- **SDK:** the Go backends read it at attach. When present, `taint()` returns **no `exhausted`
  pairs** and adds an `unresolved` diagnostic naming the degradation (`complete=False`), so a
  "no flow" certificate is never issued over a degraded graph; witnesses (`paths`) are still
  returned. Same rule for `paths_between`-style refutations. Implemented in the Go backend (the
  diagnostic goes into the `blocked` ledger the walk already returns, which `taint_verdict` honours —
  shared `commons/` unchanged).
- **Go leaves (all additive, none in the shared keystone yet):** the `degraded` envelope field
  itself; the entry's `callables[]` (`can://` ids, sorted; carried by `l3_skipped` and
  `range_func`); the codes beyond the original four (`partial_types`, `unmapped_ssa`,
  `l3_skipped`, `range_func`); and, in the vertex vocabulary it degrades over, the `_recv` `of`
  for an unnamed or `_` receiver (D6) and `<return>:<i>` (§4.6).
- Go-only leaf now; candidate for the keystone alongside the cross-language taint revisit (§11).

### 4.16 Decisions of the 2026-10-25 re-check

The L4 design was re-checked against the shipped L1–L3 code (`3504445`) and the siblings; the
amendments are folded into the sections above. Decisions:

- **D1 (#22) [user]:** all three L3 gaps are closed at L3, and L4 still adds capture ports; the
  `go/ssa` def-use differential is built with the L4 SSA join and does not gate (§4.5).
- **D2 [call]:** SSA comes from `ssautil.Packages` over the same load, built per package with
  `ssa.InstantiateGenerics|ssa.BuildSerially` under `recover` (§4.8).
- **D3 [user; amended user, 2026-10-26]:** memory is measured and reported per app (absolute
  `-a 4` peak RSS and its ratio to `-a 3`), not gated: L4 is whole-program. (Was: `-a 4` peak
  RSS ≤ 1.5× `-a 3` on the Kubernetes root.) Export-data loading is a separate follow-up (§4.13).
- **D4 [user]:** `--emit neo4j` runs at max implemented depth = L4; `-a`/`--graph-field-depth`
  with it stay an error, as in Python and Java v2 (§4.12).
- **D5 [call]:** a lazy Bolt push upserts every L4 row on every push; removing stale L4 rows needs
  `--eager`, documented in the README (§4.11).
- **D6 [call]:** `of` = `<return>` / `<return>:<i>` (never user names); unnamed or `_` params
  `_<i>` (declared-param index, receiver excluded); unnamed or `_` receiver `_recv`; captures
  `<capture>:<name>`; globals `<global>:<pkgpath>::<Name>` (§4.6).
- **D7 [call]:** an interface call always binds to the interface method (pass-through summary)
  **and** to every VTA target (§4.8).
- **D8 [call]:** range-over-func is not modelled; a program that uses it gets degraded code
  `range_func`, and a fixture pins the gap (§4.6, §4.15).
- **D9 [user]:** L4 is whole-program; `-a 1..3 -t` unchanged; `-a 4 -t` (JSON) is an error;
  `--emit neo4j -t` projects at L3 with a one-line stderr warning; exact texts and the full rule in
  §4.12; incremental L4 deferred to the CLDK roadmap (§11).

## 5. Flow coverage map (user goal 4 → mechanism)

| Flow | Mechanism | Level |
|---|---|---|
| (1) through calls | formal/actual vertices, `param_in/out`, `summary`, port wiring | L4 |
| (3) interface / func-value dispatch | VTA targets → per-target `param_in/out` | L4 |
| (2) fields & pointers | k-limited access paths; weak-def through reference args and "may hold a reference" value structs/arrays (D1 gap 2); type-based may-alias `points-to` delta | L3 + L4 |
| closures | L3: the literal's host weak-defines the captures it may write (D1 gap 1). L4: captures as `<capture>:` formals; nested callable's own SDG; capture site at the direct call node or the literal (A6), wired flow-insensitively incl. into `@formal_out` (A7) | L3 + L4 |
| goroutines | `go f(x)` is a call for data purposes (vertices + edges); no interleaving | L4 |
| channels | `ch[*]` container path; cross-goroutine via args/captures/fields | L3 + L4 |
| library calls | stand-ins with default pass-through summary | L4 |
| range-over-func iterators | **not modelled** (D8): degraded code `range_func`, fixture pins the gap | — |

## 6. Library stand-ins (sinks/sources naming library functions) [user]

**Problem:** the SDK resolves `(name, within)` by first resolving `within` to an *analysed* callable,
then a `formal_in` of it. `database/sql.(*DB).Query` is not in the tree, so it cannot be a sink.
**Decision:** the analyzer emits a **stand-in** for every external function/method the project
actually calls (resolved via `go/types`; VTA targets outside the project included). Go-only leaf
addition now; the cross-language SDK taint revisit is deferred (§11).

- **Where in the tree:** a new optional application-scope map
  `application.external_symbols{ <id>: external }` — the same name and shape Python
  (`PyApplication.external_symbols`) and Java (D19) already ship for call-graph endpoint homes —
  **extended** on Go with `callables`-like content: each external entry carries `kind:"external"`,
  `name`, `module` (import path), `signature`, `parameters[]` (real names from `go/types`; unnamed
  → `_<i>`), `return_type`, and at L4 a `body{}` holding only its `@formal_in:<i>` /
  `@formal_out[:i]` vertices. The map itself landed at L2 in Phase 0 (§4.14). **Amended (A19):**
  Phase 2 adds two optional leaves — `receiver {name, type}` (a method's receiver; it gives
  `formal_in:0`'s `of`, `_recv` when unnamed) and `body{}`. `ExternalParameter` (span-less, `_<i>`)
  fits unchanged. Externals only VTA finds are added at `-a 4` (§4.8; monotone). Reachable by
  containment from `application` ⇒ keystone-legal.
- **Id:** `can://<app>/@external/<import-path>/<sig>` where `<sig>` is the existing
  `calleeSignatureOf` canonical form minus the package path (`DB.Query`, `Sprintf`). Language-neutral
  `@external` per Python/TS/Java.
- **Vertices:** `formal_in` per receiver+param (`of` = real name, else `_recv` / `_<i>`, D6),
  `formal_out` per result (`<return>` / `<return>:<i>`), plus by-ref `formal_out` for each param and
  receiver satisfying the shared by-ref predicate (§4.6) (library may write through them —
  `json.Unmarshal(data, &v)`, `buf.Write`). Indexing per §4.6 (A9).
- **Stand-in summary (pass-through default):** every `formal_in` → every `formal_out`. Materialized
  as `summary` edges at each **caller's** call site (`actual_in:i → actual_out:j`), *not* as edges
  inside the stand-in. The stand-in has no outgoing `param_out` into unrelated callers beyond the
  ones each call site owns (param_out goes stand-in `formal_out` → *that* call site's
  `actual_out`), so — exactly as for in-project callees — an HRB-unaware shortest-path walk
  could enter at call site A and leave at call site B. **This is the existing SDK behaviour for every
  shared callee in every language** (the SDK's context-sensitivity gate is "if summary edges
  present"); not made worse by stand-ins. Recorded as a known over-approximation.
- **Models file (optional overrides):** `--library-models <path.json>` (builder may also embed a
  small default set), mapping an external id/signature to an explicit flow list, e.g.
  `{"strconv/Atoi": [], "strings/TrimSpace": [["s","<return>"]]}`. Starts empty or
  near-empty; no catalogue of sources/sinks — that stays the caller's (SDK rule).
  **Format (amended, Task 15):** a JSON object; key = `<import-path>/<sig>`, the external id's
  tail (`strconv/Atoi`, `database/sql/DB.Query`); value = a list of `[in, out]` pairs of `of`
  strings (`"s"`, `"_0"`, `"_recv"`, `"<return>"`, `"<return>:1"`, a by-ref param's name).
  `[]` = no flow; a missing key = the all-to-all default. Valid only at `-a 4` and with
  `--emit neo4j` (`-a 1..3`: error `--library-models applies only at analysis level 4`); a
  missing, unreadable or malformed file is an error naming the path. An entry naming an `of`
  the external lacks is a warning and is ignored for that external, which keeps the all-to-all
  default (a typo never refutes a flow); an entry for an external no call site targets (or a
  misspelt key, `strconv.Atoi`) is ignored with one warning per key, in sorted key order:
  `--library-models: no call site targets <key>; entry ignored (keys are <import-path>/<sig>, e.g. strconv/Atoi)`.
  A method of a generic library type is keyed by its declared (origin) signature
  (`sync/atomic/Pointer.Store`), so one entry covers every instantiation. Under
  `--emit neo4j -t` (L3, D9) the file is still read and checked but unused, and the D9 warning
  ends `; --library-models is ignored at level 3`.
- **Not modelled:** callbacks invoked by library code (`sort.Slice(xs, less)`,
  `http.HandleFunc(path, h)`): the function-value argument flows in, but the library calling it back
  is not an edge. Taint *starting* in a handler is unaffected (sources are the handler's own
  params). Recorded limitation.
- **SDK addressing:** `("query", "database/sql.DB.Query")` resolves through the Go backend's
  `resolve_callable` (domain = analysed callables ∪ stand-ins) then `resolve_value` over the stand-in
  body's `formal_in` `of`. Shared `commons/` code is untouched.

## 7. SDK (python-sdk) [user: in scope; Claude may write it]

Build on python-sdk PR #434 (L1/L2 Go support: models, ABC, local + Neo4j backends, facade, factory;
branch `pr-434-v3`, unmerged as of 2026-10-25), fixing it first against the v2-only analyzer.
`add-go-support` is obsolete.

- **Phase 3 prerequisites (amended, A22)** — on `pr-434-v3` today:
  - `GoType.kind: Literal["struct","interface"]` **rejects** `alias`/`defined`, which the analyzer
    emits today (`extra="ignore"` does not help with a Literal mismatch). Fix first, so the SDK can
    parse current output at all.
  - `GoBodyNode` lacks `id`/`parent`/`of`/`is_deferred`; there is no `external_symbols`/
    `param_in`/`summary`/`degraded`, and no `_ports_carry_dependence`. Add the L3 fields first (so
    `-a 3` output parses), then L4.
  - `extra` policy is **`ignore`** (§2).
- **Id splitting (A22).** External ids contain `/@external/` before the vertex `@`, so the Go
  backend must **never split a body-node id on its first `@`**; strip the known callable id prefix
  instead.
- **Global names (A22).** `value_candidate` turns `<global>:example.com/lib::V` into
  `example.com/lib.V`, so a `lib.V` query fails `segment_match` (dot-separated). The Go backend
  normalises the import path, or matches on `/` as well (D6 keeps `<global>:<pkgpath>::<Name>`).
- **`of`, not `parameters[]` (A9).** Go backends resolve formals by `of`; `formal_in:0` is the
  receiver for a method.
- **Models** `cldk/models/go/`: `extra="ignore"` (PR #434's choice, matching TS/Java since #386)
  mirror of Go v2 (module/type/field/callable/body node/edges/external symbols incl. `receiver` and
  `body`, `degraded`), mirroring how `cldk/models/python` and `java` model L3/L4.
- **Facade** `GoAnalysis` via `CLDK.go(...)` and the legacy shim; `GoAnalysisBackend` ABC with
  `N="Go"`, `P="GO_"`.
- **Surface:** the decided CLAUDE.md SDK facade (type-centric: `get_types`/`get_type`,
  `get_functions`, `get_methods_of`/`get_method`, `get_fields`, `get_callers`/`get_callees`) **plus**
  the L3/L4 accessors with the **same names and signatures as `PythonAnalysisBackend`**:
  `get_cfg`, `get_cdg`, `get_ddg`, `resolve_callable`, `resolve_value`, `slice_backward`,
  `slice_forward`, `paths_between`, `reaches`, `backward_cone`, `flows_to_call`,
  `flows_to_argument`, `taint`, `get_source`, `get_external_symbols`. Implementations route through
  `commons/graphs.py`, `commons/resolve.py`, `commons/results.py` exactly as Python's do.
  `resolve_callable` for Go accepts receiver forms `DB.Query`, `(*DB).Query`, `sql.DB.Query`,
  `database/sql.DB.Query` (segment matching via existing `resolve_callable_signature`).
- **Backends:** local `GoCodeanalyzer` (runs `cango` subprocess, parses JSON, builds networkx
  graphs) and `GoNeo4jBackend` (Cypher via `sdg_path_query`/`sdg_taint_query` with
  `node_label="GoBodyNode"`, `P="GO_"`, `endpoint_scope`/`interior_scope` per the multi-application
  scope audit). Contract test `test_go_backend_contract.py`; parity: both backends answer identically
  including miss paths.
- Gate `_require_dataflow` (local) at level 3; port-connectivity probe — refuse slicing/taint if no
  dependence edge leaves any `formal_in`. This exists only on the Java backend
  (`java/backend.py:2329`, abstract there, implemented in both Java backends); Go declares and
  implements it the same way (Python has none).
- **Satisfying the shared `AnalysisBackend` ABC [call, from builder check].** The ABC
  (`commons/backend.py:101-184`) requires class-centric abstracts (`get_all_classes`, `get_class`,
  `get_all_methods_in_class`, `get_method`, `get_all_fields`, `get_method_parameters`) and the
  `Py*`-typed artifact getters. Go implements them as required, without changing the ABC: "class"
  = any named Go type (`GoType`, all four kinds), methods = its receiver methods, fields = struct
  fields; artifact getters return the shared `Py*` artifact classes (as the ABC documents). The
  CLAUDE.md type-centric names (`get_types`, `get_functions`, `get_methods_of`, …) are the
  **documented** Go surface; the ABC names are thin aliases over them. This refines CLAUDE.md
  decision SDK1 ("not `get_classes`") — the ABC makes the class-named methods unavoidable;
  update CLAUDE.md accordingly.
- CLAUDE.md (python-sdk) language table updated in the same change.

## 8. Verification

**Fixtures** (`testdata/`, new `testdata/dataflow/` module + augment existing):
if/else + loop; early return + panic + defer; `recover`; `select`; labeled break/continue; `goto`;
`fallthrough`; type switch; range over chan/map/slice; infinite `for {}`; closure capture;
goroutine + channel; struct field + pointer write-through (`svc.Handle(&req)`); interface dispatch
with ≥2 implementations; func-value call; `a→b→c` chain; mutual recursion; global written in one
function read in another; multi-file flow; cross-file method; multi-result returns; variadic;
library pass-through and library sink (`database/sql`, `os/exec`, `fmt`); generics (existing
`testdata/generics`: instantiation → `Origin()` mapping, generic interface dispatch); an HTTP-handler →
SQL-sink end-to-end taint fixture with a sanitizer variant (the "done" fixture).

**Added by the 2026-10-25 re-check (A23):** a Go 1.23+ module (range-over-func — pins the D8 gap —
and range over int; `testdata/dataflow` declares `go 1.21`, #28); a method value (A8); a directly
called literal with captures (A6); a deferred closure that writes a named result (A7); a value
struct holding a slice (D1 gap 2); a forced L3-skipped callable (A16); an interface call whose
concrete target is a library type (A19); one fixture per SSA wrapper kind (§4.8 unwrap list).

**Analyzer gates** (each level gated before advancing — `codeanalyzer-backend` HARD-GATE):
CFG gate, dominance gate (exact CDG), PDG-slice gate (exact set), DFG gates, L4 gates (no dangling;
arity; summary exists for known transitive flow; `points-to` ⊇ adds-only; ports carry dependence),
determinism (`-j 1` vs `-j N`, two runs), monotonicity (`-a 1..4` superset via a test like
Python's `test_v2_superset.py`), cross-projection (Neo4j vs JSON), Neo4j conformance, SDK model
validation of every level's output.

**Metamorphic relations** (`internal/metamorphic`; Task 17b, amended 2026-10-27). A relation
transforms a copy of a module, analyzes it in-process at `-a 4`, and compares the result with
the original's through a position map. Callables are keyed by signature (plus the declaring file
for the per-file `init`), not by module id. There are three oracles: **exact** (the normalized
views are equal), **delta** (the view changes by exactly the edges each site declares, nothing
else changes, and the expected CDG is a reference post-dominator CDG of the expected CFG), and
**reach** (one-way: every `formal_in` → `actual_in`/`formal_out` flow of a sample of callables
survives over ddg ∪ cdg ∪ summary ∪ param_in ∪ param_out). MR1 adds blank lines and comments
(exact). MR2 reverses the top-level declarations (exact). MR3 moves a function and a method to
a new file (exact). MR4 alpha-renames locals (exact modulo spelling). MR5 introduces an alias
(reach). MR6 extracts a helper (reach). MR7 runs `--target-files` at `-a 3` against a full run
(exact on that file). MR8 wraps a statement in `if c {}`. MR9 inserts a kill. MR10 inserts a
copy. MR11 swaps two independent statements. MR12 adds an early `if c { return }`. MR8–MR12
use the delta oracle, on local scalars in callables with an L3 result and no defer or goto.
MR13 replaces a sink argument with a constant: it is a precision relation, so the flow into
that `actual_in` must be gone and nothing else may be added. MR14 routes a call through a
one-implementation interface, a closure or `defer` (reach, plus the summary edges for the
interface form). MR15 extracts a helper and inlines it back (exact, byte-identical source).
Loops, `select`/`switch`/`fallthrough`, panic edges and the defer chain are out of scope for
the delta relations. The fixtures (`testdata/dataflow` flow and sdg, chi) run in `go test`;
the eval apps run behind the `metamorphic` build tag (`scripts/metamorphic.sh`).

**SDK gates:** slice gate (exact set on a `can://…@line:col` criterion); taint gate (known
source→sink found; sanitizer variant reported cut, not dropped; contiguous witness); a pair with no
flow lands in `exhausted`; context-sensitivity only as far as summaries support; JSON and Neo4j
backends agree; e2e level matrix.

**"Done"** (user-confirmed intent, designer-worded): through `CLDK.go(...)`, on the HTTP→SQL
fixture, `taint()` finds the flow from a handler's request parameter to `database/sql.DB.Query`'s
`query` through an interface call; a callable sanitizer cuts it; an unrelated pair is `exhausted`;
identical answers from the JSON and Neo4j backends.

**Real-world evaluation** (non-gating metrics + crash/determinism/time gates; amended, A23): the six
apps — cobra (small), dae, openbao, listmonk and memos (HTTP → SQL; memos through the
`store.Driver` interface), nats-server (concurrency / CFG stress) — plus **Kubernetes as the scale
benchmark**, run as two modules: the root module `kubernetes` and `k8s-api`
(`docs/design/notes/l3-dataflow-metrics.md` § Corpus). cockroach is no longer a benchmark. dae
needs its code-generation step (bpf2go) before analysis (`scripts/eval_l3.sh --prepare`). Record per
app: wall time and peak RSS at `-a 3`/`-a 4`, body/edge counts, % call sites with resolved targets
(go/types vs VTA), % externals with stand-ins, `<global>:` formal volume (A5), determinism. Gate:
the six apps and both Kubernetes modules complete `-a 4` without crash and deterministically
(`-j 1`/`-j N` and two runs on Kubernetes, not just fixtures); memory is reported per app as
absolute `-a 4` peak RSS and its ratio to `-a 3`, not gated (D3, §4.13); no hard time budget, but timings are reported and a regression budget is set after
the first measurement. Results go in `l3-dataflow-metrics.md` § L4.
**Measured 2026-10-27 (go1.27.3, `c230592`):** the gate passed on all eight. Every app completes
`-a 4` and its `-j 1` / `-j 16` / rerun outputs are byte-identical. The Kubernetes root takes
31.5 s at a 7.7 GB peak (1.05× `-a 3`). nats-server is the outlier at 3.6× RSS and 1.78 GB of
JSON. The full results, insights and limitations are in `l3-dataflow-metrics.md` § L4.

## 9. Phasing and release

| Phase | Repo | Content | Release |

> **Phases 0 and 1 ship together in one PR / one release** [user, 2026-10-23]; they stay separate rows for traceability. **Both are implemented, on PR #16 (open, not yet merged).**

|---|---|---|---|
| 0 | analyzer | **implemented (PR #16, open).** id grammar `can://<app>/go/…`; **L2 external calls (§4.14)**; L1 co-positioned call keys; `is_deferred`; `typeKind` alias/defined; `GO_DDG` `_k`; call-site position index (perf) | analyzer next **breaking** version (wheel is at 0.1.0 with no local tags — builder confirms the released version and bumps the major, or the minor if still 0.x) |
| 1 | analyzer | **implemented (PR #16, open).** L3: body statements, CFG (+defer chain, panic), CDG, syntactic DDG, CLI `-a 3`/`--graph-field-depth`, Neo4j CFG/CDG/DDG, gates | minor |
| 2 | analyzer | **Prerequisites, in order (A24):** (1) #26 shared call/closure walker (incl. `//line` spans), first — returns `(*ast.CallExpr, key)`, `(*ast.FuncLit, closure name)` and call → host maps, replacing the index-paired L1/L3 walks; (2) #17 closure call resolution, after #26 — closures carry a declaring `Path` and `call_graph` gains edges from closures; (3) the #22 L3 fixes (D1, §4.5), then the #28 satellite-`parent` tests and a Go 1.23+ fixture — before L4 port wiring (changing L3 after L4 would re-baseline every L4 gate); (4) #27 streaming JSON and AST release — before the real-app L4 eval, can run in parallel with the L4 core; (5) schema and CLI scaffolding (A14, A18, A19, A17 declarations, a `-a 1..4` monotonicity harness) — the first L4 task. Dependencies: #26 → #17 → (VTA, summaries); #22 fixes → port wiring and gates; scaffolding → all of L4; #27 → the Kubernetes eval. **Then L4:** SSA spike (D3 measurement) and build (D2), SSA → L3 join, VTA, `degraded` marker (§4.15), external stand-ins (`external_symbols` `receiver`/`body`), vertices, param edges, summaries, port wiring, type-based alias delta, Neo4j L4 overlay, `--emit neo4j` at L4 (per D4), real-app eval | minor |
| 3 | python-sdk | Go v2 models (first the §7 prerequisites: `GoType.kind`, L3 fields), facade, local + Neo4j backends, L1–L4 accessors, taint e2e | minor; pins the analyzer only after Phase 2 is released |

Tracking: `codellm-devkit/.github` parent issue + one sub-issue per PR (Phase 0+1 as one) (filed just-in-time),
per `issue-and-pr-tracking.md`. Each phase gets its own implementation plan.
Docs updated per phase: CLAUDE.md § Schema decisions (new Go leaves: `defer_resume`, stand-ins,
`<return>:<i>`, kind mapping), `docs/design/roadmap.md` (L3/L4 un-deferred),
`v2-schema-structure.md`, README "Architecture & Tooling" (substrate slots: CFG = go/ast hand-built
with go/cfg differential; def-use = syntactic reaching-defs with go/ssa differential; oracle = VTA
dispatch + type-based may-alias).

## 10. Left to the builder

- Package layout under `internal/` (guide: a `dataflow/` subpackage, one module per stage, `core`
  stays a pure orchestrator), internal IR shapes, worker-pool design.
- Whether summary caching and SCC-wavefront parallelism land in Phase 2 or later.
- Exact differential-test tolerance vs `go/cfg` / `go/ssa`.

## 11. Deferred / follow-ups

- **Cross-language SDK taint revisit** [user]: library sinks/sources for Python/Java/TS (stand-ins or
  multi-node selectors in `commons`); promote `external_symbols.body` stand-ins into the keystone if
  adopted.
- Andersen points-to over `go/ssa` replacing the type-based oracle (`prov` unchanged).
- Context-sensitive (HRB two-phase) taint traversal in the SDK. With it, reinstate the global →
  global `summary` edges omitted at L4 (§4.9, [user, 2026-10-26]; codeanalyzer-go#30 — also
  the depth-bounded-slice caveat there).
- A stand-in for the universe `error.Error` (and interface literals), so an interface call on
  them gets a D7 target (§4.8, Task 10 ruling).
- Stream per callable within a module during emission, if the emission-held module peak
  (§4.13) matters on a larger app.
- `GO_EMBEDS` / `GO_SATISFIES` from `base_types[]` / `interfaces[]`: `base_types` holds raw type
  strings, not type ids, and `interfaces[]` is never computed (codeanalyzer-go#31; pre-existing,
  not an L4 change).
- Library callback modelling (`sort.Slice`, `http.HandleFunc`).
- Promote `callable.source_file` to the keystone (existing deferral).
- Struct-tag modelling (existing deferral).
- **Incremental L4 / incremental Neo4j analysis — CLDK roadmap** (D9): re-analysis after a PR merges
  without a whole-program run.
- Range-over-func modelling (D8): `for … range f` as a call site of `f`, wiring data reaching the
  iterator's `yield` arguments to the loop variables.
- App-wide L4 purge-and-rewrite on a lazy Bolt push (D5 option (c)).
- Loading dependencies from export data instead of source (drop `NeedDeps` syntax) — a large RSS
  cut at every level, but it changes the L1/L2 load path (D3).

## 12. Risks

| Risk | Mitigation |
|---|---|
| SSA/VTA cost at Kubernetes scale | SSA only at `-a 4`, project packages only (D2); memory measured and reported per app (D3); #27 streaming; `--strict` / degrade path |
| False refutations from a missed construct | over-approximation posture; "ports carry dependence" probe; per-construct fixtures; unresolved-call intra-caller pass-through |
| Over-approximation noise (defer chain, pass-through, CDG in summaries) | acceptable by anti-goal; library models file narrows hot spots |
| Breaking id change disrupts users | ship alone in Phase 0, before SDK pin; no released SDK consumes Go output |
| Determinism with parallel L3 / map iteration | collect-then-sort, ids minted outside workers, `-j` gate |
| Divergence between JSON and Neo4j | cross-projection gate; shared projector over the same v2 tree |
