// Package b instantiates the same g.Map[int] as package a.
package b

import "example.com/ssapanic/g"

func UseB() int { return g.Map[int](2) }
