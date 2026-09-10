package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"sercon/internal/proto"
	"sercon/internal/terminal"
)

const (
	// escapeKey is Ctrl-A, chosen to match minicom so the reflex carries over.
	escapeKey = 0x01

	pingInterval = 15 * time.Second
	pongDeadline = 60 * time.Second
	maxBackoff   = 15 * time.Second
)

// errReconnect asks the outer loop to retry immediately, with no backoff.
var errReconnect = errors.New("sercon: reconnect requested")

func cmdAttach(args []string) error {
	o := &options{}
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	addCommonFlags(fs, o)
	logPath := fs.String("log", "", "also write the session to this local file")
	_ = parseArgs(fs, args)

	if err := o.validate(); err != nil {
		return err
	}
	ref := fs.Arg(0)

	log, err := openLocalLog(*logPath)
	if err != nil {
		return err
	}
	defer log.Close()

	// Reconnecting is an interactive convenience: the operator is sitting
	// there and does not want to retype the command because a USB adapter got
	// bumped. On a pipe it is wrong — the caller is a shell or a program, and
	// the rule for a closed stream is to end, not to silently wait and resume.
	// `grep` would otherwise hang forever on a target that went away.
	interactive := terminal.IsTerminal(os.Stdin)
	if !interactive {
		o.noReconnect = true
	}

	backoff := time.Second
	for {
		err := attachOnce(o, ref, log, interactive)
		switch {
		case err == nil, errors.Is(err, errUserQuit):
			return nil
		case errors.Is(err, errReconnect):
			backoff = time.Second
			continue
		case o.noReconnect:
			return err
		}

		if !interactive {
			return err
		}

		fmt.Fprintf(os.Stderr, "\r\nsercon: session ended: %v\r\n", err)
		fmt.Fprintf(os.Stderr, "sercon: reconnecting in %s, Ctrl-C to stop\r\n", backoff)

		sig := make(chan os.Signal, 1)
		stop := notifySignals(sig)
		select {
		case <-sig:
			stop()
			return nil
		case <-time.After(backoff):
			stop()
		}

		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// attachOnce runs one connection attempt to completion.
func attachOnce(o *options, ref string, log *localLog, interactive bool) error {
	rm, err := dialRemote(o, o.remoteCommand("session"))
	if err != nil {
		return err
	}
	defer func() {
		rm.Close()
		if msg := rm.remoteError(); msg != "" {
			fmt.Fprintf(os.Stderr, "\r\nsercon: jump host said: %s\r\n", msg)
		}
	}()

	s := &session{o: o, remote: rm, log: log, closed: make(chan struct{}), tty: interactive}
	s.lastRecv.Store(time.Now().UnixNano())

	// The handshake happens before the terminal goes raw, so authentication
	// failures and ref-resolution errors land in a normal, cooked terminal
	// where they can actually be read.
	if err := s.handshake(); err != nil {
		return err
	}

	info, backlog, err := s.openPort(ref)
	if err != nil {
		return err
	}

	// A terminal gets raw mode and the escape key. A pipe gets neither: it is
	// being driven by a shell or a program, so its bytes are already exactly
	// what the caller meant to send. This makes the tool behave like the local
	// device does — `sercon attach ... < /dev/null` reads like `cat /dev/ttyS0`
	// and `echo -e '\r' | sercon attach ...` writes like a redirect into it.
	if !s.tty {
		return s.runPiped(info, backlog)
	}

	state, err := terminal.MakeRaw(os.Stdin)
	if err != nil {
		return err
	}

	runErr := s.run(info, backlog)
	_ = state.Restore()
	return runErr
}

// session is one live attachment.
type session struct {
	o      *options
	remote *remote
	log    *localLog

	// tty is true when stdin is a terminal, which is what selects the
	// interactive behaviour: raw mode, the escape key, and the banner.
	tty bool

	closed chan struct{}
	once   sync.Once

	mu      sync.Mutex
	failure error

	stateMu  sync.Mutex
	escaping bool

	lastRecv atomic.Int64
}

func (s *session) handshake() error {
	host, _ := os.Hostname()
	user := firstNonEmpty(os.Getenv("USER"), os.Getenv("USERNAME"))

	if err := s.remote.wr.Ctrl(&proto.Message{
		Op:     proto.OpHello,
		V:      proto.Version,
		User:   user,
		Client: host,
	}); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	if _, _, err := readUntil(s.remote.rd, proto.OpWelcome); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	return nil
}

func (s *session) openPort(ref string) (*proto.Message, []byte, error) {
	if err := s.remote.wr.Ctrl(&proto.Message{
		Op:      proto.OpOpen,
		Port:    ref,
		Baud:    s.o.baud,
		Observe: s.o.observe,
	}); err != nil {
		return nil, nil, fmt.Errorf("request port: %w", err)
	}
	info, pending, err := readUntil(s.remote.rd, proto.OpOpened)
	if err != nil {
		return nil, nil, err
	}
	return info, pending, nil
}

// run drives the three concurrent activities until one of them ends.
func (s *session) run(info *proto.Message, backlog []byte) error {
	s.banner(info)
	if len(backlog) > 0 {
		_, _ = os.Stdout.Write(backlog)
		s.log.Write(backlog)
	}

	out := make(chan error, 3)
	go func() { out <- s.pumpInput() }()
	go func() { out <- s.pumpOutput() }()
	go func() { out <- s.heartbeat() }()

	var err error
	select {
	case err = <-out:
	case <-s.closed:
	}

	s.remote.Close()
	drain(out, 2)

	// A pump may have recorded a more specific cause than the read error that
	// surfaced first.
	s.mu.Lock()
	if s.failure != nil {
		err = s.failure
	}
	s.mu.Unlock()
	return err
}

// runPiped is the non-interactive path: no raw mode, no escape key, no banner
// on stdout.
//
// The important difference from run is what stdin EOF means. Interactively,
// a closed stdin is a reason to tear the session down, because there is no
// longer anyone at the keyboard. Here it only means the caller has finished
// writing — reading continues until the link drops or a signal arrives, which
// is how the local device behaves:
//
//	cat /dev/ttyUSB1 </dev/null     keeps printing until you interrupt it
//	echo -e '\r' >/dev/ttyUSB1      writes and exits immediately
//
// Without this, piping anything in would close the port before the reply came
// back, and the tool would be useless from a shell.
func (s *session) runPiped(info *proto.Message, backlog []byte) error {
	s.banner(info)
	if len(backlog) > 0 {
		_, _ = os.Stdout.Write(backlog)
		s.log.Write(backlog)
	}

	sig := make(chan os.Signal, 1)
	stop := notifySignals(sig)
	defer stop()

	out := make(chan error, 3)
	go func() { out <- s.pumpInput() }()
	go func() { out <- s.pumpOutput() }()
	go func() { out <- s.heartbeat() }()

	var err error
	select {
	case err = <-out:
	case <-sig:
		// A signal is the normal way to end a pipeline, the same as
		// interrupting cat. Not an error.
		err = nil
	case <-s.closed:
	}

	s.remote.Close()
	drain(out, 2)

	s.mu.Lock()
	if s.failure != nil {
		err = s.failure
	}
	s.mu.Unlock()
	return err
}

func (s *session) banner(info *proto.Message) {
	if !s.tty {
		// Notes would land in the middle of the stream the caller is piping
		// somewhere, so they go to stderr where they cannot corrupt it.
		role := "writable"
		if !info.Writable {
			role = "read-only"
		}
		fmt.Fprintf(os.Stderr, "sercon: %s @ %d baud (%s)\n", info.Port, info.Baud, role)
		if info.Log != "" {
			fmt.Fprintf(os.Stderr, "sercon: console log %s\n", info.Log)
		}
		return
	}

	role := "writable"
	if !info.Writable {
		role = "read-only"
	}
	fmt.Fprintf(os.Stdout, "\r\n--- %s @ %d baud, %s ---\r\n", info.Port, info.Baud, role)
	if info.Log != "" {
		fmt.Fprintf(os.Stdout, "--- console log: %s ---\r\n", info.Log)
	}
	if !info.Writable && info.Owner != "" {
		fmt.Fprintf(os.Stdout, "--- %s holds the write lock; your input is not forwarded ---\r\n", info.Owner)
	}
	fmt.Fprint(os.Stdout, "--- Ctrl-A ? for help ---\r\n")
}

// pumpInput turns local keystrokes into serial writes.
func (s *session) pumpInput() error {
	buf := make([]byte, 1024)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if !s.tty {
				// No escape key on a pipe: every byte is meant literally, and
				// a stray 0x01 is real data rather than a command prefix.
				if werr := s.remote.wr.Data(buf[:n]); werr != nil {
					return werr
				}
			} else if herr := s.handleKeys(buf[:n]); herr != nil {
				return herr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return s.inputClosed()
			}
			return err
		}
	}
}

// inputClosed handles stdin reaching EOF.
//
// On a terminal that ends the session: the operator closed stdin, so there is
// nobody left to type. On a pipe it must not: the caller redirected a file or
// finished the left side of a pipeline, but the output it asked for is still
// arriving. Returning here would close the port mid-reply and truncate it.
//
// So the input side simply stops participating and parks until the session
// ends by other means. Reading continues; `sercon attach ... </dev/null`
// behaves like `cat /dev/ttyUSB1 </dev/null`, which also keeps printing.
func (s *session) inputClosed() error {
	if s.tty {
		return nil
	}
	<-s.closed
	return nil
}

func (s *session) handleKeys(p []byte) error {
	var pending []byte
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		err := s.remote.wr.Data(pending)
		pending = pending[:0]
		return err
	}

	for _, b := range p {
		s.stateMu.Lock()
		escaping := s.escaping
		if escaping {
			s.escaping = false
		}
		s.stateMu.Unlock()

		if escaping {
			res := s.escape(b)
			switch {
			case res.quit:
				_ = flush()
				return errUserQuit
			case res.reconnect:
				return errReconnect
			}
			pending = append(pending, res.emit...)
			continue
		}

		if b == escapeKey {
			s.stateMu.Lock()
			s.escaping = true
			s.stateMu.Unlock()
			continue
		}
		pending = append(pending, b)
	}
	return flush()
}

type escapeResult struct {
	emit      []byte
	quit      bool
	reconnect bool
}

func (s *session) escape(b byte) escapeResult {
	switch b {
	case 'x', 'X', 'q', 'Q':
		return escapeResult{quit: true}
	case 'a', 'A':
		return escapeResult{emit: []byte{escapeKey}}
	case 'l', 'L':
		on, path, err := s.log.toggle()
		switch {
		case err != nil:
			s.screen("local log: %v", err)
		case on:
			s.screen("local log: recording to %s", path)
		default:
			s.screen("local log: paused (%s)", path)
		}
		return escapeResult{}
	case 'r', 'R':
		return escapeResult{reconnect: true}
	case 'b', 'B':
		if err := s.remote.wr.Ctrl(&proto.Message{Op: proto.OpBreak}); err != nil {
			s.screen("break: %v", err)
		} else {
			s.screen("break sent")
		}
		return escapeResult{}
	case 's', 'S':
		_ = s.remote.wr.Ctrl(&proto.Message{Op: proto.OpStatus})
		return escapeResult{}
	case 'z', 'Z', '?', 'h', 'H':
		s.help()
		return escapeResult{}
	default:
		// Unknown escape: forward both bytes rather than silently swallowing
		// input the operator did not intend as a command.
		return escapeResult{emit: []byte{escapeKey, b}}
	}
}

// pumpOutput renders serial output and services server-side control messages.
func (s *session) pumpOutput() error {
	for {
		f, err := s.remote.rd.Next()
		if err != nil {
			return err
		}
		s.lastRecv.Store(time.Now().UnixNano())

		switch f.Type {
		case proto.TypeData:
			if _, err := os.Stdout.Write(f.Payload); err != nil {
				return err
			}
			s.log.Write(f.Payload)
		case proto.TypeCtrl:
			var msg proto.Message
			if err := json.Unmarshal(f.Payload, &msg); err != nil {
				continue
			}
			if err := s.handleMessage(&msg); err != nil {
				return err
			}
		}
	}
}

func (s *session) handleMessage(msg *proto.Message) error {
	switch msg.Op {
	case proto.OpNotice:
		s.screen("%s", msg.Text)
	case proto.OpError:
		s.screen("error: %s", msg.Text)
	case proto.OpStat:
		if msg.Text != "" {
			s.screen("%s", msg.Text)
		}
	case proto.OpClosed:
		s.screen("detached: %s", msg.Text)
		return errUserQuit
	case proto.OpPong:
		// Liveness only; lastRecv already recorded it.
	}
	return nil
}

// heartbeat keeps the connection observable.
//
// The daemon drops idle connections, so a parked console still has to say
// something. The reverse direction matters just as much: without a periodic
// exchange, a half-open TCP connection can look healthy for a very long time.
func (s *session) heartbeat() error {
	t := time.NewTicker(pingInterval)
	defer t.Stop()

	for {
		select {
		case <-s.closed:
			return nil
		case <-t.C:
			if err := s.remote.wr.Ctrl(&proto.Message{Op: proto.OpPing}); err != nil {
				s.fail(err)
				return err
			}
			if since := time.Since(time.Unix(0, s.lastRecv.Load())); since > pongDeadline {
				err := fmt.Errorf("no response from the daemon for %s", since.Truncate(time.Second))
				s.fail(err)
				return err
			}
		}
	}
}

// fail records the first real cause of a teardown.
func (s *session) fail(err error) {
	s.mu.Lock()
	if s.failure == nil {
		s.failure = err
	}
	s.mu.Unlock()
	s.once.Do(func() { close(s.closed) })
}

// screen writes a server-side line to the terminal. Raw mode means every line
// ending must be explicit CRLF.
func (s *session) screen(format string, args ...any) {
	// On a terminal these status notes scroll past in the same place the
	// console output does. When stdout is a pipe it is carrying only the
	// console, so notes have to go to stderr or they corrupt the stream.
	if !s.tty {
		fmt.Fprintf(os.Stderr, "sercon: "+format+"\n", args...)
		return
	}
	fmt.Fprintf(os.Stdout, "\r\n[sercond] "+format+"\r\n", args...)
}

func (s *session) help() {
	fmt.Fprint(os.Stdout, "\r\n[sercond] Ctrl-A then:\r\n"+
		"  x, q   detach and exit\r\n"+
		"  a      send a literal Ctrl-A\r\n"+
		"  l      toggle local logging\r\n"+
		"  r      reconnect now\r\n"+
		"  b      send a break on the serial line\r\n"+
		"  s      show session status\r\n"+
		"  ?      this help\r\n")
}

// localLog is the client-side copy of the session.
type localLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
	on   bool
}

func openLocalLog(path string) (*localLog, error) {
	l := &localLog{}
	if path == "" {
		return l, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open local log: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	l.f, l.path, l.on = f, abs, true
	return l, nil
}

func (l *localLog) Write(p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil && l.on {
		_, _ = l.f.Write(p)
	}
}

// toggle starts logging if it was never configured, otherwise pauses or resumes.
func (l *localLog) toggle() (on bool, path string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		name := "sercond-" + time.Now().Format("20060102-150405") + ".log"
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return false, "", err
		}
		abs, aerr := filepath.Abs(name)
		if aerr != nil {
			abs = name
		}
		l.f, l.path, l.on = f, abs, true
		return true, abs, nil
	}

	l.on = !l.on
	return l.on, l.path, nil
}

func (l *localLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Sync()
		_ = l.f.Close()
		l.f = nil
	}
}

// drain waits briefly for the remaining pumps so no goroutine writes to the
// terminal after it has been restored.
func drain(ch chan error, n int) {
	for n > 0 {
		select {
		case <-ch:
			n--
		case <-time.After(500 * time.Millisecond):
			return
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
