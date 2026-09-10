// Package audit appends a JSON-lines record of everything an operator did to a
// serial port.
//
// The port logs record what the target machine said. The audit log records who
// attached to it, when, and how much they typed — the two together are what
// make it possible to answer "who rebooted this box at 02:14" after the fact.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event names. Kept as constants so the on-disk format stays greppable.
const (
	EventDaemonStart = "daemon_start"
	EventDaemonStop  = "daemon_stop"

	EventSessionOpen  = "session_open"
	EventSessionClose = "session_close"

	EventPortOpen      = "port_open"
	EventPortClose     = "port_close"
	EventPortOnline    = "port_online"
	EventPortOffline   = "port_offline"
	EventPortBreak     = "port_break"
	EventObserveDenied = "observe_denied"
	EventBusyDenied    = "busy_denied"
)

// Event is one audit record.
type Event struct {
	TS      string `json:"ts"`
	Event   string `json:"event"`
	User    string `json:"user,omitempty"`
	Client  string `json:"client,omitempty"`
	Remote  string `json:"remote,omitempty"`
	Port    string `json:"port,omitempty"`
	Dev     string `json:"dev,omitempty"`
	Session string `json:"session,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
}

// Logger writes audit records to <dir>/audit-<day>.jsonl.
//
// Logging is best effort. A failed audit write must never take down a serial
// session, so errors are recorded and surfaced rather than returned.
type Logger struct {
	mu  sync.Mutex
	dir string
	day string
	f   *os.File
	err error
}

// New prepares the audit directory.
func New(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit: create %s: %w", dir, err)
	}
	return &Logger{dir: dir}, nil
}

// Log appends one record.
func (l *Logger) Log(ev Event) {
	if ev.TS == "" {
		ev.TS = time.Now().Format(time.RFC3339Nano)
	}
	line, err := json.Marshal(ev)
	if err != nil {
		l.setErr(fmt.Errorf("audit: marshal: %w", err))
		return
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.rotateLocked(); err != nil {
		l.err = err
		return
	}
	if _, err := l.f.Write(line); err != nil {
		l.err = fmt.Errorf("audit: write: %w", err)
	}
}

// Err returns the last write failure, if any.
func (l *Logger) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Close releases the current file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func (l *Logger) setErr(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

func (l *Logger) rotateLocked() error {
	day := time.Now().Format("2006-01-02")
	if l.f != nil && l.day == day {
		return nil
	}
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	path := filepath.Join(l.dir, "audit-"+day+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", path, err)
	}
	l.f = f
	l.day = day
	return nil
}
