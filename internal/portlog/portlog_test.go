package portlog

import "testing"

func TestSafeNameEscapesReservedDevices(t *testing.T) {
	cases := map[string]string{
		// The whole reason this function exists: a Windows serial port is
		// almost always named in this range, and mkdir on it fails.
		"COM1": "COM1_",
		"COM3": "COM3_",
		"COM9": "COM9_",
		"com3": "com3_",
		"LPT1": "LPT1_",
		"CON":  "CON_",
		"PRN":  "PRN_",
		"NUL":  "NUL_",

		// COM10 and up are ordinary names, and so is anything that merely
		// looks similar.
		"COM10":  "COM10",
		"COM256": "COM256",
		"COMA":   "COMA",
		"COM":    "COM",
		"NULL":   "NULL",
		"COM0":   "COM0",

		// The escape has to land in the base name: "COM1.txt" is reserved too.
		"COM1.txt": "COM1_.txt",

		// Linux by-id references must come through untouched.
		"usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0": "usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0",
		"/dev/ttyUSB0": "_dev_ttyUSB0",
	}

	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReservedDeviceName(t *testing.T) {
	yes := []string{"COM1", "com9", "LPT3", "CON", "nul", "AUX", "COM1.log"}
	no := []string{"COM0", "COM10", "LPT0", "COM", "CONSOLE", "-COM1", "COM1x", ""}

	for _, s := range yes {
		if !reservedDeviceName(s) {
			t.Errorf("reservedDeviceName(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if reservedDeviceName(s) {
			t.Errorf("reservedDeviceName(%q) = true, want false", s)
		}
	}
}

// TestNewCreatesDirectoryUnderPortName is the regression guard for the silent
// failure: on Windows the directory has to be creatable for a port called COM1.
func TestNewCreatesDirectoryUnderPortName(t *testing.T) {
	root := t.TempDir()

	for _, ref := range []string{"COM1", "COM3", "usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0"} {
		w, err := New(root, ref, true)
		if err != nil {
			t.Fatalf("New(%q): %v", ref, err)
		}
		if _, err := w.Write([]byte("hello\n")); err != nil {
			t.Fatalf("Write for %q: %v", ref, err)
		}
		if w.Path() == "" {
			t.Fatalf("no log path for %q", ref)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close for %q: %v", ref, err)
		}
	}
}
