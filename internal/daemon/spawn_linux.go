//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// spawn starts the daemon in a new session with no controlling terminal.
//
// Setsid is the whole trick: a process in its own session receives no SIGHUP
// when the SSH channel that started it goes away, and since its stdio points at
// a file rather than the channel, sshd sees EOF on its pipes and closes the
// connection cleanly instead of hanging on to a live writer.
func spawn(exe string, args []string, logPath string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", logPath, err)
	}
	defer f.Close()

	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap it if we outlive it. If we exit first the init process takes over,
	// which is the normal case.
	go cmd.Wait()
	return nil
}
