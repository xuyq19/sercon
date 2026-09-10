// Package portlog writes serial output to per-port, per-day files.
//
// The writer always runs, whether or not a client is attached. That is the
// entire reason the capture daemon exists as a separate process: a test machine
// that reboots at 3am should still have its console log waiting in the morning.
package portlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	stampLayout = "2006-01-02 15:04:05.000 "
	dayLayout   = "2006-01-02"
	flushAt     = 32 * 1024
)

// Writer appends timestamped serial output to <dir>/<port>/<day>.log.
type Writer struct {
	mu  sync.Mutex
	dir string
	// port is the reference as the operator wrote it, kept for the file header.
	// dir may differ: COM1 has to be escaped to COM1_ on Windows, and a header
	// claiming the port is called COM1_ would read like a bug.
	port  string
	path  string
	dev   string
	baud  int
	stamp bool

	day   string
	f     *os.File
	buf   []byte
	atBOL bool
	total int64
	err   error
}

// New creates the per-port log directory. dir is the log root, port is the
// stable port reference used as the subdirectory name.
func New(dir, port string, stamp bool) (*Writer, error) {
	sub := filepath.Join(dir, safeName(port))
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return nil, fmt.Errorf("portlog: create %s: %w", sub, err)
	}
	return &Writer{dir: sub, port: port, stamp: stamp, atBOL: true}, nil
}

// safeName turns a port reference into a name that is legal as a directory on
// this platform.
//
// The Windows reserved device names are the reason this exists. COM1 through
// COM9 are not just names — the filesystem resolves them to devices, so mkdir
// on a directory called COM1 fails outright ("The directory name is invalid"),
// and COM3 fails differently again. Since a Windows serial port is almost
// always called COMsomething in that range, naming the log directory after the
// port would quietly produce no logs at all.
//
// COM10 and above are not reserved, which is why this cannot be worked around
// by convention: the failure would appear and disappear depending on how many
// adapters a machine had ever seen.
func safeName(port string) string {
	n := sanitize(port)
	if !reservedDeviceName(n) {
		return n
	}
	// The escape has to land in the base name, before any extension: "COM1.txt"
	// is reserved too.
	if i := strings.IndexByte(n, '.'); i >= 0 {
		return n[:i] + "_" + n[i:]
	}
	return n + "_"
}

func reservedDeviceName(n string) bool {
	base := n
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.ToUpper(base)

	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	// Only COM1-LPT9 are reserved. COM10 and up are ordinary names.
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}

// Describe records the device identity, written as a header into each new file.
func (w *Writer) Describe(dev string, baud int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dev = dev
	w.baud = baud
}

// Write appends serial output. Lines are prefixed with a timestamp when the
// writer was created with stamping enabled.
//
// It always reports a full write. Losing console data silently would be worse
// than the log file reporting an error once and going quiet.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.total += int64(len(p))
	if w.err != nil {
		return len(p), w.err
	}

	sawNewline := false
	for _, b := range p {
		if w.atBOL {
			if w.stamp {
				w.buf = append(w.buf, time.Now().Format(stampLayout)...)
			}
			w.atBOL = false
		}
		w.buf = append(w.buf, b)
		if b == '\n' {
			w.atBOL = true
			sawNewline = true
		}
	}

	if sawNewline || len(w.buf) >= flushAt {
		if err := w.flushLocked(); err != nil {
			w.err = err
			return len(p), err
		}
	}
	return len(p), nil
}

// Flush pushes any partial line to disk. Called on a timer so a line that never
// ends — a boot loader waiting for input — is still visible.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if len(w.buf) == 0 {
		return nil
	}
	return w.flushLocked()
}

// Note writes an operator-visible marker line, used for session boundaries and
// device up/down transitions.
func (w *Writer) Note(format string, args ...any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if len(w.buf) > 0 {
		if err := w.flushLocked(); err != nil {
			return err
		}
	}
	msg := fmt.Sprintf(format, args...)
	w.buf = append(w.buf, fmt.Sprintf("[seriald %s] %s\n", time.Now().Format(stampLayout), msg)...)
	w.atBOL = true
	return w.flushLocked()
}

// Path returns the file currently being written, or "" if nothing has been
// written yet today.
func (w *Writer) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.path != "" {
		return w.path
	}
	return filepath.Join(w.dir, time.Now().Format(dayLayout)+".log")
}

// Total returns the number of bytes handed to the writer.
func (w *Writer) Total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

func (w *Writer) flushLocked() error {
	if err := w.ensureLocked(); err != nil {
		return err
	}
	if len(w.buf) == 0 {
		return nil
	}
	if _, err := w.f.Write(w.buf); err != nil {
		return fmt.Errorf("portlog: write %s: %w", w.path, err)
	}
	w.buf = w.buf[:0]
	return nil
}

func (w *Writer) ensureLocked() error {
	day := time.Now().Format(dayLayout)
	if w.f != nil && w.day == day {
		return nil
	}
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}

	path := filepath.Join(w.dir, day+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("portlog: open %s: %w", path, err)
	}

	// A brand-new file gets a header so an archived log is self-describing
	// months later, when nobody remembers which adapter was on which machine.
	if st, serr := f.Stat(); serr == nil && st.Size() == 0 {
		hdr := fmt.Sprintf("=== seriald | %s | port=%s dev=%s baud=%d ===\n",
			time.Now().Format(time.RFC3339), w.port, w.dev, w.baud)
		if _, werr := f.WriteString(hdr); werr != nil {
			f.Close()
			return fmt.Errorf("portlog: header %s: %w", path, werr)
		}
	}

	w.f = f
	w.path = path
	w.day = day
	return nil
}

// Close flushes and releases the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	if len(w.buf) > 0 && w.err == nil {
		err = w.flushLocked()
	}
	if w.f != nil {
		if cerr := w.f.Close(); cerr != nil && err == nil {
			err = cerr
		}
		w.f = nil
	}
	return err
}

// sanitize turns a port reference into a safe directory name. Windows COM names
// and Linux by-id names both survive this unchanged; anything exotic does not
// escape into the filesystem.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "port"
	}
	return b.String()
}
