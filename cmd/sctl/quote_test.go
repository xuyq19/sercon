package main

import "testing"

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		// Ordinary references need no quoting at all.
		"seriald":                                "seriald",
		"/usr/local/bin/seriald":                 "/usr/local/bin/seriald",
		"COM1":                                   "COM1",
		"usb-FTDI_FT232R_USB_UART_A50285BI-if00": "usb-FTDI_FT232R_USB_UART_A50285BI-if00",

		// The case this exists for: a leading tilde has to survive unquoted or
		// the remote shell looks for a file named "~/bin/seriald".
		"~/bin/seriald": "~/bin/seriald",

		// When the part after ~/ needs quoting, only that part gets quoted.
		// The tilde stays outside so expansion still happens.
		"~/my dir/seriald": "~/'my dir/seriald'",
		"~/x*y":            "~/'x*y'",

		// A tilde that is not a leading path component is just a character.
		"a~b":                "'a~b'",
		"/tmp/~/not-leading": "'/tmp/~/not-leading'",
		"~":                  "~",

		// Everything else gets quoted.
		"":            "''",
		"has space":   "'has space'",
		"a*b":         "'a*b'",
		"it's":        `'it'\''s'`,
		"$(rm -rf /)": "'$(rm -rf /)'",
		"`id`":        "'`id`'",
		"; rm -rf /":  "'; rm -rf /'",
	}

	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRemoteCommandShape pins down the exact string ssh receives, since that is
// the part a remote shell will parse and the part no compiler checks.
func TestRemoteCommandShape(t *testing.T) {
	o := &options{remoteBin: "seriald"}

	if got, want := o.remoteCommand("list", "--json"), "seriald list --json"; got != want {
		t.Errorf("remoteCommand = %q, want %q", got, want)
	}

	o = &options{remoteBin: "~/bin/seriald", remoteSock: "/run/user/1000/seriald/s.sock"}
	got := o.remoteCommand("session")
	want := "~/bin/seriald session --socket /run/user/1000/seriald/s.sock"
	if got != want {
		t.Errorf("remoteCommand = %q, want %q", got, want)
	}
}
