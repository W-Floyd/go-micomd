// Command micom-spec prints a variant (an embedded name or a file) as
// Markdown tables, parents merged in.
package main

import (
	"fmt"
	"os"

	micom "github.com/W-Floyd/go-micomd"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: micom-spec VARIANT|FILE.yaml")
		os.Exit(2)
	}
	s, err := micom.SpecMarkdown(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "micom-spec:", err)
		os.Exit(1)
	}
	fmt.Print(s)
}
