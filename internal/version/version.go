// Package version carries the build identity shared by every binary in this
// repository.
//
// It exists because the version needs exactly one home. Each command used to
// declare its own `const version = "0.1.0"`, which is three places to forget
// when tagging and three chances for them to disagree about what was shipped.
//
// The values are stamped at link time, never edited by hand:
//
//	go build -ldflags "\
//	  -X sercon/internal/version.Version=$(git describe --tags --always) \
//	  -X sercon/internal/version.Commit=$(git rev-parse --short HEAD) \
//	  -X sercon/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// `make build` and .github/workflows/release.yml both do this, so a binary
// always knows which commit it came from — which is the question that actually
// gets asked when something misbehaves in the lab.
package version

import (
	"fmt"
	"runtime"
	"strings"
)

// Set at link time. The defaults are what an unadorned `go build` produces, and
// they are deliberately recognisable: a binary reporting "devel" was not built
// by the release process.
var (
	// Version is the git tag, or a short hash when there is no tag yet.
	Version = "devel"
	// Commit is the short hash of the commit the binary was built from.
	Commit = "none"
	// Date is the build time in UTC, RFC 3339.
	Date = "unknown"
)

// Full is the version with the commit appended, unless the version already
// names it.
//
// Two cases have to be suppressed, not one. Before the first tag, git describe
// --always returns the bare hash and Version == Commit, so "abc1234+abc1234"
// helps nobody. After a tag exists it returns "v0.1.0-9-g68fabd1", which is not
// equal to the hash but already ends with it — comparing for equality alone
// produced "v0.1.0-9-g68fabd1+68fabd1", repeating the hash the describe output
// had just given.
//
// Note that release.yml stamps Version with the tag itself rather than the
// describe output, so a release binary reads "v0.1.0+abc1234" — the tag it
// claims to be, plus the commit it actually is.
func Full() string {
	v := Version
	if Commit != "" && Commit != "none" && !strings.HasSuffix(v, Commit) {
		v += "+" + Commit
	}
	return v
}

// Line renders the one-line identity each command prints for --version.
func Line(name string, protocol int) string {
	return fmt.Sprintf("%s %s (protocol v%d, %s, %s/%s)",
		name, Full(), protocol, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Short renders just the version and build time, for log headers and window
// captions where the full line would be noise.
func Short() string {
	if Date == "" || Date == "unknown" {
		return Full()
	}
	return Full() + " (" + Date + ")"
}

// Build renders a compact build identifier for the window's own chrome.
//
// The full identity is forty-odd characters — a describe summary plus the build
// timestamp — and neither place the window shows a version has room for that.
// The rail footer is 168px wide, which at 8pt Consolas holds about twenty-five
// characters, and the bottom bar already shares its single row with the socket
// path.
//
// The commit is the question that actually gets asked when something misbehaves
// in the lab, and seven characters always fit. Version is the fallback so that a
// bare `go build` still reads "devel" rather than "none".
//
// The full string remains available where there is room for it: --version, and
// the daemon started line in gui.log.
func Build() string {
	if Commit != "" && Commit != "none" {
		return Commit
	}
	return Version
}
