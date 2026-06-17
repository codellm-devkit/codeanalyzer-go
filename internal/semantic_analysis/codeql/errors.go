// Package codeql is the isolated framework-backend subpackage for CodeQL analysis.
// It provides Tier-2 (framework-based) call-graph edges beyond what go/types resolves.
//
// The seams (loader, driver, query runner, errors) are scaffolded here even though
// the implementation is stubbed — dropping in the full implementation later requires
// no refactor. Mirrors codeanalyzer-python's semantic_analysis/codeql/ split.
package codeql

import "errors"

// ErrCodeQLNotFound is returned when the CodeQL CLI binary cannot be located.
var ErrCodeQLNotFound = errors.New("codeql: CLI binary not found; install from https://github.com/github/codeql-cli-binaries")

// ErrCodeQLNotImplemented is returned when CodeQL analysis is requested but not yet implemented.
var ErrCodeQLNotImplemented = errors.New("codeql: Go backend is a wired stub — implementation TODO (level-2 analysis)")
