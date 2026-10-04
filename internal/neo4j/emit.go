package neo4j

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Facade between the CLI and the Neo4j backend. At M1 only EmitSchema exists —
// the static contract serializer, which needs no analyzed project. EmitNeo4j
// (project + write) lands at M6 once the projector (M3) and the writers (M4/M5)
// exist.

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
