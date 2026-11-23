// Package repository collects the repository-artifact layer (spec
// docs/design/specs/v2-artifacts.md): every non-.go project file as an artifact,
// and the go.mod requires as dependencies, with go.sum supplying locked versions.
//
// It reads files outside the Go source the symbol table sees, so it runs on every
// analysis (never from the cache) and is independent of the analysis level.
package repository

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
	"golang.org/x/mod/modfile"
)

// Ecosystem is the purl type used for Go modules (pkg:golang/<module-path>).
const Ecosystem = "golang"

// Options controls discovery.
type Options struct {
	// CaptureText puts each text artifact's whole source into Source (--artifact-text).
	CaptureText bool
	// OutputDir and CacheDir are the tool's own output locations. When either lies
	// inside the project, its files are not artifacts: a run that writes analysis.json
	// into the project must not capture the previous run's output.
	OutputDir string
	CacheDir  string
}

// skipDirNames are directories never walked: VCS metadata and vendored third-party code.
var skipDirNames = map[string]bool{".git": true, "vendor": true}

// outputFileNames are the files this tool writes into OutputDir.
var outputFileNames = map[string]bool{"analysis.json": true, "graph.cypher": true}

// Discover returns every non-.go regular file under root as an artifact, keyed by
// its repo-relative POSIX path. Files are visited in lexical order.
//
// The root, OutputDir and CacheDir are resolved through symlinks before the walk.
// filepath.WalkDir does not follow a symlink at the root, so a project reached
// through one would otherwise yield no artifacts at all. Symlinks found inside the
// tree are still not followed (they are skipped, as before), so discovery never
// leaves the project root.
func Discover(root string, opts Options) (map[string]schema.GoArtifact, error) {
	absRoot, err := resolveDir(root)
	if err != nil {
		return nil, fmt.Errorf("resolving project root: %w", err)
	}
	outDir, err := optionalDir(opts.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolving output directory: %w", err)
	}
	cacheDir, err := optionalDir(opts.CacheDir)
	if err != nil {
		return nil, fmt.Errorf("resolving cache directory: %w", err)
	}
	out := map[string]schema.GoArtifact{}
	err = filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if p != absRoot && (skipDirNames[d.Name()] || (cacheDir != "" && p == cacheDir)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if outDir != "" && filepath.Dir(p) == outDir && outputFileNames[d.Name()] {
			return nil
		}
		rel, err := filepath.Rel(absRoot, p)
		if err != nil {
			return err
		}
		art, err := readArtifact(p, filepath.ToSlash(rel), opts.CaptureText)
		if err != nil {
			return err
		}
		out[art.Path] = art
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("artifact discovery: %w", err)
	}
	return out, nil
}

// optionalDir resolves an optional directory setting; "" means unset.
func optionalDir(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	return resolveDir(p)
}

// resolveDir returns the absolute, symlink-free form of p. p need not exist yet
// (an output directory may be created later): the longest existing ancestor is
// resolved and the missing tail is re-appended. Any error other than "does not
// exist" (a symlink loop, a permission failure) is returned rather than ignored,
// so the self-exclusion comparisons never silently turn off.
func resolveDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	cur, tail := abs, ""
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// readArtifact hashes the whole file and classifies it. Text source is kept only
// when captureText is set; binaries are never captured.
func readArtifact(abs, rel string, captureText bool) (schema.GoArtifact, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return schema.GoArtifact{}, err
	}
	sum := sha256.Sum256(data)
	art := schema.GoArtifact{
		Path:       rel,
		Format:     formatOf(rel, data),
		SizeBytes:  int64(len(data)),
		SHA256:     hex.EncodeToString(sum[:]),
		Extraction: "none",
	}
	switch rel {
	case "go.mod":
		art.Roles = []string{"dependency-manifest"}
	case "go.sum":
		art.Roles = []string{"lockfile"}
	}
	if captureText && art.Format != "binary" {
		art.Source = string(data)
		art.Extraction = "full"
	}
	return art, nil
}

// formatOf names the file's format. go.mod and go.sum have their own names; the
// rest follow the Python analyzer's enum. Binary is a NUL byte or invalid UTF-8.
func formatOf(rel string, data []byte) string {
	base := filepath.Base(rel)
	switch {
	case base == "go.mod":
		return "gomod"
	case base == "go.sum":
		return "gosum"
	case base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") || strings.HasSuffix(base, ".dockerfile"):
		return "dockerfile"
	case strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt"):
		return "requirements"
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return "binary"
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".toml":
		return "toml"
	case ".yaml", ".yml":
		return "yaml"
	case ".json":
		return "json"
	case ".ini", ".cfg":
		return "ini"
	case ".properties":
		return "properties"
	}
	return "text"
}

// Facts builds the repository layer for a project: its artifacts, and the
// dependencies declared by the root go.mod. imports are the project's import
// paths (from the symbol table), used to fill provides_imports.
func Facts(root string, app *schema.GoApplication, opts Options) (*schema.RepositoryFacts, error) {
	artifacts, err := Discover(root, opts)
	if err != nil {
		return nil, err
	}
	facts := &schema.RepositoryFacts{Artifacts: artifacts}
	modPath := filepath.Join(root, "go.mod")
	modData, err := os.ReadFile(modPath)
	if err != nil {
		// No root go.mod: artifacts only, no dependencies.
		return facts, nil
	}
	f, err := modfile.Parse("go.mod", modData, nil)
	if err != nil {
		utils.Warn("repository: go.mod does not parse, dependencies skipped: %v", err)
		return facts, nil
	}
	locked := parseGoSum(filepath.Join(root, "go.sum"))
	imports := projectImports(app)

	deps := make([]schema.GoDependency, 0, len(f.Require))
	for _, r := range f.Require {
		dep := schema.GoDependency{
			Name:       r.Mod.Path,
			Ecosystem:  Ecosystem,
			Spec:       r.Mod.Version,
			Kind:       "runtime",
			Direct:     !r.Indirect,
			DeclaredIn: "go.mod",
			Prov:       []string{"declared"},
		}
		if locked[r.Mod.Path][r.Mod.Version] {
			dep.LockedVersion = r.Mod.Version
			dep.Prov = append(dep.Prov, "lockfile")
		}
		deps = append(deps, dep)
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	assignImports(deps, imports)
	facts.Dependencies = deps
	return facts, nil
}

// parseGoSum returns, per module path and version, whether go.sum carries both
// the module's h1 line and its /go.mod line. Only such a version is a locked
// version. A missing or unreadable go.sum locks nothing.
func parseGoSum(path string) map[string]map[string]bool {
	type seen struct{ h1, gomod bool }
	entries := map[string]map[string]*seen{}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]map[string]bool{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		mod, ver := fields[0], fields[1]
		gomod := strings.HasSuffix(ver, "/go.mod")
		ver = strings.TrimSuffix(ver, "/go.mod")
		if entries[mod] == nil {
			entries[mod] = map[string]*seen{}
		}
		if entries[mod][ver] == nil {
			entries[mod][ver] = &seen{}
		}
		if gomod {
			entries[mod][ver].gomod = true
		} else {
			entries[mod][ver].h1 = true
		}
	}
	out := map[string]map[string]bool{}
	for mod, vers := range entries {
		out[mod] = map[string]bool{}
		for ver, s := range vers {
			out[mod][ver] = s.h1 && s.gomod
		}
	}
	return out
}

// projectImports is the sorted, deduplicated set of import paths the project's
// (non-test, when the symbol table was built with --skip-tests) Go files use.
func projectImports(app *schema.GoApplication) []string {
	seen := map[string]bool{}
	for _, f := range app.SymbolTable {
		for _, imp := range f.Imports {
			seen[imp.Module] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// assignImports gives each import to the dependency whose module path is its
// longest prefix (the path itself, or a path under it followed by "/"). An
// import matching no requirement is not recorded.
func assignImports(deps []schema.GoDependency, imports []string) {
	for _, imp := range imports {
		best := -1
		bestLen := -1
		for i, d := range deps {
			if (imp == d.Name || strings.HasPrefix(imp, d.Name+"/")) && len(d.Name) > bestLen {
				best, bestLen = i, len(d.Name)
			}
		}
		if best >= 0 {
			deps[best].ProvidesImports = append(deps[best].ProvidesImports, imp)
		}
	}
}
