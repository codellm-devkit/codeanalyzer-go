# L3 dataflow metrics over the real-app corpus

L3 is the intraprocedural dataflow level. At `-a 3`, every callable with a body gets
statement-granular body nodes and three edge lists:

- `cfg`: hand-built from `go/ast`, with a defer chain and conservative panic edges.
- `cdg`: Cooper-Harvey-Kennedy post-dominators plus Ferrante-Ottenstein-Warren.
- `ddg`: syntactic may-reaching definitions over k-limited access paths, `prov:["ssa"]`.

The semantics are in [`v2-l3-l4-dataflow.md`](../specs/v2-l3-l4-dataflow.md) §4.2–§4.5.

This note records how L3 behaves on the seven-app corpus of
[`l3-eval-2026-10.md`](l3-eval-2026-10.md), which it accompanies. It covers volume, the shape of
the graphs, cost, output size and internal consistency at scale. That note holds the run
protocol, the two bugs the evaluation found, and the degraded-input details. This note reuses
its timings and extends its counts.

- **When and where:** measured 2026-10-24 on an Apple M3 Max (16 cores, 64 GB, macOS), with the
  analyzer built with go1.27.3.
- **Analyzer version:** cango 0.1.0. The evaluation binary is `fd24bdf`. HEAD of
  `feat/l3-dataflow` is `883eab1`, which adds the `<pkg>.init` call-graph fix (`679f995`) and the
  Neo4j purge fix (`6f250ea`).
- **Which outputs:** every L3 number comes from the `-a 3 -j 1` outputs in `/tmp/eval-out`,
  except dae, which uses a fresh `GOOS=linux` run of HEAD (see below). The HEAD fixes do not touch
  L3, and § Determinism and version checks this.

One analysis engine, so there is no engine comparison here. Interprocedural (L4) results will be
added under [§ L4](#l4-to-be-added).

## Results at a glance

"6 apps" is the corpus without cockroach. cockroach is degraded (no protobuf generation, 62,908
type errors), so it is reported separately throughout and never folded into the headline figures.

| | 6 apps | cockroach (degraded) |
|---|---|---|
| callables with a body | 22,971 | 82,752 |
| body nodes | 385,505 | 1,407,838 |
| `cfg` / `cdg` / `ddg` edges | 468,353 / 261,656 / 766,940 | 1,618,928 / 959,530 / 4,600,814 |
| `cfg` / `cdg` / `ddg` per callable | 20.4 / 11.4 / 33.4 | 19.6 / 11.6 / 55.6 |
| `ddg` / `cdg` | 2.93 | 4.79 |
| `exception` share of `cfg` | 31.1% | 29.7% |
| L3 wall time over `-a 2`, `-j 1` | +2.33 s on 9.84 s (19% of `-a 3` wall) | +10.28 s on 23.08 s (31%) |
| L3 throughput, `-j 1` | 165k body nodes/s | 137k body nodes/s |
| `analysis.json`, `-a 2` to `-a 3` | 82.9 to 243.7 MB (2.9x) | 335.5 to 1,112.4 MB (3.3x) |
| consistency checks (§ below) | all PASS | all PASS |
| `-j 1` = `-j 16` = `-j 16`, byte-identical | yes (6/6) | yes |

**Key takeaways.**

1. **L3 is cheap in time, not in bytes.** It adds 19% to `-a 3` wall time at `-j 1` (8% at
   `-j 16`) and at most 14% peak RSS at `-j 1`. But it makes `analysis.json` 2.9x larger.
   Two-thirds of an `-a 3` file is L3, and the largest single part of that (41%) is the global
   `id` repeated on every body node, not the edges.
2. **Panic edges shape the CFG and CDG.** The conservative `canPanic` gives 62% of
   non-synthetic CFG nodes an `exception` edge, so 31% of `cfg` edges are `exception`. 35% of
   `cdg` edges hang off a controller whose only way to branch is that edge: "the next statement
   is control-dependent on this call, because the call may panic."
3. **The DDG is long-tailed, and weak defs drive the tail.** The median callable has 4–13 `ddg`
   edges, but the top 1% of callables hold 18–53% of each app's edges. In those callables, a
   reference-typed base (a receiver `c`, a router `g`) is weak-defined by every call and never
   killed, and its distinct field writes never kill each other. Every use of the bare base then
   links to every reaching def, giving def x use blocks that are up to 100% dense.
4. **Wildcards are rare in hand-written code and concentrate in generated code.** Wildcard or
   pseudo `var`s are 2.7% of `ddg` edges on the 6 apps. On cockroach they are 15%, almost all
   k-limit truncations in `execgen` (`.eg.go`) code. There, spelled-out embedded fields use up the
   k=3 budget: `o.builderState.left.groupsIdx` becomes `o.mergeJoinBase.builderState.*`.
   Generated `.eg.go` code holds 61% of cockroach's `ddg`.
5. **Internal consistency holds at full scale, with no sampling.** Every check passes on all
   105,723 bodies: one `@entry`/`@exit` each, every endpoint a body key, no satellite endpoint,
   every node on an entry-to-exit path, no `cdg` into `@exit`, all `prov == ["ssa"]`. The
   measured inputs leave one risk open: protobuf-generated marshallers. No tree in the corpus
   contains one, and a reviewer probe put a single one at 1.27M `ddg` edges.

## How to reproduce

```bash
cd codeanalyzer-go
GOTOOLCHAIN=go1.27.3 go build -o /tmp/cango-eval ./cmd/codeanalyzer   # must be >= target go lines
scripts/eval_l3.sh /tmp/eval/cobra /tmp/eval/listmonk /tmp/eval/memos \
  /tmp/eval/nats-server /tmp/eval/openbao
GOOS=linux scripts/eval_l3.sh /tmp/eval/dae
scripts/eval_l3.sh /tmp/eval/cockroach

# L3 metrics and consistency checks, one JSON object per app:
python3 -I scripts/l3_metrics.py cobra=/tmp/eval-out/cobra-a3j1/analysis.json \
  nats-server=/tmp/eval-out/nats-server-a3j1/analysis.json ... > metrics.jsonl
```

- **Clones and setup.** Clones and pinned commits are as in `l3-eval-2026-10.md` § Reproducing:
  `git clone --filter=blob:limit=2m`, then `git checkout <sha>`, and for dae, `make ebpf`.
- **What `eval_l3.sh` does.** It runs a warm-up, `-a 2 -j 1`, `-a 3 -j 1` and two `-a 3 -j N`,
  all `--eager`. It records wall time and peak RSS from `/usr/bin/time -l`, then compares the
  three `-a 3` outputs byte for byte.
- **What `scripts/l3_metrics.py` does.** It produces every count, ratio, distribution and check in
  this note. It is committed with this note, has no dependencies and loads one file at a time.
  The 1.1 GB cockroach file needs about 11 GB of RAM and 35 s.
- **dae.** The run used here is HEAD (`883eab1`), started with
  `GOOS=linux /tmp/cango-metrics -i /tmp/eval/dae -a 3 -j 16 --eager --app-name dae`, plus one
  `GOOS=linux scripts/eval_l3.sh` pass for its timings.

## Corpus

LOC is from the analyzer plan's Task 13 table (2026-10-23) and excludes tests, generated files and
vendor. "Source analyzed" is the total `module.source` in `analysis.json`, which includes
generated files.

| app | domain | pinned | LOC | source analyzed | why it is in the set |
|---|---|---|---|---|---|
| spf13/cobra | CLI framework (library) | `adbc881` | 7k | 0.2 MB | smoke test; no concurrency |
| knadh/listmonk | newsletter / mailing-list manager | `8216c4f` | 25k | 0.8 MB | small HTTP-to-SQL app: echo handlers into `sqlx` prepared statements |
| usememos/memos | note-taking service | `28719a9` | 96k | 3.6 MB | HTTP to SQL through an interface (`store.Driver`, three implementations) |
| nats-io/nats-server | messaging server | `f8f6db8` | 151k | 4.5 MB | CFG stress: cyclomatic p99 56, max 327; 140 `select`, 855 `defer` |
| openbao/openbao | secrets manager (Vault fork) | `1d58653` | 294k | 7.6 MB | large app; 1,944 `defer` |
| daeuniverse/dae | eBPF transparent proxy | `e3fee8f` | 65k | 2.0 MB | eBPF, cgo-adjacent, heavy `select`; Linux-only |
| cockroachdb/cockroach | distributed SQL database | `d30c905f` | 1.95M | 79.4 MB | scale test (**degraded**: no protobuf generation) |

The plan's counts were taken with other tools and do not always match what cango analyzes:

- **Module scope.** cango loads one Go module per run.
  - openbao's 1,944 `defer` include the `api/` and `sdk/` submodules (533 by grep). The root
    module's are about 1,413, against 1,411 deferred call nodes emitted.
  - nats-server's 855 include 10 in non-host platform files. That leaves 845, against 844
    emitted.
- **Cyclomatic.** The plan's "max 327" for nats-server is from a different counter. cango's
  `metrics.cyclomatic` for `server.client.parse` is 453.

## Per-app counts

The columns fall into three groups.

**Structure.**

- `modules`: `symbol_table` entries.
- `types`: type declarations of every `kind` (struct, interface, alias, defined).
- `callables`: every callable node, closures included at any nesting depth.
- `w/ body`: callables that carry a `cfg`. The difference from `callables` is exactly the
  interface methods (signature only). cockroach also has 4 bodyless functions (assembly or
  linkname stubs).

**Nodes.**

- `body nodes`: every body-map entry of a callable with a body.
- `satellite`: `call` nodes with a `parent`. They are nested inside a statement and are never CFG
  steps.
- `deferred` / `goroutine`: `call` nodes with `is_deferred` / `is_goroutine`.

**Edges.** Each overlay is an edge list.

- `cfg`: control-flow edges, deduplicated by `(src, dst, kind)`.
- `cdg`: control-dependence edges `{src, dst}`.
- `ddg`: def-to-use pairs, one per `(def node, use node, use spelling)`. So this column is the
  du-pair count.

| app | modules | types | callables | functions / methods / closures | w/ body | body nodes | satellite | deferred | goroutine | cfg | cdg | ddg |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| cobra | 18 | 17 | 320 | 106 / 164 / 50 | 319 | 3,671 | 1,106 | 14 | 0 | 4,166 | 2,239 | 6,261 |
| listmonk | 101 | 191 | 820 | 200 / 533 / 87 | 800 | 16,252 | 5,863 | 51 | 17 | 17,502 | 9,815 | 39,287 |
| memos | 320 | 770 | 6,076 | 1,350 / 4,193 / 533 | 5,536 | 68,823 | 20,986 | 424 | 14 | 75,289 | 39,551 | 83,413 |
| nats-server | 97 | 749 | 5,279 | 1,114 / 3,412 / 753 | 5,041 | 105,409 | 24,890 | 844 | 81 | 141,829 | 81,598 | 340,338 |
| openbao | 702 | 1,136 | 8,712 | 1,898 / 5,664 / 1,150 | 8,447 | 147,951 | 41,526 | 1,411 | 125 | 177,091 | 98,844 | 230,816 |
| dae (linux) | 226 | 482 | 2,891 | 884 / 1,535 / 472 | 2,828 | 43,399 | 11,173 | 451 | 74 | 52,476 | 29,609 | 66,825 |
| **6 apps** | **1,464** | **3,345** | **24,098** | **5,552 / 15,501 / 3,045** | **22,971** | **385,505** | **105,544** | **3,195** | **311** | **468,353** | **261,656** | **766,940** |
| cockroach (degraded) | 5,953 | 15,002 | 88,765 | 21,223 / 53,058 / 14,484 | 82,752 | 1,407,838 | 406,091 | 6,812 | 274 | 1,618,928 | 959,530 | 4,600,814 |
| 7 incl. cockroach | 7,417 | 18,347 | 112,863 | 26,775 / 68,559 / 17,529 | 105,723 | 1,793,343 | 511,635 | 10,007 | 585 | 2,087,281 | 1,221,186 | 5,367,754 |

`call_graph` edges, for reference, are L2 and outside this note:

| app | edges | in-project / external | source run |
|---|---|---|---|
| cobra | 803 | 505 / 298 | `fd24bdf`; HEAD gives identical output |
| listmonk | 3,703 | 1,439 / 2,264 | `fd24bdf`; HEAD gives identical output |
| memos | 12,009 | 4,248 / 7,761 | `fd24bdf`; HEAD gives identical output |
| nats-server | 19,384 | 11,630 / 7,754 | `fd24bdf`; HEAD gives identical output |
| openbao | 23,914 | 10,431 / 13,483 | `fd24bdf`; HEAD gives identical output |
| dae | 7,985 | 3,230 / 4,755 | post-fix (HEAD) |
| cockroach | 212,605 | 156,876 / 55,729 | post-fix (HEAD) |

The dae and cockroach `call_graph` numbers are the post-fix ones from the rerun table in
`l3-eval-2026-10.md`. The `/tmp/eval-out` files for those two apps predate `679f995`, and their
`call_graph` is short by 10 and 913 `init` edges respectively.

## Edge and node kinds

### Body nodes by kind

| app | `call` | `statement` | `branch` | `loop` | `switch` | `return` | `entry` | `exit` | `call` share | satellite share of `call` |
|---|---|---|---|---|---|---|---|---|---|---|
| cobra | 1,474 | 633 | 448 | 88 | 7 | 383 | 319 | 319 | 40.2% | 75% |
| listmonk | 7,054 | 3,017 | 2,174 | 254 | 59 | 2,094 | 800 | 800 | 43.4% | 83% |
| memos | 23,781 | 12,481 | 8,914 | 690 | 216 | 11,669 | 5,536 | 5,536 | 34.6% | 88% |
| nats-server | 38,324 | 27,310 | 16,222 | 1,905 | 598 | 10,968 | 5,041 | 5,041 | 36.4% | 65% |
| openbao | 51,914 | 33,202 | 21,007 | 1,829 | 1,099 | 22,006 | 8,447 | 8,447 | 35.1% | 80% |
| dae | 15,353 | 9,412 | 6,015 | 753 | 355 | 5,855 | 2,828 | 2,828 | 35.4% | 73% |
| **6 apps** | **137,900** | **86,055** | **54,780** | **5,519** | **2,334** | **52,975** | **22,971** | **22,971** | **35.8%** | **77%** |
| cockroach (degraded) | 512,500 | 380,432 | 164,713 | 34,813 | 9,743 | 140,133 | 82,752 | 82,752 | 36.4% | 79% |

`switch` covers expression `switch`, type `switch` and `select`, one kind per spec §4.2. Each
`case`/`default`/comm-clause head is a `statement`. The split, recovered from the first keyword of
each `switch` node's span, follows. Cases are `switch_case` out-edges per header (mean / max).

| app | expression `switch` | type `switch` | `select` | `select` per 1k body nodes | `for {}` headers | callables with a defer chain | max defers in one callable | `if err != nil` share of `branch` |
|---|---|---|---|---|---|---|---|---|
| cobra | 7 (4.4 / 5) | 0 | 0 | 0.00 | 0 | 14 | 1 | 12% |
| listmonk | 44 (3.9 / 8) | 5 (3.2 / 4) | 10 (2.0 / 2) | 0.62 | 9 | 46 | 4 | 41% |
| memos | 161 (4.4 / 35) | 43 (4.9 / 23) | 12 (2.2 / 4) | 0.17 | 12 | 409 | 3 | 39% |
| nats-server | 404 (4.7 / **77**) | 56 (3.2 / 12) | 138 (2.8 / **12**) | 1.31 | 70 | 730 | 12 | 12% |
| openbao | 833 (3.4 / 24) | 87 (2.9 / 6) | 179 (2.2 / 6) | 1.21 | 106 | 1,078 | **20** | 38% |
| dae | 197 (3.7 / 22) | 28 (3.4 / 6) | 130 (2.1 / 5) | **3.00** | 94 | 369 | 5 | 17% |
| cockroach (degraded) | 7,413 (3.7 / 60) | 1,312 (4.7 / **134**) | 1,018 (2.4 / 7) | 0.72 | 1,928 | 5,672 | 13 | 22% |

How the last three columns are counted:

- `for {}` headers are `loop` nodes whose only non-`exception` out-edge is `fallthrough`. Those are
  infinite loops, which get the synthetic `exception` edge to `@exit`.
- `if err != nil` counts `branch` nodes whose source text matches `if [init;] err != nil {`.
- "Max defers" is the most `is_deferred` call nodes in one body, i.e. the longest defer chain.

### CFG edges by kind

| app | `fallthrough` | `true` | `false` | `switch_case` | `loop_back` | `exception` | `return` | `break` | `continue` | `defer_resume` |
|---|---|---|---|---|---|---|---|---|---|---|
| cobra | 1,115 | 536 | 492 | 31 | 54 | 1,369 | 522 | 10 | 23 | 14 |
| listmonk | 4,606 | 2,419 | 2,391 | 207 | 210 | 5,347 | 2,190 | 20 | 61 | 51 |
| memos | 18,407 | 9,592 | 9,495 | 951 | 554 | 22,668 | 13,014 | 27 | 157 | 424 |
| nats-server | 40,195 | 18,056 | 17,398 | 2,476 | 2,019 | 46,427 | 13,516 | 236 | 662 | 844 |
| openbao | 47,283 | 22,727 | 22,356 | 3,440 | 1,447 | 54,066 | 23,670 | 157 | 534 | 1,411 |
| dae | 14,273 | 6,670 | 6,423 | 1,106 | 632 | 15,679 | 6,932 | 70 | 240 | 451 |
| **6 apps** | **125,879** | **60,000** | **58,555** | **8,211** | **4,916** | **145,556** | **59,844** | **520** | **1,677** | **3,195** |
| cockroach (degraded) | 493,576 | 197,549 | 188,369 | 35,711 | 31,726 | 481,327 | 170,886 | 5,067 | 7,905 | 6,812 |

### Ratios

Per-callable figures divide by callables with a body.

| app | cfg / callable | cdg / callable | ddg / callable | ddg / cdg | `call` share of body | `exception` share of cfg | `defer_resume` share of cfg | `fallthrough` share of cfg |
|---|---|---|---|---|---|---|---|---|
| cobra | 13.1 | 7.0 | 19.6 | 2.80 | 40.2% | 32.9% | 0.34% | 26.8% |
| listmonk | 21.9 | 12.3 | 49.1 | 4.00 | 43.4% | 30.6% | 0.29% | 26.3% |
| memos | 13.6 | 7.1 | 15.1 | 2.11 | 34.6% | 30.1% | 0.56% | 24.4% |
| nats-server | 28.1 | 16.2 | 67.5 | 4.17 | 36.4% | 32.7% | 0.60% | 28.3% |
| openbao | 21.0 | 11.7 | 27.3 | 2.34 | 35.1% | 30.5% | 0.80% | 26.7% |
| dae | 18.6 | 10.5 | 23.6 | 2.26 | 35.4% | 29.9% | 0.86% | 27.2% |
| **6 apps** | **20.4** | **11.4** | **33.4** | **2.93** | **35.8%** | **31.1%** | **0.68%** | **26.9%** |
| cockroach (degraded) | 19.6 | 11.6 | 55.6 | 4.79 | 36.4% | 29.7% | 0.42% | 30.5% |

### DDG detail

**Edges per callable.** The distribution is over callables with a body. "Top 1%" is the share of
the app's `ddg` held by its 1% of callables with the most `ddg` edges.

| app | median | p90 | p99 | max | callables with 0 `ddg` | top 1% hold | callables > 1,000 edges (their share) |
|---|---|---|---|---|---|---|---|
| cobra | 5 | 34 | 322 | 693 | 7 | 28% | 0 |
| listmonk | 13 | 56 | 283 | 14,752 | 9 | 53% | 4 (48%) |
| memos | 4 | 39 | 136 | 1,259 | 864 | 18% | 1 (2%) |
| nats-server | 9 | 86 | 935 | 63,517 | 102 | 47% | 44 (45%) |
| openbao | 7 | 57 | 322 | 5,784 | 595 | 28% | 16 (14%) |
| dae | 8 | 45 | 216 | 3,810 | 62 | 30% | 4 (17%) |
| **6 apps** | | | | | | | **69 (28%)** |
| cockroach (degraded) | 6 | 54 | 303 | 73,013 | 5,682 | 65% | 201 (58%) |

**Top-3 callables by `ddg` count.** The last column is the single `var` with the most edges. It
gives the number of distinct def nodes and distinct use nodes that var's edges touch, and the
density (edges / (defs x uses)).

| app | callable | `ddg` | body nodes | cyclomatic | dominant `var` |
|---|---|---|---|---|---|
| cobra | `github.com/spf13/cobra.Command.execute` | 693 | 111 | 37 | `c`: 342 edges, 25 defs x 32 uses (43%) |
| cobra | `github.com/spf13/cobra.Command.getCompletions` | 625 | 154 | 39 | `finalCmd`: 351, 22 x 30 (53%) |
| cobra | `github.com/spf13/cobra.defaultUsageFunc` | 461 | 90 | 21 | `w`: 229, 21 x 21 (52%) |
| listmonk | `github.com/knadh/listmonk/cmd.initHTTPHandlers` | 14,752 | 510 | 9 | `a`: 7,175, 60 x 181 (66%) |
| listmonk | `…/internal/messenger/postback.easyjsonDf11841fDecode…Postback2` | 1,682 | 102 | 23 | `in`: 1,457, 41 x 42 (85%) |
| listmonk | `…/internal/messenger/postback.easyjsonDf11841fDecode…Postback` | 1,506 | 95 | 25 | `in`: 1,169, 37 x 38 (83%) |
| memos | `…/proto/gen/api/apiconnect.NewUserServiceHandler` | 1,259 | 178 | 37 | `userServiceMethods`: 595, 34 x 34 (51%) |
| memos | `…/proto/gen/api.RegisterUserServiceHandlerClient` | 663 | 37 | 69 | `mux`: 595, 34 x 34 (51%) |
| memos | `…/proto/gen/api.RegisterUserServiceHandlerServer` | 663 | 37 | 69 | `mux`: 595, 34 x 34 (51%) |
| nats-server | `github.com/nats-io/nats-server/v2/server.client.parse` | 63,517 | 917 | 453 | `c`: 50,623, 236 x 232 (**92%**) |
| nats-server | `…/server.raft.processAppendEntry` | 9,065 | 358 | 89 | `n`: 5,404, 132 x 146 (28%) |
| nats-server | `…/server.Server.updateAccountClaimsWithRefresh` | 4,231 | 410 | 120 | `a`: 1,999, 78 x 53 (48%) |
| openbao | `github.com/openbao/openbao/v2/internal/command.ServerCommand.Run` | 5,784 | 548 | 120 | `c`: 1,513, 120 x 43 (29%) |
| openbao | `…/internal/command/server.Config.Merge` | 4,034 | 177 | 50 | `result`: 3,822, 87 x 87 (50%) |
| openbao | `…/internal/vault.NewTestCluster` | 2,649 | 344 | 70 | `coreConfig`: 1,172, 53 x 48 (46%) |
| dae | `github.com/daeuniverse/dae/cmd.reloadWorker.run` | 3,810 | 346 | 53 | `w.log`: 1,560, 40 x 39 (**100%**) |
| dae | `…/cmd.Runner.Run` | 3,790 | 419 | 99 | `reloadManager`: 1,127, 38 x 38 (78%) |
| dae | `…/control.NewControlPlaneWithContextOptions` | 2,496 | 480 | 106 | `log`: 955, 41 x 46 (51%) |
| cockroach | `…/sql/colexec/colexecjoin.mergeJoinFullOuterOp.buildLeftGroupsFromBatch.closure_1` | 73,013 | 1,058 | — | `o`: 30,954, 201 x 154 (**100%**) |
| cockroach | `…/colexecjoin.mergeJoinRightAntiOp.buildLeftGroupsFromBatch.closure_1` | 73,013 | 1,058 | — | same shape |
| cockroach | `…/colexecjoin.mergeJoinRightOuterOp.buildLeftGroupsFromBatch.closure_1` | 73,013 | 1,058 | — | same shape |

**`var` classes.** Classes are by the spelling of the edge's `var`:

- plain: no wildcard;
- `[*]`: has an element step;
- trailing `.*`: a dereference, k-limit truncation, or callee-may-write wildcard;
- both: `[*]` and a trailing `.*`;
- `<defer>:` pseudo: the defer registration to deferred-call path.

"Bare base" means the `var` is a single identifier (`c`, not `c.f`), which matches every reaching
def under that base. "Def at/after use" means the def node's position is not before the use's.
It is an approximate count of loop-carried edges.

| app | plain | `[*]` | trailing `.*` | both | `<defer>:` | wildcard or pseudo | bare base | def at/after use |
|---|---|---|---|---|---|---|---|---|
| cobra | 97.64% | 2.09% | 0.05% | 0.00% | 0.22% | 2.36% | 80% | 4% |
| listmonk | 98.64% | 0.99% | 0.24% | 0.00% | 0.13% | 1.36% | 83% | 8% |
| memos | 97.03% | 1.69% | 0.77% | 0.00% | 0.51% | 2.97% | 80% | 3% |
| nats-server | 97.23% | 1.43% | 1.09% | 0.01% | 0.25% | 2.77% | 71% | 17% |
| openbao | 97.43% | 1.80% | 0.15% | 0.01% | 0.61% | 2.57% | 70% | 5% |
| dae | 97.00% | 2.14% | 0.19% | 0.00% | 0.67% | 3.00% | 68% | 12% |
| **6 apps** | **97.32%** | **1.61%** | **0.64%** | **0.01%** | **0.42%** | **2.68%** | **72%** | **11%** |
| cockroach (degraded) | 84.69% | 1.33% | **13.82%** | 0.01% | 0.15% | 15.31% | 63% | 30% |

No pseudo path other than `<defer>:` appears in any output.

**`ddg` by file class.** The class is assigned by the file the callable is declared in:
`*.pb.go` is protobuf, `*.eg.go` is cockroach `execgen`, and easyjson / `_gen.go` / `/gen/` files
are other generated code.

| app | hand-written | protobuf | execgen | other generated |
|---|---|---|---|---|
| listmonk | 84.1% | — | — | 15.9% |
| memos | 60.5% | 36.1% | — | 3.4% |
| openbao | 99.0% | 1.0% | — | — |
| dae | 99.5% | 0.5% | — | — |
| cobra, nats-server | 100% | — | — | — |
| **6 apps** | **94.5%** | **4.3%** | — | **1.2%** |
| cockroach (degraded) | 38.7% | 0 (none generated) | **60.8%** | 0.6% |

## Cost: time, memory, output size

**Time and memory.** All runs are `--eager`, one timed run each, with wall time and peak RSS from
`/usr/bin/time -l` (MB = 2^20 bytes).

- **Source of the figures.** Rows other than dae are the `fd24bdf` runs in
  `l3-eval-2026-10.md`. dae is the HEAD `GOOS=linux` `eval_l3.sh` pass. Its `-a 3` figures (1.72 s,
  1.50 s, 1,419 MB) differ from that note's `fd24bdf` run (1.74 s, 1.44 s, 1,423 MB) by run-to-run
  noise.
- **L3 delta and throughput.** The L3 delta is `-a 3 -j 1` minus `-a 2 -j 1`. Throughput is body
  nodes divided by that delta.
- **Noise.** A single run on a laptop puts noise at roughly ±0.05 s on the small apps; this is an
  estimate, since there was no repeat timing. So cobra's +0.02 s delta is not meaningful.

| app | `-a 2 -j 1` (s) | `-a 3 -j 1` (s) | L3 delta | L3 share of `-a 3` | `-a 3 -j 16` (s) | `-j 16` speedup | 2nd `-j 16` (s) | RSS `-a 2` | RSS `-a 3 -j 1` | RSS `-a 3 -j 16` | L3 throughput (nodes/s) |
|---|---|---|---|---|---|---|---|---|---|---|---|
| cobra | 0.30 | 0.32 | +0.02 | 6% | 0.31 | 1.03x | 0.31 | 243 | 262 | 265 | n/a |
| listmonk | 0.69 | 0.83 | +0.14 | 17% | 0.74 | 1.12x | 0.75 | 835 | 890 | 882 | 116,086 |
| memos | 1.96 | 2.34 | +0.38 | 16% | 2.13 | 1.10x | 2.27 | 2,384 | 2,380 | 2,549 | 181,113 |
| nats-server | 1.47 | 2.19 | +0.72 | 33% | 1.77 | 1.24x | 1.82 | 937 | 1,069 | 1,159 | 146,401 |
| openbao | 3.96 | 4.77 | +0.81 | 17% | 4.20 | 1.14x | 4.22 | 4,496 | 4,547 | 5,158 | 182,656 |
| dae (linux, HEAD) | 1.46 | 1.72 | +0.26 | 15% | 1.50 | 1.15x | 1.46 | 1,296 | 1,419 | 1,537 | 166,919 |
| **6 apps** | **9.84** | **12.17** | **+2.33** | **19%** | **10.65** | **1.14x** | | | | | **165,453** |
| cockroach (degraded) | 23.08 | 33.36 | +10.28 | 31% | 26.96 | 1.24x | 27.03 | 13,084 | 13,473 | 13,268 | 136,949 |

**Output size.** `analysis.json` sizes are on disk; for dae, `-a 2` is the HEAD linux run. The
per-section bytes ("L3 composition") were measured by re-serializing each callable's `body`,
`cfg`, `cdg` and `ddg` compactly, as cango writes them, and are given as a share of the L3 delta
in bytes:

- body-node `id`s: the `"id":"<callable-id>@<key>"` string on every body node;
- other: the new statement nodes' spans, kinds and `parent` fields.

| app | `-a 2` MB | `-a 3` MB | growth | L3 share of file | bytes / body node | L3 composition: body-node `id`s / `ddg` / `cfg` / `cdg` / other | `-a 3` bytes per source byte |
|---|---|---|---|---|---|---|---|
| cobra | 0.8 | 2.0 | 2.4x | 59% | 566 | 32 / 31 / 16 / 6 / 15% | 9.0 |
| listmonk | 3.8 | 10.0 | 2.6x | 62% | 643 | 34 / 35 / 13 / 5 / 13% | 13.9 |
| memos | 15.7 | 39.8 | 2.5x | 61% | 606 | 45 / 21 / 14 / 5 / 15% | 11.5 |
| nats-server | 21.1 | 72.5 | 3.4x | 71% | 721 | 32 / 38 / 13 / 5 / 11% | 16.8 |
| openbao | 32.3 | 94.3 | 2.9x | 66% | 668 | 47 / 22 / 13 / 5 / 13% | 13.0 |
| dae | 9.1 | 25.3 | 2.8x | 64% | 610 | 41 / 25 / 15 / 5 / 14% | 13.2 |
| **6 apps** | **82.9** | **243.7** | **2.9x** | **66%** | **663** | **41 / 28 / 13 / 5 / 13%** | **13.6** |
| cockroach (degraded) | 335.5 | 1,112.4 | 3.3x | 70% | 829 | 39 / 38 / 10 / 4 / 10% | 14.7 |

## Internal-consistency checks

Every check ran on **every** callable with a body (22,971 on the 6 apps, 105,723 with
cockroach), with no sampling, using `scripts/l3_metrics.py`.

| # | check | 6 apps | with cockroach | result |
|---|---|---|---|---|
| 1 | `entry` count == `exit` count == callables with a body, and each body has exactly one `@entry` and one `@exit` key | 22,971 = 22,971 = 22,971; 0 bodies off | 105,723 each; 0 off | **PASS** |
| 2 | every `cfg` / `cdg` / `ddg` endpoint is a body key of its own callable | 0 bad of 1,496,949 edges | 0 bad of 8,676,221 | **PASS** |
| 3 | no satellite `call` node is a `cfg`, `cdg` or `ddg` endpoint | 0 | 0 | **PASS** |
| 4 | every non-satellite node is reachable from `@entry` and reaches `@exit` (forward and backward BFS per body; full, not sampled) | 279,961 nodes checked; 0 unreachable, 0 not reaching exit | 1,281,708 checked; 0 / 0 | **PASS** |
| 5 | each `return` node has exactly one `return` edge; `return` edges = `return` nodes + fall-off-end edges | 0 off; 59,844 = 52,975 + 6,869 | 0 off; 230,730 = 193,108 + 37,622 | **PASS** |
| 6 | no `cdg` edge into `@exit`; no `cdg` self-edge | 0 / 0 | 0 / 0 | **PASS** |
| 7 | every `ddg` edge has `prov == ["ssa"]` | 0 of 766,940 differ | 0 of 5,367,754 | **PASS** |
| 8 | no duplicate `cfg` edge `(src, dst, kind)` | 0 | 0 | **PASS** |
| 9 | one `defer_resume` edge per deferred call node, and one `<defer>:` `ddg` edge per deferred call | 3,195 = 3,195 = 3,195 | 10,007 each | **PASS** |
| 10 | at most one `exception` edge per node (edges == nodes carrying one) | 145,556 = 145,556 | 626,883 = 626,883 | **PASS** |
| 11 | `-j 1` == `-j 16` == 2nd `-j 16`, byte-identical | 6 / 6 | 7 / 7 | **PASS** |

Notes on checks 5 and 9:

- **Check 5.** `return` is also the kind of a fall-off-end edge, so `return` edges exceed `return`
  nodes by exactly the fall-off count. Fall-off edges leave the last straight-line node, branch or
  loop of a body. An empty body has a single `@entry -> @exit` `return` edge: there are 478 of
  those (memos has 345, e.g. no-op marker methods such as
  `filter.ComparisonCondition.isCondition`). 11,962 of the 59,844 `return` edges go to the first
  member of a defer chain, not to `@exit`.
- **Check 9.** It confirms the §4.3.1 split end to end, by totals. Each `defer` gives a
  registration `statement`, a deferred `call`, one `defer_resume` edge (chain-internal or tail to
  `@exit`), and one registration-to-call `<defer>:` du-pair, and the three totals agree.

`true` exceeds `false` by 1,445 on the 6 apps. That matches, to within 5, the 1,450 `branch`/`loop`
headers whose not-taken arm falls off the end of the function, which is therefore kind `return`,
not `false`.

### Determinism and version

- **The five fully-built apps.** The three `-a 3` outputs are byte-identical at `fd24bdf`. A HEAD
  `-j 16` rerun is byte-identical to them.
- **dae.** Three HEAD `GOOS=linux` runs (`-j 1`, `-j 16`, `-j 16`) are byte-identical. Against a
  `GOOS=linux` run of `fd24bdf`, the hash of everything except `call_graph` is identical, and so is
  the hash of the L3 lists (`body`, `cfg`, `cdg`, `ddg` of every callable). The only difference is
  the 10 restored `init` edges (7,975 to 7,985). The same holds for darwin: the HEAD darwin run and
  the on-disk pre-fix darwin run differ only in `call_graph` (7,680 to 7,690).
- **cockroach.** Three HEAD runs are byte-identical. Their non-`call_graph` content hashes equal
  the pre-fix `/tmp/eval-out` output's, so the L3 figures above hold at HEAD.

## Observations

### Where L3 cost goes

- **Loading and type-checking dominate.** L3 adds 2.33 s to 9.84 s of `-a 2` across the 6 apps
  (19% of `-a 3` wall at `-j 1`), and 10.3 s on cockroach.
- **`-j` only parallelizes L3.** `-j` feeds only the per-callable L3 worker pool
  (`dataflow.Run`), so the `-j 16` speedup of the whole run (1.03–1.24x) is Amdahl-bound by the
  serial front. Since `-j` leaves the earlier phases unchanged, the L3 delta at `-j 16` can be
  taken against the `-a 2 -j 1` baseline. It shrinks from 0.72 to 0.30 s on nats-server and from
  10.28 to 3.88 s on cockroach, about 2.5x. At `-j 16`, L3 is 8% of 6-app wall time.
- **Edges, not nodes, seem to set L3's cost.** No per-phase profile was taken; this is an
  inference from the throughput column. nats-server has 29% fewer body nodes than openbao but 47%
  more `ddg` edges, and almost the same L3 delta (0.72 vs 0.81 s). Its throughput is accordingly
  the lowest of the large apps (146k vs 183k nodes/s).
- **The known worst case was not measured here.** A reviewer probe of one 6,000-case function took
  15 s in `BuildDDG`'s kill computation, which is O(nodes x defs) per body (§ Limitations). No
  corpus function triggers it: the largest switch has 134 cases.
- **Memory.** L3 adds 0–14% peak RSS at `-j 1` (most on nats-server, 937 to 1,069 MB). At
  `-j 16` it adds up to 24% (nats-server, 1,159 MB), presumably because 16 workers hold
  per-callable scratch at once (not profiled). `go/packages` type information dominates RSS.

### How Go idioms show up in the graph

- **The error idiom is visible in the node mix.** `if err != nil` is 28% of all `branch` nodes on
  the 6 apps (38–41% in the HTTP/SQL services listmonk, memos, openbao; 12% in cobra and
  nats-server). It accounts for much of Go's 2.4 `branch` and 2.3 `return` nodes per callable.
- **Defer chains are short and common.** 2,646 of 22,971 bodies (12%) have a defer chain, and the
  median chain has one member. The longest is 20 defers (openbao) or 12 (nats-server).
  - Every exit funnels through the chain: 39,567 `exception` and 11,962 `return` edges enter a
    chain rather than `@exit`.
  - Deferred calls that may themselves panic add 549 chain-internal `exception` edges.
  - `defer_resume` is under 1% of `cfg` edges everywhere. The defer model costs little volume.
- **`select` is where the concurrency-heavy apps differ.** dae has 3.0 `select` headers per 1,000
  body nodes, against 1.2–1.3 for nats-server and openbao and at most 0.6 for the rest.
  - `select` fan-out is narrow: a mean of 2.1–2.8 comm clauses, max 12 in nats-server.
  - Infinite `for {}` loops cluster in the same apps: 94 in dae, 70 in nats-server, 106 in openbao,
    against 0–12 in the rest. Each one carries a synthetic `exception` edge to `@exit`.
- **Wide switches are where cyclomatic complexity lives.** nats-server has the widest expression
  `switch` (77 cases), and cockroach a 134-case type switch.
  `server.client.parse`, nats-server's protocol state machine, is cyclomatic 453 in one body: a
  byte loop around a state `switch`.

### Over-approximation, measured

- **Panic edges.** 62% of non-synthetic CFG nodes on the 6 apps carry an `exception` edge, so
  `exception` is the most common kind, ahead of `fallthrough` (31.1% vs 26.9%). The cause is
  §4.3.2's rule: any call, index, dereference, division, send or unchecked assertion may panic.
  This is the price of having the defer chain and `@formal_out` see panicking paths. It is sound
  for taint, but it blurs the CFG for readers who want "normal" control flow. Filtering on
  `kind != "exception"` recovers that view exactly, since each node has at most one such edge.
- **Panic-induced control dependence.** 34.9% of `cdg` edges (36.5% with cockroach) come from a
  controller with no `true`/`false`/`switch_case` out-edge. Its only second successor is its
  `exception` edge. So in `a(); b()`, `b` is control-dependent on `a`. A backward slice over `cdg`
  therefore pulls in every earlier panic-capable statement on the path. A further 10.6% of `cdg`
  edges are the `@entry` anchors for uncontrolled nodes (spec §4.4).
- **Weak defs, and defs of distinct fields, never kill each other.** A call weak-defines `a.*`
  for every reference-typed argument and receiver, and `x.f = …` kills only `x.f…`. So along any
  path the defs on a long-lived receiver (`c`, `s`, `mset`) only accumulate. 72% of `ddg` edges
  are uses of a bare base, which by the interference rule (`u.Covers(d)`) matches every reaching
  def under that base.
- **Wildcards themselves are a minor contributor in hand-written code.** They are 2.3% of 6-app
  edges, mostly `[*]` element writes. The inflation comes from bare-base uses meeting accumulated
  weak defs, not from the wildcard spelling.

### The DDG long tail

The long tail is one mechanism repeated: an inner loop or a long straight-line sequence of calls
on one reference-typed base.

- **nats-server `client.parse`.** It holds 63,517 edges, 19% of the app's `ddg`.
  - 236 nodes define a path under `c`: field writes such as `c.state = …`, plus every call with
    `c` as pointer receiver, which weak-defines `c.*`. 232 nodes use bare `c`.
  - They sit inside a single loop, so every def reaches every use: 92% of the 236 x 232 matrix is
    filled.
  - 17% of nats-server's edges are def-at-or-after-use (loop-carried), the highest of the 6 apps.
- **listmonk `initHTTPHandlers`.** It holds 14,752 edges, 38% of the app's `ddg`.
  - It is straight-line route registration: 497 call nodes, cyclomatic 9.
  - Each `g.GET(...)` on the `*echo.Group` weak-defines `g.*` and uses `g`, so every registration
    links to every earlier one: 185 defs x 185 uses, 21% filled, cut into blocks by the two
    `g := e.Group(…)` and one `g = e.Group(…)` strong defs. The `a` (`*App`) edges are 66%
    filled.
  - Complexity metrics do not predict this tail. Call density on one base does.
- **openbao `Config.Merge`.** Each of 87 `result.X = …` statements strong-defines only
  `result.X`, and also uses bare `result` to write through it. A def of `result.X` kills only
  `result.X…`, so the defs of distinct fields accumulate, and each statement's `result` use
  matches every earlier one. The result is an exact triangle: 87 x 87, 50% filled, 3,822 `result`
  edges for 177 body nodes.
- **cockroach `execgen`.** The generated merge-joiner closures (73,013 edges each, 1,058 nodes)
  are the same pattern, 100% dense on `o`. They are also where wildcards bloat.
  - `o.builderState.left.groupsIdx` is spelled
    `o.mergeJoinBase.builderState.left.groupsIdx`, because promoted embedded fields are spelled
    out (§4.5).
  - At k=3 that truncates to `o.mergeJoinBase.builderState.*`, which interferes with every other
    `builderState` path.
  - 621,284 of cockroach's 635,616 trailing-`.*` edges are k-truncations (two or more field steps
    before the `.*`) in `.eg.go` files, by a one-off split of that class.
  - **Probe** (one `-j 16` run of HEAD with `--graph-field-depth 4`): total `ddg` falls 7.5%,
    4,600,814 to 4,255,911, and the trailing-`.*` class falls from 635,616 to 10,769. The top
    callables become `probeBody*` at about 60,800 edges each.
  - Embedded-field spelling spends the k budget, and it is the main reason cockroach's wildcard
    share (15%) is 6x the corpus's.

### Output size

**Output size is the main caution.**

- **Growth.** `-a 3` is 2.9x `-a 2` on the 6 apps and 3.3x on cockroach, at 663 to 829 bytes per
  body node and about 13.6 bytes of JSON per byte of analyzed source. The 1.1 GB cockroach file
  takes about 11 GB of RAM to load whole in Python.
- **What the bytes are.** The global body-node `id` is the largest part of L3's bytes, at 41%
  (39% on cockroach). It averages about 165–225 bytes per node by app, because it repeats the full
  callable id for every node. `ddg` is next at 28% (38% on nats-server and cockroach, the
  long-tail apps).
- **The `id` is derivable.** It is `<callable-id>@<key>`. It is present for SDK parity with Python
  (#176; spec §4.2), so dropping or compressing it is a schema-contract decision, not a free win.
  The measurement says that decision is worth about a quarter of an `-a 3` file
  (41% x 66% ≈ 27%; this is an estimate that ignores any new framing).

### Context: Java's AST-engine figures

The Java sibling's AST engine is the nearest syntactic L3, measured on a different corpus (ten
Java apps; see `codeanalyzer-java/docs/design/notes/l3-dataflow-metrics.md`). The ratios below
are context for consumers moving between languages, not a quality comparison. The two engines
make different, documented modelling choices, and the corpora differ in domain and size.

| ratio | Go, 6 apps | Java AST engine, 10 apps |
|---|---|---|
| body nodes / callable | 16.8 | 11.3 |
| `cfg` / callable | 20.4 | 6.6 |
| `cdg` / callable | 11.4 | 1.5 |
| `ddg` / callable | 33.4 | 3.2 |
| `ddg` / `cdg` | 2.93 | 2.12 |
| `call` share of body | 35.8% | 62.3% |
| `exception` share of `cfg` | 31.1% | 3.1% |
| `fallthrough` share of `cfg` | 26.9% | 75.5% |
| bytes / body node | 663 | about 1,700 (commons-lang only; derived from "~222 MB" / 128,961 nodes) |

The large differences have identifiable causes.

- **Exception edges.** Java's are syntactic throwers inside a `try` only. Go's `canPanic` applies
  everywhere, because a panic always reaches the defer chain. That alone explains the `exception`
  share, and through panic-induced control dependence, about a third of Go's `cdg`.
- **Go's error idiom adds explicit branches and returns.** Go has 2.4 branches and 2.3 returns per
  callable, against Java's 0.28 and 0.55. That lowers `call`'s share of the body and adds `cdg`.
- **Go's weak-def rule for reference-typed arguments and receivers** has no counterpart that
  would generate comparable `ddg` density in an object-insensitive Java AST pass. The ddg/cdg
  ratio, which normalizes away the control-side differences, is the closest of the figures
  (2.9 vs 2.1).

## Limitations and known gaps

### Degraded inputs

- **cockroach: no protobuf generation.** Bazel was not available, so the tree has 0 `.pb.go`
  files, 62,908 type-error warnings across 633 packages, and cgo failures. Every cockroach number
  here is real for that tree, but it is not a measurement of cockroach as built. It is reported
  separately and excluded from the 6-app totals.
- **dae: the on-disk outputs are the darwin run.** That run is degraded: 90 type errors in 4
  packages, 2,750 bodies, 41,732 nodes, and 50,605 / 28,564 / 64,746 `cfg` / `cdg` / `ddg`. Every
  dae number here is from a HEAD `GOOS=linux` run, which has 0 type errors.

### Unmeasured: protobuf-generated DDG volume (open risk)

- **No measured tree contains a gogoproto-style marshaller.** A grep for `dAtA[` finds 0 files in
  all seven trees. cockroach's gogoproto output was never generated, and memos's protobuf code is
  `google.golang.org/protobuf` (reflection-based, with no generated marshal bodies). So the 4.3%
  protobuf `ddg` share on the 6 apps is gRPC-gateway and connect registration code, not
  marshallers.
- **A reviewer probe shows the risk.** One 600-field protobuf-style marshaller produced
  **1.27M `ddg` edges and 77 MB of JSON**. Every `dAtA[i] = …` is a weak def of `dAtA[*]` that is
  never killed, and every later `dAtA[i]` read and `i` update meets all of them.
- **What it would mean for cockroach.** cockroach has 182 `.proto` files (about 1,847 `message`
  declarations by a line grep). A generated tree could therefore carry a `ddg` many times larger
  than the 4.6M measured here.
- **This is an open measurement, not a result.** Neither the corpus-level volume nor the fix
  (a kill or summarization rule for indexed sequential writes, or a size guard) has been
  evaluated.

### Model limits (by design at L3)

- **Syntactic, alias-free L3.** Two paths with different bases never interact: `p := &x; p.f = 1;
  use(x.f)` has no edge. Points-to and alias flow arrive at L4 (§4.10) as additive
  `prov:["points-to"]` edges.
- **Every defer is assumed registered on every path,** with one chain member per `defer`
  statement. `recover()` does not resume normal flow, and `os.Exit`-style calls are ordinary calls
  (§4.3.1–§4.3.2).

### Gaps from the final review

**Pending before release.**

- **Writes through a subslice argument** (`buf[:n]`, `copy(dst[i:], …)`) are not yet recorded as
  weak defs of the underlying base. A fix is pending.
- **Stale docs** to update before the release.
- **Release toolchain.** A binary built with an older Go degrades silently on a newer `go` line:
  one warning per package, partial type-checking, exit 0 (`l3-eval-2026-10.md` Finding 2). Release
  binaries must be built with the newest toolchain.
- **The `//line` L2 regression.**

**Follow-ups.**

- **Type-error defs.** L3 defs can be dropped when the receiver's type failed to type-check. The
  degraded cockroach and darwin-dae `ddg` counts are therefore likely lower bounds.
- **Closure-captured writes** are not modelled at L3.
- **Value structs with reference fields** (a value receiver or argument whose fields are pointers,
  maps or slices) are not weak-defined, so writes through those fields are missed.
- **Deferred calls reading receiver state** at exit are not modelled.
- **Quadratic kill computation.** `BuildDDG` computes KILL by testing every strong def of every
  node against every def in the body. A reviewer probe of one 6,000-case switch-heavy function
  took 15 s. Nothing in this corpus comes close (the largest switch has 134 cases).

### Found while writing this note (not yet triaged)

**Closure spans in cross-file methods.** A closure nested in a cross-file method carries no
`source_file` of its own, yet its spans index the method's declaring file.

- **Scale.** This affects 847 closures on the 6 apps and 2,112 with cockroach.
- **Why it matters.** The text-recovery rule in `v2-l1-emission.md` reads "the callable's
  `source_file`, else the nesting module". Applied literally, it slices the wrong file for these
  closures' own spans and every body-node span inside them. For example, 23 `switch` nodes in
  nats-server closures nested under `jetStream` methods (type in `jetstream.go`, methods in
  `jetstream_cluster.go`) do not start with `switch` when sliced from `jetstream.go`.
- **Workaround.** Inheriting the nearest ancestor's `source_file` recovers the right text (it is
  what `scripts/l3_metrics.py` does), and it matches the emitter's intent ("closures share their
  enclosing callable's file", `v2emit/emit.go`).
- **Open decision.** The schema docs do not state that inheritance, and the SDK should not have to
  guess. Either document it or emit `source_file` on such closures.

## L4 (to be added)

L4 (interprocedural SDG) results will be added here, over the same corpus and pinned commits, with
the same reproduction harness and the same split between the 6 apps and cockroach. Planned
measurements:

- **The points-to delta.** `ddg` edges with `prov:["points-to"]` added on top of the L3 `ssa`
  edges, which must be strictly additive (§4.10): count, share, per-app, and which access-path
  shapes the type-based oracle links.
- **Parameter wiring.** `@formal_in` / `@formal_out` vertices and the per-call-site actual
  vertices (§4.6), and `GO_PARAM_IN` / `GO_PARAM_OUT` edges (§4.7). These are counted per callable
  and per resolved call site, against L2's `call_graph` edges.
- **Summary edges** (§4.9): count, SCC sizes, fixpoint iterations on recursive SCCs, and the
  cost of the bottom-up pass.
- **Library stand-ins** (§6): the number of external stand-ins with modelled bodies, and how many
  `formal_in`/`formal_out` vertices they contribute.
- **VTA dispatch** (§4.8): call edges added over the `go/types` call graph.
- **Cost and size.** Wall time, RSS and `analysis.json` growth of `-a 4` over `-a 3`, plus the
  `-j` determinism check.
- **Consistency at scale.** Every param edge joins a real call site to a real formal, and every
  summary edge spans a call site's actual-in to actual-out.
- **Effect on the L3 caveats above.** Whether points-to edges make the weak-def tail denser, and
  whether the protobuf-marshaller risk compounds.
