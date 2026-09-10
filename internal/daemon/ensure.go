// Package daemon starts the capture daemon and waits for it to come up.
//
// There is no service manager in the picture: the jump host may run Linux
// without a systemd unit for this, or Windows without a service. Instead the
// first client to need a daemon starts one, detached from the SSH session that
// requested it, and later clients find it through the socket.
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"seriald/internal/ipc"
)

// Wait bounds how long Ensure polls for the socket after starting a daemon.
const Wait = 5 * time.Second

// LogPath returns where a detached daemon's own diagnostics are written. It
// cannot use the console: on Windows the SSH session has no usable one, and on
// Linux stdout belongs to the channel that is about to close.
func LogPath(runtimeDir string) string {
	return filepath.Join(runtimeDir, "daemon.log")
}

// Ensure guarantees a daemon is listening on sock.
//
// exe is the path to the daemon binary and args the arguments that put it into
// capture mode. Spawn failures are not reported immediately: two clients
// starting at once is normal, and in that case the loser's spawn error is noise
// because the winner's daemon answers the poll.
func Ensure(sock, exe string, args []string, logPath string) error {
	if ipc.Alive(sock) {
		return nil
	}

	// The runtime directory may not exist yet. Nothing has created it on a
	// first run — the daemon would, but it cannot be started without its own
	// log file being openable first. Creating it here is what makes a fresh
	// machine work at all.
	dir := filepath.Dir(logPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("daemon: create %s: %w", dir, err)
	}

	spawnErr := spawn(exe, args, logPath)

	deadline := time.Now().Add(Wait)
	for time.Now().Before(deadline) {
		time.Sleep(80 * time.Millisecond)
		if ipc.Alive(sock) {
			return nil
		}
	}

	if spawnErr != nil {
		return fmt.Errorf("daemon: start %s: %w", exe, spawnErr)
	}
	return fmt.Errorf("daemon: nothing listening on %s after %s (see %s)", sock, Wait, logPath)
}
