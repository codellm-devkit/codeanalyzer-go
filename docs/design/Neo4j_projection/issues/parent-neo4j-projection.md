<!--
PARENT sub-issue for the Neo4j projection, filed UNDER epic codellm-devkit/.github#94.
File on codellm-devkit/.github with the feature_request form (labels: enhancement; project
codellm-devkit/1). Requires maintainer write access (lamwassi is pull-only upstream).
After filing, attach as a native sub-issue of #94 by id:
  parent_id=$(gh api repos/codellm-devkit/.github/issues/<this-number> --jq .id)
  gh api -X POST repos/codellm-devkit/.github/issues/94/sub_issues -F sub_issue_id="$parent_id"
Then file children just-in-time on their repos and attach them to THIS issue by id.
Title: codeanalyzer-go Neo4j projection (--emit neo4j) + python-sdk GoAnalysisBackend
-->

**Is your feature request related to a problem? Please describe.**

Epic #94 brings `codeanalyzer-go` to canonical schema v2 "in both projections
(`analysis.json` + Neo4j)". The JSON projection (L1 + L2) is in flight; the **Neo4j
projection is not yet built**, and the `python-sdk` has no `go` analysis package, so a graph
database holding a Go CPG cannot be produced or queried. This issue coordinates the Neo4j
half of #94 across the two repos.

**Describe the solution you'd like**

A pure, identity-preserving **second projection** of the frozen v2 envelope into a labeled
property graph — keyed on the identical `can://` ids so JSON and graph are interchangeable —
plus the SDK consumer that reads it. Vocabulary is `GO_`/`Go`-namespaced for a polyglot
database, with the neutral `:Artifact`/`:Package`/`:ConfigKey` layer reused verbatim.

**Spec (full design + decisions — do not paste, link):**
`codeanalyzer-go/docs/design/Neo4j_projection/specs/neo4j-projection.md`
**Roadmap (milestones, commit boundaries, gates):**
`codeanalyzer-go/docs/design/Neo4j_projection/roadmap.md`

Children (filed just-in-time on their repos, attached as sub-issues of this one):

- [ ] **M1–M6 analyzer `--emit neo4j`** (`codeanalyzer-go`) — `internal/neo4j/`
  {schema, rows, project, cypher, bolt, emit}.go + CLI wiring. May be one issue with the
  milestones as a checklist, or a sub-stack per PR if reviewers prefer finer PRs.
- [ ] **M7 `GoAnalysisBackend`** (`python-sdk` / fork `lamwassi/python-sdk`) — type-centric
  Go facade + Go models + `GoBackend` union; pins the analyzer version only after the
  analyzer release is cut (lockstep).
- [ ] Docs + CHANGELOG (`finishing-cldk-work`).

**Locked design decisions** (full table in the spec):
`N="Go"`/`P="GO_"`; one `:GoType` + `kind` property; `GO_EMBEDS` vs `GO_SATISFIES`;
flattened `source_file`/`error_channel`/`is_goroutine`/`is_deferred`; narrower label set (no
decorator/attribute/variable); neutral repo layer reused verbatim; full L3/L4 overlay
vocabulary declared now, zero rows until the levels land; type-centric SDK facade.

**Describe alternatives you've considered**

- *Block the projection on L3/L4 first* — rejected: `--emit neo4j` ships now at full-depth =
  max implemented (L2) and grows additively; the reserved overlay vocabulary means L3/L4 fill
  it with no schema change.
- *Per-kind type labels (like TS)* — rejected: re-introduces at the graph layer the per-kind
  split the JSON schema deliberately collapsed into one `kind` field; breaks projection symmetry.
- *Class-centric SDK facade (reuse `get_classes`)* — rejected: Go has no class; the facade is
  type-centric (`get_types`/`get_functions`/`get_methods_of`), the SDK's first class-less surface.

**Additional context**

Scope guard: the analyzer is a pure graph **provider** of the SDG substrate — **no
`taint_flows`**; labeled reachability/slicing/source-sink are SDK Cypher queries, a later
train. Release plan (per #94): "Neo4j projection and the JSON schema move in lockstep."
