#!/usr/bin/env bash
#
# Load graph.cypher into a NATIVE Neo4j (Neo4j Desktop or a
# Homebrew/tarball install — no Docker) and run sample queries against it.
#
#   ./load_and_query.sh            # load graph.cypher, then run the sample queries
#   ./load_and_query.sh --query    # skip the load, just re-run the queries
#   ./load_and_query.sh --check    # only verify the connection + find cypher-shell
#
# Connection (override via env):
#   NEO4J_URI       default bolt://localhost:7687
#   NEO4J_USER      default neo4j
#   NEO4J_PASSWORD  default neo4j      (set to your DB's password)
#   CYPHER_SHELL    explicit path to the cypher-shell binary (else auto-detected)
#
# Example:
#   NEO4J_PASSWORD=mypass ./load_and_query.sh
#
# Prerequisite: a running Neo4j and its `cypher-shell`. Get them via EITHER:
#   • Neo4j Desktop (neo4j.com/download): create + start a local DBMS; this
#     script auto-finds the cypher-shell it bundles.
#   • Homebrew: `brew install neo4j && neo4j start` (cypher-shell lands on PATH).
set -euo pipefail

URI="${NEO4J_URI:-bolt://localhost:7687}"
USER="${NEO4J_USER:-neo4j}"
PASS="${NEO4J_PASSWORD:-neo4j}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CYPHER_FILE="$SCRIPT_DIR/graph.cypher"

# find_cypher_shell locates the binary: an explicit $CYPHER_SHELL, then PATH,
# then common Neo4j Desktop / Homebrew locations.
find_cypher_shell() {
  if [ -n "${CYPHER_SHELL:-}" ] && [ -x "$CYPHER_SHELL" ]; then echo "$CYPHER_SHELL"; return 0; fi
  if command -v cypher-shell >/dev/null 2>&1; then command -v cypher-shell; return 0; fi
  # Neo4j Desktop bundles it under its application support tree; Homebrew under its cellar.
  local c
  for c in \
    "$HOME/Library/Application Support/Neo4j Desktop/Application/"*/cypher-shell/bin/cypher-shell \
    "$HOME/Library/Application Support/com.Neo4j.Relate/Data/dbmss/"*/bin/cypher-shell \
    /opt/homebrew/bin/cypher-shell \
    /usr/local/bin/cypher-shell ; do
    if [ -x "$c" ]; then echo "$c"; return 0; fi
  done
  return 1
}

CS="$(find_cypher_shell || true)"
if [ -z "$CS" ]; then
  cat >&2 <<EOF
cypher-shell not found. Install a native Neo4j first:
  • Neo4j Desktop:  https://neo4j.com/download  (create + start a local DBMS)
  • Homebrew:       brew install neo4j && neo4j start
Then re-run, or point CYPHER_SHELL=/path/to/cypher-shell at it.
EOF
  exit 1
fi

shell() { "$CS" -a "$URI" -u "$USER" -p "$PASS" --format plain "$@"; }

check() {
  echo "cypher-shell: $CS"
  echo "connecting to $URI as $USER ..."
  if shell "RETURN 1 AS ok" >/dev/null 2>&1; then
    echo "connection OK."
  else
    cat >&2 <<EOF
could not connect / authenticate to $URI.
  - is the database started?
  - is NEO4J_PASSWORD correct? (first-run Neo4j forces a password change)
  - is the Bolt port right? (port 7687 is currently held by another process on
    this machine — set NEO4J_URI=bolt://localhost:<port> if your DB uses another)
EOF
    exit 1
  fi
}

load() {
  [ -f "$CYPHER_FILE" ] || { echo "missing $CYPHER_FILE — run 'cango -i <app> --emit neo4j -o Eval/Neo4j_eval' first" >&2; exit 1; }
  echo "loading $(basename "$CYPHER_FILE") ($(wc -c < "$CYPHER_FILE" | tr -d ' ') bytes) ..."
  shell < "$CYPHER_FILE" >/dev/null
  echo "loaded."
}

q() {
  echo
  echo "── $1"
  shell "$2" || true
}

queries() {
  q "node counts by label" \
    "MATCH (n:GoCanNode) RETURN labels(n)[0] AS label, count(*) AS n ORDER BY n DESC"
  q "relationship counts by type" \
    "MATCH ()-[r]->() RETURN type(r) AS rel, count(*) AS n ORDER BY n DESC"
  q "everything scoped to the cobra app (prefix seek)" \
    "MATCH (n:GoCanNode) WHERE n.id STARTS WITH 'can://go/cobra' RETURN count(n) AS cobra_nodes"
  q "sample call graph edges" \
    "MATCH (a:GoCallable)-[:GO_CALLS]->(b) RETURN a.signature AS caller, b.signature AS callee LIMIT 15"
  q "types and how many methods each has" \
    "MATCH (t:GoType) OPTIONAL MATCH (t)-[:GO_HAS_METHOD]->(m) RETURN t.kind AS kind, t.signature AS type, count(m) AS methods ORDER BY methods DESC LIMIT 15"
  q "interfaces structurally satisfied (GO_SATISFIES)" \
    "MATCH (t:GoType)-[:GO_SATISFIES]->(i:GoType) RETURN t.signature AS type, i.signature AS satisfies LIMIT 15"
  q "external / stdlib packages reached via imports" \
    "MATCH (:GoCanNode)-[:GO_IMPORTS]->(x:GoExternal) RETURN DISTINCT x.path AS import ORDER BY import"
  q "most-called callables (in-degree)" \
    "MATCH (:GoCallable)-[:GO_CALLS]->(b:GoCallable) RETURN b.signature AS callee, count(*) AS called_by ORDER BY called_by DESC LIMIT 10"
}

case "${1:-}" in
  --check) check ;;
  --query) check; queries ;;
  *) check; load; queries
     echo; echo "Re-run queries only:  $0 --query" ;;
esac
