// COM port name handling.
//
// Windows serial ports are named COM1, COM2, … and those names need three
// separate pieces of string surgery: a device-namespace prefix before opening,
// a validity check, and a numeric sort. None of it touches the OS, so it lives
// here rather than behind a build tag — that way the rules can be tested on any
// platform, which matters because they are exactly the rules that are painful
// to debug on a real Windows box.

package serialport

import (
	"strconv"
	"strings"
)

// devicePath adds the \\.\ prefix the Win32 device namespace requires.
//
// It is not decoration. Without it, COM10 and above are parsed as ordinary
// filenames, so the adapter simply never opens — a failure that only shows up
// once a machine has enough ports.
func devicePath(name string) string {
	if strings.HasPrefix(name, `\\.\`) || strings.HasPrefix(name, `\\?\`) {
		return name
	}
	if isComName(name) {
		return `\\.\` + name
	}
	return name
}

// isComName reports whether s looks like a COM port name, case-insensitively.
//
// COM10 and up are not reserved device names on Windows, which is why the
// failure this guards against comes and goes with how many adapters a machine
// has ever seen.
func isComName(s string) bool {
	u := strings.ToUpper(s)
	if len(u) <= 3 || !strings.HasPrefix(u, "COM") {
		return false
	}
	for i := 3; i < len(u); i++ {
		if u[i] < '0' || u[i] > '9' {
			return false
		}
	}
	return true
}

// comNumber returns the numeric part of a COM name for sorting. Anything that
// is not a COM name sorts last, so a stray non-COM path never displaces a real
// port at the top of a listing.
func comNumber(name string) int {
	u := strings.ToUpper(name)
	if !strings.HasPrefix(u, "COM") {
		return 1 << 30
	}
	n, err := strconv.Atoi(u[3:])
	if err != nil {
		return 1 << 30
	}
	return n
}
