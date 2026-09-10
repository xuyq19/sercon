//go:build !linux && !windows

package daemon

import "errors"

// spawn has no implementation here. The jump host is expected to be Linux or
// Windows; this file exists so the tree cross-compiles for every target the
// client might be built for.
func spawn(exe string, args []string, logPath string) error {
	return errors.New("daemon: detaching is implemented for linux and windows only")
}
