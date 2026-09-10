// Package winconsole keeps a double-clicked console program from vanishing
// before its output can be read.
//
// Windows opens a console for a double-clicked program, lets it run, and closes
// the console the moment it exits. A command-line tool therefore appears to
// crash: the window flashes and is gone. There is nothing wrong with the
// program, but from the user's side it is indistinguishable from a failure, so
// the honest fix is to hold the window open and say what happened.
package winconsole

import "os"

// IsConsole reports whether stdout is a real console rather than a pipe or a
// file. Piped output must never block waiting for a keypress.
func IsConsole() bool {
	return isConsole(os.Stdout)
}
