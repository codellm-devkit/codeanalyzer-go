package utils

import (
	"fmt"
	"os"
)

var verbosity int

// SetVerbosity sets the global log level (0=quiet, 1=info, 2=debug).
func SetVerbosity(v int) { verbosity = v }

// Info logs an informational message when verbosity >= 1.
func Info(format string, args ...any) {
	if verbosity >= 1 {
		fmt.Fprintf(os.Stderr, "[codeanalyzer-go] "+format+"\n", args...)
	}
}

// Debug logs a debug message when verbosity >= 2.
func Debug(format string, args ...any) {
	if verbosity >= 2 {
		fmt.Fprintf(os.Stderr, "[codeanalyzer-go DEBUG] "+format+"\n", args...)
	}
}

// Warn always prints a warning to stderr.
func Warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[codeanalyzer-go WARN] "+format+"\n", args...)
}
