//go:build windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Windows process creation flags. Go's syscall package does not export
// CREATE_BREAKAWAY_FROM_JOB, so it is declared here.
const (
	createBreakawayFromJob = 0x01000000
	createNewProcessGroup  = 0x00000200
	detachedProcess        = 0x00000008
)

// spawn starts the daemon outside the SSH session's job object.
//
// Windows OpenSSH assigns the session to a job object and tears the whole tree
// down on disconnect — the same behaviour as closing a console and watching
// its children die. DETACHED_PROCESS and CREATE_NEW_PROCESS_GROUP do not help,
// because neither leaves the job.
//
// CREATE_BREAKAWAY_FROM_JOB does, and upstream OpenSSH sets
// JOB_OBJECT_LIMIT_BREAKAWAY_OK precisely so that this is possible. It is the
// only supported way out, so if a future sshd stops allowing breakaway the
// daemon simply will not survive the session and Ensure reports the failure.
func spawn(exe string, args []string, logPath string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", logPath, err)
	}
	defer f.Close()

	flags := uint32(createBreakawayFromJob | detachedProcess | createNewProcessGroup)

	err = start(exe, args, f, flags)
	if err != nil {
		// A job without BREAKAWAY_OK rejects the flag outright. Retrying
		// without it still gives a working daemon for the lifetime of this
		// session, which is better than refusing to run.
		err = start(exe, args, f, detachedProcess|createNewProcessGroup)
	}
	return err
}

func start(exe string, args []string, out *os.File, flags uint32) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}

	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
