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

// Full is the version with the commit appended, unless that would just repeat
// it. Before the first tag, git describe --always returns the bare hash, so
// Version and Commit are the same string and "abc1234+abc1234" helps nobody.
func Full() string {
	v := Version
	if Commit != "" && Commit != "none" && Commit != v {
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
