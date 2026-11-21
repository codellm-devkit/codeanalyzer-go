# Spec: start_line and end_line on Go span-bearing models

## Contract-Impact Triage

**Does this change schema v2 output?** Yes, additively. Six Go model types gain two integer
fields. No existing field changes.

| Change type | Analyzers | SDKs | Docs |
| --- | --- | --- | --- |
| Additive schema fields | **codeanalyzer-go** (schema, emit, Neo4j projector) | none now; python-sdk can read the keys later | `CLAUDE.md` § Schema decisions, this spec |

Anchor: `codeanalyzer-python/codeanalyzer/schema/py_schema.py`. Python puts `start_line` and
`end_line` next to `span` on imports, parameters, callables, classes, call sites and others.

## Design-loop decisions

Made with the user on 2026-11-21.

1. **Placement.** Top-level keys on each model, next to `span` (Python style), not inside `span`.
   The values equal `span.start[0]` and `span.end[0]`.
2. **Scope.** All span-bearing Go models: `Import`, `Parameter`, `Callable`, `Type`, `Field`,
   and `BodyNode`.
3. **`code_start_line`.** Not added. For Go the signature and the opening brace share a line in
   practice, so the field would carry no information.
4. **Neo4j.** `start_line` and `end_line` are node properties beside `span_json`, on every
   span-bearing node the projector writes. `Import` is an edge in Neo4j, not a node, so it gets
   no property; its lines appear in the JSON only.
5. **Tracking.** One issue in codeanalyzer-go, one checklist. One PR on `add_startline_endline`.
6. **Release.** One PR, one release. The keys are additive, so existing consumers keep working.

## Amendment (2026-11-21): module

The Neo4j module node carries `start_line` and `end_line`, so the JSON module must too, or the
two projections differ. Decided with the user: the JSON `module` gains the keys, equal to its
whole-file `span.start[0]` and `span.end[0]`. The module is therefore a seventh model.

## Out of scope

- `code_start_line` (decision 3).
- Comments and variable declarations, which Python emits and v2 Go does not carry.
