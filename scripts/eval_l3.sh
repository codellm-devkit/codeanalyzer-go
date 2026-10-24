#!/usr/bin/env bash
# eval_l3.sh - real-app evaluation of cango -a 3 (L3 dataflow).
#
# Usage:  scripts/eval_l3.sh APP_DIR [APP_DIR...]
#
# For each app directory it runs, with --eager (no cache) and the analyzer's stderr captured
# separately from the /usr/bin/time -l report:
#   0. cango -a 2 -j 1     (warm-up, discarded)
#   1. cango -a 2 -j 1     (wall time baseline)
#   2. cango -a 3 -j 1
#   3. cango -a 3 -j N     (N = CPU count)
#   4. cango -a 3 -j N     (second run)
# and compares the three -a 3 analysis.json files byte for byte. It records wall time, peak
# RSS (macOS /usr/bin/time -l), module/callable/body-node/edge counts, external_symbols, the
# call_graph split, the number of type-error warnings ("package ..." stderr lines), and the number of "L3 skipped" stderr lines (caught per-callable panics).
#
# Environment:
#   CANGO       analyzer binary          (default /tmp/cango-eval)
#   EVAL_OUT    raw outputs and logs     (default /tmp/eval-out)
#   EVAL_NOTE   markdown results file    (default $EVAL_OUT/l3-<date>.md)
#   EVAL_JOBS   N for the -j N runs      (default: CPU count)
#   RUN_TIMEOUT per-run limit, seconds   (default 3600); on expiry the run is killed, the app
#               is marked TIMEOUT and its remaining runs are skipped
# Any other environment (GOOS, GOFLAGS, ...) is inherited by the analyzer unchanged.
#
# Requires: /usr/bin/time -l (BSD/macOS), cmp. jq is used for counts; without it the count
# columns are reported as "n/a" and everything else still works.
#
# Apps and pinned commits for the 2026-10 evaluation (clone into /tmp/eval with
# `git clone --filter=blob:limit=2m`, then `git checkout <sha>`):
#   spf13/cobra                adbc881      smoke test, no concurrency
#   daeuniverse/dae            e3fee8f      eBPF, heavy select; `make ebpf` (needs dae_bpf_headers submodule)
#   knadh/listmonk             8216c4f      echo handlers -> sqlx
#   usememos/memos             28719a9      HTTP -> SQL through store.Driver interface
#   nats-io/nats-server        f8f6db8      CFG stress, select / channels / defer
#   openbao/openbao            1d58653      large app, many defers
#   cockroachdb/cockroach      d30c905f     scale test; protobufs need Bazel (./dev generate go)
set -u

CANGO=${CANGO:-/tmp/cango-eval}
EVAL_OUT=${EVAL_OUT:-/tmp/eval-out}
DATE=$(date +%Y-%m-%d)
EVAL_NOTE=${EVAL_NOTE:-$EVAL_OUT/l3-$DATE.md}
RUN_TIMEOUT=${RUN_TIMEOUT:-3600}
JOBS=${EVAL_JOBS:-$( (sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4) | head -1)}

if [ $# -eq 0 ]; then
  echo "usage: $0 APP_DIR [APP_DIR...]" >&2
  exit 2
fi
[ -x "$CANGO" ] || { echo "analyzer binary not found: $CANGO" >&2; exit 2; }
mkdir -p "$EVAL_OUT"

HAVE_JQ=0
command -v jq >/dev/null 2>&1 && HAVE_JQ=1

JQ_COUNTS='
(.application) as $a
| [$a.symbol_table[]|..|objects|select(has("signature") and has("parameters"))] as $c
| [ ($a.symbol_table|length), ($c|length),
    ([$c[]|(.body//{})|length]|add//0),
    ([$c[]|(.cfg//[])|length]|add//0),
    ([$c[]|(.cdg//[])|length]|add//0),
    ([$c[]|(.ddg//[])|length]|add//0),
    (($a.external_symbols//{})|length),
    ([$a.call_graph[]|select(.dst|contains("/@external/")|not)]|length),
    ([$a.call_graph[]|select(.dst|contains("/@external/"))]|length) ] | @tsv'

# run_one NAME TAG LEVEL JOBS -> sets RC WALL RSS; output in $EVAL_OUT/NAME-TAG/analysis.json
run_one() {
  local name=$1 tag=$2 level=$3 jobs=$4
  local dir=$EVAL_OUT/$name-$tag
  rm -rf "$dir"; mkdir -p "$dir"
  /usr/bin/time -l -o "$dir.time" "$CANGO" -i "$APP" --app-name "$name" -a "$level" -j "$jobs" \
    --eager -c "$EVAL_OUT/cache-$name" -o "$dir" >/dev/null 2>"$dir.err" &
  local pid=$! waited=0
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$waited" -ge "$RUN_TIMEOUT" ]; then
      touch "$dir.timeout"
      pkill -TERM -P "$pid" 2>/dev/null; kill -TERM "$pid" 2>/dev/null
      break
    fi
    sleep 1; waited=$((waited + 1))
  done
  wait "$pid"
  RC=$?
  if [ -e "$dir.timeout" ]; then RC=124; fi
  WALL=$(awk '/ real /{print $1}' "$dir.time")
  RSS=$(awk '/maximum resident set size/{printf "%d", $1/1048576}' "$dir.time")
  [ -n "$WALL" ] || WALL=n/a
  [ -n "$RSS" ] || RSS=n/a
}

{
  echo "# L3 real-app evaluation ($DATE)"
  echo
  echo "Analyzer: $("$CANGO" --version 2>&1 | head -1); host: $(uname -sm); j=$JOBS; all runs --eager."
  echo
  echo "| app | status | wall a2 j1 (s) | wall a3 j1 (s) | wall a3 jN (s) | RSS a3 j1 (MB) | modules | callables | body nodes | cfg | cdg | ddg | ext symbols | cg in-project | cg external | -j identical | L3 skips | type errors |"
  echo "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"
} >"$EVAL_NOTE"

FAILS=""
for APP in "$@"; do
  APP=${APP%/}
  name=$(basename "$APP")
  echo "== $name" >&2
  status=ok

  rm -f "$EVAL_OUT/$name"-*.timeout
  W3=n/a; WN=n/a; R3=n/a; RC3=0; RCN=0; RCN2=0
  # Warm-up (discarded): populates the Go build/module cache and the OS file cache so the
  # timed -a 2 and -a 3 runs are comparable.
  run_one "$name" warm 2 1;        RCW=$RC
  run_one "$name" a2j1 2 1;        W2=$WALL; RC2=$RC
  if [ "$RCW" -ne 124 ] && [ "$RC" -ne 124 ]; then run_one "$name" a3j1 3 1;        W3=$WALL; R3=$RSS; RC3=$RC; fi
  if [ "$RC3" -ne 124 ] && [ "$RC2" -ne 124 ]; then
    run_one "$name" a3jN 3 "$JOBS";  WN=$WALL; RCN=$RC
    if [ "$RCN" -ne 124 ]; then run_one "$name" a3jN2 3 "$JOBS"; RCN2=$RC; fi
  fi

  ident=n/a
  if [ "$RC2" -eq 124 ] || [ "$RC3" -eq 124 ] || [ "$RCN" -eq 124 ] || [ "$RCN2" -eq 124 ]; then
    status="TIMEOUT(>${RUN_TIMEOUT}s; remaining runs skipped)"
    FAILS="$FAILS $name"
  elif [ "$RC3" -ne 0 ] || [ "$RCN" -ne 0 ] || [ "$RCN2" -ne 0 ] || [ "$RC2" -ne 0 ]; then
    status="FAIL(rc a2=$RC2 j1=$RC3 jN=$RCN jN2=$RCN2)"
    FAILS="$FAILS $name"
  else
    a=$EVAL_OUT/$name-a3j1/analysis.json
    if cmp -s "$a" "$EVAL_OUT/$name-a3jN/analysis.json" && cmp -s "$a" "$EVAL_OUT/$name-a3jN2/analysis.json"; then
      ident=yes
    else
      ident=NO; status="FAIL(-j differs)"; FAILS="$FAILS $name"
    fi
  fi

  skips=$(grep -c 'L3 skipped' "$EVAL_OUT/$name-a3j1.err" 2>/dev/null || true)
  terrs=$(grep -c 'WARN\] package ' "$EVAL_OUT/$name-a3j1.err" 2>/dev/null || true)
  counts="n/a	n/a	n/a	n/a	n/a	n/a	n/a	n/a	n/a"
  if [ "$HAVE_JQ" -eq 1 ] && [ "$RC3" -eq 0 ]; then
    counts=$(jq -r "$JQ_COUNTS" "$EVAL_OUT/$name-a3j1/analysis.json" 2>/dev/null || echo "$counts")
  fi
  IFS=$'\t' read -r c1 c2 c3 c4 c5 c6 c7 c8 c9 <<EOC
$counts
EOC
  printf '| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n' \
    "$name" "$status" "$W2" "$W3" "$WN" "$R3" "$c1" "$c2" "$c3" "$c4" "$c5" "$c6" "$c7" "$c8" "$c9" \
    "$ident" "$skips" "$terrs" >>"$EVAL_NOTE"
done

echo >>"$EVAL_NOTE"
echo "Raw outputs and logs: $EVAL_OUT/<app>-<run>{/analysis.json,.err,.time}" >>"$EVAL_NOTE"
echo "wrote $EVAL_NOTE" >&2
[ -z "$FAILS" ] || { echo "failures:$FAILS" >&2; exit 1; }
