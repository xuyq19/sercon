//go:build !windows

// The Windows GUI has no equivalent elsewhere: a Linux jump host is headless,
// and "seriald capture" already serves it. This stub exists so the tree still
// builds for every target the client is built for.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "seriald-gui: the graphical daemon is Windows-only.")
	fmt.Fprintln(os.Stderr, "On a Linux jump host use: seriald capture")
	os.Exit(1)
}
