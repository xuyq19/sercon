// Tests for the version stamp.
//
// The values are injected at link time, so what is tested here is the shaping:
// how Version and Commit combine into the string a binary reports. That string
// ends up in --version output, in the GUI's footer, and in the release
// workflow's check, so getting it wrong is visible to users rather than
// internal.
package version

import (
	"strings"
	"testing"
)

// setVars installs a Version/Commit pair and restores the previous one.
func setVars(t *testing.T, v, c string) {
	t.Helper()
	oldV, oldC := Version, Commit
	t.Cleanup(func() { Version, Commit = oldV, oldC })
	Version, Commit = v, c
}

func TestFullBeforeFirstTagDoesNotRepeatTheHash(t *testing.T) {
	// git describe --always returns the bare hash when no tag is reachable, so
	// Version and Commit are the same string.
	setVars(t, "68fabd1", "68fabd1")
	if got := Full(); got != "68fabd1" {
		t.Fatalf("Full() = %q, want %q", got, "68fabd1")
	}
}

func TestFullAfterATagDoesNotRepeatTheHashTwice(t *testing.T) {
	// This is the case that was wrong. git describe --tags --always returns
	// "v0.1.0-9-g68fabd1" once a tag is reachable, which already names the
	// commit; appending it again produced "v0.1.0-9-g68fabd1+68fabd1".
	setVars(t, "v0.1.0-9-g68fabd1", "68fabd1")
	want := "v0.1.0-9-g68fabd1"
	if got := Full(); got != want {
		t.Fatalf("Full() = %q, want %q", got, want)
	}
}

func TestFullAtATagAppendsTheCommit(t *testing.T) {
	// release.yml overrides VERSION with the tag, so at a release the two
	// parts carry different information and both belong in the string.
	setVars(t, "v0.1.0", "abc1234")
	want := "v0.1.0+abc1234"
	if got := Full(); got != want {
		t.Fatalf("Full() = %q, want %q", got, want)
	}
}

func TestFullWithoutACommit(t *testing.T) {
	// The defaults from a bare `go build`.
	setVars(t, "devel", "none")
	if got := Full(); got != "devel" {
		t.Fatalf("Full() = %q, want %q", got, "devel")
	}
	setVars(t, "devel", "")
	if got := Full(); got != "devel" {
		t.Fatalf("Full() = %q with an empty commit, want %q", got, "devel")
	}
}

func TestFullKeepsADirtySuffix(t *testing.T) {
	// `git describe --tags --always --dirty` appends -dirty to an unclean tree.
	// It does not end with the hash, so the commit is still appended, and the
	// marker has to survive.
	setVars(t, "v0.1.0-9-g68fabd1-dirty", "68fabd1")
	want := "v0.1.0-9-g68fabd1-dirty+68fabd1"
	if got := Full(); got != want {
		t.Fatalf("Full() = %q, want %q", got, want)
	}
}

func TestFullDoesNotDependOnDate(t *testing.T) {
	// Short() uses Full() plus the build date, so Short must keep the version
	// intact when the date is missing.
	setVars(t, "v0.1.0", "abc1234")
	oldDate := Date
	t.Cleanup(func() { Date = oldDate })

	Date = "unknown"
	if got := Short(); got != "v0.1.0+abc1234" {
		t.Fatalf("Short() with no date = %q, want just the version", got)
	}

	Date = "2026-09-11T00:00:00Z"
	want := "v0.1.0+abc1234 (2026-09-11T00:00:00Z)"
	if got := Short(); got != want {
		t.Fatalf("Short() = %q, want %q", got, want)
	}
}

func TestLineCarriesTheNameAndPlatform(t *testing.T) {
	setVars(t, "v0.2.0", "abc1234")
	got := Line("sercond", 1)
	// The exact Go version varies, so only the parts this package controls are
	// asserted.
	for _, want := range []string{"sercond ", "v0.2.0+abc1234", "(protocol v1,", "go"} {
		if !strings.Contains(got, want) {
			t.Errorf("Line() = %q, missing %q", got, want)
		}
	}
}

func TestBuildFitsInTheWindowsChrome(t *testing.T) {
	// The rail footer is 168px wide, about 25 characters at the font it uses,
	// and the bottom bar shares its row with the socket path. This is the
	// assertion that keeps a full identity from being put back there.
	setVars(t, "v0.1.0-9-g68fabd1-dirty", "68fabd1")
	if got := Build(); got != "68fabd1" {
		t.Fatalf("Build() = %q, want the commit", got)
	}
	if n := len(Build()); n > 16 {
		t.Fatalf("Build() is %d characters; the window has room for far fewer", n)
	}
}

func TestBuildFallsBackWhenThereIsNoCommit(t *testing.T) {
	// A bare `go build` stamps nothing, and "none" is not a useful thing to
	// show where a build identifier belongs.
	setVars(t, "devel", "none")
	if got := Build(); got != "devel" {
		t.Fatalf("Build() = %q, want %q", got, "devel")
	}
	setVars(t, "devel", "")
	if got := Build(); got != "devel" {
		t.Fatalf("Build() with an empty commit = %q, want %q", got, "devel")
	}
}

func TestBuildIsShorterThanShort(t *testing.T) {
	// Build() exists only because Short() does not fit the chrome. If that ever
	// stops being true, one of the two is redundant.
	setVars(t, "v0.1.0-9-g68fabd1", "68fabd1")
	Date = "2026-09-11T00:00:00Z"
	if len(Build()) >= len(Short()) {
		t.Fatalf("Build() = %q is not shorter than Short() = %q", Build(), Short())
	}
}
