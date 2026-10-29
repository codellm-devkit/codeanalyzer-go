#!/usr/bin/env bash
# eval_l3_test.sh - check scripts/eval_l3.sh's exit status against a stub analyzer (no analysis runs).
# Usage: scripts/eval_l3_test.sh   (exits non-zero on a failed check; needs /usr/bin/time -l, i.e. macOS)
set -u
S=$(cd "$(dirname "$0")" && pwd)/eval_l3.sh
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/app"
# The stub writes a fixed analysis.json to -o and exits $FAIL_LEVEL_RC when -a is $FAIL_LEVEL.
cat >"$T/cango" <<'STUB'
#!/usr/bin/env bash
[ "${1:-}" = "--version" ] && { echo "stub"; exit 0; }
out="" level=""
while [ $# -gt 0 ]; do
  case $1 in -o) out=$2; shift ;; -a) level=$2; shift ;; esac
  shift
done
[ -n "$out" ] && echo '{"application":{"symbol_table":{},"call_graph":[]}}' >"$out/analysis.json"
# Record the environment's USER each run sees (eval_l3.sh must not clobber it).
[ -n "$out" ] && printf '%s\n' "${USER-<unset>}" >>"${USER_LOG:-/dev/null}"
# NONDET_LEVEL: a run at that level appends a per-run nonce (a nondeterministic analyzer).
[ "$level" = "${NONDET_LEVEL:-}" ] && echo "$$-$RANDOM" >>"$out/analysis.json"
# At -a 4 with -v, print the L4 log lines eval_l3.sh parses.
if [ "$level" = 4 ]; then
  echo "[codeanalyzer-go] L4: SSA built 2 project packages in 5ms" >&2
  echo "[codeanalyzer-go] L4: SSA join: 9 functions (1 by wrapper, 0 without a unit), 20 calls; 1 functions, 3 calls unmapped in 1ms" >&2
  echo "[codeanalyzer-go] L4: VTA over 12 functions (3 library targets): 30 edges in 2ms" >&2
  echo "[codeanalyzer-go] L4: dispatch: 7 call sites with targets; 4 call_graph edges and 1 externals added by VTA" >&2
  echo "[codeanalyzer-go] L4: 40 vertices, 50 port edges (6 points-to), 8 summary edges, 11 param_in, 10 param_out, 5 stand-ins in 4ms" >&2
  echo "[codeanalyzer-go WARN] L4 degraded: unmapped_ssa (1 SSA function(s) and 3 call instruction(s) map to no callable or call node): example.com/a" >&2
  echo "[codeanalyzer-go WARN] L4 degraded: range_func (range-over-func is not modelled at L4 (D8)): example.com/b" >&2
fi
[ "$level" = "${FAIL_LEVEL:-}" ] && exit 3
exit 0
STUB
chmod +x "$T/cango"
fail=0
check() { # want-rc want-note-substring env...
  local want=$1 msg=$2; shift 2
  local rc
  env CANGO="$T/cango" EVAL_OUT="$T/out" EVAL_NOTE="$T/note.md" EVAL_JOBS=2 "$@" "$S" "$T/app" >/dev/null 2>"$T/err"; rc=$?
  if [ "$rc" != "$want" ] || ! grep -q -- "$msg" "$T/note.md"; then
    echo "FAIL: $* eval_l3.sh -> rc $rc (want $want, note containing '$msg')"; sed 's/^/  /' "$T/err" "$T/note.md"; fail=1
  else
    echo "ok:   $* eval_l3.sh -> rc $rc"
  fi
}
check 0 "| app | ok |" CANGO_LEVELS="2 3 4"
# The -a 4 triple: -j 1, -j N and a rerun, compared byte for byte; the L4 log counts and codes.
check 0 "| 40 | 11 | 10 | 8 | 50 | 6 | 5 | 7 | 1/3 | range_func,unmapped_ssa | yes |" CANGO_LEVELS="2 3 4"
check 1 "FAIL(a4 -j differs)" CANGO_LEVELS="2 3 4" NONDET_LEVEL=4
check 1 "FAIL(-j differs)" CANGO_LEVELS="2 3" NONDET_LEVEL=3
check 1 "FAIL(rc a4=3)" CANGO_LEVELS="2 3 4" FAIL_LEVEL=4
check 1 "FAIL(rc a2=0 j1=3" CANGO_LEVELS="2 3" FAIL_LEVEL=3
# $USER reaches every analyzer run unchanged (a script variable named USER would shadow it).
: >"$T/users"
env CANGO="$T/cango" EVAL_OUT="$T/out" EVAL_NOTE="$T/note.md" EVAL_JOBS=2 CANGO_LEVELS="2 3 4" \
  USER=evaluser USER_LOG="$T/users" "$S" "$T/app" >/dev/null 2>&1
if [ "$(sort -u "$T/users")" = evaluser ] && [ "$(wc -l <"$T/users" | tr -d ' ')" -ge 8 ]; then
  echo "ok:   USER unchanged across all runs"
else
  echo "FAIL: USER seen by the runs: $(sort -u "$T/users" | paste -sd, -)"; fail=1
fi
exit $fail
