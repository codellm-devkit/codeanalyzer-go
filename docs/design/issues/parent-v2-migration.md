<!--
DRAFT parent issue body — file on codellm-devkit/.github with the feature_request form.
  gh issue create --repo codellm-devkit/.github \
    --title "Migrate `codeanalyzer-go` and its `python-sdk` Go models to schema v2" \
    --label enhancement --body-file this-file
Sections below are the feature_request.md form, verbatim order, none added/removed.
Add to Project 1 board after filing. Children filed just-in-time, not now.
-->

**Is your feature request related to a problem? Please describe.**

`codeanalyzer-go` emits the v1 schema (`internal/schema/schema.go`): two-key
`{symbol_table, call_graph}`, flat `start_line`/`end_line`, per-callable `code`,
`is_*` booleans, `signature` as id. Every sibling analyzer has moved to the v2
keystone (`can://` ids, additive tree, `span` with byte offsets, `source` per module,
`{src,dst,prov}` edges). Go is the last on v1, so `CLDK(language="go")` cannot share
the one-model SDK surface. This is a coordinated schema major across two repos:
`codeanalyzer-go` (emission rewrite) and `python-sdk` (Go model remap), released in
lockstep. Reach for this train: L1 + L2 to parity; L3/L4 deferred.

**Describe the solution you'd like.**

- [ ] `codeanalyzer-go`: L1 emission — additive tree, `source` per module, `can://` ids, `body{}` with `call` nodes
- [ ] `codeanalyzer-go`: L2 emission — `call_graph: [{src,dst,prov,weight}]` at application scope
- [ ] `codeanalyzer-go`: Neo4j projection relabelled to v2 node/edge families
- [ ] `python-sdk`: Go v2 model remap, every public accessor name/return type unchanged
- [ ] Release v2.0.0 and pin analyzer in SDK **only once both are cut**
- [ ] Superset gate green: v2 output contains every v1 fact modulo sanctioned drops

**Describe alternatives you've considered.**

Does NOT build L3 (CFG/CDG/DDG) or L4 (SDG). Those are a later train reusing this
train's `can://` ids and edge shape unchanged. Does NOT add framework detection.

**Additional context.**

- Design transcript: `docs/design/specs/v2-l1-emission.md` (linked, not pasted).
- Group A vocabulary (`can://` ids, `span.bytes` UTF-8) is reused verbatim by L3/L4 — coined once here per the parity clause.
- Sanctioned drops: `code` (slice from `source`), `is_*` booleans (folded into `kind`), `is_constructor_call` (Go has no constructors).
- Ordering risk: SDK's v1 Go models will not parse v2 output; do not pin the new major prematurely.
- go.mod-derived `<app>` slug must be deterministic across runs, else ids churn.
