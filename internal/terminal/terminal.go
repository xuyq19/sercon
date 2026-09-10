// Package terminal puts the local terminal into raw mode so that a remote
// serial console behaves the way its operator expects.
//
// Raw mode here means: every keystroke goes up the wire unchanged, nothing is
// echoed locally, and the terminal's own line editor and signal generation are
// switched off. Ctrl-C must reach the target machine as byte 0x03 rather than
// killing the client — that one detail is the difference between a usable
// console and an unusable one.
package terminal

import "errors"

// ErrNotTerminal means the file is not an interactive terminal. Callers fall
// back to line-oriented behaviour rather than failing outright.
var ErrNotTerminal = errors.New("terminal: not an interactive terminal")
