//go:build !windows

package winconsole

import "os"

func isConsole(f *os.File) bool { return false }

// StartedByDoubleClick is always false off Windows.
func StartedByDoubleClick() bool { return false }

// KeepOpen does nothing off Windows.
func KeepOpen(name, hint string) {}
