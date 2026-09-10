package main

import "testing"

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		// Ordinary references need no quoting at all.
		"sercond":                                "sercond",
		"/usr/local/bin/sercond":                 "/usr/local/bin/sercond",
		"COM1":                                   "COM1",
		"usb-FTDI_FT232R_USB_UART_A50285BI-if00": "usb-FTDI_FT232R_USB_UART_A50285BI-if00",

		// The case this exists for: a leading tilde has to survive unquoted or
		// the remote shell looks for a file named "~/bin/sercond".
		"~/bin/sercond": "~/bin/sercond",

		// When the part after ~/ needs quoting, only that part gets quoted.
		// The tilde stays outside so expansion still happens.
		"~/my dir/sercond": "~/'my dir/sercond'",
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
	o := &options{remoteBin: "sercond"}

	if got, want := o.remoteCommand("list", "--json"), "sercond list --json"; got != want {
		t.Errorf("remoteCommand = %q, want %q", got, want)
	}

	o = &options{remoteBin: "~/bin/sercond", remoteSock: "/run/user/1000/sercon/s.sock"}
	got := o.remoteCommand("session")
	want := "~/bin/sercond session --socket /run/user/1000/sercon/s.sock"
	if got != want {
		t.Errorf("remoteCommand = %q, want %q", got, want)
	}
}
