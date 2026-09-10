//go:build !windows

package sshauth

import (
	"os"
	"os/user"
	"path/filepath"
)

// KeyFile returns the authorized_keys path sshd reads for the given user.
//
// Everywhere except Windows that is ~/.ssh/authorized_keys, with no special
// case for administrators, which is why this file exists at all: the Windows
// split is the only interesting one.
func KeyFile(username string) (string, error) {
	home, err := homeDir(username)
	if err != nil {
		return "", err
	}
	return DefaultKeyFile(home), nil
}

// IsAdmin reports whether the account can write to the system-wide key file.
//
// On Unix the question does not arise — there is no separate file — so this
// reports false and the caller falls back to the per-user path.
func IsAdmin(username string) bool { return false }

// TightenACL is a no-op off Windows.
//
// sshd on Unix enforces the same idea through file ownership and mode:
// authorized_keys must not be group- or world-writable. Enforcing that here
// without knowing which of StrictModes' several rules the local sshd applies
// would risk breaking a working setup, so the file is left as the user's
// umask made it. TightenACL still exists so callers need no build tags.
func TightenACL(path string) error {
	return os.Chmod(path, 0o600)
}

// NeedsElevation reports whether writing path requires a privilege the current
// process does not have.
//
// On Unix this is answered by trying, so it reports false and the caller's
// write attempt becomes the real test.
func NeedsElevation(path string) bool { return false }

func homeDir(username string) (string, error) {
	if username == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return h, nil
	}
	u, err := user.Lookup(username)
	if err != nil {
		return "", err
	}
	// A user record can carry an empty home on some systems; fall back rather
	// than returning a path rooted at the filesystem.
	if u.HomeDir == "" {
		h, herr := os.UserHomeDir()
		if herr != nil {
			return "", herr
		}
		return h, nil
	}
	return filepath.Clean(u.HomeDir), nil
}
