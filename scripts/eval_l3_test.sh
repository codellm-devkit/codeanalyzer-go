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
check 1 "FAIL(rc a4=3)" CANGO_LEVELS="2 3 4" FAIL_LEVEL=4
check 1 "FAIL(rc a2=0 j1=3" CANGO_LEVELS="2 3" FAIL_LEVEL=3
exit $fail
