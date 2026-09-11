//go:build windows

package main

import "testing"

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 4); got != "abc…" {
		t.Fatalf("truncate = %q; want %q", got, "abc…")
	}
}
