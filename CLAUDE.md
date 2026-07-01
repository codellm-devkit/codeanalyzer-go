# CLAUDE.md

Agent guidance for `codellm-devkit/codeanalyzer-go` (`codeanalyzer-go`).

Respect the global `~/.claude/CLAUDE.md` instructions strictly.

## What this project is

`codeanalyzer-go` is the CLDK Go static analyzer. It emits the canonical CLDK
`analysis.json` — a **symbol table** plus a **call graph** — consumable by the Python SDK
via `CLDK(language="go").analysis(project_path=...)`. It mirrors its
[Python](https://github.com/codellm-devkit/codeanalyzer-python) (`canpy`),
[TypeScript](https://github.com/codellm-devkit/codeanalyzer-typescript) (`cants`), and
[Java](https://github.com/codellm-devkit/codeanalyzer-java) sibling analyzers, so
output-shape parity with them is a first-class concern.

It builds on **`golang.org/x/tools/go/packages`** (loaded with syntax + types + deps) plus
stdlib `go/ast`, `go/token`, and `go/types`. The call graph is a hand-rolled, CHA-style
**resolver over `go/types`** (declared-type dispatch) — it deliberately does *not* use
`go/ssa` or `x/tools/go/callgraph`. Edges are emitted only for project-internal callees;
external/stdlib callees get their `callee_signature` backfilled but no edge (mirroring
Python/Jedi).

> **Status — read this first.** This is the newest backend. Implemented: the level-1
> symbol table, the level-2 resolver call graph, `go mod` materialization with caching, the
> cobra CLI, incremental `--target-files`, and a pluggable pass framework. **Not yet
> implemented** (be honest about these; don't describe them as working): the **CodeQL**
> provider (`--codeql` is wired but `codeql.*` returns `ErrCodeQLNotImplemented`),
> **msgpack** output, **framework entrypoint finders** (no passes are registered, so
> `entrypoints` in the output is effectively always `{}`), and **Neo4j** projection
> (there is no Neo4j code here at all — JSON is the only output). The implementation
> currently lives on `feat/initial-implementation`; `main` is a stub.

## Architecture — follow the pipeline

The whole analyzer is one orchestrator: `Analyzer.Analyze()` in
`internal/core/analyzer.go` (a pure delegator, mirroring Python's `core.py`). Read it
first; everything else is a phase it calls, in order:

1. **materialize** — `Analyzer.materialize()` runs `go mod download` (skipped without a
   `go.mod`; cached by SHA-256 of `go.sum`; failures degrade gracefully).
2. **symbol table** (`internal/syntactic_analysis`) —
   `NewSymbolTableBuilder(input).Build(targetFiles, skipTests)`.
3. **call graph** (`internal/semantic_analysis`, `Level >= 2` only) —
   `NewCallGraphBuilder(...).Build(symbolTable)` resolves each call site via `go/types`,
   backfills `callee_signature`, and emits internal `GoCallEdge`s.
4. **pass pipeline** — `analysis.RunPipeline(app, ctx)` runs topologically-ordered
   pluggable passes (none registered yet).
5. **optional CodeQL** (`--codeql` only) — currently a stub; would merge via `MergeEdges`.

Then `finalizeAndCache()` writes `<cacheDir>/analysis_cache.json`, and
`core.WriteOutput()` writes `<outputDir>/analysis.json` (or stdout).

The output shape is the **structs in `internal/schema/schema.go`** (`GoApplication` is the
top type; JSON keys are snake_case for Pydantic parity).

## Directory map

| Path | Responsibility |
|------|----------------|
| `cmd/codeanalyzer/main.go` | Entry point + cobra CLI (`rootCmd`), flag parsing |
| `internal/core/analyzer.go` | `Analyzer.Analyze()` orchestrator — the spine; `WriteOutput` |
| `internal/options/options.go` | `AnalysisOptions` + `AnalysisLevel` (`LevelSymbolTable=1`, `LevelCallGraph=2`) |
| `internal/schema/schema.go` | `GoApplication` structs (the output contract) |
| `internal/syntactic_analysis` | Symbol table (`go/packages` + `go/ast`); `signature.go` = canonical signatures; `export.go` = `Fset()`/`Pkgs()` |
| `internal/semantic_analysis` | Resolver call graph (`call_graph.go`, `go/types`); `codeql/` = CodeQL backend (stub) |
| `internal/analysis` | Pluggable pass framework: `pass.go` (interface), `registry.go` (`RegisterPass`, topo-ordered `RunPipeline`) |
| `internal/frameworks` | Entrypoint-finder base (no concrete finders yet) |
| `internal/utils` | `fs.go` (file discovery, hashing), `logging.go` |
| `testdata/{greeter,multipackage,generics,chi}` | Test fixtures, each with its own `go.mod` |

## Commands

Module `github.com/codellm-devkit/codeanalyzer-go`, **Go 1.25+**. No Makefile, no
golangci-lint config.

- `go build -o codeanalyzer-go ./cmd/codeanalyzer` — build the binary.
- `go run ./cmd/codeanalyzer -i /path/to/project -a 2` — run from source
  (`-a 1` = symbol table only, `-a 2` adds the call graph; `-o` outdir, `-t` target files,
  `--eager`, `-v`). Default cache dir `~/.cldk/go-cache`.
- `go test ./...` — run tests (force re-run: `go clean -testcache && go test ./...`).
- `go vet ./...` — the only static-check wired up (no linter configured).

## I implement features myself — you assist

For feature work, **I write the implementation** to stay fluent in my own analyzer.
Act as a helper, not the author:

- **Don't write the feature code** or apply edits to implement it unless I explicitly
  ask ("write this", "implement X", "apply it"). Default to guiding, not doing.
- **Do** move me fast: explain the relevant phase, point at prior art (e.g. the Python or
  Java backend's equivalent stage, or the resolver in `semantic_analysis/call_graph.go`),
  sketch signatures/types, outline an approach, and answer questions about the codebase.
- **Review on request:** when I share a diff or push, critique it — correctness,
  **parity with the Python/Java/TypeScript backends**, schema shape, missing tests, edge
  cases — and suggest concrete improvements.
- Scaffolding like tests or boilerplate is fine **when I ask**; otherwise leave the
  keyboard to me.
- If you think I'm about to go wrong, say so briefly and let me decide — don't pre-empt
  by implementing the fix.

## Rules

1. **Think before coding.** State assumptions explicitly; ask rather than guess. Push
   back when a simpler approach exists. Stop when confused.
2. **Simplicity first.** Guide me toward the minimum idiomatic code that solves the
   problem. Nothing speculative; no abstractions for single-use code.
3. **Issue → branch → work → PR.** Every change starts as an issue, on a branch named
   `feat/issue-XXX`, `fix/issue-XXX`, `chore/issue-XXX`, and lands via a PR.
4. **Guard the contract.** Changes to `internal/schema` must keep the JSON shape (snake_case
   keys, `CALL_DEP` edges, `provenance`) in parity with the sibling analyzers so the Python
   SDK can consume Go output interchangeably.

## Goal-driven execution, as a teaching loop

Success is measured by the sole fact that **I understand it**. The success criterion:
I can point to the exact line of code where any feature lives, however remote or
obscure, and explain why it's there and how it behaves.

To that end, be my teacher and a Socratic one — not an answer key:

- Lead with questions that make me derive the answer; don't hand me the solution.
- Verify understanding, not just behavior — have me locate and explain the relevant
  LOC, walk edge cases, and predict what a change would do before running it.
- Teach, help improve, and strengthen the weak spots you surface; circle back to them.
- The loop closes when I can **teach it back** and place every feature on a line, not
  merely when the tests pass.
- Over the session, frequently — but not so much that I am stymied — ask spaced
  repetition questions so concepts are internalized.

Learning progress is tracked globally, not per-repo: see the SRS deck and the
"continual learning" defaults in `~/.claude/CLAUDE.md`.
