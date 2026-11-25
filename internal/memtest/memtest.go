// Package memtest checks that cango pushes a large app to Neo4j inside a fixed memory limit (spec §7).
//
// The container test is in memtest_test.go behind the memtest build tag. This file holds its
// pure helpers and its pass/fail decision, untagged, so plain `go test` covers them without Docker.
package memtest

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const gib = int64(1) << 30

// peakMarker prefixes the line the cango container prints with its cgroup memory.peak.
const peakMarker = "MEMTEST_PEAK="

// ParseBytes reads a positive byte count with an optional k, m or g suffix (powers of 1024), as in "16g".
func ParseBytes(in string) (int64, error) {
	s := strings.ToLower(strings.TrimSpace(in))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult = 1 << 10
	case strings.HasSuffix(s, "m"):
		mult = 1 << 20
	case strings.HasSuffix(s, "g"):
		mult = gib
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("bad memory size %q", in)
	}
	return n * mult, nil
}

// PeakFrom returns the byte count on the last MEMTEST_PEAK= line of logs, or 0 when there is none
// (the whole cgroup was killed, or memory.peak could not be read).
func PeakFrom(logs string) int64 {
	var peak int64
	for line := range strings.Lines(logs) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), peakMarker); ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				peak = n
			}
		}
	}
	return peak
}

// Tail returns the last n lines of s.
func Tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// AppName turns an app directory's base name into the cango --app-name value.
// Bytes outside [A-Za-z0-9._-] become '-', so the name is valid for v2.ResolveIdentity
// (no slash, no control character, valid UTF-8) and needs no escaping in the can:// id.
func AppName(base string) (string, error) {
	b := []byte(base)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '-'
		}
	}
	name := string(b)
	if strings.Trim(name, "-") == "" || name == "." || name == ".." {
		return "", fmt.Errorf("app directory %q gives no usable app name", base)
	}
	return name, nil
}

// AppNames maps each app directory base name to its app name, in order.
// Two apps with the same app name would share one can://<name> keyspace in Neo4j,
// so the second push would not be a clean full push: that is an error.
func AppNames(bases []string) ([]string, error) {
	names := make([]string, 0, len(bases))
	seen := map[string]string{}
	var errs []error
	for _, base := range bases {
		name, err := AppName(base)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if prev, dup := seen[name]; dup {
			errs = append(errs, fmt.Errorf("apps %q and %q both get app name %q; rename one directory", prev, base, name))
			continue
		}
		seen[name] = base
		names = append(names, name)
	}
	return names, errors.Join(errs...)
}

// Verdict is the result of one cango container run.
type Verdict struct {
	Failed bool   // the run failed: non-zero exit (137 = OOM kill of cango) or Docker's OOMKilled
	Line   string // the log line: "<app>: exit N, OOMKilled=B, peak X GiB of Y GiB"
}

// Decide judges one run from the container's exit code, Docker's OOMKilled flag, its logs and the limit.
//
// Docker does not set memory.oom.group, so the kernel OOM killer usually kills only cango.
// The sh that is PID 1 survives, prints the peak and exits 137, and OOMKilled may stay false:
// any non-zero exit therefore fails on its own, and OOMKilled fails even with exit 0.
func Decide(app string, exitCode int, oomKilled bool, logs string, limit int64) Verdict {
	peak := "peak unavailable"
	if p := PeakFrom(logs); p > 0 {
		peak = fmt.Sprintf("peak %.1f GiB", float64(p)/float64(gib))
	}
	return Verdict{
		Failed: exitCode != 0 || oomKilled,
		Line: fmt.Sprintf("%s: exit %d, OOMKilled=%v, %s of %.1f GiB", app, exitCode, oomKilled,
			peak, float64(limit)/float64(gib)),
	}
}
