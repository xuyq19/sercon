// Command fake-ssh stands in for the ssh client in integration tests.
//
// It accepts the same argument shape sercon builds, discards the connection
// options and the destination, and runs the "remote" command on this machine
// instead. That makes it possible to exercise the whole chain — client,
// protocol, daemon, socket, serial driver — without a jump host in reach. The
// only thing not covered is ssh itself, which is the part with the least custom
// code in it.
//
// Not shipped in dist/. Build it into a directory, put that directory first on
// PATH, and sercon will pick it up:
//
//	go build -o /tmp/fakebin/ssh.exe ./hack/fake-ssh
//	PATH=/tmp/fakebin:$PATH sercon ls -t test@local
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// flagsWithValue are the ssh options that consume the following argument.
var flagsWithValue = map[string]bool{
	"-p": true, "-o": true, "-l": true, "-i": true, "-F": true, "-c": true,
	"-b": true, "-E": true, "-e": true, "-m": true, "-Q": true, "-w": true,
}

// flagsWithoutValue are accepted and ignored.
var flagsWithoutValue = map[string]bool{
	"-T": true, "-t": true, "-n": true, "-N": true, "-v": true, "-q": true,
	"-C": true, "-4": true, "-6": true, "-A": true, "-a": true, "-g": true,
}

// splitArgs walks the argument list the way ssh does: options first, then the
// destination, then the command and its arguments.
func splitArgs(args []string) (dest string, cmd []string, err error) {
	for i := 0; i < len(args); {
		a := args[i]
		switch {
		case flagsWithoutValue[a]:
			i++
		case flagsWithValue[a]:
			i += 2
		case a == "--":
			i++
			if i < len(args) {
				return args[i], args[i+1:], nil
			}
			return "", nil, fmt.Errorf("nothing after --")
		case strings.HasPrefix(a, "-"):
			// An unrecognised option. Assume it takes no value; ssh itself
			// would reject it, and this is a test shim.
			i++
		default:
			return a, args[i+1:], nil
		}
	}
	return "", nil, fmt.Errorf("no destination given")
}

func main() {
	dest, cmd, err := splitArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake-ssh: "+err.Error())
		os.Exit(2)
	}
	_ = dest

	if len(cmd) == 0 {
		fmt.Fprintln(os.Stderr, "fake-ssh: no command given")
		os.Exit(2)
	}

	// sercon passes the remote command as a single shell-quoted string, the same
	// way a real ssh would hand it to the remote login shell. Splitting on
	// whitespace is enough for the commands it builds, which never contain
	// quoted arguments.
	fields := strings.Fields(strings.Join(cmd, " "))
	if len(fields) == 0 {
		fmt.Fprintln(os.Stderr, "fake-ssh: empty command")
		os.Exit(2)
	}

	child := exec.Command(fields[0], fields[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	if err := child.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "fake-ssh: "+err.Error())
		os.Exit(1)
	}
}
