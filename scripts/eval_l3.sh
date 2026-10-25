#!/usr/bin/env bash
# eval_l3.sh - reproducible real-app evaluation of cango -a 3 (L3 dataflow).
#
# Usage:
#   scripts/eval_l3.sh --prepare [APP...]   clone, set up and type-check apps (all if none named)
#   scripts/eval_l3.sh APP [APP...]          run the evaluation on prepared apps, by manifest name
#   scripts/eval_l3.sh --all                 run every app in the manifest (each must be prepared)
#   scripts/eval_l3.sh APP_DIR [APP_DIR...]  run on plain directories (no manifest, no preparation)
#   scripts/eval_l3.sh --clean               delete run outputs (runs, caches, notes, built cango) in
#                                            $EVAL_OUT; keep the clones and the prepared state
#   scripts/eval_l3.sh --list                print the manifest
#
# Example (from the repository root):
#   scripts/eval_l3.sh --prepare kubernetes k8s-api
#   scripts/eval_l3.sh kubernetes k8s-api
#
# --prepare, for each app:
#   1. Clones into $EVAL_SRC/<repo-name> when absent: git init, then
#      git fetch --depth 1 --filter=blob:limit=2m origin <sha>, then git checkout FETCH_HEAD.
#      An existing clone is reused when HEAD is the pinned commit; at any other commit the app is
#      refused (it is never reset).
#   2. Checks the tools its prep needs. A missing tool is reported with how to get it; prep is
#      then skipped and the app is marked "degraded: missing <tool>". Nothing is installed.
#   3. Runs prep inside the clone, logging to $EVAL_OUT/prep-<name>.log, and records its exit code.
#   4. Type-checks the module dir with the app's env and GOTOOLCHAIN=$EVAL_GO:
#      go list -e -export ./... . -export compiles every package, so a type error becomes that
#      package's Error, and a package broken only by a dependency gets DepsErrors. (Plain
#      go list -e -json reports load errors only: it shows 0 errors on dae for GOOS=darwin, where
#      2 packages fail to compile and 3 more are broken through them.) The status is "complete"
#      (no package with Error or DepsErrors) or "degraded: N packages with errors"; the per-package
#      list is in $EVAL_OUT/prepared/<name>.typecheck.
#   5. Writes $EVAL_OUT/prepared/<name>.env (module dir, env, commit, prep and type-check status),
#      which the run phase reads.
#
# Run phase, for each app (manifest name or directory), with --eager (no cache) and the
# analyzer's stderr captured separately from the /usr/bin/time -l report:
#   0. cango -a 2 -j 1     (warm-up, discarded)
#   1. cango -a 2 -j 1     (wall time baseline)
#   2. cango -a 3 -j 1
#   3. cango -a 3 -j N     (N = CPU count)
#   4. cango -a 3 -j N     (second run)
#   5. cango -a 4 -j 1 -v  (only when CANGO_LEVELS includes 4)
# and compares the three -a 3 analysis.json files byte for byte. It records wall time, peak
# RSS (macOS /usr/bin/time -l), module/callable/body-node/edge counts, external_symbols, the
# call_graph split, the prepare-time type-check status, the number of type-error warnings
# ("package ..." stderr lines), the analyzer's too-old-toolchain warning, and the number of
# "L3 skipped" stderr lines (caught per-callable panics). A manifest app runs with its env
# (e.g. GOOS=linux for dae) on top of the inherited environment; every run sets
# GOTOOLCHAIN=$EVAL_GO unless GOTOOLCHAIN is already set.
#
# Environment:
#   EVAL_SRC    clones                   (default /tmp/eval)
#   EVAL_OUT    raw outputs and logs     (default /tmp/eval-out)
#   EVAL_GO     Go toolchain for the analyzer build, type-check and runs (default go1.27.3). It
#               must be at least the newest `go` line in the corpus (go 1.27.x today), or the
#               analyzer type-checks those packages only partially (one consolidated WARN).
#   CANGO       analyzer binary. When unset, the run phase builds one from this checkout with
#               GOTOOLCHAIN=$EVAL_GO go build -o $EVAL_OUT/cango ./cmd/codeanalyzer
#   EVAL_NOTE   markdown results file    (default $EVAL_OUT/l3-<date>.md)
#   EVAL_JOBS   N for the -j N runs      (default: CPU count)
#   RUN_TIMEOUT per-run limit, seconds   (default 3600); on expiry the run is killed, the app
#               is marked TIMEOUT and its remaining runs are skipped
#   CANGO_LEVELS levels to run (default "2 3"). Levels 2 and 3 always run (the L3 baseline);
#               adding 4 (e.g. "2 3 4") also runs step 5 and records its wall time, peak RSS, and the
#               SSA build time, VTA time and VTA edge count from its -v log.
#   EVAL_PRUNE  1 = after the comparison, delete the warm-up and both -j N analysis.json files
#               (keeps a2j1 and a3j1, and every .err/.time). For very large apps.
# Any other environment (GOOS, GOFLAGS, ...) is inherited by the analyzer unchanged.
#
# Requires: git, go, /usr/bin/time -l (BSD/macOS), cmp. jq is used for counts; without it the
# count columns are reported as "n/a" and everything else still works.
set -u

EVAL_SRC=${EVAL_SRC:-/tmp/eval}
EVAL_OUT=${EVAL_OUT:-/tmp/eval-out}
EVAL_GO=${EVAL_GO:-go1.27.3}
DATE=$(date +%Y-%m-%d)
EVAL_NOTE=${EVAL_NOTE:-$EVAL_OUT/l3-$DATE.md}
RUN_TIMEOUT=${RUN_TIMEOUT:-3600}
CANGO_LEVELS=${CANGO_LEVELS:-"2 3"}
RUN_L4=0
case " $CANGO_LEVELS " in *" 4 "*) RUN_L4=1 ;; esac
EVAL_PRUNE=${EVAL_PRUNE:-0}
JOBS=${EVAL_JOBS:-$( (sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4) | head -1)}
REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
PREPARED=$EVAL_OUT/prepared

# Homebrew's keg-only llvm@18 provides clang and llvm-strip for dae's `make ebpf`.
if command -v brew >/dev/null 2>&1; then
  LLVM18=$(brew --prefix llvm@18 2>/dev/null || true)
  if [ -n "$LLVM18" ] && [ -d "$LLVM18/bin" ]; then PATH=$LLVM18/bin:$PATH; export PATH; fi
fi

# MANIFEST: name|repo|commit|subdir|env|prep|tools|role   ("-" = none)
#   subdir  module dir inside the clone, analyzed with -i ("." = clone root)
#   env     extra environment for go list and the analyzer (space-separated K=V)
#   prep    shell snippet run inside the clone after checkout (no "|" allowed)
#   tools   tools prep needs, space-separated; "a/b" means either a or b
#   The clone dir is $EVAL_SRC/<basename of repo>, so entries may share one clone.
MANIFEST=$(cat <<'EOF'
cobra|https://github.com/spf13/cobra|adbc8813901bba65827259daa8e22ff94ec1f30e|.|-|-|-|smoke test; CLI library, no concurrency
listmonk|https://github.com/knadh/listmonk|8216c4f15fa918ffe7e1a9e692701df5134a5f4b|.|-|-|-|small HTTP-to-SQL app: echo handlers into sqlx
memos|https://github.com/usememos/memos|28719a925fb28432d7b253863138ab388ec4012a|.|-|-|-|HTTP to SQL through an interface (store.Driver)
nats-server|https://github.com/nats-io/nats-server|f8f6db894528640b27311353e874ac62c3a47af5|.|-|-|-|CFG stress: select, channels, defer, cyclomatic 453
openbao|https://github.com/openbao/openbao|1d58653b0c00e53484de5af18f20679d78b9fb28|.|-|-|-|large app; many defers
dae|https://github.com/daeuniverse/dae|e3fee8fbc68a65167af13b685ab0b958757e20ee|.|GOOS=linux|git submodule update --init --depth 1 && make ebpf|clang llvm-strip|eBPF proxy, heavy select; Linux-only, bpf2go code generated
kubernetes|https://github.com/kubernetes/kubernetes|f78e722310e50bcaca9276be22276d9e91d91308|.|-|-|-|scale benchmark: root module k8s.io/kubernetes (v1.37.1), fully type-checked
k8s-api|https://github.com/kubernetes/kubernetes|f78e722310e50bcaca9276be22276d9e91d91308|staging/src/k8s.io/api|-|-|-|generated protobuf marshallers (k8s.io/api, v1.37.1): DDG volume on dAtA[] code
cockroach|https://github.com/cockroachdb/cockroach|d30c905fff79ef825adc96bcc647f1872a90f2ff|.|-|./dev generate go|bazel/bazelisk|very large, partly type-checked without Bazel; not a published benchmark (proprietary license)
EOF
)

usage() { sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

# manifest_get NAME -> sets M_NAME M_REPO M_COMMIT M_SUBDIR M_ENV M_PREP M_TOOLS M_ROLE; 1 if absent
manifest_get() {
  local line
  line=$(printf '%s\n' "$MANIFEST" | awk -F'|' -v n="$1" '$1 == n { print; exit }')
  [ -n "$line" ] || return 1
  IFS='|' read -r M_NAME M_REPO M_COMMIT M_SUBDIR M_ENV M_PREP M_TOOLS M_ROLE <<EOM
$line
EOM
  [ "$M_ENV" = "-" ] && M_ENV=""
  [ "$M_PREP" = "-" ] && M_PREP=""
  [ "$M_TOOLS" = "-" ] && M_TOOLS=""
  M_CLONE=$EVAL_SRC/$(basename "$M_REPO" .git)
  if [ "$M_SUBDIR" = "." ]; then M_MODDIR=$M_CLONE; else M_MODDIR=$M_CLONE/$M_SUBDIR; fi
  return 0
}

manifest_names() { printf '%s\n' "$MANIFEST" | cut -d'|' -f1; }

tool_hint() {
  case $1 in
    clang|llvm-strip) echo "brew install llvm@18 (keg-only; this script adds \$(brew --prefix llvm@18)/bin to PATH)" ;;
    bazel|bazelisk) echo "brew install bazelisk (provides bazel), see https://github.com/bazelbuild/bazelisk" ;;
    *) echo "install $1 and put it on PATH" ;;
  esac
}

# ---------------------------------------------------------------------------------- --list
if [ "${1:-}" = "--list" ]; then
  printf '%s\n' "$MANIFEST" | awk -F'|' '{ printf "%-12s %s  %s  %s\n", $1, substr($3, 1, 12), $4, $8 }'
  exit 0
fi

# --------------------------------------------------------------------------------- --clean
if [ "${1:-}" = "--clean" ]; then
  [ -d "$EVAL_OUT" ] || exit 0
  case $EVAL_OUT in /|"$HOME"|"$EVAL_SRC") echo "refusing to clean $EVAL_OUT" >&2; exit 2 ;; esac
  for n in $(manifest_names) $(ls "$EVAL_OUT" 2>/dev/null | sed -n 's/-a3j1$//p'); do
    for t in warm a2j1 a3j1 a3jN a3jN2 a4j1; do
      rm -rf "$EVAL_OUT/$n-$t" "$EVAL_OUT/$n-$t.err" "$EVAL_OUT/$n-$t.time" "$EVAL_OUT/$n-$t.timeout"
    done
    rm -rf "$EVAL_OUT/cache-$n"
  done
  rm -f "$EVAL_OUT"/l3-*.md "$EVAL_OUT/cango"
  echo "cleaned run outputs in $EVAL_OUT (kept $PREPARED, prep logs and clones in $EVAL_SRC)" >&2
  exit 0
fi

# ------------------------------------------------------------------------------- --prepare
# prepare_one NAME -> 0 prepared (complete or degraded), 1 not preparable
prepare_one() {
  local name=$1
  manifest_get "$name" || { echo "unknown app: $name (see --list)" >&2; return 1; }
  echo "== prepare $name ($M_CLONE @ ${M_COMMIT:0:12})" >&2
  mkdir -p "$EVAL_SRC" "$PREPARED"
  rm -f "$PREPARED/$name.env"

  # 1. Clone or reuse.
  if [ ! -e "$M_CLONE" ]; then
    mkdir -p "$M_CLONE"
    if ! (cd "$M_CLONE" && git init -q && git remote add origin "$M_REPO" &&
          git fetch --depth 1 --filter=blob:limit=2m origin "$M_COMMIT" &&
          git checkout -q FETCH_HEAD); then
      echo "  clone failed: $M_REPO $M_COMMIT (left at $M_CLONE; remove it to retry)" >&2
      return 1
    fi
  fi
  local head
  head=$(git -C "$M_CLONE" rev-parse HEAD 2>/dev/null) || {
    echo "  $M_CLONE exists but is not a git checkout; refusing" >&2; return 1; }
  if [ "$head" != "$M_COMMIT" ]; then
    echo "  $M_CLONE is at $head, not the pinned $M_COMMIT; refusing (not resetting it)." >&2
    echo "  Check it out yourself, or move it aside to let --prepare clone afresh." >&2
    return 1
  fi
  [ -d "$M_MODDIR" ] || { echo "  module dir missing: $M_MODDIR" >&2; return 1; }

  # 2. Tools.
  local missing="" t alt found
  for t in $M_TOOLS; do
    found=""
    for alt in $(echo "$t" | tr '/' ' '); do command -v "$alt" >/dev/null 2>&1 && found=$alt && break; done
    if [ -z "$found" ]; then
      missing="$missing ${t//\// or }"
      echo "  missing tool: ${t//\// or } -> $(tool_hint "${t%%/*}")" >&2
    fi
  done
  missing=${missing# }

  # 3. Prep.
  local prep_status=none prep_rc=-
  local log=$EVAL_OUT/prep-$name.log
  if [ -n "$M_PREP" ]; then
    if [ -n "$missing" ]; then
      prep_status="skipped: missing $missing"
      echo "prep skipped: missing $missing" >"$log"
    else
      echo "  prep: $M_PREP (log: $log)" >&2
      (cd "$M_CLONE" && GOTOOLCHAIN=${GOTOOLCHAIN:-$EVAL_GO} bash -c "$M_PREP") >"$log" 2>&1
      prep_rc=$?
      if [ "$prep_rc" -eq 0 ]; then prep_status=ok; else prep_status="failed (rc $prep_rc)"; fi
      echo "  prep: $prep_status" >&2
    fi
  fi

  # 4. Type-check status.
  local tc=$PREPARED/$name.typecheck tc_rc nerr nown ndep tc_status
  # shellcheck disable=SC2086
  (cd "$M_MODDIR" && env $M_ENV GOTOOLCHAIN=${GOTOOLCHAIN:-$EVAL_GO} go list -e -export \
     -f '{{.ImportPath}}	{{if .Error}}E{{else}}-{{end}}{{if .DepsErrors}}D{{else}}-{{end}}	{{if .Error}}{{printf "%q" .Error.Err}}{{end}}' \
     ./...) >"$tc" 2>"$tc.err"
  tc_rc=$?
  nerr=$(awk -F'\t' '$2 != "--"' "$tc" | wc -l | tr -d ' ')
  nown=$(awk -F'\t' '$2 ~ /^E/' "$tc" | wc -l | tr -d ' ')
  ndep=$(awk -F'\t' '$2 == "-D"' "$tc" | wc -l | tr -d ' ')
  local npkg
  npkg=$(wc -l <"$tc" | tr -d ' ')
  if [ "$tc_rc" -ne 0 ] || [ "$npkg" -eq 0 ]; then
    tc_status="unknown (go list rc $tc_rc; see $tc.err)"
  elif [ "$nerr" -eq 0 ]; then
    tc_status=complete
  else
    tc_status="degraded: $nerr packages with errors"
  fi
  echo "  type-check: $tc_status ($npkg packages; $nown with errors, $ndep with dependency errors only)" >&2

  # Overall.
  local status=complete reasons=""
  [ -n "$missing" ] && reasons="missing $missing"
  case $prep_status in failed*) reasons="${reasons:+$reasons; }prep $prep_status" ;; esac
  [ "$tc_status" != complete ] && reasons="${reasons:+$reasons; }${tc_status#degraded: }"
  [ -n "$reasons" ] && status="degraded: $reasons"

  # 5. Prepared entry.
  {
    printf 'P_NAME=%q\n' "$name"
    printf 'P_REPO=%q\n' "$M_REPO"
    printf 'P_COMMIT=%q\n' "$M_COMMIT"
    printf 'P_MODDIR=%q\n' "$M_MODDIR"
    printf 'P_ENV=%q\n' "$M_ENV"
    printf 'P_PREP=%q\n' "$prep_status"
    printf 'P_PREP_RC=%q\n' "$prep_rc"
    printf 'P_TC_PKGS=%q\n' "$npkg"
    printf 'P_TC_ERR=%q\n' "$nerr"
    printf 'P_TC_OWN=%q\n' "$nown"
    printf 'P_TC_DEPS=%q\n' "$ndep"
    printf 'P_TYPECHECK=%q\n' "$tc_status"
    printf 'P_STATUS=%q\n' "$status"
    printf 'P_GO=%q\n' "${GOTOOLCHAIN:-$EVAL_GO}"
    printf 'P_DATE=%q\n' "$DATE"
  } >"$PREPARED/$name.env"
  echo "  status: $status" >&2
  return 0
}

if [ "${1:-}" = "--prepare" ]; then
  shift
  mkdir -p "$EVAL_OUT"
  [ $# -gt 0 ] || set -- $(manifest_names)
  BAD=""
  for n in "$@"; do prepare_one "$n" || BAD="$BAD $n"; done
  echo >&2
  printf '%-12s %s\n' app status >&2
  for n in "$@"; do
    if [ -f "$PREPARED/$n.env" ]; then
      ( . "$PREPARED/$n.env"; printf '%-12s %s\n' "$n" "$P_STATUS" ) >&2
    else
      printf '%-12s %s\n' "$n" "NOT PREPARED" >&2
    fi
  done
  [ -z "$BAD" ] || { echo "not prepared:$BAD" >&2; exit 1; }
  exit 0
fi

# ------------------------------------------------------------------------------- run phase
[ $# -gt 0 ] || usage
case $1 in -h|--help) usage ;; esac
if [ "$1" = "--all" ]; then set -- $(manifest_names); fi

# Resolve every argument before running anything.
TARGETS=""
for a in "$@"; do
  if manifest_get "$a"; then
    [ -f "$PREPARED/$a.env" ] || { echo "$a is not prepared: run $0 --prepare $a" >&2; exit 2; }
    TARGETS="$TARGETS name:$a"
  elif [ -d "$a" ]; then
    TARGETS="$TARGETS dir:${a%/}"
  else
    echo "not a manifest app or a directory: $a (see --list)" >&2; exit 2
  fi
done

mkdir -p "$EVAL_OUT"
if [ -z "${CANGO:-}" ]; then
  CANGO=$EVAL_OUT/cango
  echo "== building $CANGO with $EVAL_GO" >&2
  (cd "$REPO_ROOT" && GOTOOLCHAIN=$EVAL_GO go build -o "$CANGO" ./cmd/codeanalyzer) ||
    { echo "analyzer build failed" >&2; exit 2; }
fi
[ -x "$CANGO" ] || { echo "analyzer binary not found: $CANGO" >&2; exit 2; }
RUN_GO=${GOTOOLCHAIN:-$EVAL_GO}

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

# run_one NAME TAG LEVEL JOBS [FLAG...] -> sets RC WALL RSS; output in $EVAL_OUT/NAME-TAG/analysis.json
run_one() {
  local name=$1 tag=$2 level=$3 jobs=$4
  shift 4
  local dir=$EVAL_OUT/$name-$tag
  rm -rf "$dir"; mkdir -p "$dir"
  # shellcheck disable=SC2086
  env $APP_ENV GOTOOLCHAIN="$RUN_GO" /usr/bin/time -l -o "$dir.time" \
    "$CANGO" -i "$APP" --app-name "$name" -a "$level" -j "$jobs" \
    --eager -c "$EVAL_OUT/cache-$name" -o "$dir" "$@" >/dev/null 2>"$dir.err" &
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

BUILD_INFO=$(go version -m "$CANGO" 2>/dev/null)
CANGO_GO=$(printf '%s\n' "$BUILD_INFO" | awk 'NR==1{print $2}')
CANGO_REV=$(printf '%s\n' "$BUILD_INFO" | awk -F'=' '/vcs.revision=/{print substr($2,1,7)}')
CANGO_DIRTY=$(printf '%s\n' "$BUILD_INFO" | awk -F'=' '/vcs.modified=/{print $2}')
[ "$CANGO_DIRTY" = true ] && CANGO_REV="$CANGO_REV+dirty"
{
  echo "# L3 real-app evaluation ($DATE)"
  echo
  echo "Analyzer: $("$CANGO" --version 2>&1 | head -1), built with ${CANGO_GO:-unknown} (runtime.Version), rev ${CANGO_REV:-unknown}; runs use GOTOOLCHAIN=$RUN_GO; host: $(uname -sm); j=$JOBS; all runs --eager."
  echo
  if [ "$RUN_L4" -eq 1 ]; then
    L4H=" wall a4 j1 (s) | RSS a4 j1 (MB) | RSS a4/a3 | SSA a4 | VTA a4 | VTA edges |"; L4S="---|---|---|---|---|---|"
  else
    L4H=""; L4S=""
  fi
  echo "| app | status | type-check | wall a2 j1 (s) | wall a3 j1 (s) | wall a3 jN (s) | RSS a3 j1 (MB) | modules | callables | body nodes | cfg | cdg | ddg | ext symbols | cg in-project | cg external | -j identical | L3 skips | type errors | toolchain warn |$L4H"
  echo "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|$L4S"
} >"$EVAL_NOTE"

FAILS=""
for T in $TARGETS; do
  APP_ENV=""; TYPECHECK="n/a (not prepared)"; label=""
  case $T in
    name:*)
      name=${T#name:}
      # shellcheck disable=SC1090
      . "$PREPARED/$name.env"
      APP=$P_MODDIR; APP_ENV=$P_ENV
      if [ "$P_TYPECHECK" = complete ]; then TYPECHECK=complete
      else TYPECHECK="degraded ${P_TC_ERR}"; fi
      case $P_PREP in skipped*|failed*) TYPECHECK="$TYPECHECK; prep $P_PREP" ;; esac
      label="$name \`${P_COMMIT:0:7}\`${APP_ENV:+ ($APP_ENV)}"
      ;;
    dir:*)
      APP=${T#dir:}; name=$(basename "$APP"); label=$name ;;
  esac
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

  L4COLS=""
  if [ "$RUN_L4" -eq 1 ]; then
    W4=n/a; R4=n/a; RATIO=n/a; RC4=0
    if [ "$RC3" -ne 124 ] && [ "$RC2" -ne 124 ]; then
      run_one "$name" a4j1 4 1 -v; W4=$WALL; R4=$RSS; RC4=$RC
      if [ "$RC4" -ne 0 ]; then W4="$W4 (rc $RC4)"; fi
    fi
    case $R3$R4 in *[!0-9]*|"") ;; *) RATIO=$(awk -v a="$R4" -v b="$R3" 'BEGIN{printf "%.2f", a/b}') ;; esac
    e4=$EVAL_OUT/$name-a4j1.err
    SSA4=$(sed -n 's/.*L4: SSA built .* in \(.*\)$/\1/p' "$e4" 2>/dev/null | head -1)
    VTA4=$(sed -n 's/.*L4: VTA over .* edges in \(.*\)$/\1/p' "$e4" 2>/dev/null | head -1)
    VE4=$(sed -n 's/.*L4: VTA over .*: \([0-9]*\) edges in .*/\1/p' "$e4" 2>/dev/null | head -1)
    L4COLS=" $W4 | $R4 | $RATIO | ${SSA4:-n/a} | ${VTA4:-n/a} | ${VE4:-n/a} |"
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
  twarn=$(grep -c 'require a newer Go version' "$EVAL_OUT/$name-a3j1.err" 2>/dev/null || true)
  counts="n/a	n/a	n/a	n/a	n/a	n/a	n/a	n/a	n/a"
  if [ "$HAVE_JQ" -eq 1 ] && [ "$RC3" -eq 0 ]; then
    counts=$(jq -r "$JQ_COUNTS" "$EVAL_OUT/$name-a3j1/analysis.json" 2>/dev/null || echo "$counts")
  fi
  IFS=$'\t' read -r c1 c2 c3 c4 c5 c6 c7 c8 c9 <<EOC
$counts
EOC
  printf '| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |%s\n' \
    "$label" "$status" "$TYPECHECK" "$W2" "$W3" "$WN" "$R3" "$c1" "$c2" "$c3" "$c4" "$c5" "$c6" "$c7" "$c8" "$c9" \
    "$ident" "$skips" "$terrs" "$twarn" "$L4COLS" >>"$EVAL_NOTE"

  if [ "$EVAL_PRUNE" = 1 ] && [ "$ident" = yes ]; then
    rm -f "$EVAL_OUT/$name-warm/analysis.json" "$EVAL_OUT/$name-a3jN/analysis.json" \
          "$EVAL_OUT/$name-a3jN2/analysis.json" "$EVAL_OUT/$name-a4j1/analysis.json"
  fi
done

echo >>"$EVAL_NOTE"
echo "Raw outputs and logs: $EVAL_OUT/<app>-<run>{/analysis.json,.err,.time}; preparation: $PREPARED/<app>.{env,typecheck}" >>"$EVAL_NOTE"
echo "wrote $EVAL_NOTE" >&2
[ -z "$FAILS" ] || { echo "failures:$FAILS" >&2; exit 1; }
