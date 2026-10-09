package neo4j

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// Facade between the CLI and the Neo4j backend: EmitSchema (static contract,
// needs no project) and EmitNeo4j (project the v2 tree, then write — graph.cypher
// snapshot when no URI, live Bolt push when a URI is set).

// GraphFileName is the snapshot script written when no --neo4j-uri is given.
const GraphFileName = "graph.cypher"

// EmitNeo4j projects the v2 Analysis into GraphRows and writes it. The projection
// is pure and shared; only the write target differs. When cfg.URI is set it
// pushes over Bolt (incremental, additive unless cfg.Eager); otherwise it writes
// a self-contained graph.cypher snapshot into outputDir (default: current dir).
// It returns a short human-readable description of what was written.
func EmitNeo4j(ctx context.Context, a *v2.Analysis, outputDir string, cfg BoltConfig) (string, error) {
	rows := Project(a)
	appID := a.Application.ID

	if cfg.URI != "" {
		if err := WriteBolt(ctx, rows, appID, cfg); err != nil {
			return "", err
		}
		return fmt.Sprintf("pushed %d nodes, %d relationships to %s", len(rows.Nodes), len(rows.Edges), cfg.URI), nil
	}

	if outputDir == "" {
		outputDir = "."
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("create output dir %q: %w", outputDir, err)
	}
	path := filepath.Join(outputDir, GraphFileName)
	script := RenderCypher(rows, appID)
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return "wrote " + path, nil
}

// SchemaFileName is the machine-readable graph contract written by --emit schema.
const SchemaFileName = "schema.neo4j.json"

// EmitSchema serializes the static graph contract (BuildSchemaDocument) to
// <outputDir>/schema.neo4j.json. It is independent of any analyzed project:
// --emit schema requires no -i. An empty outputDir writes to the current
// directory. The JSON is indented and newline-terminated for a stable,
// diffable checked-in artifact.
func EmitSchema(outputDir string) (string, error) {
	doc := BuildSchemaDocument()
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("serialize schema document: %w", err)
	}
	data = append(data, '\n')

	if outputDir == "" {
		outputDir = "."
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("create output dir %q: %w", outputDir, err)
	}
	path := filepath.Join(outputDir, SchemaFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
