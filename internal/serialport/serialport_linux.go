//go:build linux

// Package serialport drives a Linux serial device with no third-party
// dependencies.
//
// It deliberately does not model the port as a net.Conn or io.ReadWriter. A
// serial console needs three things a plain byte stream cannot express: a
// readable timeout that does not consume data, a way to notice the device
// vanishing (USB replug), and a break signal. All three come straight from
// ioctl/epoll, so this file talks to the kernel directly.
package serialport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// ErrClosed is returned once the port has been closed.
var ErrClosed = errors.New("serialport: port is closed")

// Terminal ioctls and termios constants. Values are from asm-generic/termbits.h
// and are identical across Linux architectures.
const (
	tcgets = 0x5401
	tcsets = 0x5402
	tcflsh = 0x540b
	tcsbrk = 0x5409

	tciflsh = 0

	csize   = 0x0030
	cs8     = 0x0030
	cread   = 0x0080
	clocal  = 0x0800
	crtscts = 0x80000000

	vtime = 5
	vmin  = 6

	nccs = 19
)

// termios mirrors struct termios on Linux: four flag words, a line discipline
// byte, then NCCS control characters. The kernel layout is 36 bytes and Go's
// alignment rules produce exactly that.
type termios struct {
	Iflag uint32
	Oflag uint32
	Cflag uint32
	Lflag uint32
	Line  uint8
	Cc    [nccs]uint8
}

// baudRates maps a numeric rate to its termios Bxxx code.
var baudRates = map[int]uint32{
	50: 0x0001, 75: 0x0002, 110: 0x0003, 134: 0x0004, 150: 0x0005,
	200: 0x0006, 300: 0x0007, 600: 0x0008, 1200: 0x0009, 1800: 0x000a,
	2400: 0x000b, 4800: 0x000c, 9600: 0x000d, 19200: 0x000e, 38400: 0x000f,
	57600: 0x1001, 115200: 0x1002, 230400: 0x1003, 460800: 0x1004,
	500000: 0x1005, 576000: 0x1006, 921600: 0x1007, 1000000: 0x1008,
	1152000: 0x1009, 1500000: 0x100a, 2000000: 0x100b, 2500000: 0x100c,
	3000000: 0x100d, 3500000: 0x100e, 4000000: 0x100f,
}

// Port is an open serial device.
type Port struct {
	fd   int32
	epfd int32
	path string
	evs  []syscall.EpollEvent
}

// Open opens path at the requested baud, 8N1, raw, with no flow control unless
// rtscts is set.
func Open(path string, baud int, rtscts bool) (*Port, error) {
	code, ok := baudRates[baud]
	if !ok {
		return nil, fmt.Errorf("serialport: unsupported baud rate %d", baud)
	}

	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("serialport: open %s: %w", path, err)
	}
	if err := configure(fd, code, rtscts); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("serialport: configure %s: %w", path, err)
	}

	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("serialport: epoll_create1: %w", err)
	}
	ev := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, fd, &ev); err != nil {
		syscall.Close(epfd)
		syscall.Close(fd)
		return nil, fmt.Errorf("serialport: epoll_ctl: %w", err)
	}

	return &Port{
		fd:   int32(fd),
		epfd: int32(epfd),
		path: path,
		evs:  make([]syscall.EpollEvent, 4),
	}, nil
}

// configure puts the line into raw 8N1. Everything is cleared on purpose: a
// console line carries bare LF and arbitrary binary, and any input or output
// post-processing would corrupt it.
func configure(fd int, baud uint32, rtscts bool) error {
	var t termios
	if err := ioctl(fd, tcgets, unsafe.Pointer(&t)); err != nil {
		return fmt.Errorf("tcgets: %w", err)
	}

	t.Iflag = 0
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag = cread | clocal | cs8 | baud
	if rtscts {
		t.Cflag |= crtscts
	}
	t.Line = 0
	t.Cc[vmin] = 1
	t.Cc[vtime] = 0

	if err := ioctl(fd, tcsets, unsafe.Pointer(&t)); err != nil {
		return fmt.Errorf("tcsets: %w", err)
	}
	// Discard whatever arrived before we attached; a half-written line is noise.
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tcflsh, uintptr(tciflsh))
	return nil
}

// Path returns the device path this port was opened from.
func (p *Port) Path() string { return p.path }

// Closed reports whether Close has run.
func (p *Port) Closed() bool { return atomic.LoadInt32(&p.fd) < 0 }

// Read waits up to timeout for data. A zero-length return with a nil error
// means "nothing arrived", not EOF — a serial line has no end of stream.
func (p *Port) Read(buf []byte, timeout time.Duration) (int, error) {
	epfd := int(atomic.LoadInt32(&p.epfd))
	fd := int(atomic.LoadInt32(&p.fd))
	if epfd < 0 || fd < 0 {
		return 0, ErrClosed
	}

	ms := int(timeout / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	n, err := syscall.EpollWait(epfd, p.evs, ms)
	if err != nil {
		if err == syscall.EINTR {
			return 0, nil
		}
		return 0, fmt.Errorf("serialport: epoll_wait %s: %w", p.path, err)
	}
	if n == 0 {
		return 0, nil
	}

	for i := 0; i < n; i++ {
		if p.evs[i].Events&(syscall.EPOLLERR|syscall.EPOLLHUP|syscall.EPOLLRDHUP) != 0 {
			// Drain first. A device on its way out usually still has a final
			// burst queued, and that burst is exactly what you want in the log.
			if m, rerr := syscall.Read(fd, buf); m > 0 && (rerr == nil || rerr == syscall.EINTR) {
				return m, nil
			}
			return 0, fmt.Errorf("serialport: %s: device gone", p.path)
		}
	}

	m, err := syscall.Read(fd, buf)
	if err != nil {
		if err == syscall.EAGAIN || err == syscall.EINTR {
			return 0, nil
		}
		return 0, fmt.Errorf("serialport: read %s: %w", p.path, err)
	}
	return m, nil
}

// Write sends all of b, blocking until the kernel accepts it. A serial line is
// slow and flow-controlled, so partial writes are normal.
func (p *Port) Write(b []byte) (int, error) {
	fd := int(atomic.LoadInt32(&p.fd))
	if fd < 0 {
		return 0, ErrClosed
	}

	deadline := time.Now().Add(5 * time.Second)
	total := 0
	for total < len(b) {
		n, err := syscall.Write(fd, b[total:])
		if n > 0 {
			total += n
		}
		switch {
		case err == nil && n > 0:
			continue
		case err == syscall.EINTR:
			continue
		case err == syscall.EAGAIN || (err == nil && n == 0):
			if time.Now().After(deadline) {
				return total, fmt.Errorf("serialport: write %s: stalled after %d/%d bytes", p.path, total, len(b))
			}
			time.Sleep(2 * time.Millisecond)
		case err != nil:
			return total, fmt.Errorf("serialport: write %s: %w", p.path, err)
		}
	}
	return total, nil
}

// SendBreak raises a break condition on the line. This is how you get a boot
// loader's attention or drop a target into a debugger over a serial console.
func (p *Port) SendBreak() error {
	fd := int(atomic.LoadInt32(&p.fd))
	if fd < 0 {
		return ErrClosed
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(tcsbrk), 0)
	if errno != 0 {
		return fmt.Errorf("serialport: tcsbrk %s: %w", p.path, errno)
	}
	return nil
}

// Close releases the device and the epoll instance. It is safe to call more
// than once and safe to call while a Read is in flight; the pending Read fails
// with ErrClosed.
func (p *Port) Close() error {
	ep := atomic.SwapInt32(&p.epfd, -1)
	fd := atomic.SwapInt32(&p.fd, -1)
	if ep >= 0 {
		syscall.Close(int(ep))
	}
	if fd >= 0 {
		syscall.Close(int(fd))
	}
	return nil
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// Device is one discovered serial port.
type Device struct {
	// Ref is the stable reference. For /dev/serial/by-id entries this is the
	// by-id basename, which survives replug and ttyUSB renumbering.
	Ref string
	// Link is the path to open. Opening Ref itself re-resolves the symlink on
	// every attempt, which is what makes reconnect work after a replug.
	Link string
	// Dev is the resolved device node, for display.
	Dev string
	// Desc is the human label.
	Desc string
}

// DefaultGlobs are scanned in addition to /dev/serial/by-id. by-id is
// preferred, so any device it already covers is skipped here.
var DefaultGlobs = []string{"/dev/ttyUSB*", "/dev/ttyACM*"}

// Discover enumerates candidate serial ports, sorted by reference.
func Discover(extra []string) ([]Device, error) {
	var out []Device
	seen := make(map[string]bool)

	links, err := filepath.Glob("/dev/serial/by-id/*")
	if err != nil {
		return nil, fmt.Errorf("serialport: scan by-id: %w", err)
	}
	for _, link := range links {
		dev, err := filepath.EvalSymlinks(link)
		if err != nil {
			continue
		}
		if seen[dev] {
			continue
		}
		seen[dev] = true
		ref := filepath.Base(link)
		out = append(out, Device{Ref: ref, Link: link, Dev: dev, Desc: ref})
	}

	globs := make([]string, 0, len(DefaultGlobs)+len(extra))
	globs = append(globs, DefaultGlobs...)
	globs = append(globs, extra...)
	for _, g := range globs {
		matches, err := filepath.Glob(g)
		if err != nil {
			continue
		}
		for _, m := range matches {
			dev, err := filepath.EvalSymlinks(m)
			if err != nil {
				dev = m
			}
			if seen[dev] {
				continue
			}
			seen[dev] = true
			out = append(out, Device{Ref: filepath.Base(m), Link: m, Dev: dev, Desc: filepath.Base(m)})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// Exists reports whether the device node is present at all.
func Exists(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
