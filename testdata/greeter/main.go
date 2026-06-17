package main

import (
	"fmt"

	"example.com/greeter/pkg/greeter"
)

func main() {
	g := greeter.New("Hello")
	msg := g.Greet("World")
	fmt.Println(msg)
	loud := greeter.Shout(msg)
	fmt.Println(loud)
}
