<div align="center">

<img src="https://github.com/codellm-devkit/codeanalyzer-python/blob/main/docs/assets/logo.png?raw=true" alt="CodeLLM-DevKit" />

# codeanalyzer-go (`cango`)

**A Go static-analysis toolkit — the CLDK backend that emits a canonical symbol table and call graph as `analysis.json`.**

[![PyPI](https://img.shields.io/pypi/v/codeanalyzer-go?style=for-the-badge&logo=pypi&logoColor=white)](https://pypi.org/project/codeanalyzer-go/)
[![Go](https://img.shields.io/github/go-mod/go-version/codellm-devkit/codeanalyzer-go?style=for-the-badge&logo=go&logoColor=white)](https://go.dev/)
[![Release](https://img.shields.io/github/actions/workflow/status/codellm-devkit/codeanalyzer-go/release.yml?style=for-the-badge&label=release&logo=github)](https://github.com/codellm-devkit/codeanalyzer-go/actions/workflows/release.yml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue?style=for-the-badge)](./LICENSE)

</div>

---

`cango` is a static analyzer for Go built on [`golang.org/x/tools/go/packages`](https://pkg.go.dev/golang.org/x/tools/go/packages)
(AST + full type resolution). It produces the canonical CodeLLM-DevKit (CLDK) `analysis.json` — a
symbol table plus a resolver-based call graph — consumable by the Python SDK via
`CLDK(language="go").analysis(project_path=...)`. It is the Go backend behind
[CLDK](https://github.com/codellm-devkit/python-sdk), mirroring its
[Python](https://github.com/codellm-devkit/codeanalyzer-python),
[TypeScript](https://github.com/codellm-devkit/codeanalyzer-typescript), and
[Java](https://github.com/codellm-devkit/codeanalyzer-java) siblings.

The binary is fully self-contained: a single static executable with no runtime dependencies.

## Table of Contents

- [Features](#features)
- [Installation](#installation)
  - [Prerequisites](#prerequisites)
  - [Install via shell script](#install-via-shell-script)
  - [Install via Homebrew](#install-via-homebrew)
  - [Install via pip (PyPI)](#install-via-pip-pypi)
  - [Build from source](#build-from-source)
  - [Toolchain](#toolchain)
- [Usage](#usage)
  - [Command-line options](#command-line-options)
  - [Examples](#examples)
- [Analysis levels](#analysis-levels)
- [Output schema](#output-schema)
- [Neo4j projection (`--emit neo4j`)](#neo4j-projection---emit-neo4j)
  - [Emit a snapshot (`graph.cypher`)](#emit-a-snapshot-graphcypher)
  - [Push live to a running Neo4j](#push-live-to-a-running-neo4j)
  - [Load and query](#load-and-query)
  - [Query reference](#query-reference)
- [Python SDK (CLDK) integration](#python-sdk-cldk-integration)
- [Upgrading](#upgrading)
- [Architecture & Tooling](#architecture--tooling)
- [Development](#development)
- [License](#license)

## Features

- **Symbol table** — packages, structs, interfaces, fields, methods, package-level functions,
  imports, struct tags, and generic type parameters, with precise source spans.
- **Call graph** — a resolver-based call graph via `go/types`: each call site is resolved to its
  full import-path signature. Level 2 emits edges to project callees and to library callees
  (homed in `application.external_symbols` under `can://<app>/@external/<import-path>/<sig>`).
  Level 3 (`-a 3`) adds per-callable `cfg`, `cdg` and `ddg`.
- **Generics-aware** — Go 1.18+ generics (`Set[T]`, union-constraint interfaces, multi-type-param
  functions) are modeled in the unified type/callable schema.
- **Self-contained binary** — a single static executable (`go build`, `CGO_ENABLED=0`); no runtime
  dependencies for SDK users.
- **CLDK canonical schema** — output is spine-compatible with the Java/Python/TypeScript analyzers,
  loadable directly by the Python SDK.
- **Neo4j projection** — the same v2 envelope projected into a property graph (`--emit neo4j`),
  either as a replayable `graph.cypher` snapshot or pushed live over Bolt, keyed on the identical
  `can://` ids so JSON and graph join on one string.

## Installation

### Prerequisites

Running a prebuilt `cango` binary requires **nothing** — it is fully self-contained. To *analyze* a
project, that project should be a normal Go module (contain a `go.mod`) so the type checker can
resolve imports. Building `cango` from source requires [Go 1.26+](https://go.dev/dl/), and the
latest Go release is recommended — see [Toolchain](#toolchain).

### Install via shell script

Download and install the prebuilt binary for your platform from the latest release:

```sh
curl --proto '=https' --tlsv1.2 -LsSf https://github.com/codellm-devkit/codeanalyzer-go/releases/latest/download/cango-installer.sh | sh
```

The installer drops `cango` into `~/.local/bin` (override with `CANGO_INSTALL_DIR`) and can pin a
version with `CANGO_VERSION=vX.Y.Z`. It also creates a `codeanalyzer-go` alias symlink. Supports
macOS (arm64/x86_64) and Linux (x86_64/aarch64).

### Install via Homebrew

```sh
brew install codellm-devkit/homebrew-tap/codeanalyzer-go
```

### Install via pip (PyPI)

The wheel bundles the prebuilt, self-contained binary for your platform (no Go toolchain required):

```sh
pip install codeanalyzer-go
cango --help
```

The wheel installs both a `cango` launcher and a `codeanalyzer-go` alias on `PATH`. This is also the
package CLDK's Python SDK depends on to locate the analyzer backend; it exposes
`codeanalyzer_go.bin_path()`.

### Build from source

```sh
git clone https://github.com/codellm-devkit/codeanalyzer-go
cd codeanalyzer-go
go build -o cango ./cmd/codeanalyzer
```

This produces a single static binary `cango` with no runtime dependencies. You can also run the
analyzer directly from source without compiling:

```sh
go run ./cmd/codeanalyzer -i /path/to/project -a 2
```

### Toolchain

`cango` type-checks the projects it analyzes with the `go/types` package compiled into the
binary, so **the Go that builds `cango` must be at least as new as the newest `go` line of the
projects it analyzes**. Build with the latest Go release (the release binaries are). `go.mod`'s
`go 1.26.0` is only the minimum that compiles the analyzer (set by `golang.org/x/tools`, whose
recent releases require Go 1.26).

With a binary built by an older Go, every package that needs a newer Go type-checks only
partially, yet the run still exits 0: callees go unresolved, call edges and L3 defs are lost,
and nothing else looks wrong. `cango` prints one warning when this happens, naming the Go it was
built with, the highest version required and the number of affected packages:

```
[codeanalyzer-go WARN] 12 packages require a newer Go version (highest go1.27) than this analyzer was built with (go1.26.4); …
```

Rebuild `cango` with that Go version or newer. (`-vv` lists the affected packages.)

## Usage

```bash
cango -i /path/to/go/project
```

### Command-line options

```
codeanalyzer-go produces analysis.json (symbol table + call graph) for Go projects.

Usage:
  cango [flags]

Aliases:
  cango, codeanalyzer-go

Flags:
  -a, --analysis-level int      Analysis level: 1=symbol table, 2=+call graph, 3=+intraprocedural cfg/cdg/ddg (default 1)
      --app-name string         Application anchor name for can:// ids and Neo4j :Application (default: input dir name)
  -c, --cache-dir string        Cache directory (default: ~/.cldk/go-cache)
      --eager                   Force clean rebuild (ignore cache)
      --emit string             Output projection: json|neo4j|schema (default "json")
  -f, --format string           Output format: json|msgpack (default "json")
      --graph-field-depth int   Field-path depth bound for L3 dataflow nodes (k limit; applies at -a 3 only) (default 3)
  -h, --help                    help for cango
  -i, --input string            Project root to analyze (required)
  -j, --jobs int                Worker parallelism (default: CPU cores)
      --neo4j-database string   Neo4j database (env NEO4J_DATABASE, optional)
      --neo4j-password string   Neo4j password (env NEO4J_PASSWORD, default neo4j)
      --neo4j-uri string        Live Bolt push target (env NEO4J_URI); omit to write graph.cypher
      --neo4j-user string       Neo4j username (env NEO4J_USERNAME, default neo4j)
  -o, --output string           Output directory for analysis.json (default: stdout)
      --skip-tests              Skip *_test.go files (default true)
  -t, --target-files strings    Restrict analysis to specific files (incremental mode)
  -v, --verbose count           Verbosity (repeat for more detail)
      --version                 Print version and exit
```

### Examples

**Symbol table only (Level 1, default):**
```bash
cango -i ./my-go-project
```
Prints `analysis.json` to stdout.

**Symbol table + call graph (Level 2):**
```bash
cango -i ./my-go-project -a 2
```

**Write output to a directory:**
```bash
cango -i ./my-go-project -a 2 -o /path/to/output/
# Writes: /path/to/output/analysis.json
```

**Incremental analysis (specific files only):**
```bash
cango -i ./my-go-project -t pkg/server/server.go -t pkg/server/handler.go
```

**Force rebuild, ignore cache:**
```bash
cango -i ./my-go-project --eager
```

**Verbose output:**
```bash
cango -i ./my-go-project -a 2 -vv
```

## Generating `analysis.json` for a Go app

The end-to-end steps to produce an `analysis.json` file for any Go project.

**1. Make sure you have `cango`.** Either install a released binary (see
[Installation](#installation)) or build from source:

```bash
git clone https://github.com/codellm-devkit/codeanalyzer-go
cd codeanalyzer-go
go build -o cango ./cmd/codeanalyzer
```

**2. Point it at the project root** (the directory containing the app's `go.mod`).
`cango` resolves imports with the Go type checker, so the target must be a normal Go
module. Write the output to a directory with `-o`; `cango` names the file
`analysis.json` inside it.

```bash
# Level 2 (symbol table + call graph):
cango -i /path/to/go/app -a 2 -o /path/to/output/
# → writes /path/to/output/analysis.json
```

> **Schema.** `cango` emits the **canonical CLDK v2** tree (the shape the Python SDK's v2
> loader and the sections below describe). Omit `-o` to stream the JSON to stdout instead
> of writing a file.

**3. (Optional) name the application anchor.** `--app-name` sets the `<app>` in every
`can://<app>/…` id and defaults to the input directory's base name. Set it when the
directory name isn't the identity you want:

```bash
cango -i /path/to/go/app -a 2 --app-name myapp -o ./out/
```

**4. Verify.** A successful run exits `0` and produces a JSON document whose envelope
carries `"schema_version": "2.0.0"`, `"language": "go"`, `"max_level": 2`, and an
`application` tree under `application.symbol_table`:

```bash
cango -i /path/to/go/app -a 2 -o ./out/ && echo "exit=$?"
python3 -c "import json; d=json.load(open('out/analysis.json')); print(d['schema_version'], d['max_level'], len(d['application']['symbol_table']), 'modules')"
```

**Notes for larger apps.** Analysis parallelism defaults to your CPU count (tune with
`-j`); a warm cache (`--cache-dir`, default `~/.cldk/go-cache`) makes re-runs fast, and
`--eager` forces a clean rebuild. Projects that use **cgo** (`import "C"`) are supported:
the toolchain-synthesized wrapper functions are correctly excluded from the output (they
are build artifacts, not project source), so only your own declarations are emitted.

## Analysis levels

| Level | Flag | What runs | Status |
|-------|------|-----------|--------|
| 1 | `-a 1` (default) | Symbol table only — types, functions, call sites | Implemented |
| 2 | `-a 2` | Level 1 + resolver-based call graph via `go/types` | Implemented |
| 3 | `-a 3` | Level 2 + per-callable statement nodes and `cfg` / `cdg` / `ddg` | Implemented |
| 4 | `-a 4` | Level 3 + interprocedural parameter edges | Not implemented (rejected) |

**Level 1** loads each package with `packages.NeedSyntax | NeedTypes | NeedTypesInfo` and walks the AST file by file. Call sites are recorded with `callee_signature = null` at this stage.

**Level 3** completes every callable's `body` with its statement, control (`branch`, `loop`,
`switch`, `return`) and synthetic (`@entry`, `@exit`) nodes around the L1 call nodes, and adds
three intra-callable edge lists: `cfg` (control flow, including the `defer` chain and panic
`exception` edges), `cdg` (control dependence from post-dominators) and `ddg` (syntactic
reaching definitions over k-limited access paths, `prov: ["ssa"]`). `--graph-field-depth <k>`
(default 3, minimum 1) bounds the access paths (`x.f.g.h` → `x.f.g.*` at k = 3) and is
recorded in the envelope as `k_limit`. It is a flag error below `-a 3` and with
`--emit neo4j`. Callables are analyzed in parallel (`-j`), and the output is byte-identical
for every `-j`. L3 only adds to the L2 output: every L2 fact stays as it is.

**Level 2** adds a resolver pass: for each call site, `go/types` resolves the callee to its full import-path signature (`pkgImportPath.TypeName.MethodName`). A project callee becomes an edge to its callable; a **library callee** (stdlib, a dependency, or a package vendored under `vendor/`) becomes an edge to an `@external` symbol, recorded once in `application.external_symbols`. A callee that is neither (an interface method of a project type, a conversion, a builtin, a call through a function value) gets no edge. Every resolved call node's `callee` is backfilled.

## Output schema

`cango` emits the **canonical CLDK v2** shape — the shape the v2 Python SDK loader consumes
and the [step-by-step section above](#generating-analysisjson-for-a-go-app) produces.

### Canonical v2 shape

The document is a manifest envelope wrapping one `application` containment tree. Every
node carries a `can://<app>/…` `id`, a `kind`, and a `span`:

```json
{
  "schema_version": "2.0.0",
  "language": "go",
  "max_level": 2,
  "analyzer": { "name": "codeanalyzer-go", "version": "0.1.0" },
  "application": {
    "id": "can://myapp",
    "kind": "application",
    "symbol_table": {
      "pkg/greeter/greeter.go": {
        "id": "can://myapp/go/pkg/greeter/greeter.go",
        "kind": "module",
        "package": "greeter",
        "span": { "start": [1, 1], "end": [40, 2], "bytes": [0, 812] },
        "source": "package greeter\n\n...",
        "content_hash": "…",
        "imports": [ { "name": "fmt", "path": "fmt", "span": {…} } ],
        "types": {
          "Greeter": {
            "id": "can://myapp/go/pkg/greeter/greeter.go/example.com/pkg/greeter.Greeter",
            "kind": "struct",
            "span": { "start": [5, 1], "end": [7, 2], "bytes": [17, 63] },
            "fields": {
              "Prefix": { "id": "…/Prefix", "kind": "field", "type": "string", "span": {…} }
            },
            "callables": {
              "example.com/pkg/greeter.Greeter.Greet": {
                "id": "can://myapp/go/pkg/greeter/greeter.go/example.com/pkg/greeter.Greeter/example.com/pkg/greeter.Greeter.Greet",
                "kind": "method",
                "signature": "example.com/pkg/greeter.Greeter.Greet",
                "span": { "start": [9, 1], "end": [11, 2], "bytes": [65, 140] },
                "parameters": [ { "name": "name", "type": "string", "span": {…} } ],
                "return_type": "string",
                "error_channel": ["error"],
                "metrics": { "cyclomatic": 1 },
                "body": {
                  "10:2": {
                    "kind": "call",
                    "id": "can://myapp/go/pkg/greeter/greeter.go/…/example.com/pkg/greeter.Greeter.Greet@10:2",
                    "span": { "start": [10, 2], "end": [10, 30], "bytes": [90, 118] },
                    "callee": "can://myapp/go/pkg/greeter/greeter.go/…/example.com/pkg/greeter.format"
                  }
                }
              }
            }
          }
        },
        "functions": {
          "example.com/main.main": { "id": "…", "kind": "function", "signature": "…", "body": {…} }
        }
      }
    },
    "call_graph": [
      {
        "src": "can://myapp/go/main.go/…/example.com/main.main",
        "dst": "can://myapp/go/pkg/greeter/greeter.go/…/example.com/pkg/greeter.Greeter.Greet",
        "prov": ["go/types"],
        "weight": 1
      }
    ]
  }
}
```

Key v2 schema properties:
- **Envelope** — `schema_version` (`"2.0.0"`), `language` (`"go"`), `max_level` (1, 2 or 3),
  `k_limit` (L3 only: the `--graph-field-depth` value) and `analyzer{name,version}` wrap a
  single `application` node.
- **One containment tree** — `application → module → type → callable → body`, keyed as
  `symbol_table` (modules, by **project-relative file path**), `types`, `callables`, `body`.
- **`id`** — every node carries a durable `can://<app>/go/<file>/<type>/<signature>` id;
  `<app>` is the `--app-name` anchor. A body node is keyed by `line:col` in its callable's `body`
  map and carries `id` = `<callable-id>@<key>` at every level, from `-a 1` on.
- **`kind`** — `module` · `type` kinds `struct | interface | alias | defined` (collapses v1's
  `is_interface`) · `callable` kinds `function | method | lambda` · body `call`.
- **`base_types: []`** (type, optional) — embedded type ids (struct/interface embedding — the
  explicit spine), as opposed to interfaces the type is *computed* to satisfy.
- **`span`** — `{start:[line,col], end:[line,col], bytes:[from,to]}`; `bytes` are **UTF-8 byte
  offsets** into the owning `module.source` (source is stored once per module, every node slices it).
- **`source_file`** (callable, optional) — set when a method is declared in a *different file*
  than its receiver type, and on every closure (nested ones too) inside such a method; the
  callable stays nested under the type, and its `span.bytes` (and its body nodes' spans) index
  `symbol_table[source_file].source` instead of the nesting module's.
- **`error_channel: []`** — populated from `error`-typed returns (the returns also stay in
  `return_type`). **Closures** nest as `callables{}` on the enclosing callable.
- **`body` call nodes** — `callee` is the sanctioned `null → id` slot: `null` at L1; at L2 a
  project callable's `can://` id, or for a library (stdlib or dependency) callee its
  `can://<app>/@external/<import-path>/<sig>` id, a key of `external_symbols`. It stays `null`
  for conversions, builtins, calls through function values and unresolved sites. `is_goroutine` / `is_deferred`
  are boolean flags emitted **only when true** (a plain call omits them), for `go f()` / `defer f()`.
- **`call_graph` edges** — `{src, dst, prov, weight}`; `src` is a callable id in the tree and
  `dst` a callable id or an `external_symbols` key (never a raw signature); `prov` is resolver
  provenance, e.g. `["go/types"]`.
- **`external_symbols`** (application, L2, optional) — one entry per library callee, keyed by
  its `@external` id: `{id, kind: "external", name, module, signature, parameters[], return_type}`
  (parameters carry no span). Absent when no library call was resolved.
- **L3 (`-a 3`)** — `body` gains `statement` / `branch` / `loop` / `switch` / `return` nodes
  and the synthetic `@entry` / `@exit` (which have no `span`). Every body node carries `id`
  (`<callable-id>@<key>`).
  A call nested in a statement carries `parent`, the statement's key. The callable gains
  `cfg[] {src, dst, kind}`, `cdg[] {src, dst}` and `ddg[] {src, dst, var, prov}`, whose
  endpoints are `body` keys. See `docs/design/specs/v2-schema-structure.md`.

## Neo4j projection (`--emit neo4j`)

`--emit neo4j` projects the **same v2 envelope** into a Neo4j property graph. It is a *second
projection of the identical analysis* — JSON nodes and graph nodes carry the same `can://<app>/…`
ids, so the two join on one string and are never recomposed. The graph vocabulary is `Go`-prefixed
labels (`:GoModule`, `:GoType`, `:GoCallable`, `:GoField`, `:GoBodyNode`, `:GoExternal`, plus the
shared merge label `:GoSymbol` and marker `:GoCanNode`) and `GO_`-prefixed relationships
(`GO_HAS_MODULE`, `GO_DECLARES`, `GO_HAS_METHOD`, `GO_HAS_FIELD`, `GO_CALLS`, `GO_RESOLVES_TO`,
`GO_IMPORTS`, `GO_EMBEDS`, `GO_SATISFIES`, …).

There are two output paths, chosen by whether `--neo4j-uri` is set:

| | `--neo4j-uri` unset | `--neo4j-uri` set |
|---|---|---|
| **What happens** | Writes a replayable `graph.cypher` snapshot | Pushes the graph live over Bolt into a running Neo4j |
| **Where** | `<-o dir>/graph.cypher` (or stdout) | The target database |
| **Use for** | Review, diffing, loading on your own schedule, CI | A DB you already have running |

Both paths are **scoped to one application**: every destructive statement is anchored on
`:GoCanNode` and the `can://<app>/` id prefix, so it never touches another app (or a
sibling-language graph) in the same database. The projection is **deterministic** — a second emit
is byte-identical.

What a re-emit replaces depends on the path:

- **`graph.cypher` snapshot** and **`--eager` Bolt push**: the app's subgraph is purged and
  rebuilt, so re-emitting replaces exactly that app's subgraph.
- **Default (lazy) Bolt push**: purely additive. Changed modules are MERGE-upserted and nothing is
  deleted. After source edits, nodes and edges that the new analysis no longer emits stay in the
  database. For L3 this includes `:GoBodyNode`s and their `GO_CFG_NEXT` / `GO_CDG` / `GO_DDG`
  edges, whose ids are keyed on `line:col`: once lines shift, the old body nodes and their edges
  remain beside the new ones. Pass `--eager` when you push an L3 graph after editing source.
  The purge needs a full run: with `--target-files` it is skipped, because the push carries only
  the targeted packages.

> **`--emit neo4j` is always full depth.** The graph is projected at the max implemented level (L3
> today — symbol table, call graph, and every callable's body nodes with `GO_CFG_NEXT` / `GO_CDG` /
> `GO_DDG`) and canonical schema v2. Do **not** pass `-a`/`--analysis-level`
> with it — an explicit level is a flag error (`--analysis-level does not apply to --emit neo4j`),
> because there is no shallower graph to ask for.

### Emit a snapshot (`graph.cypher`)

```bash
cango -i /path/to/go/app --emit neo4j -o ./out/
# → writes ./out/graph.cypher
```

The snapshot is a self-contained script: constraints + indexes, a scoped wipe of this app's subgraph,
then `UNWIND … MERGE` batches for nodes and relationships. Replay it into any Neo4j with
`cypher-shell` (see below). Omit `-o` to stream the Cypher to stdout.

### Push live to a running Neo4j

Set `--neo4j-uri` (or the `NEO4J_URI` env var) to push directly over Bolt instead of writing a file:

```bash
./cango -i /path/to/go/app  --emit neo4j \
  --neo4j-uri bolt://127.0.0.1:7687 \
  --neo4j-user neo4j --neo4j-password <pass>
```

Credentials and target also read from the environment (`NEO4J_URI`, `NEO4J_USERNAME`,
`NEO4J_PASSWORD`, `NEO4J_DATABASE`), so you can keep them out of your shell history:

```bash
export NEO4J_URI=bolt://127.0.0.1:7687
export NEO4J_PASSWORD=<pass>
cango -i /path/to/go/app  --emit neo4j
```

> Use `127.0.0.1` rather than `localhost` if anything else (an SSH tunnel, another DB) might also be
> listening on `7687` — `localhost` can resolve to the wrong listener.

### Load and query

If you emitted a `graph.cypher` snapshot, load it into a running Neo4j with `cypher-shell`:

```bash
cypher-shell -a bolt://127.0.0.1:7687 -u neo4j -p <pass> < ./out/graph.cypher
```

A convenience harness, [`scripts/load_and_query.sh`](scripts/load_and_query.sh), loads the snapshot
and runs a set of sample queries — it auto-detects `cypher-shell` from a Neo4j Desktop or Homebrew
install. It reads `graph.cypher` from its own directory, so emit the snapshot into `scripts/` first
(`-o scripts`):

```bash
# brew install neo4j && neo4j start   # (or Neo4j Desktop) — then set a password
cango -i /path/to/go/app --emit neo4j -o scripts         # writes scripts/graph.cypher

NEO4J_PASSWORD=<pass> ./scripts/load_and_query.sh          # load + query
NEO4J_PASSWORD=<pass> ./scripts/load_and_query.sh --query  # queries only
NEO4J_PASSWORD=<pass> ./scripts/load_and_query.sh --check  # just test the connection
```

Or explore visually in Neo4j Browser at `http://127.0.0.1:7474` (connect to `bolt://127.0.0.1:7687`),
paste a query, and swap `RETURN p` for `RETURN …` to draw the graph.

### Query reference

Always **scope by the `can://` id prefix** (an index-backed seek via the `gocannode_id` range index),
never by a bare label scan — replace `<app>` with your `--app-name`:

```cypher
// Node counts by label.
MATCH (n:GoCanNode) RETURN labels(n)[0] AS label, count(*) AS n ORDER BY n DESC;

// Relationship counts by type.
MATCH ()-[r]->() RETURN type(r) AS rel, count(*) AS n ORDER BY n DESC;

// Everything owned by one application (prefix seek).
MATCH (n:GoCanNode) WHERE n.id STARTS WITH 'can://<app>/' RETURN count(n) AS app_nodes;

// Types ranked by method count (the "god struct" finder).
MATCH (t:GoType) OPTIONAL MATCH (t)-[:GO_HAS_METHOD]->(m)
RETURN t.kind AS kind, t.signature AS type, count(m) AS methods ORDER BY methods DESC;

// Most-called callables (in-degree) — the hot utilities.
MATCH (:GoCallable)-[:GO_CALLS]->(b:GoCallable)
RETURN b.signature AS callee, count(*) AS called_by ORDER BY called_by DESC LIMIT 15;

// Everything Command.Execute reaches, transitively (any depth).
MATCH (:GoCallable {signature:'github.com/spf13/cobra.Command.Execute'})-[:GO_CALLS*]->(b)
RETURN DISTINCT b.signature AS reachable ORDER BY reachable;

// External / stdlib packages reached via imports.
MATCH (:GoCanNode)-[:GO_IMPORTS]->(x:GoExternal) RETURN DISTINCT x.path AS import ORDER BY import;
```

The full, commented catalog — overview/sanity, containment tree, call graph (callers, callees,
reachability, shortest path, entry points, leaves), call-site resolution, imports, inheritance,
metrics, and scoped maintenance — lives in
[`Eval/Neo4j_eval/queries.cypher`](Eval/Neo4j_eval/queries.cypher), validated against the
`spf13/cobra` graph.

## Python SDK (CLDK) integration

```python
from cldk import CLDK

analysis = CLDK(language="go").analysis(project_path="/path/to/go/project")
for file_path, go_file in analysis.get_symbol_table().items():
    print(file_path, go_file.module_name)
```

The SDK locates the analyzer via the `codeanalyzer-go` command on `PATH` (installed by any of the
methods above, including `pip install codeanalyzer-go`). See
[python-sdk](https://github.com/codellm-devkit/python-sdk) for full API documentation.

## Upgrading

This release changes the output contract. If you consume `analysis.json` or the Neo4j graph,
check the following.

- **Ids are app-outermost.** `can://go/<app>/<file>/…` is now `can://<app>/go/<file>/…`. Library
  callees are `can://<app>/@external/<import-path>/<sig>`.
- **Body nodes carry `id` at every level.** Each body node has `id` = `<callable-id>@<local-key>`,
  from `-a 1` on, not only at `-a 3`.
- **L2 includes library calls.** The call graph now has edges to library (stdlib and dependency)
  callees, and `application.external_symbols` describes them. A library call's `callee` is its
  external id instead of `null`. A type conversion is still a call node, but its `callee` stays
  `null`; it is no longer resolved to a `<Type>.__new__` pseudo-callee.
- **L1 records more.**
  - A type's `kind` can now be `alias` or `defined`, not only `struct` or `interface`.
  - Deferred calls carry `is_deferred: true`.
  - Chained calls that start at the same position, such as `s.A().B()`, are now separate call
    nodes keyed `l:c` and `l:c/2`. Previously the inner call was dropped. Do not parse body keys
    as plain `line:col`.
  - Calls inside `go` statement arguments, such as `g()` in `go f(g())`, are now call sites.
  - Consumers that count call nodes will see more of them.
- **Neo4j:**
  - `--emit neo4j` projects L3, adding `GO_CFG_NEXT`, `GO_CDG` and `GO_DDG`.
  - `:GoBodyNode.call_node` is the enclosing statement's local key (the JSON `parent`), not a
    global id.
  - `--eager` now works on Neo4j 5.x.
- **Build.** Building from source needs Go 1.26 or newer. Release binaries are built with the
  latest Go (see [Toolchain](#toolchain)).

## Architecture & Tooling

| Slot | Choice | Rationale |
|------|--------|-----------|
| Runtime | Go binary | Self-contained; no runtime dep for SDK users |
| Structural parser | `go/ast` (stdlib) | Part of the standard toolchain; no external dep |
| Type resolver | `golang.org/x/tools/go/packages` | Single API for both AST + full type resolution; handles modules natively |
| Build/dep materialization | `go mod download` | Required before `packages.Load` so the module cache is warm; result cached by `go.sum` hash |
| Packaging | Native binary (`go build`) | Zero-runtime-dep distribution; matches Rust/C++ analyzers |
| Analysis depth | Levels 1–3 | Symbol table, resolver call graph, intraprocedural cfg/cdg/ddg |
| Call-graph dispatch | Declared-type resolution via `go/types.Selections` | CHA-equivalent; sufficient for cross-package reachability at Level 1 |
| CFG (L3) | Hand-built from `go/ast` (`internal/dataflow`) | Adds what `go/cfg` does not model as edges: panic `exception` edges and the `defer` chain (`defer_resume`). A differential test checks reachability against `golang.org/x/tools/go/cfg` on every fixture body |
| Def-use / DDG (L3) | Syntactic reaching definitions over k-limited access paths | `go/types` objects as bases (shadowing-safe); weak defs through reference arguments; `prov: ["ssa"]` for parity with the Java and Python L3 |

### Package structure

```
codeanalyzer-go/
├── cmd/codeanalyzer/         # CLI entry point (cobra) — builds the `cango` binary
├── internal/
│   ├── core/                 # Orchestrator — delegates only, no inlined analysis
│   ├── schema/               # GoApplication, GoFile, GoType, GoCallable, … (schema.go)
│   ├── options/              # AnalysisOptions + AnalysisLevel constants
│   ├── syntactic_analysis/   # SymbolTableBuilder (packages.Load → AST walk)
│   ├── semantic_analysis/    # CallGraphBuilder (go/types resolver)
│   ├── dataflow/             # L3: body nodes, CFG, CDG, def-use, DDG; Run fans out per callable
│   ├── analysis/             # Pluggable pass interface + registry (topo-ordered pipeline)
│   ├── frameworks/           # BaseEntrypointFinder — extension seam for framework passes
│   └── utils/                # DiscoverGoFiles, IsVendored, IsTestFile, logging
├── packaging/
│   ├── python/               # PyPI wheel wrapper (bundles the prebuilt binary per platform)
│   ├── homebrew/             # generate_formula.sh — Homebrew tap formula generator
│   └── install/              # cango-installer.sh — curl | sh installer
├── testdata/
│   ├── greeter/              # Minimal two-package fixture (basic struct/interface/call sites)
│   ├── multipackage/         # Richer fixture covering embedded fields, variadic params, goroutines, …
│   ├── generics/             # Go 1.18+ generics fixture (Set[T], union-constraint interfaces, Map[T,U])
│   └── chi/                  # External-dep fixture (chi v5, vendored) for HTTP handler patterns
```

The `core` package is a pure orchestrator: it calls `syntactic_analysis` → `semantic_analysis` → `analysis.RunPipeline` in sequence, with no inlined parsing logic. Framework-specific analysis extends through the `analysis/` + `frameworks/` layer without touching `core`.

## Development

### Running tests

```bash
go test ./...
```

Tests run against four fixtures: `testdata/greeter/` (basic), `testdata/multipackage/` (multi-file packages, goroutines, variadic params), `testdata/generics/` (Go 1.18+ generics — `Set[T]`, union constraints, multi-type-param functions), and `testdata/chi/` (external dependency via vendored chi v5, HTTP handler patterns). All 105 tests cover symbol table correctness, generic receiver attribution, call graph edges, JSON round-trip, output format validation, caching behaviour, and error paths.

`go test` caches passing results by source hash. To force a full re-run:

```bash
go clean -testcache && go test ./...
```

The analyzer's own `CacheDir` (used inside tests for `analysis_cache.json` and `go_mod_hash`) is written to OS temp directories that are wiped automatically when the test binary exits — there is no persistent on-disk state between test runs. The chi fixture is fully vendored, so tests never require network access.

### Clearing the production cache

By default the CLI writes its cache to `~/.cldk/go-cache`. To bypass it for a single run:

```bash
cango -i ./my-project --eager
```

To delete it entirely:

```bash
rm -rf ~/.cldk/go-cache
```

If you pass a custom `--cache-dir`, remove that directory instead.

### Releasing

Releases are cut by pushing a `vX.Y.Z` tag. The [release workflow](.github/workflows/release.yml)
cross-compiles the `cango` binary for all supported platforms and publishes:

1. the raw binaries + `cango-installer.sh` as **GitHub Release** assets,
2. platform-tagged wheels to **PyPI** as `codeanalyzer-go` (via Trusted Publishing/OIDC), and
3. a **Homebrew** formula pushed to `codellm-devkit/homebrew-tap`.

The version is injected into the binary at build time via `-ldflags "-X main.version=<tag>"`, so
`cango --version`, the wheel version, and the git tag always stay in lockstep. See
[packaging/](packaging/) for the build scripts.

## License

Apache 2.0 — see [LICENSE](./LICENSE).
