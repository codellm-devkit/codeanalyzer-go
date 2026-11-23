# Spec: `application.name` alongside `application_name` (codeanalyzer-go)

## Contract-Impact Triage

**Does this change schema v2 output?** Yes, additively. The v2 `application` node gains a
`name` field. No existing field is renamed or removed.

| Change type | Analyzers | SDKs | Docs |
| --- | --- | --- | --- |
| Additive schema field | **codeanalyzer-go** (schema, emit, Neo4j projector) | none now (python-sdk and codeanalyzer-python keep reading `application_name`) | `CLAUDE.md` § Schema decisions, this spec |

## Design-loop decisions

Made with the user on 2026-11-20.

1. **Field.** `application.name` is added to the v2 application node in `analysis.json`. Its
   value is the same as `application_name`.
2. **Keep `application_name`.** Renaming it was rejected. The parity rule forbids renaming shared
   fields, and `codeanalyzer-python` and `python-sdk` read `application_name`.
3. **Neo4j.** The `:GoApplication` node gets a `name` property with the same value as
   `application_name`, so the two projections keep matching.
4. **Tracking.** One issue in `codeanalyzer-go`, one checklist. A PR on `fix-app-name`.
5. **Release.** One PR, one release. The python-sdk can read `name` later; nothing is required
   of it now.

## Out of scope

- Removing or deprecating `application_name`. That is a later, separate schema change.
- Changing the python-sdk or codeanalyzer-python readers.
