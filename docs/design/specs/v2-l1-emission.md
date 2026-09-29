# Spec: v2 schema migration — Group A + L1 emission (codeanalyzer-go)

> **Cross-repo spec.** Canonical home is `codellm-devkit/.github → docs/design/specs/`,
> beside the coordinating parent issue. Staged locally until that repo is available.
> This is the design transcript — the committed provenance of the design loop, linked
> (not pasted) from the parent issue.

## Contract-Impact Triage

**Does this change schema v2 output?** Yes — it is the schema-major migration that
introduces the v2 shape for Go: `application.id`, the `can://` id grammar, `@line:col`
ordinal ids (defined here, populated by L3/L4), `span` with UTF-8 byte offsets, the
additive tree, `source` per module, and `body{}` with `call` nodes.

| Change type | Analyzers | SDKs | Docs |
| --- | --- | --- | --- |
| Schema v2 migration | **codeanalyzer-go** (emission rewrite) | **python-sdk** (Go model remap, lockstep) | CLAUDE.md, this spec |

Arrived from `planning-cldk-work` with collision **Group A** known (identity + span);
its vocabulary is designed once here and reused verbatim by the deferred L3/L4 train.

## Design-loop decisions (the transcript)

Every decision below was made with the user, node by node in spine order. Full copy in
`codeanalyzer-go/CLAUDE.md` § Schema decisions. Anchored on the keystone
(`canonical-schema.md`) and the completed **TypeScript** v2 migration (closest structural
analog: module functions + interfaces + structural types).

### Root envelope (settled spine, kept as-is)
`{ schema_version:"2.0.0", language:"go", max_level, analyzer:{name,version},
application:{ id, kind:"application", symbol_table, call_graph, param_in, param_out } }`.

### Identity & positioning (Group A)
| Decision | Choice |
| --- | --- |
| `application.id` | `can://go/<app>`, `<app>` = slug from **go.mod module path** (no flag) |
| Callable signature | existing `signatureOf()` as last id segment; receiver folded in for methods |
| `span.bytes` | **UTF-8 byte offsets** into `module.source` |

### `module`
`kind:"module"`, `package`, `source` (whole file once), `imports[]`, `types{}`,
`functions{}`, `content_hash`. Drops per-callable `code` (sliced from `source`).

### `type`
| Decision | Choice |
| --- | --- |
| `kind` | `struct \| interface \| alias \| defined` (collapses v1 `is_interface`) |
| Method placement | resolved to receiver type's `callables{}`; receiver as a field |
| `base_types[]` | embedded type ids (struct/interface embedding) |
| `interfaces[]` | computed structural satisfaction (method-set matching) |

### `callable`
| Decision | Choice |
| --- | --- |
| `error_channel[]` | from `error`-typed returns (kept in `return_type` too) |
| Closures | nested `callables{}` on enclosing callable (replaces `InnerCallables`) |

### `call` (body node, L1)
| Decision | Choice |
| --- | --- |
| Markers | typed `is_goroutine`, `is_deferred`; **drop** `is_constructor_call` |
| `callee` | sanctioned `null → id` refinement slot (null at L1, backfilled L2) |

### Edges (Group C, settled here for L2)
`call_graph: [{src, dst, prov, weight}]` at application scope (rename from v1
`{source, target, type, provenance}`).

## Release plan

- **Train 1 — v2.0.0 (coordinated major, lockstep).** codeanalyzer-go emission rewrite
  (L1 tree/source/ids/body-with-calls, then L2 call_graph) + Neo4j relabel; python-sdk
  Go model remap to v2 keeping every public accessor name/return type identical.
- **Ordering constraint (first-class):** pin the analyzer version in the SDK **only once
  both are released**. Until then the SDK's old Go models won't parse v2 output.
- **Superset gate:** v2 output must contain every fact v1 did, modulo sanctioned drops
  (`code`, `is_*` booleans folded into `kind`, `is_constructor_call`).
- **Deferred (Train 2+):** L3 (CFG/CDG/DDG), L4 (SDG). Reuse Group A ids/spans + Group C
  edge shape unchanged; no new schema-major coordination.

## Decomposition (tracking shape — to be confirmed with user)

Recommended: **parent (in `codellm-devkit/.github`) + one sub-issue per PR**, because the
work spans two repos on their own clocks and needs a coordination record:
1. codeanalyzer-go: L1 emission (tree, source, ids, body-with-call-nodes)
2. codeanalyzer-go: L2 emission (call_graph rename @ app scope)
3. codeanalyzer-go: Neo4j relabel
4. python-sdk: Go v2 model remap (accessors stable)
5. release + pin (once both cut)

Children filed just-in-time as picked up; this spec records the full plan.
