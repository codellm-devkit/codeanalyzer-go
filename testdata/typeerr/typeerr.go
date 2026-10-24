// Package typeerr does not type-check: it imports a package that does not
// exist, so missing.Req is an invalid type and go/types records no field
// selections on it. L3 must still see the writes to its fields.
package typeerr

import "example.com/typeerr/missing"

// Forward writes a field of a value of the unresolved type and reads it back.
func Forward(src string) string {
	var req missing.Req
	req.Body = src
	return req.Body
}

// Undeclared writes a field of a name that is declared nowhere: the base
// itself cannot be resolved.
func Undeclared(src string) string {
	undeclared.Body = src
	return undeclared.Body
}

// Qualified writes a field of a variable of the missing package.
func Qualified(src string) string {
	missing.Cfg.Body = src
	return missing.Cfg.Body
}

// Asserted writes through a type assertion, a target pathOf cannot walk.
func Asserted(x any, src string) any {
	x.(*missing.Req).Body = src
	return x
}
