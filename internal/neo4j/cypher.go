package neo4j

import (
	"fmt"
	"sort"
	"strings"
)

// Milestone M4: the snapshot writer. RenderCypher turns a GraphRows bag into a
// self-contained, idempotent graph.cypher script. Running it (e.g.
// `cypher-shell < graph.cypher`) rebuilds this application's subgraph from
// scratch. Block order:
//
//  1. constraints + indexes (IF NOT EXISTS) — first, so MERGE seeks an index;
//  2. scoped wipe of this app's prior subgraph — anchored on :GoCanNode and the
//     can://<app>/ id prefix, with the root id matched separately (the root is
//     outside its own descendant prefix). Rows with no proper application root
//     emit NO wipe (refused visibly), so a hand-built bag can't delete anything;
//  3. batched UNWIND … MERGE for nodes, grouped by full label set + key prop;
//  4. batched UNWIND … MERGE for edges, grouped by (type, endpoint labels/keys,
//     keyed?).
//
// A static script has no view of the live DB, so it expresses the FULL truth;
// incremental updates are the Bolt writer's job (M5). Determinism: identical
// GraphRows → byte-identical script (RowBuilder.Finish already sorted the bag).

// batchSize is the UNWIND batch size (parity with the siblings' 500).
const batchSize = 500

// RenderCypher renders the full snapshot script. appID is the application root
// id used to scope the wipe; it is normally rows' application node value.
func RenderCypher(rows GraphRows, appID string) string {
	var b strings.Builder

	writeDDL(&b)
	writeScopedWipe(&b, appID)
	writeNodeBatches(&b, rows.Nodes)
	writeEdgeBatches(&b, rows.Edges)

	return b.String()
}

func writeDDL(b *strings.Builder) {
	b.WriteString("// ─── constraints & indexes ───\n")
	for _, c := range UniquenessConstraints() {
		fmt.Fprintf(b, "CREATE CONSTRAINT %s IF NOT EXISTS FOR (n:%s) REQUIRE n.%s IS UNIQUE;\n",
			c.Name, c.Label, c.Property)
	}
	for _, idx := range Indexes() {
		fmt.Fprintf(b, "CREATE INDEX %s IF NOT EXISTS FOR (n:%s) ON (n.%s);\n",
			idx.Name, idx.Label, idx.Property)
	}
	b.WriteByte('\n')
}

// writeScopedWipe emits the DETACH DELETE scoped to this app. It refuses to emit
// anything unless appID is a bare application id (applicationPrefix), so the
// blast radius is exactly one application — never the whole store, never a
// sibling analyzer's :Go* graph (they lack :GoCanNode), never a second Go app
// sharing a module path.
func writeScopedWipe(b *strings.Builder, appID string) {
	root, ok := applicationPrefix(appID)
	if !ok {
		b.WriteString("// ─── scoped wipe SKIPPED: no bare application root id ───\n\n")
		return
	}
	desc := descendantPrefix(root)
	b.WriteString("// ─── scoped wipe (this application only) ───\n")
	// Descendants: everything under can://<app>/.
	fmt.Fprintf(b, "MATCH (n:%s) WHERE n.id STARTS WITH %s DETACH DELETE n;\n",
		MarkerLabel, cypherString(desc))
	// The root itself is outside its own descendant prefix.
	fmt.Fprintf(b, "MATCH (n:%s) WHERE n.id = %s DETACH DELETE n;\n",
		MarkerLabel, cypherString(root))
	b.WriteByte('\n')
}

// ─── node batches ─────────────────────────────────────────────────────────────

func writeNodeBatches(b *strings.Builder, nodes []NodeRow) {
	if len(nodes) == 0 {
		return
	}
	b.WriteString("// ─── nodes ───\n")
	// Group by (full label set + key prop) so one UNWIND handles a homogeneous
	// batch. Keys sorted for deterministic output.
	groups := map[string][]NodeRow{}
	for _, n := range nodes {
		groups[nodeGroupKey(n)] = append(groups[nodeGroupKey(n)], n)
	}
	for _, gk := range sortedKeys(groups) {
		g := groups[gk]
		mergeLabel := g[0].Labels[0]
		keyProp := g[0].KeyProp
		extraLabels := g[0].Labels[1:]

		for _, chunk := range chunkNodes(g, batchSize) {
			b.WriteString("UNWIND [\n")
			for i, n := range chunk {
				sep := ","
				if i == len(chunk)-1 {
					sep = ""
				}
				fmt.Fprintf(b, "  {k: %s, p: %s}%s\n", cypherString(n.Value), cypherMap(n.Props), sep)
			}
			b.WriteString("] AS row\n")
			fmt.Fprintf(b, "MERGE (n:%s {%s: row.k})\n", mergeLabel, keyProp)
			b.WriteString("SET n += row.p")
			for _, l := range extraLabels {
				fmt.Fprintf(b, "\nSET n:%s", l)
			}
			b.WriteString(";\n")
		}
	}
	b.WriteByte('\n')
}

// ─── edge batches ─────────────────────────────────────────────────────────────

func writeEdgeBatches(b *strings.Builder, edges []EdgeRow) {
	if len(edges) == 0 {
		return
	}
	b.WriteString("// ─── relationships ───\n")
	groups := map[string][]EdgeRow{}
	for _, e := range edges {
		groups[edgeGroupKey(e)] = append(groups[edgeGroupKey(e)], e)
	}
	for _, gk := range sortedEdgeKeys(groups) {
		g := groups[gk]
		e0 := g[0]
		keyed := e0.Key != ""

		for _, chunk := range chunkEdges(g, batchSize) {
			b.WriteString("UNWIND [\n")
			for i, e := range chunk {
				sep := ","
				if i == len(chunk)-1 {
					sep = ""
				}
				if keyed {
					fmt.Fprintf(b, "  {a: %s, b: %s, k: %s, p: %s}%s\n",
						cypherString(e.From.Value), cypherString(e.To.Value), cypherString(e.Key), cypherMap(e.Props), sep)
				} else {
					fmt.Fprintf(b, "  {a: %s, b: %s, p: %s}%s\n",
						cypherString(e.From.Value), cypherString(e.To.Value), cypherMap(e.Props), sep)
				}
			}
			b.WriteString("] AS row\n")
			fmt.Fprintf(b, "MATCH (a:%s {%s: row.a})\n", e0.From.Label, e0.From.KeyProp)
			fmt.Fprintf(b, "MATCH (b:%s {%s: row.b})\n", e0.To.Label, e0.To.KeyProp)
			if keyed {
				fmt.Fprintf(b, "MERGE (a)-[r:%s {_k: row.k}]->(b)\n", e0.Type)
			} else {
				fmt.Fprintf(b, "MERGE (a)-[r:%s]->(b)\n", e0.Type)
			}
			b.WriteString("SET r += row.p;\n")
		}
	}
	b.WriteByte('\n')
}

// ─── grouping keys & chunking ─────────────────────────────────────────────────

func nodeGroupKey(n NodeRow) string {
	return strings.Join(n.Labels, ":") + "\x00" + n.KeyProp
}

func edgeGroupKey(e EdgeRow) string {
	keyed := "0"
	if e.Key != "" {
		keyed = "1"
	}
	return strings.Join([]string{e.Type, e.From.Label, e.From.KeyProp, e.To.Label, e.To.KeyProp, keyed}, "\x00")
}

func chunkNodes(s []NodeRow, n int) [][]NodeRow {
	var out [][]NodeRow
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

func chunkEdges(s []EdgeRow, n int) [][]EdgeRow {
	var out [][]EdgeRow
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

func sortedKeys(m map[string][]NodeRow) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedEdgeKeys(m map[string][]EdgeRow) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
