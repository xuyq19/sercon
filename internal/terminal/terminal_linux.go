//go:build linux

package terminal

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	ioctlGetTermios = 0x5401 // TCGETS
	ioctlSetTermios = 0x5402 // TCSETS
	ioctlGetWinsize = 0x5413 // TIOCGWINSZ

	flagCSize = 0x0030
	flagCS8   = 0x0030

	ccVMIN  = 6
	ccVTIME = 5

	nccs = 19
)

type termios struct {
	Iflag uint32
	Oflag uint32
	Cflag uint32
	Lflag uint32
	Line  uint8
	Cc    [nccs]uint8
}

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// State holds the terminal settings to put back on exit.
type State struct {
	fd    int
	saved termios
}

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	var t termios
	return ioctl(f.Fd(), ioctlGetTermios, unsafe.Pointer(&t)) == nil
}

// MakeRaw switches f into raw mode and returns the state needed to undo it.
func MakeRaw(f *os.File) (*State, error) {
	fd := f.Fd()

	var t termios
	if err := ioctl(fd, ioctlGetTermios, unsafe.Pointer(&t)); err != nil {
		return nil, ErrNotTerminal
	}
	saved := t

	// cfmakeraw spelled out. All four flag words go to zero rather than being
	// masked, because a console line needs the same treatment as the serial
	// device on the other end: no translation, no processing, no echo.
	t.Iflag = 0
	t.Oflag = 0
	t.Lflag = 0
	t.Cflag &^= flagCSize
	t.Cflag |= flagCS8
	t.Cc[ccVMIN] = 1
	t.Cc[ccVTIME] = 0

	if err := ioctl(fd, ioctlSetTermios, unsafe.Pointer(&t)); err != nil {
		return nil, fmt.Errorf("terminal: enable raw mode: %w", err)
	}
	return &State{fd: int(fd), saved: saved}, nil
}

// Restore puts the terminal back the way it was.
func (s *State) Restore() error {
	t := s.saved
	if err := ioctl(uintptr(s.fd), ioctlSetTermios, unsafe.Pointer(&t)); err != nil {
		return fmt.Errorf("terminal: restore: %w", err)
	}
	return nil
}

// Size reports the terminal window in columns and rows.
func Size(f *os.File) (int, int, error) {
	var ws winsize
	if err := ioctl(f.Fd(), ioctlGetWinsize, unsafe.Pointer(&ws)); err != nil {
		return 0, 0, fmt.Errorf("terminal: window size: %w", err)
	}
	return int(ws.Col), int(ws.Row), nil
}

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
