#!/usr/bin/env bash
# eval_e2e.sh - end-to-end check: Go app -> analysis.json -> Neo4j projection -> loaded graph.
#
# Usage (from the repository root):
#   scripts/eval_e2e.sh [--json-only] APP_DIR [OUT_DIR]
#
#   APP_DIR     a Go module root (must contain go.mod).
#   OUT_DIR     where analysis.json and the run log go (default: APP_DIR).
#   --json-only write and check analysis.json only: no Neo4j push, no graph check,
#               no password or running database needed.
#
# Example:
#   NEO4J_PASSWORD='...' scripts/eval_e2e.sh /path/to/microservices-demo/src/frontend
#   scripts/eval_e2e.sh --json-only /path/to/microservices-demo/src/frontend
#
# Environment:
#   LEVEL           analysis level for analysis.json (default 2; 1..4)
#   APP_NAME        --app-name anchor (default: APP_DIR base name, as cango does)
#   CANGO           analyzer binary. When unset, it is built from this checkout into
#                   OUT_DIR/cango with `go build ./cmd/codeanalyzer`.
#   NEO4J_URI       Bolt URI (default bolt://localhost:7687)
#   NEO4J_USER      username (default neo4j)
#   NEO4J_PASSWORD  password (default neo4j). Passed to cango through its env var; never echoed.
#   EAGER           1 = reload: the push runs with --eager, purging this application's
#                   subgraph (its can:// prefix only) first. Default 0 (additive upsert).
#   CYPHER_SHELL    cypher-shell binary for verification (auto-detected when unset). When it
#                   cannot be found or connected, the count check is reported as skipped.
#
# Steps:
#   1. Validate APP_DIR and the requested level.
#   2. Build the analyzer (unless CANGO is set).
#   3. cango -a LEVEL -i APP_DIR -o OUT_DIR           -> OUT_DIR/analysis.json
#   4. cango --emit neo4j --neo4j-uri URI ...         -> live push (MERGE, idempotent)
#   5. Verify analysis.json (schema, level, app id) and that the database holds the
#      pushed node count under this application's can:// prefix.
#
# Exit status: 0 when every check passes (or is skipped with a reason), non-zero otherwise.
set -euo pipefail

JSON_ONLY=0
if [ "${1:-}" = "--json-only" ]; then JSON_ONLY=1; shift; fi

if [ $# -lt 1 ]; then
  sed -n '2,/^set -euo/p' "$0" | sed '$d' >&2
  exit 2
fi

REPO=$(cd "$(dirname "$0")/.." && pwd)
APP=$(cd "$1" && pwd)
OUT=${2:-$APP}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)

LEVEL=${LEVEL:-2}
URI=${NEO4J_URI:-bolt://localhost:7687}
USER=${NEO4J_USER:-neo4j}
PASS=${NEO4J_PASSWORD:-neo4j}
APP_NAME=${APP_NAME:-$(basename "$APP")}
# The run log lives outside OUT: a log inside the project would be captured as an artifact.
LOG=${E2E_LOG:-${TMPDIR:-/tmp}/cldk-e2e-${APP_NAME}.log}

fail=0
step() { echo; echo "== $1"; }
pass() { echo "   PASS: $1"; }
bad() { echo "   FAIL: $1" >&2; fail=1; }
skip() { echo "   SKIP: $1"; }

# Strings that become id segments or Cypher literals must be plain names.
case "$APP_NAME" in
  *[!A-Za-z0-9._-]*|"") echo "APP_NAME '$APP_NAME' must contain only letters, digits, '.', '_' or '-'" >&2; exit 2 ;;
esac
case "$LEVEL" in 1|2|3|4) ;; *) echo "LEVEL must be 1..4 (got '$LEVEL')" >&2; exit 2 ;; esac
[ -f "$APP/go.mod" ] || { echo "APP_DIR '$APP' has no go.mod; not a Go module root" >&2; exit 2; }

: >"$LOG"

step "1/5 analyzer binary"
if [ -n "${CANGO:-}" ]; then
  [ -x "$CANGO" ] || { echo "CANGO '$CANGO' is not executable" >&2; exit 2; }
  echo "   using $CANGO"
else
  # Build outside the project: a binary inside it would be captured as an artifact.
  CANGO="${E2E_BUILD_DIR:-${TMPDIR:-/tmp}}/cldk-e2e-cango-${APP_NAME}"
  (cd "$REPO" && go build -o "$CANGO" ./cmd/codeanalyzer) >>"$LOG" 2>&1
  echo "   built $CANGO from $REPO"
fi

step "2/5 analysis.json (-a $LEVEL)"
"$CANGO" -a "$LEVEL" --app-name "$APP_NAME" -i "$APP" -o "$OUT" >>"$LOG" 2>&1
[ -s "$OUT/analysis.json" ] && pass "wrote $OUT/analysis.json ($(wc -c <"$OUT/analysis.json" | tr -d ' ') bytes)" \
  || { bad "analysis.json missing or empty (see $LOG)"; exit 1; }

if [ "$JSON_ONLY" = "1" ]; then
  step "3/5 Neo4j projection"; skip "--json-only: no push"
else
step "3/5 Neo4j projection (live push to $URI as $USER)"
# EAGER=1 reloads: --eager purges this application's subgraph (its can://<ns>/<app>/
# prefix only) before the push, so nodes from an earlier id format do not linger.
# A plain string, not an array: an empty array trips `set -u` on bash 3.2 (macOS).
EAGER_FLAG=""
[ "${EAGER:-0}" = "1" ] && EAGER_FLAG="--eager"
push_out=$(NEO4J_PASSWORD="$PASS" "$CANGO" --emit neo4j --neo4j-uri "$URI" --neo4j-user "$USER" \
  --app-name "$APP_NAME" $EAGER_FLAG -i "$APP" -o "$OUT" 2>&1) && push_rc=0 || push_rc=$?
printf '%s\n' "$push_out" >>"$LOG"
if [ "$push_rc" -ne 0 ]; then
  bad "projection push failed (exit $push_rc): $(printf '%s' "$push_out" | tail -1)"
  exit 1
fi
pushed=$(printf '%s\n' "$push_out" | sed -n 's/^pushed \([0-9]*\) nodes, \([0-9]*\) relationships.*/\1 \2/p' | tail -1)
[ -n "$pushed" ] && pass "pushed $(printf '%s' "$pushed" | cut -d' ' -f1) nodes, $(printf '%s' "$pushed" | cut -d' ' -f2) relationships" \
  || bad "push output had no 'pushed N nodes' line"
fi

step "4/5 verify analysis.json"
if command -v python3 >/dev/null 2>&1; then
  if python3 -I - "$OUT/analysis.json" "$LEVEL" "$APP_NAME" <<'PY'
import json, sys
path, level, app = sys.argv[1], int(sys.argv[2]), sys.argv[3]
d = json.load(open(path, encoding="utf-8"))
errs = []
if d.get("language") != "go": errs.append(f"language is {d.get('language')!r}")
if d.get("max_level") != level: errs.append(f"max_level is {d.get('max_level')!r}, expected {level}")
app_id = d.get("application", {}).get("id", "")
if not app_id.startswith(f"can://"): errs.append(f"application.id {app_id!r} is not a can:// id")
if app not in app_id: errs.append(f"application.id {app_id!r} lacks the app name {app!r}")
st = d.get("application", {}).get("symbol_table", {})
if not st: errs.append("symbol_table is empty")
if errs:
    print("\n".join("   FAIL: " + e for e in errs)); sys.exit(1)
print(f"   PASS: schema {d.get('schema_version')}, level {level}, {len(st)} files, "
      f"{len(d['application'].get('call_graph', []))} call edges, id {app_id}")
PY
  then :; else fail=1; fi
else
  skip "python3 not found; analysis.json not inspected"
fi

if [ "$JSON_ONLY" = "1" ]; then
  step "5/5 verify loaded graph"; skip "--json-only: graph not checked"
else
step "5/5 verify loaded graph"
CS=${CYPHER_SHELL:-}
if [ -z "$CS" ]; then
  CS=$(command -v cypher-shell || true)
  [ -n "$CS" ] || for c in /opt/homebrew/bin/cypher-shell /usr/local/bin/cypher-shell; do
    [ -x "$c" ] && { CS=$c; break; }
  done
fi
if [ -z "$CS" ]; then
  skip "cypher-shell not found; set CYPHER_SHELL to enable the count check"
else
  shell() { "$CS" -a "$URI" -u "$USER" -p "$PASS" --format plain "$@" 2>/dev/null; }
  if ! shell "RETURN 1" >/dev/null; then
    skip "cannot authenticate to $URI with cypher-shell; the count check was not run"
  else
    # Nodes keyed by this application's id root, the same prefix the projector scopes on.
    prefix="can://${APP_NAME}/"
    db_nodes=$(shell "MATCH (n:GoCanNode) WHERE n.id STARTS WITH '$prefix' RETURN count(n) AS c" | tail -1 | tr -d '"[:space:]')
    want_nodes=$(printf '%s' "$pushed" | cut -d' ' -f1)
    # Dependency :Package nodes are neutral (pkg: purls, shared across apps), so they are
    # not under the prefix. They are part of the push count and checked separately.
    db_pkgs=$(shell "MATCH (n:Package) WHERE n.id STARTS WITH 'pkg:' RETURN count(n) AS c" | tail -1 | tr -d '"[:space:]')
    echo "   info: $db_pkgs :Package nodes in the database"
    # Pushed nodes are app-scoped (GoCanNode under the prefix) plus neutral :Package nodes.
    if [ -n "$db_nodes" ] && [ "$db_nodes" -gt 0 ] && [ "$((db_nodes + ${db_pkgs:-0}))" -ge "$want_nodes" ]; then
      pass "database holds $db_nodes GoCanNode rows under $prefix and ${db_pkgs:-0} :Package nodes (pushed $want_nodes nodes)"
    else
      bad "database has ${db_nodes:-no} GoCanNode rows under $prefix plus ${db_pkgs:-0} :Package; expected at least $want_nodes"
    fi
    labels=$(shell "MATCH (n:GoCallable) WHERE n.id STARTS WITH '$prefix' RETURN count(n) AS c" | tail -1 | tr -d '"[:space:]')
    echo "   info: $labels GoCallable nodes under $prefix"
  fi
fi

fi

echo
if [ "$fail" -eq 0 ]; then
  if [ "$JSON_ONLY" = "1" ]; then
    echo "E2E OK (json-only): $APP_NAME -> $OUT/analysis.json"
  else
    echo "E2E OK: $APP_NAME -> $OUT/analysis.json -> Neo4j at $URI"
  fi
else
  echo "E2E FAILED for $APP_NAME (log: $LOG)" >&2
fi
exit "$fail"
