<!--
CHILD issue (first unit, M1) filed on codellm-devkit/codeanalyzer-go with the feature_request
form (label: enhancement). Requires maintainer write access (lamwassi is pull-only upstream).
After filing, attach as a native sub-issue of the Neo4j parent by id:
  child_id=$(gh api repos/codellm-devkit/codeanalyzer-go/issues/<this-number> --jq .id)
  gh api -X POST repos/codellm-devkit/.github/issues/<parent-number>/sub_issues -F sub_issue_id="$child_id"
Later milestones (M2–M6) are filed just-in-time the same way as each is picked up.
Title: Neo4j schema contract (schema.go) + conformance test + --emit schema
-->

**Is your feature request related to a problem? Please describe.**

The Neo4j projection (parent issue) needs a single declared source of truth for its graph
vocabulary before any projector or writer can be held to it. Without it, the emitter and the
contract drift and `--emit schema` has nothing to serialize. This is milestone **M1** of the
roadmap — the foundation every later milestone depends on.

**Describe the solution you'd like**

A new `internal/neo4j/schema.go` (sibling to `internal/syntactic_analysis/v2emit/`, off the
JSON hot path) that is the declarative contract:

- [ ] Node labels + merge keys + typed properties — `:GoApplication`, `:GoModule`, `:GoType`
  (`kind` property), `:GoCallable`, `:GoField`, `:GoBodyNode`, `:GoExternal`, and the neutral
  `:Artifact`/`:Package`/`:ConfigKey`. Shared `:GoSymbol` merge-label; `:GoCanNode` marker on
  every `can://` node.
- [ ] Relationship types + endpoints — containment (`GO_HAS_MODULE`/`GO_DECLARES`/
  `GO_HAS_METHOD`/`GO_HAS_FIELD`/`GO_HAS_BODY_NODE`), `GO_CALLS`/`GO_RESOLVES_TO`,
  `GO_EMBEDS`/`GO_SATISFIES`, `GO_IMPORTS`, the neutral repo edges, and `GO_PROVIDES`/
  `GO_UNRESOLVED_IMPORT`.
- [ ] The **reserved L3/L4 overlay** vocabulary declared now (zero rows until the levels land):
  `GO_HAS_BODY_NODE`/`GO_CFG_NEXT`(`_k=kind`)/`GO_CDG`/`GO_DDG`(`_k=var,prov`)/`GO_PARAM_IN`/
  `GO_PARAM_OUT`/`GO_SUMMARY` + body-node statement kinds.
- [ ] `_k` relationship discriminants on `GO_CFG_NEXT`, `GO_DDG`, `DECLARES_DEPENDENCY`.
- [ ] **Derived DDL** — one uniqueness constraint per distinct `(mergeLabel, key)` (no second
  list to keep in sync) + curated indexes incl. the `:GoCanNode` id range index for prefix seeks.
- [ ] `SCHEMA_VERSION = "2.0.0"` + `buildSchemaDocument()`; `--emit schema` serializes it to
  `schema.neo4j.json`.
- [ ] A **conformance-test scaffold** (`schema_test.go`) that M3 fills: the real projector may
  never emit an undeclared label / relationship / property.

Full catalog + rationale: `docs/design/Neo4j_projection/specs/neo4j-projection.md` §3.

**Gate (this PR must pass):**
- `--emit schema` writes a `schema.neo4j.json` that parses and lists every declared family.
- Derived-DDL test: exactly one constraint per distinct `(mergeLabel, key)`, no duplicates.
- `go build` + `go vet` clean; checked-in `schema.neo4j.json` regenerated and committed.

**Describe alternatives you've considered**

- *Hand-written constraint list* — rejected: derive constraints from the labels so a new label
  brings its own constraint and the two cannot drift.
- *Defer the L3/L4 overlay vocabulary* — rejected: declaring it now (empty) keeps the later
  L3/L4 train a pure projection-fill, not a graph-schema change.

**Additional context**

Parent: the Neo4j projection coordination issue under epic #94. Mirror the existing test idiom
(`TestGateL1_*`, `TestDeterminism_*` in `v2emit`) for the Neo4j gates.
