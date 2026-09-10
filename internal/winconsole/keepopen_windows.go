//go:build windows

package winconsole

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode        = kernel32.NewProc("GetConsoleMode")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

func isConsole(f *os.File) bool {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}

// StartedByDoubleClick reports whether Explorer launched this process.
//
// Windows has no API that answers this directly, but the console does: a
// program started from an existing shell shares that shell's console, so at
// least two processes are attached. A double-clicked program gets a console to
// itself, so the count is one.
func StartedByDoubleClick() bool {
	// Redirected output means a pipe or a script, never a double-click.
	if !isConsole(os.Stdout) {
		return false
	}

	var pids [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n <= 1
}

// KeepOpen prints an explanation and waits for Enter, but only in the one case
// where the window would otherwise disappear before it could be read.
func KeepOpen(name, hint string) {
	if !StartedByDoubleClick() {
		return
	}
	fmt.Fprintf(os.Stderr, `
%s is a command-line program. Double-clicking it runs it with no arguments, so
it prints its usage and exits — that flash is Windows closing the console.

Run it from a terminal (PowerShell, cmd, or Windows Terminal) instead.

%s

Press Enter to close.
`, name, hint)
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
