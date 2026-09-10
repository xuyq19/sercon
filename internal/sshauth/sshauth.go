// Package sshauth manages the authorized_keys file that lets a client reach
// this machine over SSH.
//
// It exists because getting this right on Windows is not obvious. Where the
// file must live depends on whether the account is an administrator:
//
//	administrator:  %ProgramData%\ssh\administrators_authorized_keys
//	normal user:    %USERPROFILE%\.ssh\authorized_keys
//
// The stock sshd_config encodes that split as a "Match Group administrators"
// block, which is easy to miss and produces a confusing symptom: a key written
// carefully into ~/.ssh/authorized_keys is simply never read, and the server
// answers "Permission denied (publickey)" as if the key were wrong. The rules
// around the ProgramData file are also stricter — sshd rejects it if anyone
// other than SYSTEM and Administrators can reach it — so writing the key is
// only half the job; the ACL has to be set as well.
//
// The package is deliberately independent of Win32 so the parsing and editing
// rules stay testable on every platform, which is where the actual risk is.
// Anything platform-specific lives in the _windows/_other files.
package sshauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Key is one entry in an authorized_keys file.
//
// The comment is whatever followed the base64 blob on the line. It is set by
// the client when it generated the key and carries no meaning to sshd, but it
// is usually the only human-readable thing in the file, so it is what the UI
// shows next to the fingerprint.
type Key struct {
	Algorithm   string
	Blob        string // base64 body, exactly as it appears in the file
	Comment     string
	Fingerprint string // SHA256:… , the form ssh-keygen -l prints
	Line        int    // 1-based line number in the file
}

// Label is a short human-readable identifier for a key.
func (k Key) Label() string {
	if k.Comment != "" {
		return k.Comment
	}
	return k.Algorithm + " key"
}

// ErrNotAKey is returned when a line is not a public key at all. Callers
// usually treat this as "skip it" rather than a fatal error, because real
// authorized_keys files collect comments and blank lines over time.
var ErrNotAKey = errors.New("not a public key")

// keyTypePrefixes are the algorithm names this package accepts. Accepting only
// known ones keeps a stray line of text from being written into the file as
// though it were a key, which sshd would then reject in a way that is hard to
// trace back to the edit.
//
// DSA is missing on purpose: OpenSSH has disabled ssh-dss by default since 7.0,
// so accepting it would let a user add a key that can never authenticate.
var keyTypePrefixes = []string{
	"ssh-ed25519",
	"ssh-rsa",
	"ecdsa-sha2-nistp256",
	"ecdsa-sha2-nistp384",
	"ecdsa-sha2-nistp521",
	"sk-ssh-ed25519@openssh.com",
	"sk-ecdsa-sha2-nistp256@openssh.com",
}

// keyTypeMax is how much of a line is looked at when deciding whether it is a
// key. Every algorithm name above fits; the cap only stops a pathological line
// from being scanned in full.
const keyTypeMax = 128

// ParseKey parses a single line into a Key.
//
// Lines that are blank, or that begin with '#' or an option prefix such as
// 'no-pty', are reported as ErrNotAKey. Options are rejected rather than
// preserved because this package edits the file, and silently keeping a
// half-understood prefix is how a working key turns into a broken one.
func ParseKey(line string) (Key, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return Key{}, ErrNotAKey
	}

	// A line may begin with a comma-separated option list before the algorithm.
	// Anything with whitespace before the first token is assumed to be that,
	// and is left alone by the editor.
	fields := strings.Fields(trimmed)
	if len(fields) < 2 {
		return Key{}, ErrNotAKey
	}

	algorithm := fields[0]
	if !isKnownKeyType(algorithm) {
		return Key{}, ErrNotAKey
	}

	blob := fields[1]
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return Key{}, fmt.Errorf("%w: base64: %v", ErrNotAKey, err)
	}
	// A well-formed blob starts by repeating the algorithm name as a
	// length-prefixed string. Checking that catches a truncated paste, which
	// otherwise produces a key sshd accepts at load time and never matches.
	if err := checkBlobHeader(raw, algorithm); err != nil {
		return Key{}, err
	}

	var comment string
	if len(fields) > 2 {
		comment = strings.Join(fields[2:], " ")
	}

	return Key{
		Algorithm:   algorithm,
		Blob:        blob,
		Comment:     comment,
		Fingerprint: Fingerprint(raw),
	}, nil
}

func isKnownKeyType(s string) bool {
	for _, p := range keyTypePrefixes {
		if s == p {
			return true
		}
	}
	return false
}

// checkBlobHeader verifies that the decoded blob begins with its own algorithm
// name, which is how the wire format identifies the key type.
func checkBlobHeader(raw []byte, algorithm string) error {
	if len(raw) < 4 {
		return fmt.Errorf("%w: blob is too short", ErrNotAKey)
	}
	n := binary.BigEndian.Uint32(raw[:4])
	if int(n)+4 > len(raw) {
		return fmt.Errorf("%w: blob header length %d exceeds blob size %d", ErrNotAKey, n, len(raw))
	}
	if got := string(raw[4 : 4+n]); got != algorithm {
		return fmt.Errorf("%w: blob says %q but the line says %q", ErrNotAKey, got, algorithm)
	}
	return nil
}

// Text renders the key the way it belongs in an authorized_keys file.
func (k Key) Text() string {
	parts := []string{k.Algorithm, k.Blob}
	if k.Comment != "" {
		parts = append(parts, k.Comment)
	}
	return strings.Join(parts, " ")
}

// Load reads the keys currently in path.
//
// A missing file is not an error: the caller is usually about to create it.
// A permission failure is not an error either — it means the file exists but
// belongs to an administrator, and the caller is expected to offer an
// elevation step rather than to treat the panel as broken. Use LoadErr when
// the distinction matters.
func Load(path string) ([]Key, error) {
	keys, err := LoadErr(path)
	if err != nil && os.IsPermission(err) {
		return nil, nil
	}
	return keys, err
}

// LoadErr is Load with the permission error preserved.
func LoadErr(path string) ([]Key, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseKeys(string(data)), nil
}

func parseKeys(content string) []Key {
	var keys []Key
	for i, line := range strings.Split(content, "\n") {
		k, err := ParseKey(line)
		if err != nil {
			continue
		}
		k.Line = i + 1
		keys = append(keys, k)
	}
	return keys
}

// ParseKeys reads every key out of a block of text, skipping anything that is
// not one. It is the exported form of the parsing Load does, for callers that
// already have the contents in hand.
func ParseKeys(content string) ([]Key, error) { return parseKeys(content), nil }

// AddAll appends keys that are not already present and returns the file's new
// contents along with the fingerprints that were actually added.
//
// The whole file is rebuilt rather than appended to, so CRLF endings and a
// missing final newline are fixed in the same pass. sshd rejects a key line
// with a trailing CR, and a file whose last line has no newline can swallow
// the next key added to it — both bugs that only show up later, as an
// intermittent authentication failure.
func AddAll(existing []Key, content string, add []Key) (string, []Key, error) {
	have := make(map[string]bool, len(existing))
	for _, k := range existing {
		have[k.Fingerprint] = true
	}

	var added []Key
	lines := keptLines(content)

	for _, k := range add {
		if have[k.Fingerprint] {
			continue
		}
		have[k.Fingerprint] = true
		added = append(added, k)
		lines = append(lines, k.Text())
	}

	if len(added) == 0 {
		return content, nil, nil
	}
	return render(lines), added, nil
}

// Remove drops every key with the given fingerprint.
//
// Matching on the fingerprint rather than the line number means a removal
// cannot be thrown off by an edit made between loading and saving.
//
// If nothing matched, the original content is returned unchanged. Re-rendering
// an untouched file would quietly rewrite it — normalising line endings and
// dropping blank lines the user put there — which shows up as the file's
// modification time changing for no reason and makes "did the GUI do
// something?" impossible to answer.
func Remove(content string, fingerprints ...string) string {
	victims := make(map[string]bool, len(fingerprints))
	for _, f := range fingerprints {
		victims[f] = true
	}

	var kept []string
	matched := false
	for _, line := range splitLines(content) {
		k, err := ParseKey(line)
		if err == nil && victims[k.Fingerprint] {
			matched = true
			continue
		}
		kept = append(kept, line)
	}
	if !matched {
		return content
	}
	return render(kept)
}

// keptLines returns the file's lines with CR stripped and blank lines removed
// from the end, so that appending produces a clean file.
func keptLines(content string) []string {
	var out []string
	for _, line := range splitLines(content) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// splitLines splits on any convention and strips the CR, which is what turns a
// file edited by Notepad into something sshd will read.
func splitLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return strings.Split(content, "\n")
}

func render(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// Fingerprint returns the SHA256 fingerprint of a decoded blob, in the form
// "SHA256:base64" that ssh-keygen -l prints and that sshd logs on a match.
func Fingerprint(blob []byte) string {
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// HostKeyComment formats the comment used when this machine creates a listing
// of its own keys, so the entry says where it came from rather than reusing
// the client's comment.
func HostKeyComment(user, host string) string {
	switch {
	case user == "" && host == "":
		return ""
	case host == "":
		return user
	case user == "":
		return host
	}
	return user + "@" + host
}

// DefaultKeyFile is the per-user location, which is correct everywhere except
// for a Windows administrator. Callers should prefer KeyFile, which knows
// about that exception.
func DefaultKeyFile(home string) string {
	return filepath.Join(home, ".ssh", "authorized_keys")
}

// SortedByComment returns keys in a stable display order so the list does not
// reshuffle between refreshes.
func SortedByComment(keys []Key) []Key {
	out := append([]Key(nil), keys...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Label() < out[j].Label()
	})
	return out
}
