# Neo4j Projection for `codeanalyzer-go` —  Prompt

## Context & Objective

We are implementing the **Neo4j projection backend** for `codeanalyzer-go`. This is the
Go-language sibling of two existing, battle-tested implementations: `codeanalyzer-typescript`
(`src/build/neo4j/*`) and `codeanalyzer-python` (`codeanalyzer/neo4j/*`). Our implementation
must preserve **vocabulary parity** with those backends while adopting a `GO_`-prefixed,
`GoCanNode`-anchored namespace for the polyglot database.

## What the Neo4j backend is

The Neo4j backend is a **pure, identity-preserving, second projection** of the analyzer's
canonical **schema-v2 CPG envelope** — the *same* envelope the JSON path emits verbatim to
`analysis.json`. Both projections key on the *same* `can://` ids (and global-ordinal ids for
body-level nodes), so JSON and graph are interchangeable and can never diverge. The projection
is **lossless up to re-expression**: a JSON field may be re-expressed as an edge or a flattened
scalar property, but never silently dropped.

## Provider/client boundary

The analyzer is strictly a graph **provider**: it emits the **SDG substrate** only (containment
tree, call graph, CFG/CDG/DDG, interprocedural PARAM_IN/PARAM_OUT, inheritance, imports/exports,
and the language-neutral repository-artifact layer). There is **no `taint_flows` section** —
labeled reachability, slicing, and source/sink semantics belong to the CLDK frontend SDK, as
Cypher traversal queries.

## Three design invariants (non-negotiable)

1. **Two-projection agreement** — graph nodes carry the identical `can://` ids as the JSON tree;
   no id recomposition, built once upstream.
2. **Monotone / additive-safe writes** — the default incremental (lazy) Bolt push is purely
   MERGE-upsert and never deletes; destructive reconciliation is opt-in under `--eager` only.
3. **Prefix-scoped destructive statements** — every destructive statement is scoped on the
   `can://<app>/` id prefix, anchored on the `GoCanNode` index label, so one application can never
   corrupt another's subgraph in a shared, multi-language database.

## Architecture to replicate (pure projection, dumb writers)

Mirror the proven module split from the TS/Python references:

- **Declarative schema contract** (`schema.go`) as the single in-repo source of truth: node labels
  + merge keys + typed properties, relationship types + endpoints, **derived** DDL (one uniqueness
  constraint per distinct `(mergeLabel, key)`), curated indexes, and `SCHEMA_VERSION`. Backed by an
  anti-drift **conformance test** that asserts the emitter never produces an undeclared
  label/relationship/property, plus `--emit schema` serialization.
- **Pure projector** (`project.go`): walks the uniform schema-v2 IR → emits a deterministic, deduped
  `GraphRows` bag via a `RowBuilder` with in-memory MERGE semantics (node dedup on
  `(labels[0], value)`, deferred *edge-only-when-resolved* edges, `_k` relationship discriminant for
  legitimately-parallel edges, null-pruning, scalar/`*_json` property flattening). **No I/O, no
  driver.**
- **Output-agnostic row IR** (`rows.go`): `GraphRows` / `NodeRow` / `EdgeRow` / `NodeRef`, the
  in-memory-only `module` grouping field, scoping helpers (`applicationPrefix`,
  `descendantPrefix`), and Cypher-literal rendering.
- **Two writers** consuming the *identical* rows: a **snapshot writer** (`cypher.go` →
  self-contained idempotent `graph.cypher`) and an **incremental Bolt writer** (`bolt.go` →
  content-hash-diffed, per-module replacement over a live DB, with the schema-version gate and
  `--eager` purge/prune behavior).

## CLI surface

Parity with TS/Python: `--emit json|neo4j|schema`, `--neo4j-uri/-user/-password/-database`,
`--eager`/`--lazy` (lazy default). `--emit neo4j` is **always full-depth (L4)**; combining it with
`-a`/`--graphs` is a flag error.

## References & skill

Leverage the existing design/dataflow references at
[`docs/design/Neo4j_projection/Python_neo4j_design_and_dataflow.md`](docs/design/Neo4j_projection/Python_neo4j_design_and_dataflow.md)
and
[`docs/design/Neo4j_projection/Typescript_neo4j_design_and_dataflow.md`](docs/design/Neo4j_projection/Typescript_neo4j_design_and_dataflow.md),
and apply the `codeanalyzer-backend` skill's Neo4j-projection reference:
`https://github.com/codellm-devkit/cldk-devtools/blob/main/skills/codeanalyzer-backend/references/neo4j-projection.md`.

## Engineering constraints

Decompose the work into **small, self-contained, independently reviewable commits** — each commit
should land one coherent, tested unit (e.g., schema contract + conformance test, then the row IR,
then the pure projector, then each writer) so a reviewer can evaluate it in isolation.

## Deliverable

Produce a **practical, sequenced implementation roadmap** for the Go Neo4j projection — ordered
milestones mapped to the above modules, with the commit boundaries, dependencies, and the test gate
each milestone must pass.
