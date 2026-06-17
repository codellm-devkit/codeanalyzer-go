package codeql

import (
	"os/exec"
)

// Loader resolves the CodeQL CLI binary path.
type Loader struct {
	binaryPath string
}

// NewLoader creates a Loader, probing the PATH for the codeql binary.
func NewLoader() (*Loader, error) {
	path, err := exec.LookPath("codeql")
	if err != nil {
		return nil, ErrCodeQLNotFound
	}
	return &Loader{binaryPath: path}, nil
}

// BinaryPath returns the resolved CodeQL binary path.
func (l *Loader) BinaryPath() string { return l.binaryPath }
