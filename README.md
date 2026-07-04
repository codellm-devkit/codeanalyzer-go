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
- [Usage](#usage)
  - [Command-line options](#command-line-options)
  - [Examples](#examples)
- [Analysis levels](#analysis-levels)
- [Output schema](#output-schema)
- [Python SDK (CLDK) integration](#python-sdk-cldk-integration)
- [Architecture & Tooling](#architecture--tooling)
- [Development](#development)
- [License](#license)

## Features

- **Symbol table** — packages, structs, interfaces, fields, methods, package-level functions,
  imports, struct tags, and generic type parameters, with precise source spans.
- **Call graph** — a resolver-based call graph via `go/types`: each call site is resolved to its
  full import-path signature, with project-internal edges emitted (Level 2).
- **Generics-aware** — Go 1.18+ generics (`Set[T]`, union-constraint interfaces, multi-type-param
  functions) are modeled in the unified type/callable schema.
- **Self-contained binary** — a single static executable (`go build`, `CGO_ENABLED=0`); no runtime
  dependencies for SDK users.
- **CLDK canonical schema** — output is spine-compatible with the Java/Python/TypeScript analyzers,
  loadable directly by the Python SDK.

## Installation

### Prerequisites

Running a prebuilt `cango` binary requires **nothing** — it is fully self-contained. To *analyze* a
project, that project should be a normal Go module (contain a `go.mod`) so the type checker can
resolve imports. Building `cango` from source requires [Go 1.25+](https://go.dev/dl/).

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
  -a, --analysis-level int     Analysis level: 1=symbol table only, 2=+resolver call graph (default 1)
  -c, --cache-dir string       Cache directory (default: ~/.cldk/go-cache)
      --codeql                 Enable CodeQL framework-based call graph (level 2, stub)
      --eager                  Force clean rebuild (ignore cache)
  -f, --format string          Output format: json|msgpack (default "json")
  -h, --help                   help for cango
  -i, --input string           Project root to analyze (required)
  -o, --output string          Output directory for analysis.json (default: stdout)
      --skip-tests             Skip *_test.go files (default true)
  -t, --target-files strings   Restrict analysis to specific files (incremental mode)
  -v, --verbose count          Verbosity (repeat for more detail)
      --version                Print version and exit
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

## Analysis levels

| Level | Flag | What runs | Status |
|-------|------|-----------|--------|
| 1 | `-a 1` (default) | Symbol table only — types, functions, call sites | Implemented |
| 2 | `-a 2` | Level 1 + resolver-based call graph via `go/types` | Implemented |
| — | `--codeql` | CodeQL framework-based call graph (merged with Level 2 edges) | Stub (not yet implemented) |

**Level 1** loads each package with `packages.NeedSyntax | NeedTypes | NeedTypesInfo` and walks the AST file by file. Call sites are recorded with `callee_signature = null` at this stage.

**Level 2** adds a resolver pass: for each call site, `go/types` resolves the callee to its full import-path signature (`pkgImportPath.TypeName.MethodName`). Only project-internal edges (both endpoints present in the symbol table) are emitted. `callee_signature` is backfilled on all successfully resolved sites.

## Output schema

The root object is `GoApplication`:

```json
{
  "symbol_table": {
    "pkg/greeter/greeter.go": {
      "file_path": "pkg/greeter/greeter.go",
      "module_name": "greeter",
      "imports": [...],
      "classes": {
        "Greeter": {
          "name": "Greeter",
          "signature": "example.com/pkg/greeter.Greeter",
          "is_interface": false,
          "fields": [{ "name": "Prefix", "type": "string", "tags": {"json": "prefix"} }],
          "methods": { ... }
        }
      },
      "functions": { ... }
    }
  },
  "call_graph": [
    {
      "source": "example.com/main.main",
      "target": "example.com/pkg/greeter.Greeter.Greet",
      "type": "CALL_DEP",
      "weight": 1,
      "provenance": ["go/types"]
    }
  ],
  "entrypoints": {}
}
```

Key schema properties:
- `symbol_table` — keyed by **file path relative to the project root** (never absolute)
- `classes` — JSON key for types (spine compatibility with Java/Python schemas); value is `GoType`
- `module_name` — JSON key for the Go package name (spine compatibility)
- `GoType.is_interface: bool` — unified type model; structs and interfaces are both `GoType`
- `GoCallable.receiver_type / receiver_name` — non-empty for methods, empty for package-level functions
- `GoCallable.return_types: List[str]` — individual return types (Go-specific extension)
- `GoCallsite.is_goroutine: bool` — true when the call is preceded by the `go` keyword
- `GoCallEdge.provenance: List[str]` — resolver identifiers, e.g. `["go/types"]` or `["go/types","codeql"]`
- Call edges are **identity-only**: source and target are `GoCallable.signature` strings that exist in the symbol table

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

## Architecture & Tooling

| Slot | Choice | Rationale |
|------|--------|-----------|
| Runtime | Go binary | Self-contained; no runtime dep for SDK users |
| Structural parser | `go/ast` (stdlib) | Part of the standard toolchain; no external dep |
| Type resolver | `golang.org/x/tools/go/packages` | Single API for both AST + full type resolution; handles modules natively |
| Optional enrichment | CodeQL (stubbed) | Same enrichment path as Python/Java analyzers; stubbed for Level 1 |
| Build/dep materialization | `go mod download` | Required before `packages.Load` so the module cache is warm; result cached by `go.sum` hash |
| Packaging | Native binary (`go build`) | Zero-runtime-dep distribution; matches Rust/C++ analyzers |
| Analysis depth | Level 1 (rapid) | Symbol table + resolver call graph; CodeQL stub wired but not implemented |
| Call-graph dispatch | Declared-type resolution via `go/types.Selections` | CHA-equivalent; sufficient for cross-package reachability at Level 1 |

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
│   │   └── codeql/           # CodeQL backend subpackage (stubbed)
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

The `core` package is a pure orchestrator: it calls `syntactic_analysis` → `semantic_analysis` → `analysis.RunPipeline` → optional CodeQL in sequence, with no inlined parsing logic. Framework-specific analysis extends through the `analysis/` + `frameworks/` layer without touching `core`.

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
