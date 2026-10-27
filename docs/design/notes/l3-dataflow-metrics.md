# L3 dataflow metrics over the real-app corpus

L3 is the intraprocedural dataflow level. At `-a 3`, every callable with a body gets
statement-granular body nodes and three edge lists:

- `cfg`: hand-built from `go/ast`, with a defer chain and conservative panic edges.
- `cdg`: Cooper-Harvey-Kennedy post-dominators plus Ferrante-Ottenstein-Warren.
- `ddg`: syntactic may-reaching definitions over k-limited access paths, `prov:["ssa"]`.

The semantics are in [`v2-l3-l4-dataflow.md`](../specs/v2-l3-l4-dataflow.md) §4.2–§4.5.

This note records how L3 behaves on the corpus of [`l3-eval-2026-10.md`](l3-eval-2026-10.md),
which it accompanies. It covers volume, the shape of the graphs, cost, output size and internal
consistency at scale. That note holds the run protocol, the bugs the evaluation found, and the
degraded-input details. This note reuses its timings and extends its counts.

- **When and where:** measured 2026-10-24 on an Apple M3 Max (16 cores, 64 GB, macOS), with the
  analyzer built with go1.27.3 (`runtime.Version()`; no too-old-toolchain warning on any run).
- **Corpus.** Six apps, plus **Kubernetes v1.37.1 as the scale benchmark**, run as two modules:
  `kubernetes` (the root module `k8s.io/kubernetes`) and `k8s-api` (`staging/src/k8s.io/api`,
  almost all generated protobuf). cockroach is no longer a benchmark (see § Corpus); its pre-fix
  figures are kept in one labelled subsection and never folded into totals.
- **Analyzer version (provenance).** Every number for the six apps and Kubernetes comes from the
  `-a 3 -j 1` outputs of one `scripts/eval_l3.sh` pass with the binary it auto-builds from
  `feat/l3-dataflow` at `9aeda43`. The analyzer code there is identical to `5508010` (the commit
  after `9aeda43` touches only `scripts/`), so it includes every final-review fix: C1 (subslice
  weak defs, `258b22f`), I2 (type-error defs, `6fdc2c5`), R15 (body-node `id` from L1, `9ae739e`)
  and I7 (deferred calls read receiver and argument state, `3f31626`). The cockroach subsection is
  the earlier `fd24bdf` binary, **pre-fix**.
- **"Before" figures** (§ What the final-review fixes changed) are the `fd24bdf` evaluation binary,
  rerun on the same trees where the earlier outputs were not kept (dae linux, both Kubernetes
  modules).

One analysis engine, so there is no engine comparison here. Interprocedural (L4) results, measured
2026-10-27, are under [§ L4](#l4).

## Results at a glance

"6 apps" is cobra, listmonk, memos, nats-server, openbao and dae (`GOOS=linux`). "Kubernetes" is
the two Kubernetes modules together, `kubernetes` + `k8s-api`; they are disjoint (`go list ./...` of
the root does not descend into the `staging/` modules), so their counts add. All 8 outputs type-check
completely (0 packages with errors at prepare time, 0 type-error warnings at run time).

| | 6 apps | Kubernetes (`kubernetes` + `k8s-api`) |
|---|---|---|
| callables with a body | 22,971 | 58,030 (42,603 + 15,427) |
| body nodes | 385,505 | 848,801 (492,627 + 356,174) |
| `cfg` / `cdg` / `ddg` edges | 468,353 / 261,656 / 777,658 | 923,459 / 545,150 / 1,464,541 |
| `cfg` / `cdg` / `ddg` per callable | 20.4 / 11.4 / 33.9 | 15.9 / 9.4 / 25.2 |
| `ddg` / `cdg` | 2.97 | 2.69 |
| `exception` share of `cfg` | 31.1% | 26.6% |
| L3 wall time over `-a 2`, `-j 1` | +2.35 s on 10.53 s (18% of `-a 3` wall) | +2.81 s on 9.62 s (23%) / +1.93 s on 3.13 s (38%) |
| L3 throughput, `-j 1` | 164k body nodes/s | 175k / 185k body nodes/s |
| peak RSS, `-a 3 -j 1` | 263 MB to 5.1 GB | 7.1 GB / 2.2 GB |
| `analysis.json`, `-a 2` to `-a 3` | 106.5 to 244.4 MB (2.3x) | 158.2 to 317.6 MB (2.0x) / 55.6 to 183.4 MB (3.3x) |
| consistency checks (§ below) | all PASS | all PASS |
| `-j 1` = `-j 16` = `-j 16`, byte-identical | yes (6/6) | yes (2/2) |

**Key takeaways.**

1. **L3 is cheap in time, not in bytes.** It adds 18% to `-a 3` wall time at `-j 1` on the 6 apps
   and 23% on the Kubernetes root (2.8 s on 9.6 s for 493k body nodes), and at most 15% peak RSS.
   But it doubles to triples `analysis.json`: L3 is 50–70% of an `-a 3` file. The global `id`
   on every body node is about 30% of those L3 bytes, as large as the `ddg`.
2. **Kubernetes is the scale benchmark, and it is easy.** The root module (3,414 files, 44,514
   callables) runs `-a 3` in 12.4 s at 7.1 GB, against cockroach's 33 s at 13.5 GB, with full
   type-checking where cockroach had 62,908 type errors.
3. **Panic edges shape the CFG and CDG.** The conservative `canPanic` gives 62% of non-synthetic
   CFG nodes on the 6 apps an `exception` edge, so 31% of `cfg` edges are `exception`. 35% of `cdg`
   edges hang off a controller whose only way to branch is that edge: "the next statement is
   control-dependent on this call, because the call may panic."
4. **The DDG is long-tailed, and weak defs drive the tail.** The median callable has 4–13 `ddg`
   edges, but the top 1% of callables hold 18–53% of each app's edges. In those callables, a
   reference-typed base (a receiver `c`, a router `g`, a conversion `Scheme` `s`) is weak-defined
   by every call and never killed, and every use of the bare base links to every reaching def.
   The single largest callable in the corpus is Kubernetes' generated
   `core/v1.RegisterConversions`: 130,304 edges, 16% of the root module's `ddg`.
5. **Generated protobuf marshallers (review finding I5): measured, and bounded.** `k8s-api` holds
   2,440 gogoproto `Marshal`/`Unmarshal` bodies over 60 `generated.pb.go` files. Its `ddg` is
   626,883 edges (93% in `.pb.go` code) and its `-a 3` file is 183 MB. The largest marshaller,
   `PodSpec.MarshalToSizedBuffer` (43 fields), has 14,796 edges, and the per-message cost grows
   about as fields^1.8. That is the same mechanism as the 600-field probe's 1.27M edges, but real
   messages are small (median 3 fields, max 43), so the largest real marshaller is 86x below the
   probe. § Protobuf-generated DDG volume has the detail.
6. **Internal consistency holds at full scale, with no sampling.** Every check passes on all
   81,001 bodies of the 8 outputs: one `@entry`/`@exit` each, every endpoint a body key, no
   satellite endpoint, every node on an entry-to-exit path, no `cdg` into `@exit`, all
   `prov == ["ssa"]`.
7. **The final-review fixes only add `ddg` edges.** Against the pre-fix binary, `body`, `cfg` and
   `cdg` are identical on every app, no `ddg` edge is removed, and `ddg` grows by 0.6–1.9% on the
   hand-written apps (mostly I7's deferred-call uses) and by 14.6% on `k8s-api` (C1's subslice
   writes, `MarshalToSizedBuffer(dAtA[:i])`).

## How to reproduce

```bash
cd codeanalyzer-go
scripts/eval_l3.sh --prepare          # clone at the pinned commits, prep, type-check (all apps)
scripts/eval_l3.sh cobra listmonk memos nats-server openbao dae kubernetes k8s-api
                                      # builds $EVAL_OUT/cango with go1.27.3, then runs

# L3 metrics and consistency checks, one JSON object per app:
python3 -I scripts/l3_metrics.py cobra=/tmp/eval-out/cobra-a3j1/analysis.json \
  k8s-api=/tmp/eval-out/k8s-api-a3j1/analysis.json ... > metrics.jsonl

# § L4: add the -a 4 triple (-j 1, -j N, rerun; byte-compared) and its counts:
CANGO_LEVELS="2 3 4" scripts/eval_l3.sh cobra listmonk memos nats-server openbao dae kubernetes k8s-api
python3 -I scripts/l3_metrics.py cobra=/tmp/eval-out/cobra-a4j1/analysis.json ... > metrics-a4.jsonl
```

(`--all` also names cockroach, which is not prepared on this host, so the apps are listed.)

- **What `--prepare` does.** For each app in the script's manifest: a shallow, blob-filtered fetch
  of the pinned commit into `/tmp/eval/<repo>` (an existing clone is reused only at that commit),
  a tool check, the app's prep (dae: the `dae_bpf_headers` submodule and `make ebpf`, which needs
  `clang` and `llvm-strip` from Homebrew `llvm@18`), and a type-check with
  `go list -e -export ./...`. It writes the result to `/tmp/eval-out/prepared/<app>.env`.
- **What `eval_l3.sh` does.** It runs a warm-up, `-a 2 -j 1`, `-a 3 -j 1` and two `-a 3 -j N`,
  all `--eager`, with each app's manifest env (dae: `GOOS=linux`). It records wall time and peak
  RSS from `/usr/bin/time -l`, then compares the three `-a 3` outputs byte for byte.
- **Metamorphic relations (`scripts/metamorphic.sh`).** Run the default, `scripts/metamorphic.sh`
  (memos, about 2 min), for any L3/L4 change; run `scripts/metamorphic.sh --all` (memos and
  nats-server, about 25 min) before a release and for the Task 18 eval.
- **What `scripts/l3_metrics.py` does.** It produces every count, ratio, distribution and check in
  this note. It has no dependencies and loads one file at a time; the 318 MB Kubernetes root file
  takes about 10 s. The per-family protobuf breakdown and the before/after edge diffs were
  one-off scripts over the same outputs.

## Corpus

LOC for the six apps is from the analyzer plan's Task 13 table (2026-10-23) and excludes tests,
generated files and vendor. Kubernetes LOC is by `wc -l` over non-test `.go` files outside
`vendor/` and `staging/`, excluding `zz_generated*`, `generated.pb.go` and `*_generated.go` files
(for `k8s-api`, the module's own non-generated files). "Source analyzed" is the total
`module.source` in `analysis.json`, which includes generated files.

| app | domain | pinned | LOC | source analyzed | why it is in the set |
|---|---|---|---|---|---|
| spf13/cobra | CLI framework (library) | `adbc881` | 7k | 0.2 MB | smoke test; no concurrency |
| knadh/listmonk | newsletter / mailing-list manager | `8216c4f` | 25k | 0.7 MB | small HTTP-to-SQL app: echo handlers into `sqlx` prepared statements |
| usememos/memos | note-taking service | `28719a9` | 96k | 3.5 MB | HTTP to SQL through an interface (`store.Driver`, three implementations) |
| nats-io/nats-server | messaging server | `f8f6db8` | 151k | 4.3 MB | CFG stress: cyclomatic p99 56, max 327; 140 `select`, 855 `defer` |
| openbao/openbao | secrets manager (Vault fork) | `1d58653` | 294k | 7.3 MB | large app; 1,944 `defer` |
| daeuniverse/dae | eBPF transparent proxy | `e3fee8f` | 65k | 1.9 MB | eBPF, cgo-adjacent, heavy `select`; Linux-only |
| kubernetes/kubernetes (root module) | container orchestration | `f78e722` (v1.37.1) | 748k | 34.6 MB | **scale benchmark**: Apache-2.0, fully type-checked from a plain checkout |
| kubernetes/kubernetes `k8s.io/api` | Kubernetes API types | `f78e722` (v1.37.1) | 50k (+316k `generated.pb.go`) | 12.4 MB | generated gogoproto marshallers (`dAtA[...]`): the I5 DDG-volume question |

Why Kubernetes and not cockroach:

- cockroach is now under the proprietary CockroachDB Software License, so it cannot serve as a
  published benchmark.
- Without Bazel (`./dev generate go`), cockroach is only partly type-checked: 62,908 type-error
  warnings, and at prepare time 973 of 1,380 packages with errors.
- Kubernetes commits its generated code, including
  `staging/src/k8s.io/api/**/generated.pb.go`, so a plain checkout type-checks fully (1,468 and 61
  packages, 0 with errors) and needs no prep.

The plan's counts were taken with other tools and do not always match what cango analyzes:

- **Module scope.** cango loads one Go module per run.
  - openbao's 1,944 `defer` include the `api/` and `sdk/` submodules (533 by grep). The root
    module's are about 1,413, against 1,411 deferred call nodes emitted.
  - nats-server's 855 include 10 in non-host platform files. That leaves 845, against 844
    emitted.
  - Kubernetes is a workspace of 34 modules (`go.work`); the corpus runs the root and `k8s.io/api`.
- **Cyclomatic.** The plan's "max 327" for nats-server is from a different counter. cango's
  `metrics.cyclomatic` for `server.client.parse` is 453.

## Per-app counts

The columns fall into three groups.

**Structure.**

- `modules`: `symbol_table` entries.
- `types`: type declarations of every `kind` (struct, interface, alias, defined).
- `callables`: every callable node, closures included at any nesting depth.
- `w/ body`: callables that carry a `cfg`. The difference from `callables` is exactly the
  interface methods (signature only).

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
| cobra | 18 | 17 | 320 | 106 / 164 / 50 | 319 | 3,671 | 1,106 | 14 | 0 | 4,166 | 2,239 | 6,319 |
| listmonk | 101 | 191 | 820 | 200 / 533 / 87 | 800 | 16,252 | 5,863 | 51 | 17 | 17,502 | 9,815 | 39,391 |
| memos | 320 | 770 | 6,076 | 1,350 / 4,193 / 533 | 5,536 | 68,823 | 20,986 | 424 | 14 | 75,289 | 39,551 | 84,100 |
| nats-server | 97 | 749 | 5,279 | 1,114 / 3,412 / 753 | 5,041 | 105,409 | 24,890 | 844 | 81 | 141,829 | 81,598 | 345,318 |
| openbao | 702 | 1,136 | 8,712 | 1,898 / 5,664 / 1,150 | 8,447 | 147,951 | 41,526 | 1,411 | 125 | 177,091 | 98,844 | 234,460 |
| dae (linux) | 226 | 482 | 2,891 | 884 / 1,535 / 472 | 2,828 | 43,399 | 11,173 | 451 | 74 | 52,476 | 29,609 | 68,070 |
| **6 apps** | **1,464** | **3,345** | **24,098** | **5,552 / 15,501 / 3,045** | **22,971** | **385,505** | **105,544** | **3,195** | **311** | **468,353** | **261,656** | **777,658** |
| kubernetes | 3,414 | 4,890 | 44,514 | 18,788 / 16,132 / 9,594 | 42,603 | 492,627 | 158,009 | 2,346 | 388 | 515,474 | 280,473 | 837,658 |
| k8s-api | 494 | 1,462 | 15,427 | 504 / 14,500 / 423 | 15,427 | 356,174 | 82,880 | 0 | 0 | 407,985 | 264,677 | 626,883 |
| **8 incl. Kubernetes** | **5,372** | **9,697** | **84,039** | **24,844 / 46,133 / 13,062** | **81,001** | **1,234,306** | **346,433** | **5,541** | **699** | **1,391,812** | **806,806** | **2,242,199** |

`call_graph` edges, for reference, are L2 and outside this note. All are from the same HEAD runs:

| app | edges | in-project / external |
|---|---|---|
| cobra | 803 | 505 / 298 |
| listmonk | 3,703 | 1,439 / 2,264 |
| memos | 12,009 | 4,248 / 7,761 |
| nats-server | 19,384 | 11,630 / 7,754 |
| openbao | 23,914 | 10,431 / 13,483 |
| dae | 7,985 | 3,230 / 4,755 |
| kubernetes | 58,735 | 33,157 / 25,578 |
| k8s-api | 24,127 | 16,500 / 7,627 |

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
| kubernetes | 193,415 | 90,334 | 51,468 | 8,208 | 1,247 | 62,749 | 42,603 | 42,603 | 39.3% | 82% |
| k8s-api | 87,808 | 110,567 | 57,208 | 10,180 | 1,282 | 58,275 | 15,427 | 15,427 | 24.7% | 94% |

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
| kubernetes | 969 (3.3 / 25) | 91 (4.2 / 18) | 187 (2.1 / 8) | 0.38 | 137 | 1,992 | 10 | 21% |
| k8s-api | 1,282 (4.5 / 44) | 0 | 0 | 0.00 | **5,748** | 0 | 0 | 11% |

How the last three columns are counted:

- `for {}` headers are `loop` nodes whose only non-`exception` out-edge is `fallthrough`. Those are
  infinite loops, which get the synthetic `exception` edge to `@exit`. In `k8s-api` they are the
  generated varint decoders, `for shift := uint(0); ; shift += 7 { … }`, which have no condition
  (5,748, one per field decode loop in `Unmarshal`).
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
| kubernetes | 150,551 | 59,476 | 57,341 | 3,993 | 6,464 | 161,502 | 71,770 | 414 | 1,617 | 2,346 |
| k8s-api | 121,312 | 61,640 | 61,640 | 5,703 | 8,389 | 84,057 | 59,496 | 5,748 | 0 | 0 |

### Ratios

Per-callable figures divide by callables with a body.

| app | cfg / callable | cdg / callable | ddg / callable | ddg / cdg | `call` share of body | `exception` share of cfg | `defer_resume` share of cfg | `fallthrough` share of cfg |
|---|---|---|---|---|---|---|---|---|
| cobra | 13.1 | 7.0 | 19.8 | 2.82 | 40.2% | 32.9% | 0.34% | 26.8% |
| listmonk | 21.9 | 12.3 | 49.2 | 4.01 | 43.4% | 30.6% | 0.29% | 26.3% |
| memos | 13.6 | 7.1 | 15.2 | 2.13 | 34.6% | 30.1% | 0.56% | 24.4% |
| nats-server | 28.1 | 16.2 | 68.5 | 4.23 | 36.4% | 32.7% | 0.60% | 28.3% |
| openbao | 21.0 | 11.7 | 27.8 | 2.37 | 35.1% | 30.5% | 0.80% | 26.7% |
| dae | 18.6 | 10.5 | 24.1 | 2.30 | 35.4% | 29.9% | 0.86% | 27.2% |
| **6 apps** | **20.4** | **11.4** | **33.9** | **2.97** | **35.8%** | **31.1%** | **0.68%** | **26.9%** |
| kubernetes | 12.1 | 6.6 | 19.7 | 2.99 | 39.3% | 31.3% | 0.46% | 29.2% |
| k8s-api | 26.4 | 17.2 | 40.6 | 2.37 | 24.7% | 20.6% | 0.00% | 29.7% |

`k8s-api` is the outlier on the control side: generated `Unmarshal` bodies are long byte loops
with a wide field `switch`, so the `cfg` per callable is the highest in the corpus, while the
`exception` share is the lowest (most statements are index arithmetic on locals, which cannot
panic, rather than calls).

### DDG detail

**Edges per callable.** The distribution is over callables with a body. "Top 1%" is the share of
the app's `ddg` held by its 1% of callables with the most `ddg` edges.

| app | median | p90 | p99 | max | callables with 0 `ddg` | top 1% hold | callables > 1,000 edges (their share) |
|---|---|---|---|---|---|---|---|
| cobra | 5 | 34 | 322 | 722 | 7 | 29% | 0 |
| listmonk | 13 | 57 | 283 | 14,752 | 9 | 53% | 4 (48%) |
| memos | 4 | 41 | 138 | 1,259 | 864 | 18% | 1 (1%) |
| nats-server | 9 | 89 | 935 | 63,747 | 102 | 47% | 46 (46%) |
| openbao | 7 | 58 | 326 | 5,819 | 595 | 28% | 18 (14%) |
| dae | 8 | 47 | 219 | 3,810 | 62 | 30% | 4 (17%) |
| **6 apps** | | | | | | | **73 (29%)** |
| kubernetes | 4 | 30 | 160 | **130,304** | 3,003 | 45% | 50 (30%) |
| k8s-api | 5 | 101 | 539 | 14,796 | 1,969 | 29% | 45 (17%) |

**Top-3 callables by `ddg` count.** The last column is the single `var` with the most edges. It
gives the number of distinct def nodes and distinct use nodes that var's edges touch, and the
density (edges / (defs x uses)).

| app | callable | `ddg` | body nodes | cyclomatic | dominant `var` |
|---|---|---|---|---|---|
| cobra | `github.com/spf13/cobra.Command.execute` | 722 | 111 | 37 | `c`: 342 edges, 25 defs x 32 uses (43%) |
| cobra | `github.com/spf13/cobra.Command.getCompletions` | 625 | 154 | 39 | `finalCmd`: 351, 22 x 30 (53%) |
| cobra | `github.com/spf13/cobra.defaultUsageFunc` | 461 | 90 | 21 | `w`: 229, 21 x 21 (52%) |
| listmonk | `github.com/knadh/listmonk/cmd.initHTTPHandlers` | 14,752 | 510 | 9 | `a`: 7,175, 60 x 181 (66%) |
| listmonk | `…/internal/messenger/postback.easyjsonDf11841fDecode…Postback2` | 1,682 | 102 | 23 | `in`: 1,457, 41 x 42 (85%) |
| listmonk | `…/internal/messenger/postback.easyjsonDf11841fDecode…Postback` | 1,506 | 95 | 25 | `in`: 1,169, 37 x 38 (83%) |
| memos | `…/proto/gen/api/apiconnect.NewUserServiceHandler` | 1,259 | 178 | 37 | `userServiceMethods`: 595, 34 x 34 (51%) |
| memos | `…/proto/gen/api.RegisterUserServiceHandlerClient` | 663 | 37 | 69 | `mux`: 595, 34 x 34 (51%) |
| memos | `…/proto/gen/api.RegisterUserServiceHandlerServer` | 663 | 37 | 69 | `mux`: 595, 34 x 34 (51%) |
| nats-server | `github.com/nats-io/nats-server/v2/server.client.parse` | 63,747 | 917 | 453 | `c`: 50,840, 237 x 232 (**92%**) |
| nats-server | `…/server.raft.processAppendEntry` | 9,065 | 358 | 89 | `n`: 5,404, 132 x 146 (28%) |
| nats-server | `…/server.Server.updateAccountClaimsWithRefresh` | 4,231 | 410 | 120 | `a`: 1,999, 78 x 53 (48%) |
| openbao | `github.com/openbao/openbao/v2/internal/command.ServerCommand.Run` | 5,819 | 548 | 120 | `c`: 1,513, 120 x 43 (29%) |
| openbao | `…/internal/command/server.Config.Merge` | 4,133 | 177 | 50 | `result`: 3,918, 93 x 87 (48%) |
| openbao | `…/internal/vault.NewTestCluster` | 2,649 | 344 | 70 | `coreConfig`: 1,172, 53 x 48 (46%) |
| dae | `github.com/daeuniverse/dae/cmd.reloadWorker.run` | 3,810 | 346 | 53 | `w.log`: 1,560, 40 x 39 (**100%**) |
| dae | `…/cmd.Runner.Run` | 3,790 | 419 | 99 | `reloadManager`: 1,127, 38 x 38 (78%) |
| dae | `…/control.NewControlPlaneWithContextOptions` | 2,496 | 480 | 106 | `log`: 955, 41 x 46 (51%) |
| kubernetes | `k8s.io/kubernetes/pkg/apis/core/v1.RegisterConversions` | **130,304** | 2,548 | 510 | `s`: 129,795, 509 x 509 (50%) |
| kubernetes | `…/pkg/printers/internalversion.AddHandlers` | 9,247 | 616 | 1 | `h`: 9,045, 134 x 134 (50%) |
| kubernetes | `…/pkg/kubelet/apis/config/v1beta1.autoConvert_v1beta1_KubeletConfiguration_To_config_KubeletConfiguration` | 6,247 | 258 | 38 | `out`: 5,344, 126 x 89 (48%) |
| k8s-api | `k8s.io/api/core/v1.PodSpec.MarshalToSizedBuffer` | 14,796 | 558 | 75 | `dAtA`: 13,877, 165 x 166 (51%) |
| k8s-api | `k8s.io/api/core/v1.VolumeSource.MarshalToSizedBuffer` | 6,935 | 369 | 61 | `dAtA`: 5,672, 106 x 107 (50%) |
| k8s-api | `k8s.io/api/core/v1.PodSpec.Unmarshal` | 6,640 | 1,380 | 443 | `m`: 2,752, 64 x 43 (100%) |

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
| cobra | 96.74% | 2.07% | 0.97% | 0.00% | 0.22% | 3.26% | 79% | 5% |
| listmonk | 98.39% | 0.99% | 0.49% | 0.00% | 0.13% | 1.61% | 83% | 8% |
| memos | 96.43% | 1.71% | 1.36% | 0.00% | 0.50% | 3.57% | 79% | 4% |
| nats-server | 96.13% | 1.42% | 2.20% | 0.01% | 0.24% | 3.87% | 71% | 18% |
| openbao | 95.96% | 1.78% | 1.65% | 0.01% | 0.60% | 4.04% | 69% | 6% |
| dae | 95.73% | 2.13% | 1.45% | 0.02% | 0.66% | 4.27% | 67% | 12% |
| **6 apps** | **96.20%** | **1.61%** | **1.78%** | **0.01%** | **0.41%** | **3.80%** | **72%** | **12%** |
| kubernetes | 95.62% | 1.56% | 2.43% | 0.11% | 0.28% | 4.38% | 80% | 4% |
| k8s-api | 93.39% | **5.21%** | 1.40% | 0.00% | 0.00% | 6.61% | 85% | **20%** |

No pseudo path other than `<defer>:` appears in any output. The trailing-`.*` share rose from
0.64% (pre-fix) to 1.78% on the 6 apps: I7 makes every deferred call *use* the `recv.*` / `a.*`
paths it weak-defines, so `defer mu.Unlock()` now reads `mu.*` (§ What the final-review fixes
changed).

**`ddg` by file class.** The class is assigned by the file the callable is declared in:
`*.pb.go` is protobuf, `*.eg.go` is cockroach `execgen`, and easyjson / `_gen.go` / `/gen/` /
`zz_generated*` files are other generated code.

| app | hand-written | protobuf | other generated |
|---|---|---|---|
| listmonk | 84.1% | — | 15.9% |
| memos | 60.8% | 35.8% | 3.4% |
| openbao | 99.0% | 1.0% | — |
| dae | 99.5% | 0.5% | — |
| cobra, nats-server | 100% | — | — |
| **6 apps** | **94.6%** | **4.2%** | **1.2%** |
| kubernetes | 61.4% | 0.0% | **38.6%** (`zz_generated.conversion.go` etc.) |
| k8s-api | 0.3% | **93.1%** | 6.6% (`zz_generated.deepcopy.go`) |

## Protobuf-generated DDG volume (`k8s-api`, review finding I5)

The question: how much `ddg` do gogoproto-style generated marshallers produce at real scale? A
reviewer probe of one synthetic 600-field marshaller produced **1.27M `ddg` edges and 77 MB of
JSON**, and no tree in the earlier corpus contained a `dAtA[` marshaller to check it against.
`k8s.io/api` does: 60 committed `generated.pb.go` files (316k lines, 7.3 MB) holding 1,220
messages, each with `Marshal`, `MarshalTo`, `MarshalToSizedBuffer`, `Size`, `Unmarshal`, `String`
and `Reset`, all type-checked.

**Totals.** `ddg` is 626,883 edges for 15,427 callables, 93.1% of them in `.pb.go` code. The
`-a 3` file is 183.4 MB, 3.3x the `-a 2` file (55.6 MB), the largest growth in the corpus.
The `ddg` section alone is 37 MB of it (about 62 bytes per edge). Wall time is 5.1 s at
`-j 1` (L3: +1.9 s), 2.2 GB RSS.

**By generated method family** (each family has 1,220 bodies; "L3 bytes per node" is the
re-serialized `body`+`cfg`+`cdg`+`ddg` of the family divided by its body nodes):

| family | `ddg` | share | median | p90 | p99 | max | body nodes | `ddg` per node | L3 MiB | L3 bytes per node |
|---|---|---|---|---|---|---|---|---|---|---|
| `Unmarshal` | 311,110 | 49.6% | 173 | 443 | 1,359 | 6,640 | 175,274 | 1.77 | 77.0 | 461 |
| `MarshalToSizedBuffer` | 208,977 | 33.3% | 73 | 296 | 1,477 | 14,796 | 54,150 | 3.86 | 30.0 | 581 |
| `Size` | 23,984 | 3.8% | 13 | 35 | 119 | 588 | 31,723 | 0.76 | 11.6 | 383 |
| `Marshal` | 13,420 | 2.1% | 11 | 11 | 11 | 11 | 13,420 | 1.00 | 5.1 | 396 |
| `String` (1,209) | 11,056 | 1.8% | 6 | 18 | 43 | 149 | 18,828 | 0.59 | — | — |
| other `.pb.go` (`MarshalTo`, `Reset`, `skipGenerated`, varint helpers, …) | 14,940 | 2.4% | | | | | | | | |
| `zz_generated.deepcopy.go` and closures | 41,620 | 6.6% | | | | | | | | |
| hand-written (`types.go` helpers) | 1,776 | 0.3% | | | | | | | | |

Over the whole module, the `-a 3` file is 540 bytes per body node (the corpus's lowest; generated
nodes have short spans), and 14.9 bytes per byte of source.

**Top callables.** All ten largest are `core/v1` marshallers: `PodSpec.MarshalToSizedBuffer`
14,796; `VolumeSource.MarshalToSizedBuffer` 6,935; `PodSpec.Unmarshal` 6,640;
`VolumeSource.Unmarshal` 5,183; `Container.` and `EphemeralContainerCommon.MarshalToSizedBuffer`
4,181 each; `PodStatus.MarshalToSizedBuffer` 3,567; `PersistentVolumeSource.MarshalToSizedBuffer`
3,519 and `.Unmarshal` 3,111; `ServiceSpec.MarshalToSizedBuffer` 3,006.

**The mechanism is the probe's.** `MarshalToSizedBuffer` writes the buffer backwards: each field is
`i -= len(x); copy(dAtA[i:], x); i = encodeVarint(dAtA, i, …); i--; dAtA[i] = 0x…`, or for a nested
message `m.X.MarshalToSizedBuffer(dAtA[:i])`. Every one of those statements weak-defines
`dAtA[*]` (an index store, `copy` into a subslice, or a call with the subslice argument, C1), and
none kills, so every later use of `dAtA` meets all of them. In `PodSpec.MarshalToSizedBuffer`,
`dAtA` has 13,877 edges between 165 defs and 166 uses, 51% of the full matrix: the exact triangle
of a straight-line sequence. `i` adds only 624, because `i -= …` is a strong def. `Unmarshal` is
different: its volume is spread over the field `switch` (`m` and `iNdEx` in a loop), with a lower
`ddg` per node (1.77) but many more nodes.

**Scaling, and the comparison with the probe.** Over the 226 messages with at least 5 fields, a
log-log fit of `MarshalToSizedBuffer` `ddg` against the message's field count gives about
**10 x fields^1.8**, i.e. slightly sub-quadratic. Extrapolated to 600 fields that is about 1.07M
edges, the same order as the probe's 1.27M (the probe's exact code is not recorded, so only the
order is comparable). So the probe was right about the mechanism and the growth rate. What it did not tell is the distribution: real
Kubernetes messages have a median of 3 fields and a maximum of 43 (`PodSpec`), so the largest
real marshaller is 14,796 edges, 86x below the probe, and the whole module's 2,440 marshal and
unmarshal bodies together hold 520k edges, 0.4x one probe.

**Verdict on I5.** On real gogoproto output the volume is large but bounded. It is the biggest
L3 file per source byte in the corpus (3.3x growth), but no single callable is an outlier on the
scale of `RegisterConversions` (130k) or `client.parse` (64k). A guard is not needed for
Kubernetes-sized messages. It would still matter for a schema with hundred-field messages, since
the cost per message grows as fields^1.8. A kill or summarization rule for sequential indexed
writes to one buffer (`dAtA[i] = …` after `i--`) would collapse the triangle; it is not
implemented and not needed for this corpus.

## Cost: time, memory, output size

**Time and memory.** All runs are `--eager`, one timed run each, with wall time and peak RSS from
`/usr/bin/time -l` (MB = 2^20 bytes). All rows are the one HEAD pass (`9aeda43` binary).

- **L3 delta and throughput.** The L3 delta is `-a 3 -j 1` minus `-a 2 -j 1`. Throughput is body
  nodes divided by that delta.
- **Noise.** Repeated `-a 2 -j 1` runs of one binary on openbao spread over 3.75–4.37 s, so a
  single wall time is good to roughly ±0.2 s on the mid-size apps and ±0.05 s on the small ones.
  cobra's negative delta (-0.05 s) is noise.

| app | `-a 2 -j 1` (s) | `-a 3 -j 1` (s) | L3 delta | L3 share of `-a 3` | `-a 3 -j 16` (s) | `-j 16` speedup | 2nd `-j 16` (s) | RSS `-a 2` | RSS `-a 3 -j 1` | RSS `-a 3 -j 16` | L3 throughput (nodes/s) |
|---|---|---|---|---|---|---|---|---|---|---|---|
| cobra | 0.47 | 0.42 | -0.05 | n/a | 0.41 | 1.02x | 0.43 | 243 | 263 | 252 | n/a |
| listmonk | 0.80 | 0.97 | +0.17 | 18% | 0.86 | 1.13x | 0.90 | 915 | 936 | 885 | 95,600 |
| memos | 2.26 | 2.52 | +0.26 | 10% | 2.32 | 1.09x | 2.25 | 2,308 | 2,613 | 2,364 | 264,703 |
| nats-server | 1.44 | 2.24 | +0.80 | 36% | 1.74 | 1.29x | 1.77 | 1,034 | 1,078 | 1,155 | 131,761 |
| openbao | 4.13 | 4.98 | +0.85 | 17% | 4.39 | 1.13x | 4.41 | 4,983 | 5,082 | 4,966 | 174,059 |
| dae (linux) | 1.43 | 1.75 | +0.32 | 18% | 1.63 | 1.07x | 1.58 | 1,332 | 1,534 | 1,461 | 135,621 |
| **6 apps** | **10.53** | **12.88** | **+2.35** | **18%** | **11.35** | **1.13x** | | | | | **164,044** |
| kubernetes | 9.62 | 12.43 | +2.81 | 23% | 11.76 | 1.06x | 10.92 | 6,961 | 7,135 | 7,592 | 175,312 |
| k8s-api | 3.13 | 5.06 | +1.93 | 38% | 3.97 | 1.27x | 4.02 | 2,043 | 2,198 | 2,203 | 184,546 |

**Output size.** `analysis.json` sizes are on disk. The per-section bytes ("L3 composition") were
measured by re-serializing each callable's `body`, `cfg`, `cdg` and `ddg` compactly, as cango
writes them, and are given as a share of the L3 delta in bytes:

- body-node `id`s: the `"id":"<callable-id>@<key>"` string on every body node L3 adds (the
  non-`call` nodes). Since R15 (`9ae739e`), `call` nodes carry their `id` from L1, so those ids
  are in the `-a 2` file and not in the delta;
- other: the new statement nodes' spans, kinds and `parent` fields.

| app | `-a 2` MB | `-a 3` MB | growth | L3 share of file | bytes / body node | L3 composition: body-node `id`s / `ddg` / `cfg` / `cdg` / other | `-a 3` bytes per source byte |
|---|---|---|---|---|---|---|---|
| cobra | 1.0 | 2.0 | 2.1x | 51% | 567 | 22 / 36 / 19 / 6 / 17% | 9.0 |
| listmonk | 4.7 | 10.0 | 2.1x | 53% | 643 | 23 / 41 / 15 / 5 / 15% | 13.9 |
| memos | 19.5 | 39.8 | 2.0x | 51% | 606 | 35 / 25 / 17 / 6 / 18% | 11.5 |
| nats-server | 27.3 | 72.8 | 2.7x | 63% | 724 | 23 / 44 / 15 / 5 / 13% | 16.9 |
| openbao | 42.6 | 94.5 | 2.2x | 55% | 670 | 36 / 27 / 16 / 6 / 15% | 13.0 |
| dae | 11.5 | 25.3 | 2.2x | 55% | 612 | 31 / 29 / 17 / 6 / 16% | 13.3 |
| **6 apps** | **106.5** | **244.4** | **2.3x** | **56%** | **665** | **31 / 33 / 16 / 6 / 15%** | **13.7** |
| kubernetes | 158.2 | 317.6 | 2.0x | 50% | 676 | 34 / 31 / 15 / 5 / 15% | 9.2 |
| k8s-api | 55.6 | 183.4 | **3.3x** | **70%** | 540 | 31 / 29 / 15 / 6 / 19% | 14.9 |

The `-a 2` files are about 28% larger than in the pre-fix evaluation (6 apps: 82.9 to 106.5 MB)
because of R15's call-node `id`; the `-a 3` files barely moved (243.7 to 244.4 MB), because
those ids were already there at L3. Growth is accordingly lower than before (2.9x to 2.3x).

## Internal-consistency checks

Every check ran on **every** callable with a body (22,971 on the 6 apps, 58,030 on Kubernetes,
81,001 in all), with no sampling, using `scripts/l3_metrics.py`.

| # | check | 6 apps | Kubernetes (both modules) | result |
|---|---|---|---|---|
| 1 | `entry` count == `exit` count == callables with a body, and each body has exactly one `@entry` and one `@exit` key | 22,971 = 22,971 = 22,971; 0 bodies off | 58,030 each; 0 off | **PASS** |
| 2 | every `cfg` / `cdg` / `ddg` endpoint is a body key of its own callable | 0 bad of 1,507,667 edges | 0 bad of 2,933,150 | **PASS** |
| 3 | no satellite `call` node is a `cfg`, `cdg` or `ddg` endpoint | 0 | 0 | **PASS** |
| 4 | every non-satellite node is reachable from `@entry` and reaches `@exit` (forward and backward BFS per body; full, not sampled) | 279,961 nodes checked; 0 unreachable, 0 not reaching exit | 607,912 checked; 0 / 0 | **PASS** |
| 5 | each `return` node has exactly one `return` edge; `return` edges = `return` nodes + fall-off-end edges | 0 off; 59,844 = 52,975 + 6,869 | 0 off; 131,266 = 121,024 + 10,242 | **PASS** |
| 6 | no `cdg` edge into `@exit`; no `cdg` self-edge | 0 / 0 | 0 / 0 | **PASS** |
| 7 | every `ddg` edge has `prov == ["ssa"]` | 0 of 777,658 differ | 0 of 1,464,541 | **PASS** |
| 8 | no duplicate `cfg` edge `(src, dst, kind)` | 0 | 0 | **PASS** |
| 9 | one `defer_resume` edge per deferred call node, and one `<defer>:` `ddg` edge per deferred call | 3,195 = 3,195 = 3,195 | 2,346 each | **PASS** |
| 10 | at most one `exception` edge per node (edges == nodes carrying one) | 145,556 = 145,556 | 245,559 = 245,559 | **PASS** |
| 11 | `-j 1` == `-j 16` == 2nd `-j 16`, byte-identical | 6 / 6 | 2 / 2 | **PASS** |
| 12 | no closure whose spans index another file lacks `source_file` (R19, `b87c636`) | 0 | 0 | **PASS** |

Notes on checks 5 and 9:

- **Check 5.** `return` is also the kind of a fall-off-end edge, so `return` edges exceed `return`
  nodes by exactly the fall-off count. Fall-off edges leave the last straight-line node, branch or
  loop of a body. An empty body has a single `@entry -> @exit` `return` edge: there are 478 of
  those on the 6 apps (memos has 345, e.g. no-op marker methods such as
  `filter.ComparisonCondition.isCondition`). 11,962 of the 6 apps' 59,844 `return` edges go to the
  first member of a defer chain, not to `@exit`.
- **Check 9.** It confirms the §4.3.1 split end to end, by totals. Each `defer` gives a
  registration `statement`, a deferred `call`, one `defer_resume` edge (chain-internal or tail to
  `@exit`), and one registration-to-call `<defer>:` du-pair, and the three totals agree.

`true` exceeds `false` by 1,445 on the 6 apps. That matches, to within 5, the 1,450 `branch`/`loop`
headers whose not-taken arm falls off the end of the function, which is therefore kind `return`,
not `false`. In `k8s-api`, `true` and `false` are exactly equal (61,640): no generated body ends
in a branch.

### What the final-review fixes changed

The previous version of this note measured the `fd24bdf` binary. HEAD adds four fixes that change
L3 or the file: C1 (`258b22f`, a subslice or array-slice argument weak-defines `base[*]`), I2
(`6fdc2c5`, defs kept in packages with type errors), R15 (`9ae739e`, body-node `id` from L1) and
I7 (`3f31626`, a deferred call uses the paths it may write: `defer mu.Unlock()` uses `mu.*`).
Each pre-fix output was diffed against HEAD's per callable, edge by edge:

| app | `ddg` before | after | added | removed | added into a deferred call (I7) | added elsewhere (C1) |
|---|---|---|---|---|---|---|
| cobra | 6,261 | 6,319 | +58 (+0.9%) | 0 | 58 | 0 |
| listmonk | 39,287 | 39,391 | +104 (+0.3%) | 0 | 99 | 5 |
| memos | 83,413 | 84,100 | +687 (+0.8%) | 0 | 503 | 184 |
| nats-server | 340,338 | 345,318 | +4,980 (+1.5%) | 0 | 3,900 | 1,080 |
| openbao | 230,816 | 234,460 | +3,644 (+1.6%) | 0 | 3,515 | 129 |
| dae (linux) | 66,825 | 68,070 | +1,245 (+1.9%) | 0 | 879 | 366 |
| **6 apps** | **766,940** | **777,658** | **+10,718 (+1.4%)** | **0** | **8,954** | **1,764** |
| kubernetes | 833,012 | 837,658 | +4,646 (+0.6%) | 0 | 4,518 | 128 |
| k8s-api | 547,122 | 626,883 | **+79,761 (+14.6%)** | 0 | 0 | 79,761 |

- **Only `ddg` changed.** Per callable, `body`, `cfg` and `cdg` hash identically before and after
  on all 8 apps (the body-node `id` strings were already present at L3 before R15), and no `ddg`
  edge disappeared. Every fix widens the over-approximation; none narrows it.
- **The non-deferred additions are almost all C1.** In nats-server, 1,079 of the 1,080 are defs at a
  statement that slices (`a.w.Write(p[:wanted])`, `binary.PutUvarint(tmp[:], v)`,
  `tokenizeSubjectIntoSlice(rsa[:0], r)`); in openbao, `copy(result.Audits[len(c.Audits):], …)`
  in `Config.Merge` (4,034 to 4,133 edges).
- **`k8s-api` is the C1 showcase.** All 79,761 added edges are on `dAtA` (59,077) or `dAtA[*]`
  (20,684), in `MarshalToSizedBuffer` (+39,575, via `m.X.MarshalToSizedBuffer(dAtA[:i])` and
  `copy(dAtA[i:], …)`) and `Unmarshal` (+38,966). Before C1 those calls were invisible as writes
  to the buffer, a false refutation of exactly the protobuf flow; `PodSpec.MarshalToSizedBuffer`
  went from 12,019 to 14,796.
- **I2 changed nothing measurable here.** All 8 trees type-check fully, so no def depended on it.
  It matters for degraded inputs such as cockroach.
- **Bytes.** R15 moved about 23 MB of call-node ids from the L3 delta into the `-a 2` file on the
  6 apps; the `-a 3` totals grew 0.3% (243.7 to 244.4 MB, from the extra `ddg` edges).
- **Time and memory** moved within noise (6-app `-a 3 -j 1` 12.17 to 12.88 s; repeated runs of
  one binary spread ±0.2 s), and L3 throughput is unchanged (165k to 164k body nodes/s).

### Determinism

All 8 outputs are byte-identical across `-j 1`, `-j 16` and a second `-j 16`. Neither Kubernetes
module emitted any stderr line (no type error, no toolchain warning, no `L3 skipped`).

## Observations

### Where L3 cost goes

- **Loading and type-checking dominate.** L3 adds 2.35 s to 10.5 s of `-a 2` across the 6 apps
  (18% of `-a 3` wall at `-j 1`), and 2.8 s to 9.6 s on the Kubernetes root.
- **`-j` only parallelizes L3.** `-j` feeds only the per-callable L3 worker pool
  (`dataflow.Run`), so the `-j 16` speedup of the whole run (1.02–1.29x) is Amdahl-bound by the
  serial front. It is largest where L3 is the largest share: nats-server (36%) and `k8s-api` (38%).
- **Edges, not nodes, seem to set L3's cost.** No per-phase profile was taken; this is an
  inference from the throughput column. nats-server has 29% fewer body nodes than openbao but 47%
  more `ddg` edges, and almost the same L3 delta (0.80 vs 0.85 s), so its throughput is lower
  (132k vs 174k nodes/s).
- **The known worst case was not measured here.** A reviewer probe of one 6,000-case function took
  15 s in `BuildDDG`'s kill computation, which is O(nodes x defs) per body (§ Limitations). No
  corpus function triggers it: the largest real body is `RegisterConversions` (2,548 nodes, 509
  defs of `s`), which the whole 2.8 s Kubernetes L3 delta absorbs.
- **Memory.** L3 adds 2–15% peak RSS at `-j 1` (most on dae and memos). `go/packages` type
  information dominates RSS: the Kubernetes root needs 7.0 GB at `-a 2`, 7.1 GB at `-a 3`.

### How Go idioms show up in the graph

- **The error idiom is visible in the node mix.** `if err != nil` is 28% of all `branch` nodes on
  the 6 apps (38–41% in the HTTP/SQL services listmonk, memos, openbao; 12% in cobra and
  nats-server; 21% in the Kubernetes root). It accounts for much of Go's 2.4 `branch` and 2.3
  `return` nodes per callable.
- **Defer chains are short and common.** 2,646 of 22,971 bodies (12%) on the 6 apps have a defer
  chain, and the median chain has one member. The longest is 20 defers (openbao) or 12
  (nats-server).
  - Every exit funnels through the chain: 39,567 `exception` and 11,962 `return` edges enter a
    chain rather than `@exit`.
  - Deferred calls that may themselves panic add 549 chain-internal `exception` edges.
  - `defer_resume` is under 1% of `cfg` edges everywhere. The defer model costs little volume.
- **`select` is where the concurrency-heavy apps differ.** dae has 3.0 `select` headers per 1,000
  body nodes, against 1.2–1.3 for nats-server and openbao, 0.4 for the Kubernetes root and at most
  0.6 for the rest.
  - `select` fan-out is narrow: a mean of 2.1–2.8 comm clauses, max 12 in nats-server.
  - Infinite `for {}` loops cluster in the same apps: 94 in dae, 70 in nats-server, 106 in openbao,
    against 0–12 in the rest of the 6.
- **Wide switches are where cyclomatic complexity lives.** nats-server has the widest expression
  `switch` (77 cases). `server.client.parse`, nats-server's protocol state machine, is cyclomatic
  453 in one body: a byte loop around a state `switch`. Kubernetes' largest cyclomatic is the
  generated `RegisterConversions` (510), a straight sequence of `if err := s.AddGeneratedConversionFunc(…); err != nil { return err }`.

### Over-approximation, measured

- **Panic edges.** 62% of non-synthetic CFG nodes on the 6 apps (65% on the Kubernetes root) carry
  an `exception` edge, so `exception` is the most common kind, ahead of `fallthrough` (31.1% vs
  26.9%). The cause is §4.3.2's rule: any call, index, dereference, division, send or unchecked
  assertion may panic. This is the price of having the defer chain and `@formal_out` see
  panicking paths. It is sound for taint, but it blurs the CFG for readers who want "normal"
  control flow. Filtering on `kind != "exception"` recovers that view exactly, since each node has
  at most one such edge.
- **Panic-induced control dependence.** 34.9% of `cdg` edges on the 6 apps (33.6% on Kubernetes)
  come from a controller with no `true`/`false`/`switch_case` out-edge. Its only second successor
  is its `exception` edge. So in `a(); b()`, `b` is control-dependent on `a`. A backward slice over
  `cdg` therefore pulls in every earlier panic-capable statement on the path. A further 10.6% of
  `cdg` edges are the `@entry` anchors for uncontrolled nodes (spec §4.4).
- **Weak defs, and defs of distinct fields, never kill each other.** A call weak-defines `a.*`
  for every reference-typed argument and receiver, and `x.f = …` kills only `x.f…`. So along any
  path the defs on a long-lived receiver (`c`, `s`, `mset`) only accumulate. 72% of `ddg` edges
  are uses of a bare base (80–85% on Kubernetes), which by the interference rule (`u.Covers(d)`)
  matches every reaching def under that base.
- **Wildcards themselves are a minor contributor in hand-written code.** They are 3.8% of 6-app
  edges, mostly `[*]` element writes and (since I7) the `recv.*` uses of deferred calls. The
  inflation comes from bare-base uses meeting accumulated weak defs, not from the wildcard
  spelling. `k8s-api` is the exception: 5.2% `[*]`, from `dAtA[*]`.

### The DDG long tail

The long tail is one mechanism repeated: an inner loop or a long straight-line sequence of calls
on one reference-typed base.

- **Kubernetes `core/v1.RegisterConversions`.** It holds 130,304 edges, 16% of the root module's
  `ddg`, in one generated function (`zz_generated.conversion.go`).
  - It is 509 consecutive `if err := s.AddGeneratedConversionFunc(…); err != nil { return err }`.
    Each call weak-defines `s.*` (the `*runtime.Scheme`) and uses `s`, so every registration
    links to every earlier one: 509 defs x 509 uses, 50% filled, the exact triangle
    (509 x 510 / 2 = 129,795, which is the `s` count).
  - The 100 `RegisterConversions` functions of the root module hold 191,433 edges, 23% of its
    `ddg`; `zz_generated*` files hold 38.6%.
  - `printers/internalversion.AddHandlers` (9,247; 134 `h.TableHandler(…)` calls) is the same
    pattern in hand-written code.
- **nats-server `client.parse`.** It holds 63,747 edges, 18% of the app's `ddg`.
  - 237 nodes define a path under `c`: field writes such as `c.state = …`, plus every call with
    `c` as pointer receiver, which weak-defines `c.*`. 232 nodes use bare `c`.
  - They sit inside a single loop, so every def reaches every use: 92% of the 237 x 232 matrix is
    filled.
  - 18% of nats-server's edges are def-at-or-after-use (loop-carried), the highest of the 6 apps.
- **listmonk `initHTTPHandlers`.** It holds 14,752 edges, 37% of the app's `ddg`.
  - It is straight-line route registration: 497 call nodes, cyclomatic 9.
  - Each `g.GET(...)` on the `*echo.Group` weak-defines `g.*` and uses `g`, so every registration
    links to every earlier one: 185 defs x 185 uses, 21% filled, cut into blocks by the two
    `g := e.Group(…)` and one `g = e.Group(…)` strong defs. The `a` (`*App`) edges are 66%
    filled.
  - Complexity metrics do not predict this tail. Call density on one base does.
- **openbao `Config.Merge`.** Each of 87 `result.X = …` statements strong-defines only
  `result.X`, and also uses bare `result` to write through it. A def of `result.X` kills only
  `result.X…`, so the defs of distinct fields accumulate, and each statement's `result` use
  matches every earlier one: 3,918 `result` edges for 177 body nodes (93 defs since C1 adds the
  `copy(result.Audits[…:], …)` writes).
- **Generated marshallers** (`k8s-api`): the same triangle on `dAtA`, § Protobuf-generated DDG
  volume.

### Output size

**Output size is the main caution.**

- **Growth.** `-a 3` is 2.3x `-a 2` on the 6 apps, 2.0x on the Kubernetes root and 3.3x on
  `k8s-api`, at 540 to 724 bytes per body node and 9 to 17 bytes of JSON per byte of analyzed
  source. The 318 MB Kubernetes root file loads whole in Python in about 10 s.
- **What the bytes are.** Body-node `id` strings are 27% of an `-a 3` file on the 6 apps (28–29%
  on Kubernetes). They average about 165–225 bytes per node by app, because each repeats the full
  callable id. Of the L3 delta alone, the ids L3 adds are 31% and `ddg` 33% (44% on nats-server,
  the long-tail app).
- **The `id` is derivable.** It is `<callable-id>@<key>`. It is present for SDK parity with Python
  (#176, R15; spec §4.2), so dropping or compressing it is a schema-contract decision, not a free
  win. The measurement says that decision is worth about a quarter of an `-a 3` file.

### Context: Java's AST-engine figures

The Java sibling's AST engine is the nearest syntactic L3, measured on a different corpus (ten
Java apps; see `codeanalyzer-java/docs/design/notes/l3-dataflow-metrics.md`). The ratios below
are context for consumers moving between languages, not a quality comparison. The two engines
make different, documented modelling choices, and the corpora differ in domain and size.

| ratio | Go, 6 apps | Go, Kubernetes root | Java AST engine, 10 apps |
|---|---|---|---|
| body nodes / callable | 16.8 | 11.6 | 11.3 |
| `cfg` / callable | 20.4 | 12.1 | 6.6 |
| `cdg` / callable | 11.4 | 6.6 | 1.5 |
| `ddg` / callable | 33.9 | 19.7 | 3.2 |
| `ddg` / `cdg` | 2.97 | 2.99 | 2.12 |
| `call` share of body | 35.8% | 39.3% | 62.3% |
| `exception` share of `cfg` | 31.1% | 31.3% | 3.1% |
| `fallthrough` share of `cfg` | 26.9% | 29.2% | 75.5% |
| bytes / body node | 665 | 676 | about 1,700 (commons-lang only; derived from "~222 MB" / 128,961 nodes) |

The large differences have identifiable causes.

- **Exception edges.** Java's are syntactic throwers inside a `try` only. Go's `canPanic` applies
  everywhere, because a panic always reaches the defer chain. That alone explains the `exception`
  share, and through panic-induced control dependence, about a third of Go's `cdg`.
- **Go's error idiom adds explicit branches and returns.** Go has 2.4 branches and 2.3 returns per
  callable, against Java's 0.28 and 0.55. That lowers `call`'s share of the body and adds `cdg`.
- **Go's weak-def rule for reference-typed arguments and receivers** has no counterpart that
  would generate comparable `ddg` density in an object-insensitive Java AST pass. The ddg/cdg
  ratio, which normalizes away the control-side differences, is the closest of the figures
  (3.0 vs 2.1).

## cockroach: very large, partly type-checked (pre-fix L3)

cockroach `d30c905f` is kept for the record as a very large tree, not as a benchmark: it is under
the proprietary CockroachDB Software License, and without Bazel (`./dev generate go`) it has no
generated protobuf code (183 `.proto`, 0 `.pb.go`). `scripts/eval_l3.sh --prepare cockroach`
reports it `degraded: missing bazel or bazelisk; 973 packages with errors` (22 packages that fail
to compile, 951 more broken through them; 1,380 in all). It was **not** rerun with the HEAD
binary: every figure below is the `fd24bdf` binary (pre-fix: no C1, I2, R15 or I7), and none is
in any total above.

| metric | cockroach (pre-fix L3, degraded) |
|---|---|
| modules / types / callables (functions / methods / closures) | 5,953 / 15,002 / 88,765 (21,223 / 53,058 / 14,484) |
| callables with a body / body nodes / satellite | 82,752 / 1,407,838 / 406,091 |
| `cfg` / `cdg` / `ddg` | 1,618,928 / 959,530 / 4,600,814 |
| per callable `cfg` / `cdg` / `ddg`; `ddg` / `cdg` | 19.6 / 11.6 / 55.6; 4.79 |
| `exception` share of `cfg`; `fallthrough` share | 29.7%; 30.5% |
| `ddg` per callable median / p90 / p99 / max; top 1% hold | 6 / 54 / 303 / 73,013; 65% |
| wildcard or pseudo `var`s; trailing `.*` | 15.31%; 13.82% (k-limit truncations in `.eg.go`) |
| `ddg` by file class | hand-written 38.7%, `execgen` 60.8%, other generated 0.6% |
| wall `-a 2` / `-a 3 -j 1` / `-a 3 -j 16`; RSS `-a 3 -j 1` | 23.08 / 33.36 / 26.96 s; 13,473 MB |
| `analysis.json` `-a 2` to `-a 3` | 335.5 to 1,112.4 MB (3.3x) |
| type-error warnings at run time | 62,908 across 633 packages |
| consistency checks; `-j` byte-identical (after `679f995`) | all PASS; yes |

- **The `execgen` tail.** The top callables are three generated merge-joiner closures in
  `colexecjoin` (73,013 edges each, 1,058 nodes, 100% dense on `o`). Promoted embedded fields are
  spelled out (§4.5), so `o.builderState.left.groupsIdx` becomes
  `o.mergeJoinBase.builderState.left.groupsIdx`, which k=3 truncates to
  `o.mergeJoinBase.builderState.*`. With `--graph-field-depth 4`, total `ddg` falls 7.5% and the
  trailing-`.*` class falls from 635,616 to 10,769.
- **Type-error defs.** The pre-fix binary dropped L3 defs whose receiver failed to type-check. I2
  fixes that, so a HEAD rerun would have more `ddg` edges than shown; these figures are a lower
  bound even for this degraded tree.

## Limitations and known gaps

### Degraded inputs

- **cockroach** is the only degraded tree, and is reported only in its own subsection above.
- **dae** is Linux-only. It is run with `GOOS=linux` (from the manifest), where it type-checks
  fully. On darwin it would have 90 type errors in 4 packages.

### Model limits (by design at L3)

- **Syntactic, alias-free L3.** Two paths with different bases never interact: `p := &x; p.f = 1;
  use(x.f)` has no edge. Points-to and alias flow arrive at L4 (§4.10) as additive
  `prov:["points-to"]` edges.
- **Not refutation-complete** (spec §4.5 "L3 precision"): no edge does not mean no flow.
  Writes to captured variables inside closures, value structs whose fields are slices, maps or
  pointers, and `append` into a destination's backing array are not modelled. Deferred calls now
  read their receiver and argument paths at exit (I7), keyed on the variable, not the value
  bound at registration.
- **Every defer is assumed registered on every path,** with one chain member per `defer`
  statement. `recover()` does not resume normal flow, and `os.Exit`-style calls are ordinary calls
  (§4.3.1–§4.3.2).

### Open follow-ups

- **Sequential buffer writes.** Generated marshallers produce a def x use triangle on their output
  buffer (§ Protobuf-generated DDG volume). Fine for Kubernetes-sized messages, quadratic-ish
  (fields^1.8) for very wide ones. A kill rule for `dAtA[i] = …` after `i--`, or a summarization,
  is not implemented.
- **Quadratic kill computation.** `BuildDDG` computes KILL by testing every strong def of every
  node against every def in the body. A reviewer probe of one 6,000-case switch-heavy function
  took 15 s. Nothing in this corpus comes close.

## L4

L4 is the interprocedural system dependence graph (SDG). At `-a 4`, on top of L3, every callable
gets `formal_in` / `formal_out` vertices, and every resolved call site gets `actual_in` /
`actual_out` vertices. These are wired into the callable's `ddg` (`prov:["reaching-defs"]`). The
application carries `param_in` / `param_out`, every call site carries `summary` edges, VTA adds
dispatch targets to `call_graph` (`prov:["vta"]`), library callees get stand-in formals in
`external_symbols[].body`, and a type-based may-alias oracle adds `ddg` edges with
`prov:["points-to"]`. The semantics are in [`v2-l3-l4-dataflow.md`](../specs/v2-l3-l4-dataflow.md)
§4.6–§4.16 and §6.

- **When and where:** measured 2026-10-27 on the same host, an Apple M3 Max (16 cores, 64 GB,
  macOS 15). The analyzer was built by `scripts/eval_l3.sh` with **go1.27.3** (`runtime.Version()`),
  and every run used `GOTOOLCHAIN=go1.27.3`. Every number in this section is go1.27.3 unless it
  says otherwise: figures quoted from earlier task reports carry their toolchain in parentheses.
  Absolute RSS differs by about 1 GB between go1.26.4 and go1.27.3 on nats-server (Task 15b), so
  do not compare figures across toolchains.
- **Binary:** `feat/l4-sdg` at `c230592`. The `+dirty` marker covers uncommitted docs only; the
  analyzer code is that commit's. It includes the Task 17 alias rounds (`04244db`, `18fca40`),
  the capture fixes (`078616f`, `cd37383`) and the Task 18a fix-ups.
- **Protocol:** `CANGO_LEVELS="2 3 4" scripts/eval_l3.sh` over the same eight prepared trees and
  pinned commits as the L3 sections. Each app runs the L3 sequence (§ How to reproduce) and then
  the **`-a 4` triple**: `-a 4 -j 1 -v`, `-a 4 -j 16`, and a second `-a 4 -j 16`, all
  `--eager`. The three `-a 4` files are compared byte for byte. Wall time, user CPU and peak RSS
  come from `/usr/bin/time -l`. That peak is the **whole process**: load, L1–L3, SSA, VTA, the
  L4 overlay and the streamed emission. L4 counts come from `scripts/l3_metrics.py` (its new `l4`
  object) and the `-v` log.
- **Gate (spec §8):** every app completes `-a 4` with no crash, and its three outputs are
  byte-identical. Memory is **measured and reported, not gated** (D3, user ruling 2026-10-26):
  L4 is whole-program, and the former "≤ 1.5× `-a 3`" budget is dropped. The table reports the
  absolute peak and its ratio to `-a 3`.

### Results at a glance (L4)

| | 6 apps | Kubernetes (`kubernetes` + `k8s-api`) |
|---|---|---|
| `-a 4` complete, no crash | 6/6 | 2/2 |
| `-j 1` = `-j 16` = `-j 16`, byte-identical | 6/6 | 2/2 |
| wall `-a 3` → `-a 4`, `-j 1` | 12.9 → 45.4 s | 13.5 → 31.5 s / 5.6 → 11.0 s |
| peak RSS `-a 4 -j 1` (× `-a 3`) | 290 MB (1.08×) to 5.9 GB (1.14×); nats-server 4.1 GB (3.64×) | **7.7 GB (1.05×)** / 2.4 GB (1.15×) |
| `analysis.json` `-a 3` → `-a 4` | 263 MB → 2.66 GB (10.1×; nats-server alone 1.78 GB) | 343 MB → 1.34 GB (3.9×) / 193 → 360 MB (1.9×) |
| vertices | 2.35M | 1.15M / 242k |
| `param_in` / `param_out` | 1.16M / 0.70M | 449k / 380k / 91k / 88k |
| `summary` | 3.55M | 1.26M / 206k |
| port `ddg` (`reaching-defs`) / `points-to` `ddg` | 4.69M / 653k | 1.84M / 373k / 439k / 39k |
| stand-ins (externals with a body) | 4,575 | 1,677 / 129 |
| bound call sites with a VTA-only target | 2.6–6.6% | 11.3% (11,294) / 0.4% |
| consistency checks (L3 and L4) | all PASS | all PASS |
| degraded codes | `range_func` (5 apps), `unmapped_ssa` (4) | `range_func`, `unmapped_ssa` / none |

**Key takeaways.**

1. **The scale gate passes.** The Kubernetes root completes `-a 4` in 31.5 s at `-j 1` (30.0 s at
   `-j 16`) with a whole-process peak of 7.7 GB, only 1.05× its `-a 3` peak, and its three
   outputs are byte-identical. The `go/packages` load still dominates RSS. L4 adds 18 s of wall
   time: SSA takes 1.0 s, VTA 2.3 s and the overlay 10.3 s, and the rest is emitting the 1.34 GB
   file.
2. **Memory is not the problem L4 has; output size is.** RSS grows by 5–40% on every app but
   one. nats-server is the outlier at 3.6× (4.1 GB). It has one giant recursive SCC whose
   global footprint (A4) gives 91% of its 1.58M vertices a `<global>:` `of`. The JSON grows
   4–10× on the hand-written apps and 22.7× on nats-server (78 MB → 1.78 GB). Most of those
   bytes are `<global>:` vertices, their port wiring, and param edges carrying full global ids.
3. **Summaries are near all-to-all, as A15 predicts.** 74–94% of call sites with summary edges
   have every `actual_in` → `actual_out` pair. At statement granularity the host already links
   every use to every def (the L3 call rule), so a summary contributes the callee's global and
   by-ref footprint, not intra-caller precision.
4. **VTA matters on a small share of call sites, but they are the ones taint needs.** Of the call
   sites that bind a target, 2.6–6.6% gain one that only VTA found on the six apps, and 11.3% on
   the Kubernetes root: interface calls (`store.Driver` in memos, `Repo` in the done fixture),
   function values, and the D7 interface-method binding on top. `k8s-api`, generated marshallers
   with no interfaces, has 0.4%.
5. **The points-to delta is moderate and strictly additive.** It is 7–19% of the port `ddg` on
   the six apps (nats-server 9.6%) and 20% / 9% on Kubernetes. The superset gate holds: every
   `-a 3` edge is in the `-a 4` file unchanged.
6. **Global port wiring is the volume driver.** On nats-server, 2.87M of 3.09M port `ddg` edges
   (93%) carry a global `var`. On listmonk, one function, `cmd.initHTTPHandlers` (a route table
   that registers about 300 handlers), holds 475k of the app's 523k port edges (91%). It is the
   host of hundreds of calls, each with the same global actuals, and the coarse host wiring
   multiplies them: every host use feeds every `actual_in`.
7. **Degradation is reported, never silent.** Five of the six apps and the Kubernetes root use
   range-over-func somewhere (`range_func`, D8). Four apps and the root carry `unmapped_ssa`,
   which comes almost entirely from closures in package-level `var` initializers. No app hit
   `no_ssa`, `no_vta`, `partial_ssa`, `partial_types` or `l3_skipped`.

### Per-app results (L4)

Times are seconds, RSS is MB (2^20 bytes), and JSON is MB (10^6 bytes). Every row is
go1.27.3, `--eager`, one timed run per configuration.

| app | wall `-a 3 -j 1` | wall `-a 4 -j 1` | wall `-a 4 -j 16` | user CPU `-a 4 -j 1` | RSS `-a 3 -j 1` | RSS `-a 4 -j 1` | RSS × | JSON `-a 3` | JSON `-a 4` | JSON × | SSA | VTA | overlay | `-a 4` identical |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| cobra | 0.45 | 0.54 | 0.52 | 1.13 | 268 | 290 | 1.08 | 2.1 | 6.7 | 3.2 | 9 ms | 5 ms | 74 ms | yes |
| listmonk | 0.93 | 2.37 | 2.37 | 5.84 | 989 | 1,384 | 1.40 | 10.6 | 103.3 | 9.8 | 33 ms | 31 ms | 1.04 s | yes |
| memos | 2.47 | 4.68 | 4.43 | 13.62 | 2,501 | 3,153 | 1.26 | 43.2 | 174.5 | 4.0 | 127 ms | 148 ms | 1.27 s | yes |
| nats-server | 2.31 | 23.48 | 23.27 | 42.70 | 1,128 | 4,103 | **3.64** | 78.2 | 1,775.2 | **22.7** | 224 ms | 183 ms | 14.76 s | yes |
| openbao | 5.01 | 10.77 | 10.27 | 30.13 | 5,173 | 5,893 | 1.14 | 101.6 | 473.4 | 4.7 | 292 ms | 403 ms | 3.59 s | yes |
| dae (linux) | 1.71 | 3.53 | 3.23 | 8.65 | 1,385 | 1,743 | 1.26 | 27.5 | 130.7 | 4.8 | 125 ms | 69 ms | 1.04 s | yes |
| **kubernetes** | 13.49 | **31.53** | 30.01 | 68.56 | 7,315 | **7,697** | 1.05 | 343.2 | 1,341.5 | 3.9 | 971 ms | 2.30 s | 10.31 s | **yes** |
| k8s-api | 5.55 | 10.98 | 9.44 | 24.27 | 2,044 | 2,356 | 1.15 | 192.8 | 360.2 | 1.9 | 517 ms | 291 ms | 3.44 s | yes |

`-j 16` barely helps at `-a 4`, by at most 1.5 s: SSA, VTA and the summary pass run serially by
design (D2, `ssa.BuildSerially`; the SCC pass is serial for determinism), and emission is one
stream.

| app | vertices | `formal_in` / `formal_out` | `actual_in` / `actual_out` | `<global>:` share of vertices | `param_in` | `param_out` | `summary` | sites with summary: all-to-all | port `ddg` | `points-to` `ddg` | stand-ins (formals) |
|---|---|---|---|---|---|---|---|---|---|---|---|
| cobra | 7,491 | 945 / 965 | 2,593 / 2,988 | 21.1% | 2,635 | 3,038 | 6,050 | 1,232: 74.1% | 10,790 | 5,637 | 91 (337) |
| listmonk | 58,546 | 3,698 / 3,721 | 26,432 / 24,695 | 46.6% | 28,270 | 26,653 | 40,912 | 6,105: 80.3% | 522,668 | 20,767 | 506 (2,021) |
| memos | 171,002 | 26,770 / 27,174 | 55,933 / 61,125 | 28.3% | 59,004 | 64,762 | 216,629 | 19,147: 86.6% | 237,601 | 68,956 | 830 (3,248) |
| nats-server | 1,584,949 | 324,205 / 155,426 | 734,728 / 370,590 | **91.2%** | 862,759 | 431,317 | 2,576,921 | 29,878: 78.9% | 3,093,971 | 295,776 | 667 (2,503) |
| openbao | 402,746 | 74,643 / 50,842 | 154,802 / 122,459 | 50.0% | 164,146 | 132,226 | 550,412 | 36,933: 82.0% | 605,515 | 195,288 | 1,639 (6,816) |
| dae | 120,882 | 18,889 / 17,257 | 42,266 / 42,470 | 49.6% | 43,611 | 43,881 | 155,698 | 12,163: 82.7% | 215,620 | 67,138 | 842 (3,298) |
| **kubernetes** | 1,154,746 | 229,049 / 197,197 | 394,069 / 334,431 | 40.9% | 448,676 | 380,030 | 1,260,761 | 97,185: 76.0% | 1,840,380 | 373,490 | 1,677 (6,447) |
| k8s-api | 242,414 | 27,556 / 35,489 | 91,428 / 87,941 | 7.2% | 91,428 | 87,941 | 205,643 | 40,856: 93.7% | 439,414 | 38,963 | 129 (812) |

Notes:
- The `-v` log's "port edges" count includes the `points-to` edges. The table splits them:
  port = `reaching-defs`, plus `points-to`.
- A stand-in is an external with a `body`. Every external a call site targets has one, except
  for the two to five per app whose formals are empty (`runtime.Gosched`).

| app | call nodes | sites that bind a target | with a VTA-only target | with ≥ 2 targets | `call_graph` VTA edges | VTA function set (library targets) | VTA edges | sites with targets (log) | non-call capture sites | degraded |
|---|---|---|---|---|---|---|---|---|---|---|
| cobra | 1,474 | 1,240 | 32 (2.6%) | 14 | 45 | 411 (83) | 1,294 | 1,244 | 37 | none |
| listmonk | 7,057 | 6,126 | 220 (3.6%) | 63 | 272 | 1,809 (640) | 7,304 | 6,194 | 47 | `range_func` (1 pkg, 1 callable) |
| memos | 23,781 | 19,209 | 997 (5.2%) | 516 | 1,059 | 7,893 (1,443) | 21,417 | 19,436 | 380 | `range_func` (7, 10); `unmapped_ssa` (2 fn / 4 calls) |
| nats-server | 38,335 | 30,007 | 1,633 (5.4%) | 898 | 2,133 | 10,473 (718) | 39,668 | 30,679 | 587 | `range_func` (2, 27); `unmapped_ssa` (25 / 4) |
| openbao | 51,991 | 37,139 | 2,461 (6.6%) | 1,802 | 4,389 | 19,978 (3,181) | 58,876 | 38,377 | 822 | `range_func` (5, 6); `unmapped_ssa` (10 / 12) |
| dae | 15,354 | 12,218 | 628 (5.1%) | 228 | 888 | 4,498 (1,286) | 14,104 | 12,598 | 263 | `range_func` (5, 5); `unmapped_ssa` (40 / 125) |
| **kubernetes** | 193,481 | 99,757 | 11,294 (11.3%) | 4,652 | 15,897 | 94,601 (41,107) | 249,493 | 102,407 | 3,890 | `range_func` (26, 49); `unmapped_ssa` (3,720 / 39,700) |
| k8s-api | 87,808 | 40,856 | 172 (0.4%) | 0 | 172 | 26,527 (8,570) | 52,526 | 40,856 | 175 | none |

- **"Sites that bind a target"** are call nodes with at least one `param_in` from an `actual_in`.
  A call node binds nothing when its callee has no formals (`runtime.Gosched`), when the CFG
  pruned it, or when it is unresolved: a builtin, a conversion, or a function value VTA did not
  resolve.
- **"VTA-only"** means at least one bound target is not the call node's static `callee`.
- **"Sites with targets (log)"** is the analyzer's own dispatch count. It also counts sites whose
  only target has no formals.
- **Degraded:** the `range_func` figures are (packages, callables) and the `unmapped_ssa`
  figures are (functions / calls), from the `-v` line.

### What changed since the earlier measurements

The task reports measured nats-server at several points on the branch, at `-j 8`, on the
toolchain given with each figure. On the eval binary, go1.27.3 `-j 1`:
- **summary** is 2,576,921, against 2,547,371 after the Task 17 alias round (`04244db`;
  go1.26.4) (+1.2%);
- **points-to** is 295,776, against 290,499 then and 292,707 after round 2 (`18fca40`; both
  go1.26.4);
- **param_out** is 431,317, against 431k at Task 14 (go1.27.3).

The growth since `18fca40` comes from:
- the reference-holding capture `formal_out` (`078616f`, MR5);
- the L3 host weak-def of every reference-holding capture (`cd37383`).

Both add edges only. The L3 `ddg` on nats-server went from 345,318 (§ Per-app counts) to 358,979
(+4.0%). That includes `cd37383`'s capture weak defs (chi +0.26%, go1.26.4) and the D1 L3 fixes since the
L3 sections were measured. Wall time and RSS in those reports were measured for one stage, not
end to end, on another toolchain. Compare the numbers here only with each other.

**`unmapped_ssa`, re-measured.** The Kubernetes root has 3,720 unmapped functions and 39,700
unmapped calls, against 3,754 / 39,728 at Task 9 (go1.27.3). Classified from `-vv`:
- 3,717 are closures in package-level `var` initializers (`init$N`; Ginkgo
  `var _ = Describe(…, func(){…})`; 3,555 of them under `test/`);
- 3 are a second `func init()` in one file (`init#N`).

The "method declared above its receiver type" cause Task 9 found is gone (fixed in `5c57a1a`).
None of these is a join bug. The other apps have the same cause in smaller numbers (memos 2,
dae 40, openbao 10, nats-server 25 functions).

### Cost of the memory levers

`sdg.Run` lowers the process GC target to 50 while it runs (Task 15b), and L1–L3 and the
emission keep the default. Here is what that costs, measured end to end against a local build
identical except for `l4GCPercent = 100` (not committed). Both builds are go1.27.3, `-a 4 -j 1
--eager`, two runs each, and every output is byte-identical to the eval's.

| app | GC target in L4 | wall (s) | user CPU (s) | sys (s) | peak RSS (MB) |
|---|---|---|---|---|---|
| nats-server | 50 (shipped) | 23.69 / 23.56 | 43.26 / 43.19 | 2.19 / 2.29 | 3,951 / 4,157 |
| nats-server | 100 | 23.40 / 23.00 | 34.80 / 34.61 | 1.87 / 2.16 | 4,463 / 4,563 |
| kubernetes | 50 (shipped) | 31.66 / 31.74 | 67.36 / 67.53 | 12.60 / 16.49 | 7,713 / 7,742 |
| kubernetes | 100 | 31.55 / 30.82 | 59.70 / 59.92 | 12.78 / 12.63 | 8,254 / 8,518 |

- **CPU.** GOGC=50 costs **+24.6% user CPU** on nats-server (+8.5 s) and **+12.8%** on Kubernetes
  (+7.6 s).
- **Wall time.** It costs +0.4 s on nats-server and +0.5 s on Kubernetes, about 1–2%. The extra
  GC work runs on idle cores of a 16-core host. **On a CPU-starved host the extra CPU becomes wall
  time**, up to the user-CPU delta.
- **Memory.** It saves 9–12% of peak RSS on nats-server (about 450 MB) and 7–9% on Kubernetes
  (about 650 MB).
- The other levers (per-SCC cache release, interned port names) have no CPU cost to report.
  Task 15b measured them: nats-server's live heap after fill went from 1.81 to 1.45 GB
  (go1.26.4).

**Where the end-to-end peak sits.** With the levers on, the `-a 4` peak is no longer in `sdg.Run`.
It is in the emission, which streams the envelope but builds each module's subtree whole before
writing it. The largest L4 modules are:

| app | module | JSON |
|---|---|---|
| nats-server | `server/server.go` | 340 MB |
| nats-server | `server/jetstream.go` | 164 MB |
| nats-server | `server/client.go` | 131 MB |
| kubernetes | `test/integration/dra/dra.go` | 66 MB |
| listmonk | `cmd/handlers.go` | 58 MB |

The application-scope `param_in` and `param_out` lists stream (nats-server: 368 MB and 180 MB of
JSON). The next lever, if a larger app needs one, is to stream each callable within a module
(spec §4.13, §11). It is not needed at this corpus's scale: Kubernetes is 1.05× its `-a 3` peak.

### Consistency at scale (L4)

`scripts/l3_metrics.py` runs its L4 checks on every callable and every edge of the eight `-a 4`
files. It applies the L3 checks of § Internal-consistency checks to the L3 part of the body only,
with vertices and port and points-to edges removed, so the L3 figures compare across levels.
Every check passes on every app:

| check | result |
|---|---|
| every `param_in` / `param_out` endpoint is a body node id (a callable's or a stand-in's) | 0 dangling, 8/8 apps |
| every `summary` endpoint is a vertex of its callable | 0 failures |
| no `summary` edge has `<global>:` vertices at both ends (#30) | 0 |
| L3 checks over the L3 part of every `-a 4` body (one `@entry`/`@exit`, reachability, endpoints, `prov == ["ssa"]` on the L3 edges) | all PASS |
| `-a 3` ⊆ `-a 4` | gated on the fixtures (`TestGateL4_SupersetOfL3`); on cobra, listmonk, memos and dae the L3 `body` / `cfg` / `cdg` / `ssa`-`ddg` counts of the `-a 4` file equal the `-a 3` file's exactly (counts, not an edge-by-edge diff; the other four apps were not compared) |

The Go-side gates do the arity and kind checks per edge on the fixtures
(`TestGateL4_ArityMatchesFormals`, `TestGateL4_NoDanglingEndpoints`,
`TestGateL4_VertexIdsAreGlobalOrdinals`). The HTTP → SQL "done" fixture
(`testdata/dataflow/httpsql`, `TestGateL4_HTTPToSQLPath`) does a BFS over the emitted JSON graph.
The handler's `r` reaches `database/sql.DB.Query`'s `query` formal through a VTA-bound `param_in`
into `sqlRepo.Find`. A handler that never queries does not reach it, and neither does a separate
entry, `Audit`, reach `Find`'s `id`. `TestGateL4_HTTPToSQLSanitizerIsSDKSemantics` pins the
sanitizer variant (§ Limitations).

**Metamorphic relations.** `scripts/metamorphic.sh` (memos, the default) passes on this HEAD with
0 violations in all 14 relations. It covered 20–32 sites for MR1–MR6, MR14 and MR15, and 33M
reachability pairs for MR5 and MR6. The run took about 2 min. The last `--all` run (memos and
nats-server, 0 violations, 23 min, 16.2 GB; go1.27.3, the script's `EVAL_GO`) is from Task 17b
on `e160b17` and `4a6c885`.

### Insights

- **The coarse host wiring is cheap where calls are sparse, quadratic where they are dense.** A
  statement that hosts n calls, each with g global actuals, gets about n·g port edges in each
  direction, and a body that hosts many such statements multiplies that again. listmonk's
  `initHTTPHandlers` (one function, 91% of the app's port edges) and nats-server's
  `client.parse` (162k) are the extremes. Python's reaching-def-matched host wiring (spec §4.7,
  optional) would cut this volume without losing a flow; it is not built.
- **Globals are most of L4 on server-shaped code.** nats-server's `Server` methods form one large
  recursive SCC. Every package variable any member reads or writes becomes a formal of every
  member (A4, transitive), so 91% of its vertices are `<global>:` vertices. The cut of global →
  global summaries (#30) already removed 98M summary edges there (Task 15, go1.27.3; spec §4.9). The vertices and
  their port edges remain, and they are what sources and sinks can name.
- **VTA is cheap.** Under 2.3 s on Kubernetes, with a function set of 94,601, of which 41,107 are
  target-only library functions. The SSA build is about 1 s. The dominant L4 compute cost is the
  overlay: flow graphs, summaries and port wiring, where `graphOf`/`bodyFlows` and GC dominate
  (Task 18a profile, go1.26.4). The dominant wall-time cost on the large files is emission.
- **k8s-api is the counter-example.** Generated marshallers have few interface calls (0.4% VTA),
  few globals (7% of vertices) and leaf-heavy call graphs. L4 adds only 1.9× bytes there, the
  smallest growth in the corpus, because its L3 `ddg` already dominates the file.

### Limitations (L4)

These are known, by design or measured, and each is a source of noise (extra edges) or of a gap
(no edge does not mean no flow). Under the anti-goal, every gap is either closed by
over-approximation or reported as degraded.

- **D7 noise: every interface call also binds the interface method.**
  - Its pass-through summary is all-to-all, so an interface call with concrete VTA targets still
    carries the all-to-all edges of the interface stand-in.
  - Over-approximation by design: a call whose VTA set is empty still keeps its flow.
  - Interfaces with no symbol-table node (the universe `error.Error()`, interface literals) get
    no interface-method target. They keep only their VTA targets, and with none the L3 host
    pass-through applies (spec §4.8). A sink cannot be named as `error.Error`.
- **D8: range-over-func is not modelled.** `for v := range seq` routes the loop body through a
  synthetic `yield`, so what the iterator passes to `yield` does not reach `v`
  interprocedurally. Every program that uses it is degraded `range_func`: 5 of the 6 apps and
  the Kubernetes root. The SDK then refuses to certify "no flow" over that graph (spec §4.15).
  That is safe, but it means `exhausted` is never returned on most real apps until D8 is
  modelled.
- **Library models and sanitizers change summaries, not statement-level reachability**
  [user, 2026-10-27].
  - L4 is reaching-definitions data flow. In `n, err := strconv.Atoi(id)` the host statement
    links `id` to `n` and `err` (the L3 call rule), and that is correct.
  - `--library-models` `strconv/Atoi: []` removes only the Atoi call site's `summary` edges, so
    the done fixture's sanitized path still reaches `DB.Query`.
  - An SDK callable sanitizer cuts only nodes under the sanitizer's id, never the caller's host.
  - A variable cut on `n` within `SanitizedFind` is bypassed three ways, each correct
    dependence:
    1. `err` → `if err != nil` → the Query statement (cdg, implicit flow);
    2. `return nil, err` → the caller → the receiver `actual_in`;
    3. the caller's host feeding `id` into the receiver `actual_in` (coarse host wiring).
  - Sanitizer semantics are the SDK taint analysis's job: [codellm-devkit/.github#103](https://github.com/codellm-devkit/.github/issues/103).
    `TestGateL4_HTTPToSQLSanitizerIsSDKSemantics` pins each fact.
  - The same statement-level call rule exists in codeanalyzer-python
    (`access_paths.py:500-501`, `:350-356`), so this is cross-language.
- **Library callbacks are not edges.** In `sort.Slice(xs, less)` and `http.HandleFunc(path, h)`,
  the function value flows into the stand-in, but the library calling it back is not modelled.
  Taint that *starts* in a handler (its own parameters) is unaffected.
- **A15: summaries are near all-to-all** (74–94% of sites, table above). Under the coarse host
  wiring, a summary edge adds no reachability for the SDK's context-insensitive walk. It is a
  shortcut hop, and a precondition for a future HRB two-phase walk (spec §11).
- **#30: the global → global summary cut, and the depth-bounded slice caveat.**
  - Summary edges with both ends `<global>:` are omitted [user, 2026-10-26]. Measured: 97.7% of
    101M summary edges on nats-server (Task 14, go1.27.3).
  - Reachability is unchanged. But python-sdk's slices and taint walks are depth-bounded
    (`DEFAULT_DEPTH = 5` for slices). A dropped shortcut makes the remaining route longer (into
    the callee's body and back), so a depth-bounded slice may stop short of a node it would
    otherwise reach.
  - They return with the HRB walk (codeanalyzer-go#30).
- **Option A alias volume.**
  - The one-level offset also applies to reference-compatible field types (`m := s.cache;
    p := r.ptr`), not only to `&x.f` (Task 17 ruling). That is most of the `points-to` volume:
    217k of 290k on nats-server at Task 17 (go1.26.4).
  - `MayHoldRef` also admits value structs that hold a reference, so `s1.n` → `s2.n` between two
    distinct value copies is a spurious edge.
  - Both are sound over-approximations: 7–20% of port `ddg` here.
- **One-level alias offsets only.**
  - Not caught: offsets deeper than one step (`&x.f.g` against a `*G`), two prefixes of one base
    that alias each other, and an interface holding a value struct whose reference field is
    written through the interface (spec §4.10).
  - These are false negatives of the type-based oracle that an Andersen points-to over `go/ssa`
    would close (§11). They are the one class of L4 gap that is neither over-approximated nor
    degraded.
- **L3 capture weak-def breadth (`cd37383`).**
  - At L3, a closure's host weak-defines every reference-holding captured base, read-only
    captures included. That is broader than needed: a read-only capture could weak-define
    `x.*` without downgrading the base.
  - Sound. It costs about 0.26% L3 `ddg` on chi (go1.26.4).
- **The stored literal reached through an unresolved function value (Task 13).**
  - A global written only inside a literal that is stored and later called through a function
    value nothing resolves has no `param_out` from that write. Inside the encloser, the global's
    `formal_out` is fed only by its `formal_in` (the identity edge).
  - The flow is still reachable at the caller, whose host statement weak-defines the global.
  - `TestGateL4_StoredLiteralGlobalReachable` pins both the reachable flow and the
    intra-procedural gap, so the gap closing fails the test on purpose.
- **#29: cgo.** cgo-generated wrapper functions have no L1 callable, so SSA functions and calls
  inside them are `unmapped_ssa`. A flow through C is not modelled (non-goal). The fixture test
  `TestJoin_CgoNamesIssue29` skips when cgo is off.
- **#31: `base_types` / `interfaces`.**
  - This predates L4 and is not an L4 change, but L4 consumers meet it. `base_types[]` holds raw
    type strings, not type ids, so `GO_EMBEDS` never resolves, and `interfaces[]` is never
    computed (`GO_SATISFIES` is empty).
  - VTA does not need either; it reads `go/types` directly.
- **Memory and size are reported, not bounded** (D3). The largest file in the corpus is 1.78 GB
  (nats-server). Loading it whole in Python, as `scripts/l3_metrics.py` does, needs about
  10–20 GB. The SDK's JSON backend loads it whole too. For apps that size the Neo4j backend is
  the practical one.
