# Spec: repository artifacts and dependencies (codeanalyzer-go)

> **Cross-repo spec.** Canonical home is `codellm-devkit/.github → docs/design/specs/`,
> beside the coordinating parent issue. Staged locally on `feat/artifacts` until that repo
> is available (same precedent as `v2-l1-emission.md`). This is the design transcript of the
> datamodel loop, linked from the tracking record, not pasted into it.

## Contract-Impact Triage

**Does this change schema v2 output?** Yes. It adds two application-scoped fields
(`artifacts`, `dependencies`), new id shapes for artifact and package nodes, one new CLI flag
(`--artifact-text`), and populates the already-declared Neo4j neutral layer (`:Artifact`,
`:Package`, `HAS_ARTIFACT`, `DECLARES_DEPENDENCY`, `LOCKS`).

| Change type | Analyzers | SDKs | Docs |
| --- | --- | --- | --- |
| Schema v2 evolution (additive) | **codeanalyzer-go** (emit + projector); codeanalyzer-python: none (parity adopted, no id change) | **python-sdk** (Go backend reconstruction of `:Artifact` / `:Package`) | `CLAUDE.md` § Schema decisions, this spec |

Anchor: `codeanalyzer-python` already emits this layer (`PyArtifact`, `PyDependency`,
`PyConfigKey`, `application.artifacts`, `application.dependencies`,
`codeanalyzer/schema/py_schema.py`), and `python-sdk/cldk/analysis/commons/artifacts.py`
rebuilds it from Neo4j, with the neutral labels shared across analyzers.

## Problem

The v2 schema and projector declare the neutral repository-artifact layer, but the Go
analyzer never populates it. `go.mod` is read only for `go mod download`. Against the Go
analysis of `microservices-demo/src/frontend`, `MATCH (n:Artifact)` returns 0 rows.

## Design-loop decisions (the transcript)

Each decision was made with the user, one question per decision, in the order below.

### Application placement
- **`application.artifacts`** is a **map keyed by repo-relative path**, as Python does. Stays
  reachable from `application` by containment (keystone rule).
- **`application.dependencies`** is a **list** (Python parity).

### Artifact scope and fields
- **Scope: all non-`.go` project files** (user decision, broader than the recommended manifests-only
  scope). `.go` files are modules, not artifacts. Directories `.git/` and `vendor/` are excluded
  (implementation default, see Open items).
- **Artifact fields** (Python `PyArtifact` parity):
  | field | type | notes |
  | --- | --- | --- |
  | `id` | string | `can://<app>/artifact/<path>` |
  | `kind` | string | `"artifact"` |
  | `path` | string | repo-relative POSIX path (also the map key) |
  | `format` | string | `toml\|yaml\|json\|ini\|properties\|requirements\|dockerfile\|text\|binary`, plus Go's `gomod` / `gosum` (Open items) |
  | `roles` | string[] | `dependency-manifest` for `go.mod`, `lockfile` for `go.sum`; others only when recognized |
  | `size_bytes` | int | always |
  | `sha256` | string | full file hash, always |
  | `source` | string | whole file text **only** with `--artifact-text`; otherwise `""` |
  | `extraction` | string | `none` \| `partial` \| `full`; `full` when `source` is set, else `none` |
- **Binary files**: metadata only. `format:"binary"`, `size_bytes` and `sha256` set,
  `source:""` (user decision).
- **`--artifact-text`** (default **off**): when on, `source` carries the whole file text
  (user decision). Off keeps default output to metadata.
- **`config_keys`** is **deferred** (user decision): not emitted in this change. No
  `:ConfigKey` rows are written.

### Artifact id
- **`can://<app>/artifact/<path>`** (Python parity, user decision). App-scoped, so the
  projector's `can://<namespace>/<app>/` prefix scoping covers artifact nodes. This changes the
  Go projector's `:Artifact` flag to `CanNode=true`; the current Go schema comment ("neutral,
  not a can:// id") is superseded.

### Package and dependencies
- **Package id: version-less `pkg:golang/<module-path>`** (user decision), for example
  `pkg:golang/github.com/gorilla/mux`. Mirrors Python's `pkg:pypi/<name>`. The version lives
  on the edge and the record, so every dependent app converges on one `:Package`.
- **Dependency records come from every `go.mod` `require`** (user decision). Direct and
  indirect entries alike.
  | field | source |
  | --- | --- |
  | `name` | module path |
  | `ecosystem` | `"golang"` |
  | `spec` | version in `go.mod` (for example `v1.8.0`) |
  | `kind` | `"runtime"` for every entry (`go.mod` has no dev/optional split) |
  | `direct` | `false` when the line carries `// indirect`, else `true` |
  | `locked_version` | version pinned in `go.sum` for that module, when present |
  | `declared_in` | the `go.mod` artifact id |
  | `provides_imports` | import paths in the project that resolve to this module (see below) |
  | `prov` | `["declared"]`, plus `"lockfile"` when `go.sum` supplies `locked_version` |
- **`go.sum`-only entries are not dependencies** (user decision, the non-recommended option
  would have included them; the recommended default is not to, because `go.sum` retains
  entries for pruned modules). `go.sum` contributes only `locked_version` and `prov`.
- **`provides_imports`**: populated by longest-prefix match of each project import path
  against the `go.mod` module paths (user decision, Python parity). Import paths that match
  no requirement are **not** recorded in this change (see Open items).

### Analysis level
- **L1, every level** (user decision). Parse-only facts, identical at `-a 1` through `-a 4`.
  Not gated.

### Neo4j projection (the neutral layer, unchanged labels)
- `:Artifact {id, path, format}` (keyed by `id`, `CanNode=true`), under `HAS_ARTIFACT`
  from `:GoApplication`.
- `:Package {id, ecosystem, name}` keyed by `id`.
- `DECLARES_DEPENDENCY` `(:Artifact)→(:Package)`, `_k = kind` (`runtime`), carrying `spec`,
  `direct`, `prov` as properties.
- `LOCKS` `(:Artifact)→(:Package)` from `go.sum`, carrying `locked_version`.
- No `:ConfigKey` rows and no `DEFINES_CONFIG` rows in this change.

## Open items (flagged for review, not asked)

These are defaults I chose while writing the spec. Confirm them during review:

1. **`format` values for go.mod and go.sum.** I used `gomod` and `gosum`, because the Python
   enum has no Go case. The alternative is `text`.
2. **Excluded directories.** `.git/` and `vendor/` are skipped. `vendor/` is third-party
   source, not project configuration.
3. **Imports with no matching requirement.** Not recorded. They are `unresolved_imports` in
   Python. That field is deferred with the repository-provenance field.
4. **`locked_version` when `go.sum` has several lines for one module** (the `h1:` and
   `/go.mod` hashes). Take the version common to both lines; error if they disagree.

## Resolution (implementation, branch `feat/artifacts`)

The open items were resolved with the defaults below. Two are deviations from the wording
above, marked **(deviation)**:

1. **`format`**: `gomod` for `go.mod`, `gosum` for `go.sum`, adopted.
2. **Excluded directories**: `.git/` and `vendor/` are skipped at any depth. The tool's own
   outputs are also skipped when they sit in the project: `analysis.json` and `graph.cypher`
   in the `-o` directory, and the `-c` cache subtree. Without this, a run that writes into the
   project would capture the previous run's output.
3. **Unrecorded imports**: an import matching no `go.mod` requirement is not recorded.
4. **Locked version (deviation)**: a requirement is locked only when go.sum has **both** the
   `h1:` and the `/go.mod` line for the exact required version. A version with only one line
   is left unlocked (`locked_version` absent, `prov` without `lockfile`), with no error. A
   disagreement cannot arise, since the lookup is keyed by the required version.
5. **Artifact id root (deviation)**: the code's grammar is `can://<namespace>/<app>/…`, the
   same root every other node uses, so an artifact id is
   `can://<namespace>/<app>/artifact/<path>`. The `can://<app>/artifact/<path>` form in the
   decisions above is shorthand for that root.

Also decided at implementation: `source` is omitted from JSON when empty (`omitempty`), not
written as `""`; only the root `go.mod` contributes dependencies (nested modules are artifacts
only); `dependencies` is absent when there is no root `go.mod`.

## Consumers

- **codeanalyzer-go**: emits the fields; the projector writes the rows above. Tests: a fixture
  with `go.mod` and `go.sum`, a projector test for the new rows, and a `--artifact-text`
  on/off test.
- **python-sdk**: the Go backend reads `:Artifact` / `:Package` through the shared
  `cldk/analysis/commons/artifacts.py` reconstructors. The Go backend needs its artifact getters
  added. Scope of that rung is decided at decomposition.

## Out of scope (deferred)

- Config keys and `DEFINES_CONFIG` (user decision).
- `unresolved_imports`, repository provenance (`repository`), `config_uses`.
- Codeanalyzer-python: no change.
