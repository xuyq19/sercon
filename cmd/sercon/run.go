package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"sercon/internal/proto"
)

// errNoMatch means a wait ran out of time without seeing its pattern.
var errNoMatch = errors.New("pattern not seen before the timeout")

// maxCollect bounds the in-memory transcript. A boot log is megabytes at most;
// anything larger is a runaway target and the memory is better spent elsewhere.
const maxCollect = 4 << 20

func cmdRun(args []string) error {
	o := &options{}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	addCommonFlags(fs, o)
	scriptPath := fs.String("script", "", "script file: send, sendln, wait, sleep")
	send := fs.String("send", "", "send this text once, before the waits")
	var expects multiFlag
	fs.Var(&expects, "expect", "regex to wait for, repeatable, matched in order")
	timeout := fs.Duration("timeout", 60*time.Second, "timeout applied to each wait")
	outPath := fs.String("out", "", "capture everything received to this file")
	quiet := fs.Bool("quiet", false, "do not echo received data to stdout")
	_ = parseArgs(fs, args)

	if err := o.validate(); err != nil {
		return err
	}
	ref := fs.Arg(0)

	steps, err := buildSteps(*scriptPath, *send, expects)
	if err != nil {
		return err
	}

	rm, err := dialRemote(o, o.remoteCommand("session"))
	if err != nil {
		return err
	}
	defer rm.Close()

	s := &session{o: o, remote: rm, log: &localLog{}, closed: make(chan struct{})}
	s.lastRecv.Store(time.Now().UnixNano())

	if err := s.handshake(); err != nil {
		return err
	}
	info, backlog, err := s.openPort(ref)
	if err != nil {
		return err
	}

	// Received data goes to stdout unless suppressed, and always into the
	// capture file when one was asked for. Progress notes go to stderr so that
	// piping stdout into a boot-log parser stays clean.
	var echo io.Writer
	if !*quiet {
		echo = os.Stdout
	}
	if *outPath != "" {
		f, ferr := os.OpenFile(*outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if ferr != nil {
			return fmt.Errorf("open capture: %w", ferr)
		}
		defer f.Close()

		if echo != nil {
			echo = io.MultiWriter(os.Stdout, f)
		} else {
			echo = f
		}
	}

	col := newCollector(echo)
	if len(backlog) > 0 {
		col.feed(backlog)
	}

	readErr := make(chan error, 1)
	go func() { readErr <- s.collect(col) }()

	role := "writable"
	if !info.Writable {
		role = "read-only"
	}
	fmt.Fprintf(os.Stderr, "sercond: attached to %s @ %d baud (%s)\n", info.Port, info.Baud, role)
	if info.Log != "" {
		fmt.Fprintf(os.Stderr, "sercond: console log %s\n", info.Log)
	}

	for _, st := range steps {
		select {
		case err := <-readErr:
			return err
		default:
		}

		switch st.kind {
		case kindSend:
			if err := rm.wr.Data([]byte(st.text)); err != nil {
				return fmt.Errorf("%s: send: %w", st.origin, err)
			}
			fmt.Fprintf(os.Stderr, "sercond: sent %d bytes (%s)\n", len(st.text), st.origin)

		case kindSleep:
			time.Sleep(st.delay)

		case kindWait:
			start := time.Now()
			hit, werr := col.wait(st.re, *timeout)
			if werr != nil {
				return fmt.Errorf("%s: %w", st.origin, werr)
			}
			fmt.Fprintf(os.Stderr, "sercond: %s matched after %s, %d bytes past the previous marker\n",
				st.origin, time.Since(start).Truncate(time.Millisecond), len(hit))
		}
	}

	// Give the target a moment to finish the burst the last match triggered,
	// so the capture file is not truncated mid-line.
	time.Sleep(300 * time.Millisecond)
	return nil
}

// collect drains the protocol into the transcript until the link ends.
func (s *session) collect(col *collector) error {
	for {
		f, err := s.remote.rd.Next()
		if err != nil {
			return err
		}
		s.lastRecv.Store(time.Now().UnixNano())

		switch f.Type {
		case proto.TypeData:
			col.feed(f.Payload)
		case proto.TypeCtrl:
			var msg proto.Message
			if err := json.Unmarshal(f.Payload, &msg); err != nil {
				continue
			}
			switch msg.Op {
			case proto.OpError:
				return errors.New(msg.Text)
			case proto.OpNotice:
				fmt.Fprintf(os.Stderr, "sercond: %s\n", msg.Text)
			}
		}
	}
}

type stepKind int

const (
	kindSend stepKind = iota
	kindWait
	kindSleep
)

type step struct {
	kind   stepKind
	text   string
	re     *regexp.Regexp
	delay  time.Duration
	origin string
}

func buildSteps(scriptPath, send string, expects []string) ([]step, error) {
	if scriptPath != "" {
		return parseScript(scriptPath)
	}

	var steps []step
	if send != "" {
		text, err := unescape(send)
		if err != nil {
			return nil, fmt.Errorf("--send: %w", err)
		}
		steps = append(steps, step{kind: kindSend, text: text, origin: "--send"})
	}
	for i, e := range expects {
		re, err := regexp.Compile(e)
		if err != nil {
			return nil, fmt.Errorf("--expect #%d: %w", i+1, err)
		}
		steps = append(steps, step{kind: kindWait, re: re, origin: fmt.Sprintf("--expect #%d", i+1)})
	}
	if len(steps) == 0 {
		return nil, errors.New("nothing to do: pass --script, or --send/--expect")
	}
	return steps, nil
}

// parseScript reads a small line-oriented language:
//
//	send <text>      write text, escapes decoded
//	sendln <text>    write text followed by CRLF
//	wait <regex>     block until the pattern appears
//	sleep <dur>      pause, e.g. 2s
//
// It exists so that a "reboot and capture the boot log" workflow is a file
// rather than a shell pipeline.
func parseScript(path string) ([]step, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open script: %w", err)
	}
	defer f.Close()

	var steps []step
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}

		verb, arg := raw, ""
		if i := strings.IndexAny(raw, " \t"); i >= 0 {
			verb, arg = raw[:i], strings.TrimSpace(raw[i+1:])
		}
		arg = strings.Trim(arg, `"`)
		origin := fmt.Sprintf("%s:%d", path, lineNo)

		switch verb {
		case "send", "sendln":
			text, terr := unescape(arg)
			if terr != nil {
				return nil, fmt.Errorf("%s: %w", origin, terr)
			}
			if verb == "sendln" {
				text += "\r\n"
			}
			steps = append(steps, step{kind: kindSend, text: text, origin: origin})

		case "wait":
			re, rerr := regexp.Compile(arg)
			if rerr != nil {
				return nil, fmt.Errorf("%s: %w", origin, rerr)
			}
			steps = append(steps, step{kind: kindWait, re: re, origin: origin})

		case "sleep":
			d, derr := time.ParseDuration(arg)
			if derr != nil {
				return nil, fmt.Errorf("%s: %w", origin, derr)
			}
			steps = append(steps, step{kind: kindSleep, delay: d, origin: origin})

		default:
			return nil, fmt.Errorf("%s: unknown command %q", origin, verb)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read script: %w", err)
	}
	return steps, nil
}

// unescape decodes the backslash escapes accepted in send text, so a script can
// carry the CR, LF and NUL bytes a boot loader actually wants.
func unescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			return "", errors.New(`trailing backslash`)
		}
		switch s[i] {
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		case 'x':
			if i+2 >= len(s) {
				return "", errors.New(`truncated \x escape`)
			}
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf(`bad \x escape %q`, s[i+1:i+3])
			}
			b.WriteByte(byte(v))
			i += 2
		default:
			return "", fmt.Errorf(`unknown escape \%c`, s[i])
		}
	}
	return b.String(), nil
}

// collector accumulates console output for pattern matching.
type collector struct {
	mu       sync.Mutex
	buf      []byte
	consumed int
	echo     io.Writer
	signal   chan struct{}
}

func newCollector(echo io.Writer) *collector {
	return &collector{echo: echo, signal: make(chan struct{}, 1)}
}

func (c *collector) feed(p []byte) {
	c.mu.Lock()
	c.buf = append(c.buf, p...)
	if len(c.buf) > maxCollect {
		drop := len(c.buf) - maxCollect
		n := copy(c.buf, c.buf[drop:])
		c.buf = c.buf[:n]
		c.consumed -= drop
		if c.consumed < 0 {
			c.consumed = 0
		}
	}
	c.mu.Unlock()

	if c.echo != nil {
		_, _ = c.echo.Write(p)
	}
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

// wait scans for re starting where the previous wait left off, so that two
// waits for the same pattern match two distinct occurrences in order.
func (c *collector) wait(re *regexp.Regexp, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)

	for {
		c.mu.Lock()
		window := c.buf[c.consumed:]
		loc := re.FindIndex(window)
		if loc != nil {
			hit := append([]byte(nil), window[:loc[1]]...)
			c.consumed += loc[1]
			c.mu.Unlock()
			return hit, nil
		}
		c.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("%w: %s", errNoMatch, re)
		}
		// The channel is the fast path; the short poll is the backstop for a
		// signal that arrived while the lock was held.
		if remaining > 200*time.Millisecond {
			remaining = 200 * time.Millisecond
		}
		select {
		case <-c.signal:
		case <-time.After(remaining):
		}
	}
}
