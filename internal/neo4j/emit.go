package neo4j

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Facade between the CLI and the Neo4j backend: EmitSchema (static contract,
// needs no project) and EmitNeo4j (stream the v2 payload into a graph.cypher
// snapshot when no URI is set, or into a live Bolt push when one is).

// GraphFileName is the snapshot script written when no --neo4j-uri is given.
const GraphFileName = "graph.cypher"

// EmitNeo4j streams src, one module at a time, into the write target. When
// cfg.URI is set it pushes over Bolt (incremental, additive unless cfg.Eager);
// otherwise it writes a self-contained graph.cypher snapshot into outputDir
// (default: current dir). Neither target holds the whole tree or every row
// (#59). It returns a short human-readable description of what was written.
// The counts are rows sent: a duplicate edge row across modules counts twice.
func EmitNeo4j(ctx context.Context, src Source, outputDir string, cfg BoltConfig) (string, error) {
	env := src.Envelope()
	if env == nil || env.IdentityVersion != 2 || env.Application.IdentityVersion != 2 {
		return "", fmt.Errorf("incompatible identity_version")
	}
	if cfg.URI != "" {
		nodes, edges, err := PushBolt(ctx, src, cfg)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("pushed %d nodes, %d relationships to %s", nodes, edges, cfg.URI), nil
	}

	if outputDir == "" {
		outputDir = "."
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("create output dir %q: %w", outputDir, err)
	}
	path := filepath.Join(outputDir, GraphFileName)
	if _, _, err := WriteCypher(src, path); err != nil {
		return "", err
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
