// Package ipc locates and manages the local endpoint the capture daemon
// listens on.
//
// Both Linux and Windows use an AF_UNIX socket here. Windows has supported
// AF_UNIX since 10 1803 and Go's net package drives it, so there is no reason
// to fork the design per platform — the same code path gives the same
// single-user-only access semantics on both.
package ipc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// maxSocketPath is the usable length of sun_path (108 bytes) minus the trailing
// NUL. Windows enforces the same limit, and both fail with a confusing message
// when exceeded, so it is checked up front.
const maxSocketPath = 100

// SocketName is the daemon's endpoint filename inside the runtime directory.
const SocketName = "s.sock"

// RuntimeDir returns the per-user directory holding the socket.
//
// XDG_RUNTIME_DIR is preferred on Linux because it is a tmpfs cleaned at
// logout. Everywhere else falls back to the user cache directory, which is
// already per-user and needs no uid call (os.Getuid does not exist on Windows).
func RuntimeDir() (string, error) {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "sercon", "run"), nil
	}
	c, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("ipc: locate runtime directory: %w", err)
	}
	return filepath.Join(c, "sercon", "run"), nil
}

// Endpoint returns the runtime directory and the socket path inside it.
func Endpoint() (dir string, sock string, err error) {
	dir, err = RuntimeDir()
	if err != nil {
		return "", "", err
	}
	sock = filepath.Join(dir, SocketName)
	if len(sock) > maxSocketPath {
		return "", "", fmt.Errorf(
			"ipc: socket path is %d bytes, over the %d-byte limit: %s (set XDG_RUNTIME_DIR to something shorter)",
			len(sock), maxSocketPath, sock)
	}
	return dir, sock, nil
}

// Alive reports whether something is accepting connections on sock.
func Alive(sock string) bool {
	c, err := Dial(sock, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Dial connects to the daemon endpoint.
func Dial(sock string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", sock, timeout)
}

// Listen binds the endpoint, taking over from a socket file left behind by a
// daemon that crashed.
//
// Binding is also the single-instance lock: the kernel refuses a second bind on
// the same path, so no separate lock file is needed on either platform.
func Listen(dir, sock string) (net.Listener, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ipc: create %s: %w", dir, err)
	}

	if _, err := os.Lstat(sock); err == nil {
		if Alive(sock) {
			return nil, fmt.Errorf("ipc: another daemon is already listening on %s", sock)
		}
		// Nothing answers, so the file is a leftover. Remove it or bind fails.
		if err := os.Remove(sock); err != nil {
			return nil, fmt.Errorf("ipc: remove stale socket %s: %w", sock, err)
		}
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("ipc: listen %s: %w", sock, err)
	}
	// Best effort. On Linux this is what restricts the endpoint to its owner;
	// on Windows the directory ACL already does that and chmod is a no-op.
	_ = os.Chmod(sock, 0o600)
	return ln, nil
}
