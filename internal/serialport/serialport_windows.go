//go:build windows

package serialport

// Windows serial backend, built on the Win32 communications API.
//
// The structure deliberately mirrors the Linux implementation rather than
// trying to look like an io.ReadWriter. Three things have to be expressible: a
// read that blocks until the line says something, a way to notice the adapter
// disappearing, and a break signal. On Windows those map to WaitCommEvent,
// ClearCommError and SetCommBreak.
//
// All I/O is overlapped. That is not for performance — it is the only way to
// unblock a pending read from another goroutine, which is what Close has to do.
// A synchronous ReadFile on a comm handle cannot be interrupted safely.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var ErrClosed = errors.New("serialport: port is closed")

const (
	genericRead  = 0x80000000
	genericWrite = 0x40000000
	openExisting = 3

	// Required so that reads can be cancelled. Without it a read blocks a
	// thread until data arrives and Close would have to kill the process.
	fileFlagOverlapped = 0x40000000

	errorIOPending        = 997
	errorOperationAborted = 995
	errorNotFound         = 1168

	waitTimeout = 258

	// Comm events worth waking up for. RXCHAR is the one that matters; ERR and
	// BREAK are how a console tells you the line went wrong.
	evRxChar = 0x0001
	evBreak  = 0x0040
	evErr    = 0x0080

	purgeTxAbort = 0x0001
	purgeRxAbort = 0x0002
	purgeTxClear = 0x0004
	purgeRxClear = 0x0008

	keyRead = 0x20019
	regSZ   = 1

	// Predefined registry handles are sign-extended 32-bit constants. Building
	// the value with NOT keeps it correct on both 32- and 64-bit targets.
	hkeyLocalMachine = ^uintptr(0x7FFFFFFD)

	errorNoMoreItems = 259

	baudUnset = 0
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	pCreateEventW        = kernel32.NewProc("CreateEventW")
	pResetEvent          = kernel32.NewProc("ResetEvent")
	pGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	pSetupComm           = kernel32.NewProc("SetupComm")
	pGetCommState        = kernel32.NewProc("GetCommState")
	pSetCommState        = kernel32.NewProc("SetCommState")
	pSetCommTimeouts     = kernel32.NewProc("SetCommTimeouts")
	pSetCommMask         = kernel32.NewProc("SetCommMask")
	pWaitCommEvent       = kernel32.NewProc("WaitCommEvent")
	pClearCommError      = kernel32.NewProc("ClearCommError")
	pPurgeComm           = kernel32.NewProc("PurgeComm")
	pSetCommBreak        = kernel32.NewProc("SetCommBreak")
	pClearCommBreak      = kernel32.NewProc("ClearCommBreak")

	pRegOpenKeyExW = advapi32.NewProc("RegOpenKeyExW")
	pRegEnumValueW = advapi32.NewProc("RegEnumValueW")
	pRegCloseKey   = advapi32.NewProc("RegCloseKey")
)

// dcb mirrors the Win32 DCB. The bitfields are collapsed into one word; every
// one of them is set through an explicit shift, which is less error-prone than
// hoping a struct of single-bit fields lays out the way the compiler would.
type dcb struct {
	Length    uint32
	BaudRate  uint32
	Flags     uint32
	Reserved  uint16
	XonLim    uint16
	XoffLim   uint16
	ByteSize  uint8
	Parity    uint8
	StopBits  uint8
	XonChar   uint8
	XoffChar  uint8
	ErrorChar uint8
	EofChar   uint8
	EvtChar   uint8
	Reserved1 uint16
}

// Flag bit positions inside dcb.Flags.
const (
	bitBinary        = 0
	bitOutxCtsFlow   = 2
	bitDtrControl    = 4  // two bits
	bitRtsControl    = 12 // two bits
	dtrControlEnable = 1
	rtsControlEnable = 1
)

// comStat mirrors COMSTAT.
type comStat struct {
	Flags    uint32
	CbInQue  uint32
	CbOutQue uint32
}

// commTimeouts mirrors COMMTIMEOUTS. All zero means "wait for the full
// requested count"; every read here asks for exactly the bytes already queued,
// so that completes at once.
type commTimeouts struct {
	ReadIntervalTimeout         uint32
	ReadTotalTimeoutMultiplier  uint32
	ReadTotalTimeoutConstant    uint32
	WriteTotalTimeoutMultiplier uint32
	WriteTotalTimeoutConstant   uint32
}

// Port is an open serial device.
type Port struct {
	path   string
	handle syscall.Handle

	evRead  syscall.Handle
	evWrite syscall.Handle

	// ioMu serializes I/O and guards the handle lifetime. Overlapped structs are
	// reused between calls, so two concurrent operations would corrupt each
	// other's completion state.
	ioMu    sync.Mutex
	ovRead  syscall.Overlapped
	ovWrite syscall.Overlapped
	mask    uint32

	closed    atomic.Bool
	closeOnce sync.Once
}

// Open opens path at the requested rate, 8N1, no flow control unless rtscts.
//
// baud is handed to the driver unchanged: unlike termios, the Win32 DCB takes
// an arbitrary rate rather than a code from a fixed table, so there is nothing
// to validate against.
func Open(path string, baud int, rtscts bool) (*Port, error) {
	if baud <= 0 {
		return nil, fmt.Errorf("serialport: invalid baud rate %d", baud)
	}
	full := devicePath(path)

	name, err := syscall.UTF16PtrFromString(full)
	if err != nil {
		return nil, fmt.Errorf("serialport: %w", err)
	}

	handle, err := syscall.CreateFile(name,
		genericRead|genericWrite, 0, nil, openExisting, fileFlagOverlapped, 0)
	if err != nil {
		return nil, fmt.Errorf("serialport: open %s: %w", full, err)
	}

	p := &Port{path: path, handle: handle}

	if p.evRead, err = createEvent(); err != nil {
		syscall.CloseHandle(handle)
		return nil, fmt.Errorf("serialport: create read event: %w", err)
	}
	if p.evWrite, err = createEvent(); err != nil {
		syscall.CloseHandle(handle)
		syscall.CloseHandle(p.evRead)
		return nil, fmt.Errorf("serialport: create write event: %w", err)
	}

	if err := p.configure(baud, rtscts); err != nil {
		p.Close()
		return nil, err
	}

	// Buffer sizes are advisory; the driver may pick its own. Failure here is
	// not fatal, so it is not worth aborting the open over.
	_, _, _ = pSetupComm.Call(uintptr(handle), 65536, 65536)

	if err := p.setMask(evRxChar | evErr | evBreak); err != nil {
		p.Close()
		return nil, err
	}

	// Drop anything buffered before we attached. A half-received line is noise,
	// and on a console that has been up for weeks it can be a lot of noise.
	_, _, _ = pPurgeComm.Call(uintptr(handle), purgeRxClear|purgeTxClear|purgeRxAbort|purgeTxAbort)

	return p, nil
}

func (p *Port) configure(baud int, rtscts bool) error {
	var d dcb
	d.Length = uint32(unsafe.Sizeof(d))

	if err := p.getCommState(&d); err != nil {
		return err
	}

	// fBinary must be set; the rest is what a console line needs. DTR and RTS
	// are asserted because plenty of adapters will not pass data without them,
	// and source their power from the signal lines.
	var flags uint32
	flags |= 1 << bitBinary
	if rtscts {
		flags |= 1 << bitOutxCtsFlow
	}
	flags |= dtrControlEnable << bitDtrControl
	flags |= rtsControlEnable << bitRtsControl

	d.BaudRate = uint32(baud)
	d.Flags = flags
	d.ByteSize = 8
	d.Parity = 0   // NOPARITY
	d.StopBits = 0 // ONESTOPBIT

	if err := p.setCommState(&d); err != nil {
		return err
	}

	var to commTimeouts
	r, _, errno := pSetCommTimeouts.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&to)))
	if r == 0 {
		return fmt.Errorf("serialport: SetCommTimeouts %s: %w", p.path, errno)
	}
	return nil
}

func (p *Port) getCommState(d *dcb) error {
	r, _, errno := pGetCommState.Call(uintptr(p.handle), uintptr(unsafe.Pointer(d)))
	if r == 0 {
		return fmt.Errorf("serialport: GetCommState %s: %w", p.path, errno)
	}
	return nil
}

func (p *Port) setCommState(d *dcb) error {
	r, _, errno := pSetCommState.Call(uintptr(p.handle), uintptr(unsafe.Pointer(d)))
	if r == 0 {
		return fmt.Errorf("serialport: SetCommState %s: %w", p.path, errno)
	}
	return nil
}

func (p *Port) setMask(mask uint32) error {
	r, _, errno := pSetCommMask.Call(uintptr(p.handle), uintptr(mask))
	if r == 0 {
		return fmt.Errorf("serialport: SetCommMask %s: %w", p.path, errno)
	}
	return nil
}

// Path returns the device path this port was opened from.
func (p *Port) Path() string { return p.path }

// Closed reports whether Close has run.
func (p *Port) Closed() bool { return p.closed.Load() }

// Read waits up to timeout for data. A zero-length return with a nil error means
// "nothing arrived", not EOF — a serial line has no end of stream.
//
// Unlike POSIX, Windows has no read that simply times out: an overlapped
// operation either completes or stays pending. So a timeout is implemented by
// cancelling the pending operation and consuming its aborted completion before
// the overlapped struct may be reused. Skipping that consumption would leave a
// live operation pointing at a struct the next call is about to overwrite.
func (p *Port) Read(buf []byte, timeout time.Duration) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	p.ioMu.Lock()
	defer p.ioMu.Unlock()
	if p.closed.Load() {
		return 0, ErrClosed
	}

	// Whatever is already queued goes out before blocking.
	if n, err := p.drain(buf); n > 0 || err != nil {
		return n, err
	}

	p.resetRead()
	p.mask = 0
	err := p.waitCommEvent()

	// A synchronous success means the event had already fired.
	if err == nil {
		if p.closed.Load() {
			return 0, ErrClosed
		}
		return p.drain(buf)
	}
	if !errors.Is(err, syscall.Errno(errorIOPending)) {
		return 0, err
	}

	ms := uint32(timeout / time.Millisecond)
	if ms == 0 {
		ms = 1
	}

	ev, werr := syscall.WaitForSingleObject(p.evRead, ms)
	if werr != nil {
		return 0, fmt.Errorf("serialport: wait %s: %w", p.path, werr)
	}

	if ev == waitTimeout {
		// Close may also have cancelled this, which is why closed is checked
		// before the completion is interpreted.
		_ = syscall.CancelIoEx(p.handle, &p.ovRead)

		var done uint32
		gerr := p.overlappedResult(&p.ovRead, &done)
		if p.closed.Load() {
			return 0, ErrClosed
		}
		switch {
		case gerr == nil:
			// It completed in the window between the timer firing and the
			// cancel. There may be data after all.
			return p.drain(buf)
		case errors.Is(gerr, syscall.Errno(errorOperationAborted)),
			errors.Is(gerr, syscall.Errno(errorNotFound)):
			return 0, nil
		default:
			return 0, fmt.Errorf("serialport: WaitCommEvent %s: %w", p.path, gerr)
		}
	}

	var done uint32
	if gerr := p.overlappedResult(&p.ovRead, &done); gerr != nil {
		if p.closed.Load() {
			return 0, ErrClosed
		}
		return 0, fmt.Errorf("serialport: WaitCommEvent %s: %w", p.path, gerr)
	}

	if p.closed.Load() {
		return 0, ErrClosed
	}
	return p.drain(buf)
}

func (p *Port) waitCommEvent() error {
	r, _, errno := pWaitCommEvent.Call(
		uintptr(p.handle),
		uintptr(unsafe.Pointer(&p.mask)),
		uintptr(unsafe.Pointer(&p.ovRead)),
	)
	if r != 0 {
		return nil
	}
	return errno
}

func (p *Port) resetRead() {
	p.ovRead = syscall.Overlapped{HEvent: p.evRead}
	_, _, _ = pResetEvent.Call(uintptr(p.evRead))
}

func (p *Port) resetWrite() {
	p.ovWrite = syscall.Overlapped{HEvent: p.evWrite}
	_, _, _ = pResetEvent.Call(uintptr(p.evWrite))
}

func (p *Port) overlappedResult(ov *syscall.Overlapped, done *uint32) error {
	r, _, errno := pGetOverlappedResult.Call(
		uintptr(p.handle),
		uintptr(unsafe.Pointer(ov)),
		uintptr(unsafe.Pointer(done)),
		0, // do not wait; the event has already been signalled
	)
	if r == 0 {
		return errno
	}
	return nil
}

// drain reads whatever the driver has queued, up to the buffer size.
func (p *Port) drain(buf []byte) (int, error) {
	var st comStat
	var errs uint32
	r, _, errno := pClearCommError.Call(
		uintptr(p.handle),
		uintptr(unsafe.Pointer(&errs)),
		uintptr(unsafe.Pointer(&st)),
	)
	if r == 0 {
		// A yanked USB adapter surfaces here first. Reporting it as the port's
		// error is what drives the daemon's reconnect loop.
		return 0, fmt.Errorf("serialport: ClearCommError %s: %w", p.path, errno)
	}
	if st.CbInQue == 0 {
		return 0, nil
	}

	want := int(st.CbInQue)
	if want > len(buf) {
		want = len(buf)
	}

	p.resetRead()
	var read uint32
	err := syscall.ReadFile(p.handle, buf[:want], &read, &p.ovRead)
	if err == nil {
		return int(read), nil
	}
	if !errors.Is(err, syscall.Errno(errorIOPending)) {
		return 0, fmt.Errorf("serialport: ReadFile %s: %w", p.path, err)
	}

	// The count came from the driver, so this normally completes immediately.
	// The pending path exists for the race where the driver drained the buffer
	// between ClearCommError and ReadFile, and it is bounded so that a driver
	// promising bytes it never delivers cannot wedge the reader forever.
	const readTimeoutMS = 2000
	ev, werr := syscall.WaitForSingleObject(p.evRead, readTimeoutMS)
	if werr != nil {
		return 0, fmt.Errorf("serialport: wait %s: %w", p.path, werr)
	}
	if ev == waitTimeout {
		_ = syscall.CancelIoEx(p.handle, &p.ovRead)
		var done uint32
		_ = p.overlappedResult(&p.ovRead, &done)
		if p.closed.Load() {
			return 0, ErrClosed
		}
		return 0, nil
	}
	if gerr := p.overlappedResult(&p.ovRead, &read); gerr != nil {
		if p.closed.Load() {
			return 0, ErrClosed
		}
		return 0, fmt.Errorf("serialport: ReadFile %s: %w", p.path, gerr)
	}
	return int(read), nil
}

// Write sends b, waiting a bounded time for the driver to accept it.
func (p *Port) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}

	p.ioMu.Lock()
	defer p.ioMu.Unlock()
	if p.closed.Load() {
		return 0, ErrClosed
	}

	p.resetWrite()
	var written uint32
	err := syscall.WriteFile(p.handle, b, &written, &p.ovWrite)
	if err != nil && !errors.Is(err, syscall.Errno(errorIOPending)) {
		return int(written), fmt.Errorf("serialport: WriteFile %s: %w", p.path, err)
	}

	if errors.Is(err, syscall.Errno(errorIOPending)) {
		const writeTimeoutMS = 5000
		ev, werr := syscall.WaitForSingleObject(p.evWrite, writeTimeoutMS)
		if werr != nil {
			return 0, fmt.Errorf("serialport: wait %s: %w", p.path, werr)
		}
		if ev == waitTimeout {
			// Nothing will complete this on its own, so clear it rather than
			// leave an operation pending against a struct we are about to reuse.
			_ = syscall.CancelIoEx(p.handle, &p.ovWrite)
			return 0, fmt.Errorf("serialport: write %s: timed out after %dms", p.path, writeTimeoutMS)
		}
		if gerr := p.overlappedResult(&p.ovWrite, &written); gerr != nil {
			return int(written), fmt.Errorf("serialport: write %s: %w", p.path, gerr)
		}
	}
	return int(written), nil
}

// SendBreak holds a break condition on the line.
//
// Unlike POSIX, where the duration is chosen by the kernel, Win32 keeps the
// break asserted until it is cleared, so the timing is ours. 250ms is in the
// range a boot loader or a serial console expects.
func (p *Port) SendBreak() error {
	p.ioMu.Lock()
	defer p.ioMu.Unlock()
	if p.closed.Load() {
		return ErrClosed
	}

	if r, _, errno := pSetCommBreak.Call(uintptr(p.handle)); r == 0 {
		return fmt.Errorf("serialport: SetCommBreak %s: %w", p.path, errno)
	}
	time.Sleep(250 * time.Millisecond)
	if r, _, errno := pClearCommBreak.Call(uintptr(p.handle)); r == 0 {
		return fmt.Errorf("serialport: ClearCommBreak %s: %w", p.path, errno)
	}
	return nil
}

// Close releases the device and its events.
//
// Order matters. The port is marked closed and the pending overlapped
// operations are cancelled first, without holding the I/O lock — that is what
// wakes a Read parked in WaitForSingleObject. Only then does Close take the
// lock, which now returns as soon as the woken reader has let go. Closing the
// handles while an operation was still pending would leave the driver writing
// into a freed event object.
func (p *Port) Close() error {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		if p.handle != 0 {
			_ = syscall.CancelIoEx(p.handle, &p.ovRead)
			_ = syscall.CancelIoEx(p.handle, &p.ovWrite)
		}

		p.ioMu.Lock()
		defer p.ioMu.Unlock()

		if p.handle != 0 {
			syscall.CloseHandle(p.handle)
			p.handle = 0
		}
		if p.evRead != 0 {
			syscall.CloseHandle(p.evRead)
			p.evRead = 0
		}
		if p.evWrite != 0 {
			syscall.CloseHandle(p.evWrite)
			p.evWrite = 0
		}
	})
	return nil
}

func createEvent() (syscall.Handle, error) {
	// Auto-reset: the driver signals it once per completed operation, and the
	// wait in Read is followed immediately by a fresh reset.
	r, _, errno := pCreateEventW.Call(0, 0, 0, 0)
	if r == 0 {
		return 0, errno
	}
	return syscall.Handle(r), nil
}

// Device is one discovered serial port.
type Device struct {
	// Ref is the stable reference. Windows has nothing like the Linux by-id
	// links, so this is the COM name.
	Ref string
	// Link is what Open receives.
	Link string
	// Dev is what gets displayed, identical to Ref here.
	Dev string
	// Desc distinguishes a USB adapter from a motherboard port using the
	// registry's device path. Without SetupAPI there is no friendlier name to
	// be had.
	Desc string
}

var DefaultGlobs []string

// Discover enumerates serial ports from the registry.
//
// HKLM\HARDWARE\DEVICEMAP\SERIALCOMM is the one place Windows lists serial
// ports by name without requiring SetupAPI or an open attempt. The value names
// are device paths and the values are the COM names.
func Discover(extra []string) ([]Device, error) {
	entries, err := registryPorts()
	if err != nil {
		return nil, err
	}

	devs := make([]Device, 0, len(entries))
	for _, e := range entries {
		devs = append(devs, Device{
			Ref:  e.name,
			Link: e.name,
			Dev:  e.name,
			Desc: e.desc,
		})
	}

	// The registry order is arbitrary; COM2 must not sort after COM10.
	sort.Slice(devs, func(i, j int) bool {
		ni, nj := comNumber(devs[i].Ref), comNumber(devs[j].Ref)
		if ni != nj {
			return ni < nj
		}
		return devs[i].Ref < devs[j].Ref
	})
	return devs, nil
}

type regPort struct {
	name string
	desc string
}

func registryPorts() ([]regPort, error) {
	sub, err := syscall.UTF16PtrFromString(`HARDWARE\DEVICEMAP\SERIALCOMM`)
	if err != nil {
		return nil, fmt.Errorf("serialport: %w", err)
	}

	var key uintptr
	if err := regOpenKeyEx(hkeyLocalMachine, sub, keyRead, &key); err != nil {
		return nil, fmt.Errorf("serialport: open SERIALCOMM: %w", err)
	}
	defer pRegCloseKey.Call(key)

	var out []regPort
	for i := uint32(0); ; i++ {
		var (
			nameBuf = make([]uint16, 512)
			nameLen = uint32(len(nameBuf))
			dataBuf = make([]uint16, 512)
			dataLen = uint32(len(dataBuf) * 2)
			typ     uint32
		)

		err := regEnumValue(key, i,
			&nameBuf[0], &nameLen, &typ,
			unsafe.Pointer(&dataBuf[0]), &dataLen)
		if errors.Is(err, syscall.Errno(errorNoMoreItems)) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("serialport: enumerate SERIALCOMM: %w", err)
		}
		if typ != regSZ || dataLen < 2 {
			continue
		}

		name := syscall.UTF16ToString(dataBuf[:dataLen/2])
		devicePath := syscall.UTF16ToString(nameBuf[:nameLen])
		if !isComName(name) {
			continue
		}
		out = append(out, regPort{name: name, desc: shortDeviceName(devicePath)})
	}
	return out, nil
}

// regOpenKeyEx and regEnumValue wrap the registry calls. These return their
// status code directly rather than through GetLastError, which is why the
// result is inspected instead of the errno.
func regOpenKeyEx(root uintptr, subkey *uint16, access uint32, result *uintptr) error {
	r, _, _ := pRegOpenKeyExW.Call(
		root,
		uintptr(unsafe.Pointer(subkey)),
		0,
		uintptr(access),
		uintptr(unsafe.Pointer(result)),
	)
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func regEnumValue(key uintptr, index uint32, name *uint16, nameLen *uint32, typ *uint32, data unsafe.Pointer, dataLen *uint32) error {
	r, _, _ := pRegEnumValueW.Call(
		key,
		uintptr(index),
		uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(nameLen)),
		0,
		uintptr(unsafe.Pointer(typ)),
		uintptr(data),
		uintptr(unsafe.Pointer(dataLen)),
	)
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// Exists reports whether the port is currently listed by the driver.
//
// This is answered from the registry rather than by opening the device: an open
// attempt would claim the port, and Discover runs every few seconds.
func Exists(path string) bool {
	entries, err := registryPorts()
	if err != nil {
		return false
	}
	want := strings.ToUpper(path)
	for _, e := range entries {
		if strings.ToUpper(e.name) == want {
			return true
		}
	}
	return false
}

// devicePath, isComName and comNumber live in comname.go — they are pure string
// handling, so they stay testable on every platform.

func shortDeviceName(p string) string {
	return strings.TrimPrefix(p, `\Device\`)
}
