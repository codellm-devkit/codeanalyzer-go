// Package malformed holds syntax errors the parser recovers from with a
// function node whose End lies one past the end of its file: in the FileSet
// that position has no token.File (the last file) or belongs to the next
// file. Each file holds one reproducer after a well-formed function, which
// the analyzer must still emit; recovery from the error swallows the rest of
// the file, so each reproducer gets its own file.
package malformed
