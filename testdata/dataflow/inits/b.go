package inits

import "os"

func init() {
	setup()
	_ = os.Getenv("INITS_DEBUG")
}
