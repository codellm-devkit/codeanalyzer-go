# codeanalyzer-go

Static analysis for Go using `golang.org/x/tools/go/packages` (AST + type resolution).

Produces `analysis.json` (symbol table + call graph) in the [CLDK canonical schema](https://github.com/codellm-devkit/python-sdk), consumable by the Python SDK via `CLDK(language="go").analysis(project_path=...)`.

## Prerequisites

- **Go 1.25+** — the only required runtime. Install from [go.dev/dl](https://go.dev/dl/). Developed and tested on Go 1.26.4.

Verify:
```bash
go version
```

The binary is self-contained. No other tools are required for Level 1 analysis.

## Building

```bash
git clone https://github.com/codellm-devkit/codeanalyzer-go
cd codeanalyzer-go
go build -o codeanalyzer-go ./cmd/codeanalyzer
```

This produces a single static binary `codeanalyzer-go` with no runtime dependencies.

## Usage

```bash
codeanalyzer-go -i /path/to/go/project
```

### Command-line options

```
codeanalyzer-go produces analysis.json (symbol table + call graph) for Go projects.

Usage:
  codeanalyzer-go [flags]

Flags:
  -a, --analysis-level int     Analysis level: 1=symbol table only, 2=+resolver call graph (default 1)
  -c, --cache-dir string       Cache directory (default: ~/.cldk/go-cache)
      --codeql                 Enable CodeQL framework-based call graph (level 2, stub)
      --eager                  Force clean rebuild (ignore cache)
  -f, --format string          Output format: json (default "json"); msgpack is not yet implemented
  -h, --help                   help for codeanalyzer-go
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
codeanalyzer-go -i ./my-go-project
```
Prints `analysis.json` to stdout.

**Symbol table + call graph (Level 2):**
```bash
codeanalyzer-go -i ./my-go-project -a 2
```

**Write output to a directory:**
```bash
codeanalyzer-go -i ./my-go-project -a 2 -o /path/to/output/
# Writes: /path/to/output/analysis.json
```

**Incremental analysis (specific files only):**
```bash
codeanalyzer-go -i ./my-go-project -t pkg/server/server.go -t pkg/server/handler.go
```

**Force rebuild, ignore cache:**
```bash
codeanalyzer-go -i ./my-go-project --eager
```

**Verbose output:**
```bash
codeanalyzer-go -i ./my-go-project -a 2 -vv
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

See [python-sdk](https://github.com/codellm-devkit/python-sdk) for full API documentation.

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
├── cmd/codeanalyzer/         # CLI entry point (cobra)
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
codeanalyzer-go -i ./my-project --eager
```

To delete it entirely:

```bash
rm -rf ~/.cldk/go-cache
```

If you pass a custom `--cache-dir`, remove that directory instead.

### Running from source

```bash
go run ./cmd/codeanalyzer -i /path/to/project -a 2
```
