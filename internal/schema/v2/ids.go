package v2

import "strings"

// Group A vocabulary: the `can://` durable-id grammar (>= callable) and the
// `@line:col` ordinal-id grammar (< callable), plus span construction. These
// helpers are pure so they can be tested in isolation and reused verbatim by
// the deferred L3/L4 train (which must not re-decide id or span shape).
//
// Durable id grammar (the app segment is outermost so a polyglot app shares one
// `can://<app>` prefix across languages; the language follows it):
//
//	can://<app>/<lang>/<file>/<type>/<callable-signature>
//	can://myapp/go/src/util.go/Hasher/Hash(string)uint64
//
// External (out-of-project) symbols are language-neutral:
//
//	can://<app>/@external/<import-path>/<sig>
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

// AppID builds the application id: can://<app>. The language is not part of it
// (it follows the app in module ids) so sibling analyzers share the prefix.
func AppID(app string) string {
	return scheme + app
}

// ModuleID builds a module id from the application id and the module's file
// path relative to the input root (forward-slashed, no leading slash, no "..").
// The language segment is fixed to the package Language constant so callers
// cannot drift it.
func ModuleID(appID, relPath string) string {
	return appID + idSep + Language + idSep + normalizePath(relPath)
}

// ExternalID builds the id of an out-of-project symbol:
// <app-id>/@external/<import-path>/<sig>. It is language-neutral. An empty sig
// (an import-level ghost, no symbol) yields <app-id>/@external/<import-path>.
func ExternalID(appID, importPath, sig string) string {
	if sig == "" {
		return appID + idSep + "@external" + idSep + importPath
	}
	return appID + idSep + "@external" + idSep + importPath + idSep + sig
}

// GlobalOrdinal joins a callable id and a local ordinal: callableID + local
// when local already starts with "@" (a tag), else callableID + "@" + local
// (a "<line>:<col>" position).
func GlobalOrdinal(callableID, local string) string {
	if strings.HasPrefix(local, ordinalSep) {
		return callableID + local
	}
	return callableID + ordinalSep + local
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

// CallBodyKeys returns the body-map key of each call site, given every site's
// [line, col] in recording order (ast.Inspect pre-order, so a call precedes the
// calls nested in it). The first site at a position gets the bare LocalID
// "l:c"; later sites at the same position (a chained call such as s.A().B()
// starts where s.A() does) get "l:c/2", "l:c/3", … This is the single source of
// call-node keys, shared by the L1 emitter and the L3 body builder so both
// produce byte-identical keys.
func CallBodyKeys(positions [][2]int) []string {
	keys := make([]string, len(positions))
	seen := make(map[[2]int]int, len(positions))
	for i, p := range positions {
		seen[p]++
		keys[i] = LocalID(p[0], p[1])
		if n := seen[p]; n > 1 {
			keys[i] += "/" + itoa(n)
		}
	}
	return keys
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
