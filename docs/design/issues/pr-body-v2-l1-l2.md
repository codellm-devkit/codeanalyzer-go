Migrate `codeanalyzer-go` to emit the canonical CLDK **v2** `analysis.json` for analysis levels **L1** (additive containment tree) and **L2** (call-graph refinement). v1 output is unchanged and remains the default; v2 is opt-in behind `--analysis-schema 2`.

## Motivation and Context

Closes #8
Closes #9

Part of epic codellm-devkit/.github#94. Go was the last analyzer on v1; this moves it onto the shared v2 keystone (`can://` ids, additive tree, `span` with UTF-8 byte offsets, `source` per module, `{src,dst,prov}` edges) so `CLDK(language="go")` can share the one-model SDK surface. Design transcript: `docs/design/specs/v2-l1-emission.md`.

## How Has This Been Tested?

Gates run on this commit (analyzer matrix; `testdata/multipackage` fixture):

```
# Fixture suite (forced, uncached)
$ go test -count=1 ./...
ok  cmd/codeanalyzer  ok  internal/core  ok  internal/schema/v2  ok  internal/syntactic_analysis/v2emit  (all green, exit 0)

# Determinism: -j 1 vs -j 8, v2 L2
$ cango -i testdata/multipackage --analysis-schema 2 -a 2 -j 1 -o j1/ ; ... -j 8 -o jN/
$ diff j1/analysis.json jN/analysis.json      # byte-identical -> PASS

# Monotonicity: v2 L1 ⊆ v2 L2
L1 node ids: 33; L2 node ids: 33; L1 ids missing from L2: 0
L1 call_graph: 0 edges; L2 call_graph: 9 edges   # L2 adds, never rewrites -> PASS
```

Schema-conformance gate (validate against the SDK CPG model) is **deferred**: `python-sdk` has no Go model yet (`cldk/analysis/` has no `go` package) — that is the separate SDK-wiring child of epic #94. Structural self-check passed: valid JSON, `schema_version 2.0.0`, `application.id can://go/multipackage`, edge keys exactly `src/dst/prov/weight`.

## Breaking Changes

None for existing users: v1 is still the default output. v2 is additive and opt-in behind `--analysis-schema 2`. The v2 contract itself is a new major, consumed by nothing yet (no SDK Go pin); the default flip and v1-emitter removal are later units, filed when due.

## Types of changes
- [ ] Bug fix (non-breaking change which fixes an issue)
- [x] New feature (non-breaking change which adds functionality)
- [ ] Breaking change (fix or feature that would cause existing functionality to change)
- [x] Documentation update

## Checklist
- [x] I have read the [Codellm-Devkit Documentation](https://codellm-devkit.info)
- [x] My code follows the repository's style guidelines
- [x] New and existing tests pass locally
- [x] I have added appropriate error handling
- [x] I have added or updated documentation as needed

## Additional context

- L1 computes no new facts beyond span offsets; L2 is pure refinement (signature→`can://` id via `buildSigIndex`). Parser/resolver unchanged (golden rule: replace what serializes, keep what computes).
- `span.bytes` are UTF-8 byte offsets into `module.source` (Go strings are UTF-8).
- `source_file` carried on a callable only when a method is declared apart from its receiver type.
- L3/L4 deferred to a later train; Group A ids/spans already designed for verbatim reuse.
