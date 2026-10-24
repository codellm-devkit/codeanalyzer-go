# Spec: Go L3/L4 dataflow (CFG/CDG/DDG + SDG) with SDK taint (codeanalyzer-go)

**Status:** design, awaiting approval · **Date:** 2026-10-23 · **Repos:** `codeanalyzer-go`,
`python-sdk` · **Supersedes:** the "L3/L4 deferred" entries in `docs/design/roadmap.md`.

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
- **Substrate:** `golang.org/x/tools v0.46.0` is already a dependency — `go/cfg`, `go/ssa`,
  `go/ssa/ssautil`, `go/callgraph/vta`, `go/callgraph/cha` are available with no new module.
  `x/tools/go/pointer` is deprecated: **do not adopt**.
- Level flag semantics (`cli-contract.md`): `-a 3` implies `-a 2`; `-a 4` implies `-a 3`; an `-a 3`
  run must not pay L4 cost; `--emit neo4j` runs at max implemented depth (now 4) and rejects `-a` and
  `--graph-field-depth`.
- python-sdk rules: the Iron Rule (no public accessor changes name/signature/return type); new
  capability only as new methods; models are `extra="forbid"` mirrors of v2.

## 3. Current state (what exists)

- `codeanalyzer-go` L1/L2 on v2, Neo4j projection live. Analysis loads packages once via
  `go/packages` (`NeedSyntax|NeedTypes|NeedTypesInfo|NeedDeps`); the internal v1-shaped model
  (`internal/schema/schema.go`) is re-serialized by `internal/syntactic_analysis/v2emit`.
- No per-statement facts survive into the internal model; L3 must work from `pkg.Syntax` +
  `pkg.TypesInfo` directly.
- `internal/schema/v2/ids.go` already defines `OrdinalID` (`<callable>@<line>:<col>`) and `TagID`
  (`<callable>@<tag>`). `internal/neo4j/schema.go` already reserves the L3/L4 relationships.
- Known defects to fix on the way: `GO_DDG` discriminant is `var` only (must be `(var, prov)` per
  CLAUDE.md K1 and siblings); `typeKind` never emits `alias`/`defined`; `is_deferred` is declared but
  never populated; L2 call-site resolution re-walks the whole package AST per site (parked memory
  `parked-callsite-resolution-perf`) — must be fixed before L3/L4 multiply call-site lookups.
- L2 dispatch: an interface-method call's callee is the interface method (CHA-style); no concrete
  targets. This is insufficient for must-have flow (3).
- python-sdk `main` (2.0.0, c125d48) has **no Go support at all** — verified 2026-10-23: no tracked
  Go files, `CLDK` has only `java`/`python`/`typescript` factories, the legacy `analysis()` shim
  dispatches only those three. Branch `add-go-support` (July 2026, never merged, 332 commits behind)
  is an old **v1-shaped** implementation and is obsolete. Since then PR #434 (unmerged) adds v2
  L1/L2 Go support; the SDK work builds on it (§7).

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
`id` field (Python #176 precedent, which the SDK reads).

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
  callee may write through it; Python's suffixed-weak-def precedent). Value-typed struct arguments and
  receivers are not weak-defined (copy semantics). A **slice expression** argument (`r.Read(buf[:n])`,
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
  and its result flows to the LHS (it is a builtin; treat as library pass-through).
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

### 4.6 L4 vertices [call; naming follows Python because the SDK reads it]

Per callable (formals), all with `kind` + `of`, no span:

| Vertex | `of` | When |
|---|---|---|
| `@formal_in:<i>` | real parameter name; receiver name for the receiver | receiver first (index 0) if method, then declared params in order (unnamed `_`/blank params get `_<i>`) |
| `@formal_in:<i>` | `<capture>:<name>` | each variable a closure captures (after declared params) |
| `@formal_in:<i>` | `<global>:<pkgpath>::<Name>` | each package-level variable the callable (transitively, via summary) reads |
| `@formal_out` / `@formal_out:<i>` | `<return>` | **one per result position** — Go's multiple returns: `<return>` for a single result; for >1 results, `@formal_out:<i>` with `of` = the named result's name if named else `<return>:<i>` |
| `@formal_out:<i>` | parameter name | each pointer-/reference-typed parameter (incl. pointer receiver) the callable may write through (by-ref out) |
| `@formal_out:<i>` | `<global>:<pkgpath>::<Name>` | each package-level variable written |

Per call site with a resolved callee (in-project **or stand-in**, §6):

| Vertex | `of` | `parent` |
|---|---|---|
| `<call-local>/actual_in:<i>` | the bound callee formal's name (mirrors the callee's `formal_in:<i>` `of`) | call-site local id |
| `<call-local>/actual_out` / `…/actual_out:<i>` | mirrors callee's `formal_out` `of` | call-site local id |

**Why Python's naming, not the keystone's `$ret`/`argN` [call, the one non-trivial parity choice]:**
the SDK's `resolve_value` resolves a source/sink `(name, within)` against `formal_in` `of`, and
`value_candidate` translates exactly the Python markers (`<global>:`, `<capture>:`, `<return>`).
With Python's spelling the shared resolver works unchanged; Java/TS's `$ret`/`argN` would require a
Go-specific translation layer. The `<return>:<i>` multi-result form is a Go-only leaf extension of
`<return>`. Sources/sinks address `formal_in` only, so `<return>:<i>` never reaches
`value_candidate`; it does reach `body_node_kind` (which labels path hops), where `RETURN_MARKER`
is compared by equality — so an unnamed multi-result hop would be labelled `return` with name
`<return>:1`. The Go backend normalises this locally before calling the shared helper; shared
`commons/` stays untouched.

**Closures — by-reference captures, flow-insensitive binding [call, from builder check].** Go
closures capture variables by reference, and the closure may run later (`defer func(){…}()`,
`go func(){…}()`, stored callbacks), so binding a capture to the defs reaching the `FuncLit` would
miss later assignments (false refutation). Rule:

- The closure callable gets `@formal_in:<i>` `of:"<capture>:<name>"` per captured variable and
  `@formal_out:<i>` `of:"<capture>:<name>"` per captured variable it **writes** (strong or weak
  def).
- In the enclosing callable, the `FuncLit` has a **capture site**: the statement containing the
  `FuncLit` is the host; vertices `<funclit-key>/actual_in:<i>` and `<funclit-key>/actual_out:<i>`
  (`<funclit-key>` = `line:col` of the `func` keyword; `parent` = host statement's local id).
  `param_in`/`param_out` connect them to the closure's capture formals. No `summary` edges at a
  capture site (it is not a call; flow through the closure is via its own body).
- **Flow-insensitive wiring for captured variables** (port wiring, `prov:["reaching-defs"]`):
  *every* def of a captured variable anywhere in the enclosing callable → the capture site's
  `actual_in`; the capture site's `actual_out` → *every* use of that variable in the enclosing
  callable. This over-approximates timing (covers defer, goroutines and callbacks alike) and is the
  same weak-update posture as the rest of the design.
- Calls *of* the closure value (`f := func(){…}; f()`) resolve to the closure callable via VTA at
  L4 and get ordinary call-site vertices for its declared parameters.

Go specifics:
- **Variadic** `f(xs ...T)`: excess actuals collapse onto the last formal (weak, as Java).
- **Multi-value call as args** `f(g())`: each `g` result binds to the corresponding `f` formal.
- **Method value / expression** (`f := s.M; f(x)`; `T.M(s, x)`): resolved callee is `M`; receiver
  binds `formal_in:0`.
- **Global state as extra parameters** (keystone L4 rule): globals read/written are computed in the
  summary pass and propagated up the call graph, so a caller's `formal_in`/`formal_out` set includes
  globals its callees touch (Python precedent).
- **Global filter — only globals that can carry data [call, from builder check].** Python emits
  546k `<global>:` formals on one real app; transitive propagation is quadratic at cockroach scale.
  A package-level variable is modelled as an extra parameter only if it is **mutable in a way that
  can carry a value**: it has at least one write (strong or weak def, incl. a method call through it
  or `&v` escaping) in any project function body including `init`, **or** its initializer contains a
  call (e.g. `os.Getenv`). A global initialized only from literals/constants and never written
  cannot carry taint, so dropping it removes no flow. Constants and functions are never globals.
  This filter is sound under the over-approximation rule; output size is measured in the eval (§8)
  and a size budget set from the first measurement.

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
  @formal_out[:i]` (`var` `<return>`/name), statement writing a by-ref param/global →
  corresponding `@formal_out`; `host stmt → <call>/actual_in:i` (`var` = arg spelling or `argN` if
  not an access path); `<call>/actual_out[:i] → host stmt` (`var` = the LHS base it defines). **This
  wiring is what the SDK's `_ports_carry_dependence` probe checks; without it taint certifies false
  refutations.**
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
`x/tools/go/callgraph/vta` over `go/ssa` (built with `ssautil.AllPackages` from the same
`go/packages` load, `ssa.InstantiateGenerics`), seeded with a CHA call graph
(`callgraph/cha`) — the construction guide's recommended Go oracle.

- **L2 refinement:** `call_graph` gains VTA-derived edges for interface/func-value call sites, `prov`
  `["vta"]`, merged with existing `["go/types"]` edges via `MergeEdges` (union prov, sum weight).
  This is additive (monotone). Body `callee` stays a single id (the static callee — the interface
  method for an interface call) — the schema has one slot. Multiple concrete targets are carried by
  `call_graph` and, at L4, by one `param_in`/`param_out` set **per target** from the same
  `actual_in` vertices (the actual vertices are per call site, not per target).
- **Interface methods are already callable nodes** (bodiless, in the interface type's
  `callables{}` — `collectInterfaceMethods`), so an interface call's static `callee` resolves today.
  At L4 a bodiless interface method gets `formal_in`/`formal_out` vertices (so the call site's
  `actual_*` have a target for the static edge) and a **pass-through summary** (like a stand-in, §6),
  so a call whose VTA target set is empty still does not drop flow. When VTA finds concrete targets,
  `param_in`/`param_out` go to each concrete target **and** to the interface method; the
  call-site `summary` edges are the union over targets.
- **SSA/VTA scope [call, from builder check]:** build SSA with `ssautil.AllPackages` over the
  loaded packages (dependency SSA is needed for types and wrappers), but give VTA a function set of
  **project functions only** (the SSA functions whose position lies in a project file, plus their
  anonymous functions), seeded with a CHA graph restricted the same way. VTA needs no `main`/roots,
  so libraries work. Library callbacks are therefore not discovered (§6, as intended) and cost stays
  bounded by project size. Packages that fail to type-check get no SSA (`AllPackages` yields nil);
  see §4.12 degradation.
- **SSA → `can://` mapping [call]:** map an SSA function to a callable by source position:
  `fn.Origin()` for generic instantiations; synthetic wrappers (bound `$bound`, thunk `$thunk`,
  promoted-method and pointer wrappers from embedding) unwrap to the method they wrap; anonymous
  functions (`f$1`) map to the L1 closure whose `FuncLit` position equals `fn.Pos()` (L1 closure
  naming `closure_N` stays; the position is the join key). A target that maps to nothing in-project
  is an external stand-in (§6) if it has a package path outside the project, else dropped with a
  debug log. Build an `ast` position → callable index once (also fixes the parked per-site AST walk).
- **VTA runs only at `-a 4`** (and `--emit neo4j`, which is `-a 4`): SSA construction is the
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
- `global_reads`, `global_writes`, `mutated_params` (written through, from the weak-def rule),
  propagated up the SCC DAG.
- No hammock regions (neither sibling built them; recorded as a known simplification).
- Stand-ins (§6) contribute their modelled summary (default all-inputs → all-outputs).
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

### 4.11 Neo4j projection [call]

Fill the reserved overlay, no contract renames:

- `:GoBodyNode` for every body node incl. synthetic vertices; props add `of` → **`var`** and
  `parent` → **`call_node`** (global id) — Python's projection names, which the SDK's Cypher reads
  (`projection` includes `n.var`). Keep `kind`, `is_goroutine`, `is_deferred`, `span_json`; add the
  flat span props siblings use if the SDK backend needs them (builder confirms against
  `PyNeo4jBackend`).
- `GO_HAS_BODY_NODE` callable → every body node (incl. synthetic).
- `GO_CFG_NEXT {kind}` `_k=kind`; `GO_CDG`; `GO_DDG {var, prov}` **`_k=var|prov`** (fix the
  current `var`-only discriminant); `GO_SUMMARY`; `GO_PARAM_IN {var}` / `GO_PARAM_OUT {var}`.
- Stand-ins (§6) project onto the existing `:GoExternal:GoSymbol:GoCanNode` ghost (same
  `can://<app>/@external/…` id, so today's call-graph ghosts and stand-ins are one node), gaining
  `signature`/`module` props and `GO_HAS_BODY_NODE` to their `formal_*` vertices. Their vertex ids
  are `<external-id>@formal_in:<i>`, so the SDK's `under_callable` id-prefix rule covers them.
  `RelHasBodyNode.From` widens to `{GoCallable, GoExternal}` in `schema.go`.
- Flip `Reserved:false`; conformance test must see every declared relationship emitted on the
  fixture; cross-projection gate: Neo4j counts == JSON counts modulo `HAS_*`.
- `--emit neo4j` now runs `-a 4`.

### 4.12 CLI and envelope [call]

- `-a 3`, `-a 4` accepted; `--graph-field-depth <k>` (default 3, error below `-a 3`, error with
  `--emit neo4j`).
- Envelope: emit `k_limit` at L3+ (Python precedent; harmless superset of the keystone's "at L4").
- `max_level` = requested level.
- `--strict`: fail non-zero when SSA/VTA construction fails at `-a 4` (default: degrade — keep L3,
  derived vertices and param edges from `go/types` dispatch, log a warning to stderr, and record
  the `degraded` envelope marker, §4.15), Java precedent.
- `-j N`: parallelize L3 per callable and L4 per SCC wavefront if implemented; collect then sort by
  id; never mint ids or emit inside a worker. Byte-identical to `-j 1`.

### 4.13 Caps [call]

No hard node/time caps (neither sibling has them; a cap silently drops edges ⇒ false refutation).
Termination comes from k-limiting and finite summary domains. Memory: build L3 per callable and
discard ASTs-derived scratch after emission; SSA program is shared and freed after L4.

### 4.14 L2 external calls — fixing the current gap (Phase 0) [user: fix it; details are calls]

Today the L2 resolver resolves library callees (`internal/semantic_analysis/call_graph.go:105-131`)
and then drops them: no `call_graph` edge, and the emitter leaves the call node's `callee` `null`
(`v2emit/emit.go:207-216`). Python and TS emit these; Java emits them behind `--external-calls`
(always under `--emit neo4j`). Go fixes this at L2:

- **`application.external_symbols{}`** (the Python/Java name) — one entry per distinct external
  callee: `id` (`can://<app>/@external/<import-path>/<sig>`, §6), `kind:"external"`, `name`,
  `module` (import path), `signature`. Parameters/return type are added here too (cheap from
  `go/types`; span-less `{name, type, is_variadic?}`); the L4 `body{}` vertices come in Phase 2.
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
  `callee` on library calls, a new `external_symbols` map).
- Neo4j: the existing `:GoExternal` ghost path in `project.go` (`callEndpoint`) starts receiving
  rows; `GO_RESOLVES_TO` extends to external callees. `GO_CALLS` endpoint set already allows
  `GoExternal`.
- Gate update: the L2 fixture gate (`gate_l2_test.go`) currently asserts external callees stay
  `null`; it changes to assert the external id plus a matching `external_symbols` entry, and no
  dangling endpoint.

### 4.15 Degradation marker [user]

When `-a 4` cannot build what L4 needs — SSA/VTA construction fails, or some packages failed to
type-check and got no SSA — the analyzer still emits (L3 + derived vertices + `go/types` dispatch)
but marks the envelope:

```json
"degraded": [{"code": "no_vta", "detail": "...", "packages": ["example.com/x/pkg"]}]
```

- Envelope leaf, sibling of `max_level`; **absent** when nothing degraded (absence = no fact).
  Codes (open vocabulary, plain strings): `no_ssa` (SSA build failed entirely), `no_vta` (VTA
  failed), `partial_ssa` (listed packages had type errors / no SSA), `partial_types` (packages with
  `go/packages` errors at any level). `max_level` still states the requested level.
- Projected to Neo4j as a `degraded_json` property on `:GoApplication`.
- `--strict` turns any degradation into a non-zero exit.
- **SDK:** the Go backends read it at attach. When present, `taint()` returns **no `exhausted`
  pairs** and adds an `unresolved` diagnostic naming the degradation (`complete=False`), so a
  "no flow" certificate is never issued over a degraded graph; witnesses (`paths`) are still
  returned. Same rule for `paths_between`-style refutations. Implemented in the Go backend (the
  diagnostic goes into the `blocked` ledger the walk already returns, which `taint_verdict` honours —
  shared `commons/` unchanged).
- Go-only leaf now; candidate for the keystone alongside the cross-language taint revisit (§11).

## 5. Flow coverage map (user goal 4 → mechanism)

| Flow | Mechanism | Level |
|---|---|---|
| (1) through calls | formal/actual vertices, `param_in/out`, `summary`, port wiring | L4 |
| (3) interface / func-value dispatch | VTA targets → per-target `param_in/out` | L4 |
| (2) fields & pointers | k-limited access paths; weak-def through reference args; type-based may-alias `points-to` delta | L3 + L4 |
| closures | captures as `<capture>:` formals; nested callable's own SDG; capture bound at the `FuncLit` statement as `actual_in` (Python precedent) | L4 |
| goroutines | `go f(x)` is a call for data purposes (vertices + edges); no interleaving | L4 |
| channels | `ch[*]` container path; cross-goroutine via args/captures/fields | L3 + L4 |
| library calls | stand-ins with default pass-through summary | L4 |

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
  `@formal_out[:i]` vertices. The map itself lands at L2 in Phase 0 (§4.14); Phase 2 adds only
  `body`. Reachable by containment from `application` ⇒ keystone-legal.
- **Id:** `can://<app>/@external/<import-path>/<sig>` where `<sig>` is the existing
  `calleeSignatureOf` canonical form minus the package path (`DB.Query`, `Sprintf`). Language-neutral
  `@external` per Python/TS/Java.
- **Vertices:** `formal_in` per receiver+param (`of` = real name), `formal_out` per result
  (`<return>` / `<return>:<i>`), plus by-ref `formal_out` for each pointer/slice/map/interface param
  and pointer receiver (library may write through them — `json.Unmarshal(data, &v)`, `buf.Write`).
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
  `strconv.Atoi: [] ` (no flow), `strings.TrimSpace: [[0,"<return>"]]`. Starts empty or
  near-empty; no catalogue of sources/sinks — that stays the caller's (SDK rule).
- **Not modelled:** callbacks invoked by library code (`sort.Slice(xs, less)`,
  `http.HandleFunc(path, h)`): the function-value argument flows in, but the library calling it back
  is not an edge. Taint *starting* in a handler is unaffected (sources are the handler's own
  params). Recorded limitation.
- **SDK addressing:** `("query", "database/sql.DB.Query")` resolves through the Go backend's
  `resolve_callable` (domain = analysed callables ∪ stand-ins) then `resolve_value` over the stand-in
  body's `formal_in` `of`. Shared `commons/` code is untouched.

## 7. SDK (python-sdk) [user: in scope; Claude may write it]

Build on python-sdk PR #434 (L1/L2 Go support: models, ABC, local + Neo4j backends, facade, factory;
unmerged as of 2026-10-23), fixing it first against the v2-only analyzer. `add-go-support` is obsolete.

- **Models** `cldk/models/go/`: `extra="ignore"` (PR #434's choice, matching TS #386) mirror of Go v2 (module/type/field/callable/body
  node/edges/external symbols), mirroring how `cldk/models/python` and `java` model L3/L4.
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

**Analyzer gates** (each level gated before advancing — `codeanalyzer-backend` HARD-GATE):
CFG gate, dominance gate (exact CDG), PDG-slice gate (exact set), DFG gates, L4 gates (no dangling;
arity; summary exists for known transitive flow; `points-to` ⊇ adds-only; ports carry dependence),
determinism (`-j 1` vs `-j N`, two runs), monotonicity (`-a 1..4` superset via a test like
Python's `test_v2_superset.py`), cross-projection (Neo4j vs JSON), Neo4j conformance, SDK model
validation of every level's output.

**SDK gates:** slice gate (exact set on a `can://…@line:col` criterion); taint gate (known
source→sink found; sanitizer variant reported cut, not dropped; contiguous witness); a pair with no
flow lands in `exhausted`; context-sensitivity only as far as summaries support; JSON and Neo4j
backends agree; e2e level matrix.

**"Done"** (user-confirmed intent, designer-worded): through `CLDK.go(...)`, on the HTTP→SQL
fixture, `taint()` finds the flow from a handler's request parameter to `database/sql.DB.Query`'s
`query` through an interface call; a callable sanitizer cuts it; an unrelated pair is `exhausted`;
identical answers from the JSON and Neo4j backends.

**Real-world evaluation** (non-gating metrics + crash/determinism/time gates): the prior eval set —
cobra (small), dae, openbao, cockroach (scale) — plus listmonk and memos (HTTP → SQL; memos through
the `store.Driver` interface) and nats-server (concurrency / CFG stress) [user, 2026-10-23]. dae and
cockroach need their code-generation step (bpf2go, protobuf) before analysis.  Record per app: wall time and peak RSS at `-a 3`/`-a 4`, body/edge counts, % call
sites with resolved targets (go/types vs VTA), % externals with stand-ins, determinism. Gate: all
four complete `-a 4` without crash and deterministically; no hard time budget, but timings are
reported and a regression budget is set after the first measurement.

## 9. Phasing and release

| Phase | Repo | Content | Release |

> **Phases 0 and 1 ship together in one PR / one release** [user, 2026-10-23]; they stay separate rows for traceability.

|---|---|---|---|
| 0 | analyzer | id grammar `can://<app>/go/…`; **L2 external calls (§4.14)**; L1 co-positioned call keys; `is_deferred`; `typeKind` alias/defined; `GO_DDG` `_k`; call-site position index (perf) | analyzer next **breaking** version (wheel is at 0.1.0 with no local tags — builder confirms the released version and bumps the major, or the minor if still 0.x) |
| 1 | analyzer | L3: body statements, CFG (+defer chain, panic), CDG, syntactic DDG, CLI `-a 3`/`--graph-field-depth`, Neo4j CFG/CDG/DDG, gates | minor |
| 2 | analyzer | L4: SSA+VTA, `degraded` marker (§4.15), external stand-ins (`external_symbols`), vertices, param edges, summaries, port wiring, type-based alias delta, Neo4j L4 overlay, `--emit neo4j` at L4, real-app eval | minor |
| 3 | python-sdk | Go v2 models, facade, local + Neo4j backends, L1–L4 accessors, taint e2e | minor; pins the analyzer only after Phase 2 is released |

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
- Context-sensitive (HRB two-phase) taint traversal in the SDK.
- Library callback modelling (`sort.Slice`, `http.HandleFunc`).
- Promote `callable.source_file` to the keystone (existing deferral).
- Struct-tag modelling (existing deferral).

## 12. Risks

| Risk | Mitigation |
|---|---|
| SSA/VTA cost on cockroach-scale | SSA only at `-a 4`; measure in Phase 2 eval; `--strict` / degrade path |
| False refutations from a missed construct | over-approximation posture; "ports carry dependence" probe; per-construct fixtures; unresolved-call intra-caller pass-through |
| Over-approximation noise (defer chain, pass-through, CDG in summaries) | acceptable by anti-goal; library models file narrows hot spots |
| Breaking id change disrupts users | ship alone in Phase 0, before SDK pin; no released SDK consumes Go output |
| Determinism with parallel L3 / map iteration | collect-then-sort, ids minted outside workers, `-j` gate |
| Divergence between JSON and Neo4j | cross-projection gate; shared projector over the same v2 tree |
