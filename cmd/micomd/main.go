// Command micomd is the micom package's daemon as a program.
package main

import (
	"os"

	micom "github.com/W-Floyd/go-micomd"
)

func main() {
	os.Exit(micom.Main(os.Args[1:], os.Stdout, os.Stderr))
}
