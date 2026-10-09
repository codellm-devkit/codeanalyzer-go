<!--
DRAFT epic body — mirrors codellm-devkit/.github#42 (Java v2 epic) in shape.
File on codellm-devkit/.github with the `Epic` label (free-form body, NOT the
feature_request form — matching the live #42/#35 precedent in the same tracker).
  gh issue create --repo codellm-devkit/.github \
    --title "`codeanalyzer-go` → canonical schema v2 (L1 + L2)" \
    --label Epic --body-file this-file
Add to Project 1 board after filing. Sub-issues filed on codeanalyzer-go and
attached by id just-in-time. Do NOT auto-link #3 (old L3 work) — linking is manual.
-->

## Spec

https://github.com/codellm-devkit/codeanalyzer-go/blob/main/docs/design/specs/v2-l1-emission.md

## Summary

Migrate `codeanalyzer-go` from the legacy v1 output to the **canonical schema v2** (one
additive tree: `application → module → type → callable → body` with typed edge overlays)
and bring it to **analysis level 2** — L1 structural containment + L2 identity-only
`call_graph`, in both projections (`analysis.json` + Neo4j). This is a **schema-major**
change (new envelope, `can://` ids, `span` with UTF-8 byte offsets, `source` per module,
`body` call nodes, `{src,dst,prov}` edges). The `python-sdk` Go wiring lands in lockstep
behind a frozen public API. **Reach for this train: L1 + L2 to parity; L3/L4 deferred.**

Golden rule: keep everything that *computes* facts (parser, resolver, call-graph
builder); replace only what *serializes* them.

## Affected repos (from Contract-Impact Triage)

- `codeanalyzer-go` — analyzer emission (L1 + L2) + Neo4j v2 projection — backend rung
- `python-sdk` — net-new Go model wiring to v2 (`cldk/analysis/` has no `go` package today) — frontend rung
- docs (user-facing schema/levels) — later, `finishing-cldk-work`

## Design decisions

- **D1** Pure canonical v2 (drop per-callable `code`/flat `start_line`/`end_line`; recover text by slicing `module.source`)
- **D2** `application.id` = `can://go/<app>`, `<app>` = `--app-name` (default: input dir base name)
- **D3** `span.bytes` = UTF-8 byte offsets into `module.source` (Go strings are UTF-8; avoids the UTF-16/rune mismatch)
- **D4** Single type `kind` ∈ `struct|interface|alias|defined` (collapses v1 `is_interface`); methods nest under receiver type's `callables{}`
- **D5** `base_types[]` = embedded ids (explicit spine) vs `interfaces[]` = computed structural satisfaction (method-set matching)
- **D6** `error_channel[]` from `error`-typed returns; closures → nested `callables{}`; `is_constructor_call` dropped (Go has none)
- **D7** `call` body nodes carry typed `is_goroutine`/`is_deferred`; `callee` is the null→id refinement slot (null at L1, backfilled at L2)
- **D8** `source_file` on a callable when a method is declared apart from its receiver type (cross-file-method span fix)
- **Scope guard:** analyzer is a **pure graph provider** — no slicing/taint in the analyzer (those are SDK queries)

## Release plan

- Analyzer = **major** release (breaking output). L1 and L2 are independently shippable behind `--analysis-schema 2`; v1 stays the default until the SDK migrates.
- SDK = **major** release; pins the analyzer **only after** the analyzer v2 release is cut (old SDK Go wiring can't parse v2 until then — and there is no Go wiring yet, so this is net-new).
- Neo4j projection and the JSON schema move in lockstep.

## Deferred (not this train)

- **L3** (CFG/CDG/DDG) and **L4** (SDG `param_in`/`param_out`/`summary`) — a later train reusing this train's `can://` ids and `{src,dst,prov}` edge shape verbatim (Group A/C vocabulary, coined once here per the parity clause).
- Go-specific `cfg` edge kinds (`defer_resume`, `select`) — recorded when L3 is designed.
- Points-to oracle (L4 prerequisite); `--materialize-expressions`/`--materialize-basic-blocks`; framework detection.

## Definition of done (epic-level)

- Every sub-issue closed and its PR's gate green.
- Analyzer output validates against the SDK v2 Go models; the `L1 ⊆ L2` superset gate holds; parity clause holds (no renamed/repurposed shared vocabulary).
- Superset gate: v2 output carries every v1 fact modulo the sanctioned drops (`code`, `is_*` booleans folded into `kind`, `is_constructor_call`).
- `--analysis-schema 2` output is deterministic across runs (ids and edge order stable).
- SDK public API unchanged (major bump + documented semantic shifts); analyzer↔SDK versions pinned in lockstep.
- Docs / CHANGELOG updated.
- The two switch-over steps: (1) flip the default output from v1 to v2; (2) remove the v1 emitter once `python-sdk` has migrated and nothing reads v1 — both filed as their own units when due.
