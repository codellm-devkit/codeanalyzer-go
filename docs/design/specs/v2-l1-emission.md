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
| `application.id` | `can://go/<app>`, `<app>` = **`--app-name`** (default: input dir base name), per CLI contract |
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
| `source_file` | **optional**; emitted **only when** the callable's declaring file differs from the module it is nested under (a method whose receiver type lives in another file). Value = declaring file path relative to input root, same shape as a `symbol_table` key |

#### Cross-file methods — the `source_file` amendment (design loop, 2026-09-29)

The eval on real Go apps (dae/openbao/cockroach, ~3292 empty spans; cobra clean)
exposed a datamodel collision: v2 nests a method under its **receiver type**
(containment-by-receiver, § `type` above), but Go lets a method be declared in a
*different file* than its type. The keystone and the **Java** reference both fix
`span.bytes` as offsets into the **nesting module's** `source` (Java's SDK slices a
node against the `JCompilationUnit` it is hydrated under). Java can't produce the
conflict — its methods are lexically inside their type's file — so there was no
precedent; this is a genuine Go divergence brought to the user.

Decision (Go-only for now; parity deferred — see below):
- **Containment unchanged.** The method node stays under its receiver type's
  `callables{}`. The tree still reflects containment-by-receiver.
- **New leaf-level optional field `callable.source_file`**, present only when the
  declaring file ≠ the nesting module. Parity-safe (an additive leaf field, never a
  rename of shared vocabulary).
- **`span.bytes` for a cross-file method index the `source_file` module's `source`**,
  not the nesting module's.
- **Text-recovery rule (the SDK contract):** a node's text =
  `symbol_table[source_file].source[span.bytes]` when `source_file` is present, else
  `<nesting module>.source[span.bytes]`. The declaring file is always another module
  already in `symbol_table` (Go analyzes every `.go` file), so its `source` is present
  to slice — the rule is fully data-driven, no extra I/O.

**Deferred — cross-language parity.** C# partial classes, Rust `impl` blocks, Ruby
reopened classes, and TS declaration merging all hit the same
member-declared-apart-from-its-type shape. `source_file` is scoped to Go here; when a
second language needs it, promote the same field + text-recovery rule into the shared
keystone vocabulary in a design pass (the parity clause forbids a divergent second
spelling). Recorded so the promotion is a known follow-up, not a rediscovery.

### `call` (body node, L1)
| Decision | Choice |
| --- | --- |
| Markers | typed `is_goroutine`, `is_deferred`; **drop** `is_constructor_call` |
| `callee` | sanctioned `null → id` refinement slot (null at L1, backfilled L2) |

### Edges (Group C, settled here for L2)
`call_graph: [{src, dst, prov, weight}]` at application scope (rename from v1
`{source, target, type, provenance}`).

### CLI conformance (cli-contract.md)
The current `cango` CLI is non-conformant. Bring it to the contract as part of this
train (the SDK facade depends on a uniform flag surface across backends):
- **`--emit <json|neo4j|schema>`** — output target; `json` default. `neo4j`/`schema`
  return "not yet implemented" (non-zero) until the Neo4j child lands, never silent
  fallback.
- **`--app-name <name>`** — `application.id`/`:Application` anchor; default = input dir
  base name. Precedence: explicit flag > env > default.
- **`--neo4j-uri` / `--neo4j-user` / `--neo4j-password` / `--neo4j-database`** — accepted
  and validated now; consumed by the Neo4j child.
- **`-j, --jobs <n>`** — worker parallelism (default CPU cores); output byte-identical
  across `-j` values.
- Flag validation: unknown/unimplemented value → non-zero exit + clear message. stdout =
  data channel only; all diagnostics to stderr.

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
- **`source_file` SDK follow-up (Train 1, SDK rung):** python-sdk's Go model must add
  the optional `source_file` field and its `slice()`/`get_method_body` must resolve
  text against `symbol_table[source_file].source` when present (else the nesting
  module). Deferred to the SDK rung — the analyzer ships the field first; the SDK
  catches up on its own clock (analyzer release must precede the SDK rung regardless).

## Decomposition (tracking shape — to be confirmed with user)

Recommended: **parent (in `codellm-devkit/.github`) + one sub-issue per PR**, because the
work spans two repos on their own clocks and needs a coordination record:
1. codeanalyzer-go: CLI contract conformance (`--emit`, `--app-name`, `--neo4j-*`, `-j`)
2. codeanalyzer-go: L1 emission (tree, source, ids, body-with-call-nodes)
3. codeanalyzer-go: L2 emission (call_graph rename @ app scope)
4. codeanalyzer-go: Neo4j projection (implements the deferred `--emit neo4j`/`schema`)
5. python-sdk: Go v2 model remap (accessors stable)
6. release + pin (once both cut)

Children filed just-in-time as picked up; this spec records the full plan.
