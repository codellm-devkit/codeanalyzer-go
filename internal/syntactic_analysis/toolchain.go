package syntactic_analysis

import (
	"go/version"
	"regexp"
	"runtime"

	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// tooNewRE matches go/types' TooNew error: "package requires newer Go version
// go1.27 (application built with go1.26)", or "file requires …" for a file
// whose //go:build line names a newer release.
var tooNewRE = regexp.MustCompile(`requires newer Go version (go[0-9][0-9A-Za-z.\-]*)`)

// toolchainGap collects the packages that failed to type-check because they
// need a newer Go than the one this analyzer was built with. go/types is
// compiled into the binary, so such a package is only partially typed and the
// analysis degrades silently (exit 0); warn records one stderr summary.
type toolchainGap struct {
	pkgs    map[string]bool
	highest string
}

// record notes pkg as affected when msg is a TooNew error, and reports
// whether it was one.
func (g *toolchainGap) record(pkg, msg string) bool {
	m := tooNewRE.FindStringSubmatch(msg)
	if m == nil {
		return false
	}
	if g.pkgs == nil {
		g.pkgs = map[string]bool{}
	}
	g.pkgs[pkg] = true
	if g.highest == "" || version.Compare(m[1], g.highest) > 0 {
		g.highest = m[1]
	}
	return true
}

// warn prints the one consolidated warning, if any package was affected.
func (g *toolchainGap) warn() {
	if len(g.pkgs) == 0 {
		return
	}
	noun := "packages"
	if len(g.pkgs) == 1 {
		noun = "package"
	}
	utils.Warn("%d %s require a newer Go version (highest %s) than this analyzer was built with (%s); "+
		"their type information is partial and the analysis is degraded. "+
		"Rebuild codeanalyzer-go with %s or newer (the latest Go release is recommended); run with -vv to list them",
		len(g.pkgs), noun, g.highest, runtime.Version(), g.highest)
}
