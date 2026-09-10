//go:build windows

package terminal

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableProcessedInput = 0x0001
	enableLineInput      = 0x0002
	enableEchoInput      = 0x0004

	// Without this the console delivers whatever the key layout produces.
	// With it, arrows and function keys arrive as the ANSI sequences the
	// remote line editor is waiting for.
	enableVirtualTerminalInput = 0x0200

	enableProcessedOutput = 0x0001
	enableWrapAtEOLOutput = 0x0002

	// Makes the remote's ANSI output render instead of printing raw escapes.
	// DISABLE_NEWLINE_AUTO_RETURN is deliberately not set, so that a bare LF
	// still returns the cursor to column zero — serial consoles emit both CRLF
	// and bare LF depending on the target.
	enableVirtualTerminalProcessing = 0x0004
)

// State holds the console modes to put back on exit.
type State struct {
	in      uintptr
	inMode  uint32
	out     uintptr
	outMode uint32
}

// IsTerminal reports whether f is attached to a console.
func IsTerminal(f *os.File) bool {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}

// MakeRaw reconfigures the console for interactive console work.
func MakeRaw(f *os.File) (*State, error) {
	in := f.Fd()
	out := os.Stdout.Fd()

	var inMode, outMode uint32
	if r, _, err := procGetConsoleMode.Call(in, uintptr(unsafe.Pointer(&inMode))); r == 0 {
		return nil, fmt.Errorf("%w: stdin: %v", ErrNotTerminal, err)
	}
	if r, _, err := procGetConsoleMode.Call(out, uintptr(unsafe.Pointer(&outMode))); r == 0 {
		return nil, fmt.Errorf("%w: stdout: %v", ErrNotTerminal, err)
	}

	// Clearing ENABLE_PROCESSED_INPUT is the important part: it stops the
	// console from turning Ctrl-C into a control event, so the byte reaches the
	// target machine instead of terminating this process.
	newIn := inMode &^ (enableEchoInput | enableLineInput | enableProcessedInput)
	newIn |= enableVirtualTerminalInput
	if r, _, err := procSetConsoleMode.Call(in, uintptr(newIn)); r == 0 {
		return nil, fmt.Errorf("terminal: set console input mode: %v", err)
	}

	newOut := outMode | enableProcessedOutput | enableWrapAtEOLOutput | enableVirtualTerminalProcessing
	if r, _, err := procSetConsoleMode.Call(out, uintptr(newOut)); r == 0 {
		// Roll back the input half rather than leaving the console half-raw.
		procSetConsoleMode.Call(in, uintptr(inMode))
		return nil, fmt.Errorf("terminal: set console output mode: %v", err)
	}

	return &State{in: in, inMode: inMode, out: out, outMode: outMode}, nil
}

// Restore puts the console modes back.
func (s *State) Restore() error {
	procSetConsoleMode.Call(s.in, uintptr(s.inMode))
	procSetConsoleMode.Call(s.out, uintptr(s.outMode))
	return nil
}

type coord struct {
	x int16
	y int16
}

type smallRect struct {
	left   int16
	top    int16
	right  int16
	bottom int16
}

type consoleScreenBufferInfo struct {
	size              coord
	cursorPosition    coord
	attributes        uint16
	window            smallRect
	maximumWindowSize coord
}

// Size reports the console window in columns and rows.
func Size(f *os.File) (int, int, error) {
	var info consoleScreenBufferInfo
	r, _, err := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0, 0, fmt.Errorf("terminal: console size: %v", err)
	}
	cols := int(info.window.right-info.window.left) + 1
	rows := int(info.window.bottom-info.window.top) + 1
	return cols, rows, nil
}
