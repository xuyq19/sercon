package serialport

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDevicePathAddsNamespace(t *testing.T) {
	// The \\.\ prefix is what keeps COM10 from being read as a filename, so it
	// is worth pinning down.
	cases := map[string]string{
		"COM1":         `\\.\COM1`,
		"COM10":        `\\.\COM10`,
		"com3":         `\\.\com3`,
		`\\.\COM4`:     `\\.\COM4`,
		`\\?\COM5`:     `\\?\COM5`,
		"/dev/ttyUSB0": "/dev/ttyUSB0",
		"notacom":      "notacom",
		"COM":          "COM",
	}
	for in, want := range cases {
		if got := devicePath(in); got != want {
			t.Errorf("devicePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsComName(t *testing.T) {
	yes := []string{"COM1", "COM10", "com3"}
	no := []string{"COM", "COMX", "COM1X", "", "/dev/ttyS0", "COM-1"}

	for _, s := range yes {
		if !isComName(s) {
			t.Errorf("isComName(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isComName(s) {
			t.Errorf("isComName(%q) = true, want false", s)
		}
	}
}

func TestComNumberOrdersNumerically(t *testing.T) {
	// COM2 must not sort after COM10; the registry hands ports back in an
	// arbitrary order, so the sort is what makes the listing readable.
	if comNumber("COM2") >= comNumber("COM10") {
		t.Fatal("COM2 must sort before COM10")
	}
	if comNumber("COM10") >= comNumber("COM100") {
		t.Fatal("COM10 must sort before COM100")
	}
}

func TestDiscoverDoesNotError(t *testing.T) {
	devs, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, d := range devs {
		if d.Ref == "" || d.Link == "" {
			t.Errorf("incomplete device: %+v", d)
		}
	}
	t.Logf("found %d port(s)", len(devs))
	for _, d := range devs {
		t.Logf("  %s  dev=%s  desc=%s  exists=%v", d.Ref, d.Dev, d.Desc, Exists(d.Dev))
	}
}

// TestOpenHardware exercises the real backend against a real adapter.
//
// It is gated behind an environment variable because opening a serial port is
// not a side-effect-free operation: it claims the device and asserts the modem
// control lines. On a machine with something important wired to COM1, a test
// suite that grabs ports on its own would be a nasty surprise.
//
//	SERCON_TEST_HARDWARE=1 go test ./internal/serialport/ -run Hardware -v
func TestOpenHardware(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hardware test in short mode")
	}
	if v := strings.TrimSpace(os.Getenv("SERCON_TEST_HARDWARE")); v != "1" {
		t.Skip("set SERCON_TEST_HARDWARE=1 to open a real port")
	}

	devs, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(devs) == 0 {
		t.Skip("no serial ports present")
	}

	// Prefer a named port so the choice is reproducible and visible in the
	// output rather than whatever happens to be first.
	dev := devs[0]
	if want := strings.TrimSpace(os.Getenv("SERCON_TEST_PORT")); want != "" {
		found := false
		for _, d := range devs {
			if strings.EqualFold(d.Ref, want) {
				dev, found = d, true
				break
			}
		}
		if !found {
			t.Fatalf("SERCON_TEST_PORT=%s not among the discovered ports", want)
		}
	}

	t.Logf("opening %s (%s)", dev.Ref, dev.Dev)

	p, err := Open(dev.Link, 115200, false)
	if err != nil {
		t.Fatalf("Open(%s): %v", dev.Link, err)
	}
	if p.Closed() {
		t.Fatal("a freshly opened port reports itself closed")
	}

	// Reading is safe; nothing is written to the line, so whatever is on the
	// other end sees no traffic.
	buf := make([]byte, 256)
	n, err := p.Read(buf, 300*time.Millisecond)
	if err != nil {
		t.Errorf("Read: %v", err)
	}
	t.Logf("read %d byte(s) with no error", n)

	// A second read must also come back cleanly, which is what proves the
	// overlapped state is being re-armed rather than reused half-consumed.
	if _, err := p.Read(buf, 200*time.Millisecond); err != nil {
		t.Errorf("second Read: %v", err)
	}

	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !p.Closed() {
		t.Error("port does not report itself closed after Close")
	}

	// Close is called by the shutdown path and by the worker, so it has to be
	// safe twice.
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	if _, err := p.Read(buf, time.Millisecond); !errors.Is(err, ErrClosed) {
		t.Errorf("Read after Close = %v, want ErrClosed", err)
	}
	if _, err := p.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
	if err := p.SendBreak(); !errors.Is(err, ErrClosed) {
		t.Errorf("SendBreak after Close = %v, want ErrClosed", err)
	}
}

// TestReopenAfterClose covers the reconnect path: the daemon reopens a device
// every time the target comes back, and a handle left behind would turn that
// into a permanent failure.
func TestReopenAfterClose(t *testing.T) {
	if v := strings.TrimSpace(os.Getenv("SERCON_TEST_HARDWARE")); v != "1" {
		t.Skip("set SERCON_TEST_HARDWARE=1 to open a real port")
	}

	devs, err := Discover(nil)
	if err != nil || len(devs) == 0 {
		t.Skip("no serial ports present")
	}
	link := devs[0].Link

	for i := 0; i < 3; i++ {
		p, err := Open(link, 115200, false)
		if err != nil {
			t.Fatalf("open %d of %s: %v", i+1, link, err)
		}
		if err := p.Close(); err != nil {
			t.Fatalf("close %d of %s: %v", i+1, link, err)
		}
	}
}
