package main

import (
	"fmt"

	"example.com/fixture/pkg/greeter"
)

func main() {
	g := greeter.New("Hello")
	msg := g.Greet("World")
	fmt.Println(msg)
	loud := greeter.Shout(msg)
	fmt.Println(loud)
}
