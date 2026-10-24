// Command pins prints the kit's pinned build arguments, one NAME=VALUE per
// line, from the constants in internal/sandboximage. kit/build.sh passes them
// to the Dockerfile so the Go pins stay the single source.
package main

import (
	"fmt"

	"github.com/smeltdev/smelt/internal/sandboximage"
)

func main() {
	for _, a := range sandboximage.KitPins() {
		fmt.Printf("%s=%s\n", a.Name, a.Value)
	}
}
