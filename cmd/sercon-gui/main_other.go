//go:build !windows

// The Windows GUI has no equivalent elsewhere: a Linux jump host is headless,
// and "sercond capture" already serves it. This stub exists so the tree still
// builds for every target the client is built for.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "sercon-gui: the graphical daemon is Windows-only.")
	fmt.Fprintln(os.Stderr, "On a Linux jump host use: sercond capture")
	os.Exit(1)
}
