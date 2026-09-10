// Command sercon is the operator's client. It runs on the machine the engineer is
// sitting at — Windows, WSL, or Linux — and reaches serial consoles through a
// jump host that may be running any of those too.
//
// The client owns the protocol conversation end to end. The SSH channel only
// carries it: sercon spawns ssh, speaks the framed protocol over its stdin and
// stdout, and never depends on the jump host having a usable shell beyond
// running one command.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"sercon/internal/proto"
	"sercon/internal/version"
	"sercon/internal/winconsole"
)

const hint = `List the serial ports on a jump host:

    sercon ls -t user@jump

Then attach to one:

    sercon attach -t user@jump PORT

For a Windows jump host with a GUI, start sercon-gui.exe there instead.`

func main() {
	code := run()
	winconsole.KeepOpen("sercon", hint)
	os.Exit(code)
}

// run holds the real entry point so that every exit path goes back through
// main and gets the double-click guard.
func run() (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "sercon: internal error: %v\n", r)
			code = 1
		}
	}()

	if len(os.Args) < 2 {
		usage()
		return 2
	}

	var err error
	switch os.Args[1] {
	case "ls", "list":
		err = cmdList(os.Args[2:])
	case "attach", "a":
		err = cmdAttach(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "stop":
		err = cmdStop(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(version.Line("sercon", proto.Version))
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "sercon: unknown command %q\n\n", os.Args[1])
		usage()
		return 2
	}

	if err != nil {
		if errors.Is(err, errUserQuit) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "sercon: "+err.Error())
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `sercon - serial console client

usage:
  sercon ls      -t user@jump [--json]           list ports and who holds them
  sercon attach  -t user@jump [PORT] [options]   interactive console
  sercon run     -t user@jump  PORT [options]    scripted session, for automation
  sercon status  -t user@jump                    report daemon state
  sercon stop    -t user@jump                    shut the daemon down
  sercon version

attach options:
  --baud N          line rate for this attachment
  --observe         attach read-only, do not take the write lock
  --log FILE        also write the session to a local file
  --no-reconnect    exit when the link drops instead of retrying

run options:
  --script FILE     commands: send, sendln, wait, sleep
  --send TEXT       send once before waiting
  --expect REGEX    wait for REGEX (repeatable, matched in order)
  --timeout DUR     per-wait timeout (default 60s)
  --out FILE        capture everything received
  --quiet           do not echo received data to stdout

connection options:
  -t, --target      SSH target, user@jump (also honours ~/.ssh/config)
  --ssh-port N      SSH port
  --ssh-opt K=V     extra ssh -o option (repeatable)
  --remote-bin P    path to sercond on the jump host (default: sercond)
`)
}

// options carries the connection settings shared by every subcommand.
type options struct {
	target      string
	sshPort     string
	sshBin      string
	sshOpts     multiFlag
	remoteBin   string
	remoteSock  string
	baud        int
	observe     bool
	noReconnect bool
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func addCommonFlags(fs *flag.FlagSet, o *options) {
	fs.StringVar(&o.target, "t", "", "SSH target, user@jump (required)")
	fs.StringVar(&o.target, "target", "", "SSH target, user@jump (required)")
	fs.StringVar(&o.sshPort, "ssh-port", "", "SSH port")
	fs.StringVar(&o.sshBin, "ssh-bin", "ssh", "ssh client to run (use a full path to pick a specific install)")
	fs.Var(&o.sshOpts, "ssh-opt", "extra ssh -o option, repeatable")
	fs.StringVar(&o.remoteBin, "remote-bin", "sercond", "path to sercond on the jump host")
	fs.StringVar(&o.remoteSock, "remote-socket", "", "override the daemon socket path")
	fs.IntVar(&o.baud, "baud", 0, "line rate for this attachment")
	fs.BoolVar(&o.observe, "observe", false, "attach read-only")
	fs.BoolVar(&o.noReconnect, "no-reconnect", false, "exit instead of retrying when the link drops")
}

func (o *options) validate() error {
	if o.target == "" {
		return errors.New("no SSH target: pass -t user@jump")
	}
	return nil
}

// parseArgs parses args after letting flags appear anywhere on the line.
//
// Go's flag package stops at the first non-flag argument, so the natural
//
//	sercon attach -t host COM1 --baud 115200
//
// would leave --baud unparsed and silently ignored — the port would open at the
// default rate with nothing to say otherwise. Silently is the unacceptable
// part, so positional arguments are moved to the end before parsing.
func parseArgs(fs *flag.FlagSet, args []string) error {
	return fs.Parse(reorderArgs(fs, args))
}

func reorderArgs(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]

		// "--" ends option processing, exactly as flag itself treats it.
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' || a == "-" {
			positional = append(positional, a)
			continue
		}

		flags = append(flags, a)

		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			continue // the value is already attached
		}
		// A value-less flag must not swallow the argument after it.
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func isBoolFlag(f *flag.Flag) bool {
	type boolFlag interface{ IsBoolFlag() bool }
	if bf, ok := f.Value.(boolFlag); ok {
		return bf.IsBoolFlag()
	}
	return false
}

// sshCommand builds the transport.
//
// -T is not optional. Allocating a remote pty would put a line discipline
// between the protocol and us, and that layer happily rewrites 0x0d, 0x0a and
// 0x03 on the way through — which is fatal for framed binary data and for a
// serial console that needs those bytes intact.
func (o *options) sshCommand(remoteCmd string) *exec.Cmd {
	args := []string{"-T"}
	if o.sshPort != "" {
		args = append(args, "-p", o.sshPort)
	}
	// Detect a dead link in about a minute instead of waiting for TCP.
	args = append(args,
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
	)
	// Every operator-supplied option needs its own -o. Dropping the flag would
	// silently turn the option into a positional argument, and ssh would then
	// treat it as the destination host.
	for _, opt := range o.sshOpts {
		args = append(args, "-o", opt)
	}
	args = append(args, o.target, remoteCmd)

	// Not hardcoded to "ssh": Windows commonly has two installs (Git's and the
	// system one), and a wrapper script is a legitimate stand-in for either.
	bin := o.sshBin
	if bin == "" {
		bin = "ssh"
	}
	return exec.Command(bin, args...)
}

// remoteCommand renders the command the jump host will run, shell-quoted.
//
// ssh hands the command to the remote login shell, so anything with a space or
// a metacharacter must be quoted. Doing it unconditionally for every argument
// keeps paths and port references safe without a per-caller decision.
func (o *options) remoteCommand(sub string, extra ...string) string {
	parts := []string{shellQuote(o.remoteBin), shellQuote(sub)}
	if o.remoteSock != "" {
		parts = append(parts, shellQuote("--socket"), shellQuote(o.remoteSock))
	}
	for _, a := range extra {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote renders one argument for the remote login shell.
//
// ssh hands the command string to whatever shell the account uses, which in
// practice is not always bash: zsh, for one, aborts on an unmatched glob where
// bash would pass it through. Quoting anything with a special character covers
// that, with a single exception below.
func shellQuote(s string) string {
	// A leading ~ must stay outside the quotes. Tilde expansion happens before
	// quote removal, so quoting it produces a filename that literally starts
	// with a tilde — and "~/bin/sercond" is the most natural way to refer to a
	// binary installed under a home directory.
	if s == "~" {
		return "~"
	}
	if rest, ok := strings.CutPrefix(s, "~/"); ok {
		// "~/" + 'quoted rest' concatenates after expansion, which every
		// POSIX shell handles.
		return "~/" + quoteToken(rest)
	}
	return quoteToken(s)
}

func quoteToken(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		safe := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			strings.ContainsRune("/._-@:,+=", r)
		if !safe {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// remote is a live protocol conversation over a spawned ssh process.
type remote struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *syncBuffer
	rd     *proto.Reader
	wr     *proto.Writer
}

func dialRemote(o *options, remoteCmd string) (*remote, error) {
	cmd := o.sshCommand(remoteCmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdout: %w", err)
	}

	// ssh writes prompts for passphrases and host keys straight to /dev/tty, so
	// capturing stderr costs nothing and keeps ssh's error text out of the
	// console stream, where it would corrupt the display.
	stderr := &syncBuffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh: %w", err)
	}
	return &remote{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		rd:     proto.NewReader(stdout),
		wr:     proto.NewWriter(stdin),
	}, nil
}

func (r *remote) Close() {
	r.stdin.Close()
	r.stdout.Close()

	done := make(chan struct{})
	go func() {
		_ = r.cmd.Wait()
		close(done)
	}()
	// A vanished link can leave ssh wedged. Give it a moment to notice, then
	// stop waiting — the caller has a reconnect loop to get back to.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
	}
}

func (r *remote) remoteError() string { return strings.TrimSpace(r.stderr.String()) }

// runRemote executes a one-shot command on the jump host and returns its stdout.
func runRemote(o *options, remoteCmd string) ([]byte, error) {
	cmd := o.sshCommand(remoteCmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Inherit stdin so ssh can still prompt for a host key or a passphrase.
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("jump host: %s", msg)
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}
	return stdout.Bytes(), nil
}

// readUntil consumes frames until the wanted control reply arrives.
//
// Serial data can legitimately arrive before the reply does — the daemon queues
// the backlog ahead of the "opened" acknowledgement to keep history ordered —
// so that data is returned rather than dropped.
func readUntil(rd *proto.Reader, want string) (*proto.Message, []byte, error) {
	var pending []byte
	for {
		f, err := rd.Next()
		if err != nil {
			return nil, pending, err
		}
		switch f.Type {
		case proto.TypeData:
			pending = append(pending, f.Payload...)
		case proto.TypeCtrl:
			var msg proto.Message
			if err := json.Unmarshal(f.Payload, &msg); err != nil {
				continue
			}
			if msg.Op == proto.OpError {
				return nil, pending, errors.New(msg.Text)
			}
			if msg.Op == want {
				return &msg, pending, nil
			}
		}
	}
}

func cmdList(args []string) error {
	o := &options{}
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	addCommonFlags(fs, o)
	asJSON := fs.Bool("json", false, "print JSON")
	_ = parseArgs(fs, args)

	if err := o.validate(); err != nil {
		return err
	}

	out, err := runRemote(o, o.remoteCommand("list", "--json"))
	if err != nil {
		return err
	}

	var ports []proto.PortInfo
	if err := json.Unmarshal(bytes.TrimSpace(out), &ports); err != nil {
		return fmt.Errorf("parse port list: %w", err)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(ports)
	}
	return renderPorts(ports)
}

func cmdStatus(args []string) error {
	o := &options{}
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addCommonFlags(fs, o)
	_ = parseArgs(fs, args)

	if err := o.validate(); err != nil {
		return err
	}

	out, err := runRemote(o, o.remoteCommand("status"))
	if err != nil {
		return err
	}
	fmt.Print(string(out))
	return nil
}

func cmdStop(args []string) error {
	o := &options{}
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	addCommonFlags(fs, o)
	_ = parseArgs(fs, args)

	if err := o.validate(); err != nil {
		return err
	}

	out, err := runRemote(o, o.remoteCommand("stop"))
	if err != nil {
		return err
	}
	fmt.Print(string(out))
	return nil
}

func renderPorts(ports []proto.PortInfo) error {
	if len(ports) == 0 {
		fmt.Println("no serial ports found on the jump host")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REF\tDEV\tBAUD\tSTATE\tOWNER\tOBS\tDESC")
	for _, p := range ports {
		state := "offline"
		if p.Online {
			state = "online"
		}
		owner := dash(p.Owner)
		observers := "-"
		if p.Observers > 0 {
			observers = strconv.Itoa(p.Observers)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			p.Ref, p.Dev, p.Baud, state, owner, observers, dash(p.Desc))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nattach with: sercon attach -t %s\n", "<user@jump>")
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// syncBuffer is a goroutine-safe byte sink for capturing a child's stderr.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var errUserQuit = errors.New("sercon: user quit")

// notifySignals reports Ctrl-C and SIGTERM while the terminal is not in raw
// mode, which is the only time they can be delivered.
func notifySignals(ch chan os.Signal) func() {
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	return func() { signal.Stop(ch) }
}
