//go:build !linux && !windows

package serialport

import (
	"errors"
	"os"
	"time"
)

// ErrUnsupported is returned on platforms whose serial handling has not been
// implemented. The capture daemon runs on a Linux or Windows jump host; the
// client never touches a serial device directly, so this exists only so that
// the tree cross-compiles cleanly for every target.
var ErrUnsupported = errors.New("serialport: not supported on this platform")

var ErrClosed = errors.New("serialport: port is closed")

var DefaultGlobs []string

type Port struct{}

func Open(path string, baud int, rtscts bool) (*Port, error) { return nil, ErrUnsupported }

func (p *Port) Path() string { return "" }

func (p *Port) Closed() bool { return true }

func (p *Port) Read(buf []byte, timeout time.Duration) (int, error) { return 0, ErrUnsupported }

func (p *Port) Write(b []byte) (int, error) { return 0, ErrUnsupported }

func (p *Port) Close() error { return nil }

func (p *Port) SendBreak() error { return ErrUnsupported }

type Device struct {
	Ref  string
	Link string
	Dev  string
	Desc string
}

func Discover(extra []string) ([]Device, error) { return nil, ErrUnsupported }

func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
