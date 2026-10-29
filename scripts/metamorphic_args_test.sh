#!/usr/bin/env bash
# metamorphic_args_test.sh - check scripts/metamorphic.sh's argument handling (no analysis runs).
# Usage: scripts/metamorphic_args_test.sh   (exits non-zero on a failed check)
set -u
S=$(cd "$(dirname "$0")" && pwd)/metamorphic.sh
fail=0
check() { # want-rc want-stderr-substring args...
  local want=$1 msg=$2; shift 2
  local err rc
  err=$("$S" "$@" 2>&1 >/dev/null); rc=$?
  if [ "$rc" != "$want" ] || [[ "$err" != *"$msg"* ]]; then
    echo "FAIL: metamorphic.sh $* -> rc $rc, stderr: $err (want rc $want, '$msg')"; fail=1
  else
    echo "ok:   metamorphic.sh $* -> rc $rc"
  fi
}
check 2 "unknown flag: --al" --al
check 2 "unknown flag: -x" -x memos
check 2 "unknown flag: --memos" memos --memos
check 2 "--all takes no other arguments" --all memos
check 2 "--all takes no other arguments" memos --all
check 0 "Usage:" --help
exit $fail
