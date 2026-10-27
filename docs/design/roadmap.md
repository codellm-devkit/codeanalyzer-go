# CLDK Go analyzer — roadmap

> Local staging copy. Canonical home is `codellm-devkit/.github → docs/design/roadmap.md`;
> port there when that repo is available. One roadmap, amended in place — never a second file.

## Initiative: v1 → v2 schema migration (codeanalyzer-go + python-sdk)

A schema **major**: move the Go analyzer off the v1 keystone
(`{symbol_table, call_graph}`, flat `start_line`/`end_line`, per-callable `code`, `is_*`
booleans, `signature` as id) onto the v2 canonical schema (additive tree, `can://` ids,
`span` with byte offsets, `source` per module, typed edge lists). The SDK moves in
lockstep as a coordinated major release. **Reach for this train: L1 + L2 to parity.**
L3 shipped in release A (below); L4 is implemented on `feat/l4-sdg` (release B, below).

Golden rule carried from `schema-migration.md`: keep everything that *computes* facts
(parser, resolver, call-graph builder); replace only what *serializes* them.

## Starting now (exactly one)

- **Group A + L1 emission** — the identity, span, and `body`-with-call-nodes decisions.
  This is the keystone rung; everything below reuses its vocabulary. → `designing-cldk-changes`.

Everything else below has no issue filed yet, by design.

## Collision groups (the reason this is planning, not design)

Contract decisions that share v2 vocabulary — **each group is one design session**, even
when its members ship in different release trains. Coining a shared term twice coins it
wrong permanently (parity clause).

- **A — Identity & positioning.** `application.id` (`can://<lang>/<app>`), the `can://`
  durable-id grammar (≥ callable), the `@line:col` ordinal ids (< callable), and `span`
  with byte offsets. **L3/L4 in a later train reuse this id/span shape verbatim** — so it
  must be settled now, in one session, not re-decided per level. This is the group the
  gate exists to protect.
- **B — Node shape.** `type.kind` collapsing the `is_interface`/`is_enum`/… booleans;
  structured `decorators[]` (`{name,args,span}`) replacing flat annotation strings;
  generalized `error_channel[]`; drop per-callable `code`, add `source` once per module.
- **C — Edge shape.** Rename edge keys `source/target/type` → `src/dst/prov`; `call_graph`
  as a list at application scope. **Every future edge family (cfg/cdg/ddg/param_*) inherits
  this record shape** — coin `{src, dst, prov, weight}` correctly now.
- **D — Neo4j projection.** Node/edge family names must match A/B/C exactly; it is a
  relabel of the same tree, always full-depth.

## Dependency order (DAG)

```
A (identity + span) ─┬─► L1 emission (tree, source, ids, body-with-call-nodes)
B (node shape)      ─┤
C (edge shape)      ─┴─► L2 emission (call_graph rename @ application scope)
                          │
A/B/C ─► D (Neo4j relabel) ─► SDK v2 model remap ─► pin SDK→analyzer (once BOTH cut)
                                                       │
                          L3 (release A) ─► L4 (release B) ┘   reuse A & C unchanged
```

## Release trains

- **Train 1 — v2.0.0 (coordinated major, lockstep).** Analyzer L1+L2 emission rewrite +
  Neo4j relabel; SDK v2 model remap keeping every public accessor name/return type
  identical (two-layer model). **Ordering constraint (first-class on the parent):** pin the
  analyzer version in the SDK *only once both are released* — until then the SDK's old
  models won't parse v2 output. Superset gate: v2 output must contain every fact v1 did,
  modulo the sanctioned drops (`code`, `is_*` booleans folded into `kind`).
- **Release A — v2 with L3 (`-a 3`).** L3 is no longer deferred. Ships the new `can://` id
  grammar, L2 library calls (`external_symbols`, `@external` call-graph endpoints), and
  per-callable L3 CFG/CDG/DDG (AST-only, intraprocedural, `--graph-field-depth` k-limit),
  with the Neo4j `GO_CFG_NEXT`/`GO_CDG`/`GO_DDG` projection.
  Real-app evaluation: `docs/design/notes/l3-eval-2026-10.md`. It and the live Neo4j run
  found two issues, both fixed before release: L2 call-graph dedup on repeated `<pkg>.init`
  signatures (nondeterministic, dropped edges) and the Neo4j `--eager` purge failing on
  Neo4j 5.x. The release tag is the user's decision.
- **Release B — L4 (`-a 4`, SDG).** Implemented on `feat/l4-sdg`
  (spec `docs/design/specs/v2-l3-l4-dataflow.md` Phase 2). It adds:
  - VTA dispatch over `go/ssa`;
  - formal/actual vertices, `param_in`/`param_out`, call-site `summary` edges and port-wiring
    `ddg`;
  - library stand-ins with `--library-models`;
  - a type-based may-alias oracle (`points-to` `ddg`);
  - the `degraded` envelope marker and `--strict`;
  - the Neo4j `GO_PARAM_IN`/`GO_PARAM_OUT`/`GO_SUMMARY` overlay, with `--emit neo4j` at L4.

  It reuses the group A and C vocabulary; every L4 fact is additive (`-a 3` ⊆ `-a 4`). The
  real-app evaluation is in `docs/design/notes/l3-dataflow-metrics.md` § L4 and
  `l3-eval-2026-10.md`. Kubernetes `-a 4` completes deterministically, and memory is reported
  per app, not gated (D3). Follow-ups:
  - #30: global → global summaries for an HRB walk;
  - #31: `GO_EMBEDS`/`GO_SATISFIES`;
  - range-over-func (D8);
  - library callbacks;
  - Andersen points-to;
  - export-data loading.

  The python-sdk Go L3/L4 surface (spec §7) is the next rung.

## Not now (considered, deliberately excluded)

- **Andersen points-to over `go/ssa`** — a precision upgrade replacing L4's type-based may-alias
  oracle behind the same interface (`prov` stays `points-to`); not this train.
- **`--materialize-expressions` / `--materialize-basic-blocks`** — optional body node kinds;
  out of scope for the parity migration.
- **New framework detection** — orthogonal enrichment axis
  (provenance-merged evidence), not a schema level; does not belong on the migration train.
