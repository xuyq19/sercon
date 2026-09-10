//go:build !linux && !windows

package terminal

import (
	"fmt"
	"os"
)

// State is empty on platforms without a raw-mode implementation.
type State struct{}

func IsTerminal(f *os.File) bool { return false }

func MakeRaw(f *os.File) (*State, error) {
	return nil, fmt.Errorf("%w: raw mode is implemented for linux and windows only", ErrNotTerminal)
}

func (s *State) Restore() error { return nil }

func Size(f *os.File) (int, int, error) {
	return 0, 0, fmt.Errorf("terminal: window size is implemented for linux and windows only")
}
