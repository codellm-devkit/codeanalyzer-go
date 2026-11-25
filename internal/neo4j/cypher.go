package neo4j

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// Milestone M4: the snapshot writer. RenderCypher turns a GraphRows bag into a
// self-contained, idempotent graph.cypher script. Running it (e.g.
// `cypher-shell < graph.cypher`) rebuilds this application's subgraph from
// scratch. Block order:
//
//  1. constraints + indexes (IF NOT EXISTS) — first, so MERGE seeks an index;
//  2. scoped wipe of this app's prior subgraph — anchored on :GoCanNode and the
//     can://<namespace>/<app>/ id prefix, with the root id matched separately (the root is
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

// scriptWriter is the part of strings.Builder and bufio.Writer that the renderer uses.
type scriptWriter interface {
	io.Writer
	io.StringWriter
	io.ByteWriter
}

// RenderCypher renders the full snapshot script. appID is the application root
// id used to scope the wipe; it is normally rows' application node value.
func validateIdentity(rows GraphRows, appID string) error {
	for _, n := range rows.Nodes {
		if n.Value != appID {
			continue
		}
		for _, label := range n.Labels {
			if label != LabelApplication {
				continue
			}
			version, ok := n.Props["identity_version"].(int)
			if !ok || version != 2 {
				return fmt.Errorf("incompatible identity_version")
			}
			name, ok := n.Props["application_name"].(string)
			if !ok {
				return fmt.Errorf("missing application_name")
			}
			ns, ok := n.Props["identity_namespace"].(string)
			if !ok {
				return fmt.Errorf("missing identity_namespace")
			}
			identity, err := v2.ResolveIdentity(name, &ns)
			if err != nil {
				return err
			}
			if identity.ApplicationID != appID {
				return fmt.Errorf("application root does not match metadata")
			}
			return nil
		}
	}
	return fmt.Errorf("missing application anchor")
}
func RenderCypher(rows GraphRows, appID string) (string, error) {
	if err := validateIdentity(rows, appID); err != nil {
		return "", err
	}
	return renderCypher(rows, appID), nil
}
func renderCypher(rows GraphRows, appID string) string {
	var b strings.Builder

	writeDDL(&b)
	writeScopedWipe(&b, appID)
	writeNodeBatches(&b, rows.Nodes)
	writeEdgeBatches(&b, rows.Edges)

	return b.String()
}

func writeDDL(b scriptWriter) {
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
func writeScopedWipe(b scriptWriter, appID string) {
	root, ok := applicationPrefix(appID)
	if !ok {
		b.WriteString("// ─── scoped wipe SKIPPED: no bare application root id ───\n\n")
		return
	}
	desc := descendantPrefix(root)
	b.WriteString("// ─── scoped wipe (this application only) ───\n")
	// Descendants: everything under can://<namespace>/<app>/.
	fmt.Fprintf(b, "MATCH (n:%s) WHERE n.id STARTS WITH %s DETACH DELETE n;\n",
		MarkerLabel, cypherString(desc))
	// The root itself is outside its own descendant prefix.
	fmt.Fprintf(b, "MATCH (n:%s) WHERE n.id = %s DETACH DELETE n;\n",
		MarkerLabel, cypherString(root))
	b.WriteByte('\n')
}

// ─── node batches ─────────────────────────────────────────────────────────────

func writeNodeBatches(b scriptWriter, nodes []NodeRow) {
	if len(nodes) == 0 {
		return
	}
	b.WriteString(nodesHeader)
	// Group by (full label set + key prop) so one UNWIND handles a homogeneous
	// batch. Keys sorted for deterministic output.
	groups := map[string][]NodeRow{}
	for _, n := range nodes {
		groups[nodeGroupKey(n)] = append(groups[nodeGroupKey(n)], n)
	}
	for _, gk := range sortedKeys(groups) {
		for _, chunk := range chunkNodes(groups[gk], batchSize) {
			writeNodeChunk(b, chunk)
		}
	}
	b.WriteByte('\n')
}

const (
	nodesHeader = "// ─── nodes ───\n"
	edgesHeader = "// ─── relationships ───\n"
)

// writeNodeChunk writes one UNWIND … MERGE statement for a non-empty chunk of
// one node group (same labels and key property).
func writeNodeChunk(b scriptWriter, chunk []NodeRow) {
	mergeLabel := chunk[0].Labels[0]
	keyProp := chunk[0].KeyProp
	extraLabels := chunk[0].Labels[1:]
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

// ─── edge batches ─────────────────────────────────────────────────────────────

func writeEdgeBatches(b scriptWriter, edges []EdgeRow) {
	if len(edges) == 0 {
		return
	}
	b.WriteString(edgesHeader)
	groups := map[string][]EdgeRow{}
	for _, e := range edges {
		groups[edgeGroupKey(e)] = append(groups[edgeGroupKey(e)], e)
	}
	for _, gk := range sortedEdgeKeys(groups) {
		for _, chunk := range chunkEdges(groups[gk], batchSize) {
			writeEdgeChunk(b, chunk)
		}
	}
	b.WriteByte('\n')
}

// writeEdgeChunk writes one UNWIND … MERGE statement for a non-empty chunk of
// one edge group (same type, endpoint labels and keys, keyed or not).
func writeEdgeChunk(b scriptWriter, chunk []EdgeRow) {
	e0 := chunk[0]
	keyed := e0.Key != ""
	b.WriteString("UNWIND [\n")
	for i, e := range chunk {
		sep := ","
		if i == len(chunk)-1 {
			sep = ""
		}
		if keyed {
			fmt.Fprintf(b, "  {a: %s, b: %s, k: %s, f: %s, p: %s}%s\n",
				cypherString(e.From.Value), cypherString(e.To.Value), cypherString(e.Key), cypherString(edgeFactID(e)), cypherMap(e.Props), sep)
		} else {
			fmt.Fprintf(b, "  {a: %s, b: %s, f: %s, p: %s}%s\n",
				cypherString(e.From.Value), cypherString(e.To.Value), cypherString(edgeFactID(e)), cypherMap(e.Props), sep)
		}
	}
	b.WriteString("] AS row\n")
	fmt.Fprintf(b, "MATCH (a:%s {%s: row.a})\n", e0.From.Label, e0.From.KeyProp)
	fmt.Fprintf(b, "MATCH (b:%s {%s: row.b})\n", e0.To.Label, e0.To.KeyProp)
	if keyed {
		fmt.Fprintf(b, "MERGE (a)-[r:%s {_k: row.k, %s: row.f}]->(b)\n", e0.Type, RelationshipFactProperty)
	} else {
		fmt.Fprintf(b, "MERGE (a)-[r:%s {%s: row.f}]->(b)\n", e0.Type, RelationshipFactProperty)
	}
	b.WriteString("SET r += row.p;\n")
}

// ─── streamed snapshot (#59, spec §5.4) ───────────────────────────────────────

// cypherWriter is the snapshot stager. It writes the same statements as
// renderCypher, with the node and edge groups flushed per batchSize chunk as
// the stream goes, so it never holds more than one chunk per group. Every node
// statement precedes every edge statement: late flushes all node groups.
type cypherWriter struct {
	w       *bufio.Writer
	appID   string
	nodeBuf map[string][]NodeRow
	edgeBuf map[string][]EdgeRow
	inEdges bool // the relationships header is written
}

func newCypherWriter(w *bufio.Writer, appID string) *cypherWriter {
	return &cypherWriter{w: w, appID: appID, nodeBuf: map[string][]NodeRow{}, edgeBuf: map[string][]EdgeRow{}}
}

// begin writes the DDL, the scoped wipe and the nodes header. The node
// section is never empty (late always holds the application row), so the
// header is unconditional; the row begin gets is written by late.
func (c *cypherWriter) begin(NodeRow) error {
	writeDDL(c.w)
	writeScopedWipe(c.w, c.appID)
	c.w.WriteString(nodesHeader)
	return nil
}

func (c *cypherWriter) shared(nodes []NodeRow) error           { c.bufferNodes(nodes); return nil }
func (c *cypherWriter) module(_ string, nodes []NodeRow) error { c.bufferNodes(nodes); return nil }

// late buffers the last node rows and flushes every node group in key order.
func (c *cypherWriter) late(nodes []NodeRow) error {
	c.bufferNodes(nodes)
	for _, gk := range sortedKeys(c.nodeBuf) {
		if g := c.nodeBuf[gk]; len(g) > 0 {
			writeNodeChunk(c.w, g)
		}
	}
	clear(c.nodeBuf)
	return nil
}

// edges closes the node section and writes the relationships header on its
// first non-empty call, then buffers per edge group.
func (c *cypherWriter) edges(edges []EdgeRow) error {
	if len(edges) == 0 {
		return nil
	}
	if !c.inEdges {
		c.w.WriteByte('\n')
		c.w.WriteString(edgesHeader)
		c.inEdges = true
	}
	for _, e := range edges {
		gk := edgeGroupKey(e)
		g := append(c.edgeBuf[gk], e)
		if len(g) == batchSize {
			writeEdgeChunk(c.w, g)
			g = g[:0]
		}
		c.edgeBuf[gk] = g
	}
	return nil
}

// end flushes the edge groups in key order and closes the last section.
func (c *cypherWriter) end([]string) error {
	for _, gk := range sortedEdgeKeys(c.edgeBuf) {
		if g := c.edgeBuf[gk]; len(g) > 0 {
			writeEdgeChunk(c.w, g)
		}
	}
	clear(c.edgeBuf)
	c.w.WriteByte('\n')
	return nil
}

func (c *cypherWriter) bufferNodes(nodes []NodeRow) {
	for _, n := range nodes {
		gk := nodeGroupKey(n)
		g := append(c.nodeBuf[gk], n)
		if len(g) == batchSize {
			writeNodeChunk(c.w, g)
			g = g[:0]
		}
		c.nodeBuf[gk] = g
	}
}

// WriteCypher streams src into path. It writes a temporary file and renames it at the end,
// so a failed run leaves an earlier graph.cypher unchanged.
func WriteCypher(src Source, path string) (nodes, edges int, err error) {
	env := src.Envelope()
	if env == nil || env.IdentityVersion != 2 || env.Application.IdentityVersion != 2 {
		return 0, 0, fmt.Errorf("incompatible identity_version")
	}
	appID := env.Application.ID
	if err := validateIdentity(GraphRows{Nodes: []NodeRow{appRow(env)}}, appID); err != nil {
		return 0, 0, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".graph-*.cypher")
	if err != nil {
		return 0, 0, fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	w := bufio.NewWriterSize(f, 1<<20)
	if nodes, edges, err = streamGraph(src, newCypherWriter(w, appID)); err != nil {
		return 0, 0, fmt.Errorf("write %s: %w", path, err)
	}
	if err = w.Flush(); err == nil {
		if err = f.Chmod(0o644); err == nil {
			if err = f.Close(); err == nil {
				err = os.Rename(tmp, path)
			}
		}
	}
	if err != nil {
		return 0, 0, fmt.Errorf("write %s: %w", path, err)
	}
	return nodes, edges, nil
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
