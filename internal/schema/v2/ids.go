package v2

import "strings"

// Group A vocabulary: the `can://` durable-id grammar (>= callable) and the
// `@line:col` ordinal-id grammar (< callable), plus span construction. These
// helpers are pure so they can be tested in isolation and reused verbatim by
// the deferred L3/L4 train (which must not re-decide id or span shape).
//
// Durable id grammar (a containment path with an app segment so multiple apps
// in one language don't collide):
//
//	can://<lang>/<app>/<file>/<type>/<callable-signature>
//	can://go/myapp/src/util.go/Hasher/Hash(string)uint64
//
// Ordinal id grammar (statements and synthetic vertices, addressed WITHIN their
// callable):
//
//	<callable-id>@<line>:<col>   e.g. …/Hash(string)uint64@15:2
//	<callable-id>@<tag>          e.g. …/Hash(string)uint64@entry
//
// The delimiters `/`, `@`, `:` are fixed by the keystone; do not substitute.
const (
	scheme = "can://"
	// idSep joins containment segments in a durable id.
	idSep = "/"
	// ordinalSep separates a callable id from an ordinal (line:col or @tag).
	ordinalSep = "@"
)

// AppID builds the application id: can://<lang>/<app>. lang is fixed to the
// package Language constant so callers cannot drift it.
func AppID(app string) string {
	return scheme + Language + idSep + app
}

// ModuleID builds a module id from the application id and the module's file
// path relative to the input root (forward-slashed, no leading slash, no "..").
func ModuleID(appID, relPath string) string {
	return appID + idSep + normalizePath(relPath)
}

// TypeID builds a type id under its module. sig is the type's signatureOf()
// output — the last (and only) segment the type contributes.
func TypeID(moduleID, sig string) string {
	return moduleID + idSep + sig
}

// CallableID builds a callable id under its parent. For a method the parent is
// its receiver Type id; for a module-level function the parent is the Module
// id. sig is the callable's signatureOf() output — the last path segment.
func CallableID(parentID, sig string) string {
	return parentID + idSep + sig
}

// OrdinalID addresses a real body node (statement / call) within its callable
// by source position: <callable-id>@<line>:<col>. Both line and col are
// required — a bare line is not unique within a callable.
func OrdinalID(callableID string, line, col int) string {
	return callableID + ordinalSep + itoa(line) + ":" + itoa(col)
}

// TagID addresses a synthetic vertex within its callable by tag:
// <callable-id>@<tag> (e.g. "entry", "exit", "formal_in:0").
func TagID(callableID, tag string) string {
	return callableID + ordinalSep + tag
}

// LocalID is the key a real body node is stored under inside body{}: the bare
// "<line>:<col>" ordinal (the id relative to the enclosing callable).
func LocalID(line, col int) string {
	return itoa(line) + ":" + itoa(col)
}

// NewSpan builds a Span from 1-based line/col positions and the [from, to) UTF-8
// byte offsets into module.source. Byte offsets are what make source slicing
// O(1); line:col is what addresses and displays.
func NewSpan(startLine, startCol, endLine, endCol, fromByte, toByte int) Span {
	return Span{
		Start: [2]int{startLine, startCol},
		End:   [2]int{endLine, endCol},
		Bytes: [2]int{fromByte, toByte},
	}
}

// Slice returns the node's source text: module source sliced by the span's byte
// offsets. It is the O(1) replacement for the v1 per-node `code` field. Out-of-
// range or inverted offsets yield "" rather than panicking, so a malformed span
// degrades gracefully.
func (s Span) Slice(source string) string {
	from, to := s.Bytes[0], s.Bytes[1]
	if from < 0 || to > len(source) || from > to {
		return ""
	}
	return source[from:to]
}

// normalizePath makes a relative file path safe for a can:// segment: forward
// slashes, no leading slash. Callers are responsible for passing a path already
// relative to the input root (no "..").
func normalizePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return strings.TrimPrefix(p, "/")
}

// itoa is a tiny non-allocating-path integer formatter for id assembly, kept
// local so this file has no fmt dependency for its hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
