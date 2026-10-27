#!/usr/bin/env bash
# metamorphic.sh - run the L3/L4 metamorphic relations on prepared eval apps.
#
# Usage:
#   scripts/metamorphic.sh                  the default set: memos (~2 min); run it for any L3/L4 change
#   scripts/metamorphic.sh --all            every app below: memos nats-server (~25 min; before a release)
#   scripts/metamorphic.sh APP [APP...]     the named apps, e.g. scripts/metamorphic.sh nats-server
#
# Each APP is a clone under $EVAL_SRC, prepared with scripts/eval_l3.sh --prepare APP. A missing
# app is skipped by the test (and reported). The relations transform a copy of the app in a temp
# dir and analyze it in-process; the app itself is never modified. Each relation samples
# MR_SAMPLE_FILES files and MR_SAMPLE_CALLABLES callables with seed 1
# (internal/metamorphic/realapp_test.go).
#
# Environment:
#   EVAL_SRC             clones                           (default /tmp/eval)
#   EVAL_GO              Go toolchain for the test run    (default go1.27.3; eval_l3.sh's default)
#   MR_RELATIONS         relations to run, space-separated (default: all of MR1-MR6, MR8-MR15)
#   MR_SAMPLE_FILES      files transformed per relation   (default 20)
#   MR_SAMPLE_CALLABLES  callables sampled by the reachability oracle (default 50; 20 gives a
#                        quicker nats-server run)
#   MR_TIMEOUT           go test -timeout                 (default 120m)
#   MR_LOG               log file                         (default $EVAL_OUT/metamorphic-<date>.log)
#
# Prints one MRSTAT line per relation and app (sites, reachability pairs, violations, time,
# skips) and exits non-zero on any violation.
set -u

EVAL_SRC=${EVAL_SRC:-/tmp/eval}
EVAL_OUT=${EVAL_OUT:-/tmp/eval-out}
EVAL_GO=${EVAL_GO:-go1.27.3}
MR_TIMEOUT=${MR_TIMEOUT:-120m}
MR_LOG=${MR_LOG:-$EVAL_OUT/metamorphic-$(date +%Y-%m-%d).log}
REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)

DEFAULT_APPS="memos"
ALL_APPS="memos nats-server"
for a in "$@"; do
  case $a in
    -h|--help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 0 ;;
    --all) [ $# -eq 1 ] || { echo "--all takes no other arguments" >&2; exit 2; } ;;
    -*) echo "unknown flag: $a" >&2; exit 2 ;;
  esac
done
case "${1:-}" in
  --all) set -- $ALL_APPS ;;
  "") set -- $DEFAULT_APPS ;;
esac
for a in "$@"; do
  [ -f "$EVAL_SRC/$a/go.mod" ] || echo "warning: $a is not prepared at $EVAL_SRC/$a (skipped)" >&2
done
mkdir -p "$(dirname "$MR_LOG")"

start=$(date +%s)
(cd "$REPO_ROOT" && EVAL_SRC=$EVAL_SRC MR_APPS="$*" MR_RELATIONS="${MR_RELATIONS:-}" \
  MR_SAMPLE_FILES="${MR_SAMPLE_FILES:-20}" MR_SAMPLE_CALLABLES="${MR_SAMPLE_CALLABLES:-50}" \
  GOTOOLCHAIN=${GOTOOLCHAIN:-$EVAL_GO} \
  go test -tags metamorphic -count=1 -timeout "$MR_TIMEOUT" -v -run 'TestMR_RealApps' ./internal/metamorphic/) \
  >"$MR_LOG" 2>&1
rc=$?
end=$(date +%s)

grep -E 'MRLOAD|MRSTAT' "$MR_LOG" | sed 's/^ *[a-z_]*\.go:[0-9]*: //'
grep -E '^\s+(realapp|mr)_test\.go:[0-9]+: MR[0-9]+ on ' "$MR_LOG" | sed 's/^ *//' | head -200
echo "log: $MR_LOG ($((end - start)) s, go test rc $rc)"
exit $rc
