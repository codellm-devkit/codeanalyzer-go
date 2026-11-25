# Design: streaming Neo4j push (#59, part 2)

Status: approved by the user. Branch: `fix/issue-59-oom-on-large-apps`.

## 1. Intent

`cango --emit neo4j` must not hold the whole graph in client memory.
Today the client holds three full copies at the same time. On nats-server the push
exhausted a 69 GB machine.

Part 1 of #59 (commit `99ec0b6`) bounded the Neo4j transaction size.
This design bounds the client memory of the projection and the push.

## 2. Measurements (nats-server, L4, live heap after a forced GC)

| Stage | Live heap | Note |
|---|---|---|
| After `Analyze` | 10.9 GB | analysis results (SSA, L3, L4) |
| After `v2emit.Emit`, analyzer kept | 34.1 GB | the full `v2` tree adds 23 GB |
| After `v2emit.Emit`, analyzer released | 24.5 GB | the tree alone is 13.6 GB |
| During `Project` | more than 69 GB (RAM) | `GraphRows` copy, heavy swap, stopped after 30 min |

Largest parts of the `v2` tree:

- `GlobalOrdinal` id strings use 10.2 GB.
  The `param_in` / `param_out` endpoints use 6.2 GB of that, because `Emit` mints them all at once.
- The L4 vertex body nodes and the L4 `ddg` entries use about 11 GB.

## 3. Goals, non-goals, anti-goals

Goals:

- The push never holds the full `v2` tree or the full `GraphRows`.
- The client peak is the analysis results, plus one module, plus small global sets.
- On nats-server the push completes with a peak RSS near the analysis heap (about 11 to 14 GB).
- nats-server and gitea each complete `--emit neo4j` at `-a 4` within a 16 GB container with no swap (§7, §8).
- The graph in the DB is the same as the graph the current code writes.

Non-goals:

- Push speed (larger batches, parallel writers).
- Changes to the graph vocabulary, ids, or the JSON output.

Anti-goals (a failure even if memory goes down):

- A missing edge. An edge `MATCH` drops an edge silently if an endpoint node is not in the DB.
- A change to the lazy-push rules (D5), the `--eager` purges, or the stored-level gate.
- A non-deterministic push or snapshot.

## 4. Approaches

1. **Two passes over the emitter (chosen by the user).**
   Pass 1 emits each module, pushes its nodes, and drops it.
   Pass 2 emits each module again and pushes its edges.
   There is no full tree and no temporary file. Emission and projection run two times.
2. **One pass with the edges in a temporary file.**
   The peak memory is the same as approach 1.
   It needs an edge encoder and decoder, and several GB of disk for gitea.
3. **Keep `GraphRows`, but make it smaller (Steps A and B).**
   This is the least work, but the peak still grows with the graph.
   Gitea is about 4 times as large as nats-server, so this approach does not fit.

## 5. Design

### 5.1 Emitter: a re-iterable module stream (`internal/syntactic_analysis/v2emit`)

Add an exported stream type next to `EmitTo`, for example `v2emit.Open(...) *Stream`.
It takes the same arguments as `Emit` and exposes:

- `Envelope() *v2.Analysis`: the manifest, call graph, external symbols, artifacts and dependencies.
  Its `SymbolTable`, `ParamIn`, `ParamOut` and `Degraded` fields are empty.
- `Modules() iter.Seq2[string, v2.Module]`: each module in `sortedFileKeys` order.
  Each call emits the modules again. A module is garbage after its loop step.
- `ParamIn()`, `ParamOut() iter.Seq[v2.ParamEdge]`: the same streams `EmitTo` writes.
  They are valid only after one full `Modules()` walk, because that walk fills the `l4Overlay` id map.
- `Degraded() []v2.Degraded`: valid only after one full `Modules()` walk.

The second walk must not print the `l4Overlay.apply` warnings again.
`EmitTo` and `Emit` must keep their exact output (the JSON golden and parity tests prove this).

### 5.2 Projector: per-module projection into a sink (`internal/neo4j`)

Split `projector` so that it can project one module at a time into a row sink.
The sink receives node rows and edge rows. A pass selects which rows it keeps.

The projector needs two global facts today. The streaming design gets them as follows:

- **Declared ids** (`collectDeclaredIDs`): pass 1 records the type and callable ids of each module.
  After pass 1 the set is complete. Pass 2 uses it for `GO_RESOLVES_TO` and `GO_CALLS`.
- **Ghost nodes** (`callEndpoint`, import ghosts): pass 1 records each referenced endpoint id.
  After the last module, pass 1 pushes one ghost node for each referenced id that no module declares.
  A ghost id appears one time in the push, with the same props that `Project` gives it.

The `EdgeToSymbol` rule (`GO_EMBEDS`, `GO_SATISFIES`) checks its target against one set.
That set holds the declared ids and the ghost ids, which is every node with the `:GoSymbol` label.
It does not hold body-node ids, so it stays small.

Within one module, a `RowBuilder` still removes duplicate nodes and duplicate edge facts.
Across modules, the DB `MERGE` makes a duplicate edge row a no-op.

`Project(a *v2.Analysis) GraphRows` stays.
It drives the same per-module code over an in-memory tree and collects every row.
The unit tests and the parity gate (§6) use it as the reference.

### 5.3 Bolt writer: the push order (`internal/neo4j/bolt.go`)

The writer receives rows from the sink and buffers them per group key.
When a group buffer reaches `boltBatchSize` rows, the writer runs one `UNWIND` transaction.
At the end of each phase it flushes every buffer.

| Step | Action | Why the order matters |
|---|---|---|
| 0 | `validateIdentity` on the envelope application row. `storedAppStale`. DDL. Eager whole-app purge (full + eager only). | Same as today. Nothing is written before the identity checks. |
| 1 | Push the repository nodes from the envelope. | Shared nodes. They have no module. |
| 2 | Pass 1, for each module in order: read its stored `content_hash`. If the module changed or a full upsert is forced, purge it (`--eager` only) and push its L1–L3 nodes. Always push its L4 vertex nodes, after its purge. | A module purge deletes only ids under that module prefix. So "purge m, then upsert m's L4 vertices" keeps D5 for each module. |
| 3 | Push the ghost nodes, the stand-in vertex nodes, and the application node with `degraded_json`. | The degraded list is complete only after pass 1. |
| 4 | Pass 2, for each module: push every edge. Then push `GO_CALLS`, the stand-in `GO_HAS_BODY_NODE` edges, `GO_PARAM_IN` and `GO_PARAM_OUT`. | Every node is in the DB before the first edge, so no edge `MATCH` fails. |
| 5 | `pruneVanishedModules` (full + eager only), with the module ids from the symbol table keys. | Same as today. |

A lazy push still upserts every edge, as it does today.
The push message reports the number of node rows and edge rows sent.

### 5.4 CLI (`cmd/codeanalyzer/main.go`)

The neo4j branch opens the stream and passes it to the new entry point.
It no longer calls `v2emit.Emit`. The analysis results stay live, because both passes read them.

## 6. Verification

- **Parity gate (the main test):** for the fixtures in `testdata` at `-a 4`, the streamed rows equal `Project(Emit(...))`.
  The test collects the streamed rows through a fake sink. It sorts them, removes duplicates, and compares them with the reference rows.
- **Order test (fake DB):** no edge statement runs before the last node statement.
  For each module, its purge runs before its nodes and its L4 vertices.
- **Existing tests:** every test in `bolt_session_test.go`, `project_test.go` and the v2emit golden tests passes without change.
  The exception is a test that checks the internal call order which this design changes on purpose.
- **Live checks:** run the nats-server push against the `cango-neo4j` container.
  Record the peak RSS and the wall time.
  Compare the node and relationship counts in the DB with the counts from the `99ec0b6` run (1,858,128 nodes, 10,419,341 relationships).
  Then run gitea and record the result.
- **Container check:** run the `memtest` test on nats-server and gitea with the 16 GB limit (§7), and record both peak values.

## 7. Memory-limit test harness (from the user's review)

The user asked for containers that test whether `cango` analyzes gitea and nats-server within a fixed memory limit.
The user chose testcontainers-go, as a Go test behind a build tag.

### 7.1 Files

- `scripts/memtest/Dockerfile.cango`: a two-stage build.
  - Stage 1 (`golang`) builds `cango` from the repository.
  - Stage 2 (`ubuntu:24.04`) holds the `cango` binary and a Go toolchain.
    `cango` needs the toolchain at run time, because `go/packages` runs `go list`.
- `internal/memtest/memtest_test.go`, with the build tag `memtest`.
  - It starts the official `neo4j:5.26-community` image and waits for Bolt.
  - It builds the `cango` image from `Dockerfile.cango` and starts it on the same network.
  - It runs one push for each app and checks the result.
- `go.mod` gets `github.com/testcontainers/testcontainers-go` and its `neo4j` module. Only the `memtest` test imports them.

Run: `MEMTEST_APPS=<dir>[,<dir>...] go test -tags memtest ./internal/memtest -v -timeout 6h`.
The test skips when `MEMTEST_APPS` is empty or Docker is not available.

### 7.2 Limits

| Container | Memory limit | Swap | Other settings |
|---|---|---|---|
| `cango` | 16 GB (`HostConfig.Memory`) | none (`MemorySwap` = `Memory`) | `-j` = container CPUs |
| `neo4j` | 12 GB | none | heap 8 GB, page cache 2 GB, `dbms.memory.transaction.total.max` 5.6 GB (same as the local container) |

`MEMTEST_MEMORY` changes the `cango` limit. The default is `16g`.
With no swap, the kernel stops `cango` when it exceeds the limit.
The test is therefore strict, and swap cannot hide a memory problem.

### 7.3 Inputs and caches

- The test mounts each app directory read-only at `/app`. The test apps stay outside the repository.
- A named volume holds the Go module cache (`GOMODCACHE`). The first run downloads the modules of the app, and later runs reuse them.
- Each run starts with an empty `cango` cache, so the analysis runs in full.

### 7.4 Result

For each app, the test passes when `cango` exits with code 0 and Docker reports `OOMKilled=false`.
The test also logs the peak memory of the `cango` container, from the cgroup file `memory.peak`.
The peak memory shows the margin under the limit.

### 7.5 Expected results

Both apps pass at 16 GB when this branch is complete.
nats-server needs only the streaming push (§5).
gitea was expected to need the analysis-memory work too, but it passed with the streaming push alone, with about 0.2 GiB of margin (§8).

## 8. Analysis memory (#62, split out)

The user first moved #62 into this branch, then split it back out on 2026-11-25, after the streaming push passed on both apps.
#59 closes on the streaming push alone.

Result at 16 GB, no swap: nats-server peaks at 7.8 GiB and gitea at 15.8 GiB; both pass.
Gitea also peaks at 15.8 GiB under a 24 GB limit, so its margin at 16 GB is about 0.2 GiB.
Widening that margin is #62's job.
Its draft design, with the gitea profile, is `docs/design/specs/2026-11-25-analysis-memory.md`.

## 9. Decision: the `graph.cypher` snapshot (no `--neo4j-uri`)

Today the snapshot renders the full `GraphRows` into one string, and it writes that string in one call.
It has the same memory problem as the push.

The user chose S1: the snapshot streams through the same two passes into a buffered file.
The file stays deterministic, but its row order changes.
The order is now modules in order, instead of one global sort.
The snapshot keeps its scoped wipe at the top, and all node statements stay above all edge statements.

The rejected option S2 kept the snapshot on `Project`. With S2, large apps still run out of memory on the snapshot path.

## 10. Calls made by the designer (check these)

- The emitter emits each module two times, instead of a declared-id pre-pass. This adds about 30 s on nats-server.
- The writer pushes ghost nodes one time, after pass 1, instead of per module.
- The writer pushes the application node after pass 1, because `degraded_json` needs the full module walk.
- The client does not remove duplicate edge rows across modules. `MERGE` makes them no-ops.
- The emitter source cache keeps every file's source text for the whole run, as it does today. On nats-server this is small.
- `pruneVanishedModules` and `purgeModuleQuery` stay unbatched. A follow-up issue can batch them.
- The Neo4j container uses the official image with environment settings. A separate Neo4j Dockerfile adds nothing.
- The two containers together need 28 GB. The local Docker VM has 33.6 GB.
- The harness reads the peak from `memory.peak` inside the `cango` container, after `cango` exits. A wrapper shell command prints it.

## 11. Left to the builder

- The names of the stream type, the sink interface, and the new entry point.
- How the second module walk suppresses duplicate warnings.
- The buffer structure in the Bolt writer, within the `boltBatchSize` limit.
