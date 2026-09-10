package sshauth

import (
	"strings"
	"testing"
)

// A real ed25519 key, generated with ssh-keygen and copied verbatim. The
// fingerprint below was read from `ssh-keygen -lf`, which is what makes the
// fingerprint test meaningful rather than a restatement of the code.
const sampleKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKI+Z+pWFHlo1bnYEv+DA/RABRQfuDO37FSBJyVFVyx5 lucas@example"
const sampleKeyFP = "SHA256:wNK9KFW11uz6k/TskU3JJ9mbclC7cY8BGGoO4VxkGzI"

// A real RSA key, longer and the one most likely to be pasted from a
// colleague's machine.
const rsaKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDxLRwKb62kAUTAGDEzwHJzRvBFCvA1Dh2EYYVqsRuvWzF/5zYB9mNXtnMUqdNI3ULVMiH/nhJIFh4jE9gA617DrzMYyP9YsSnhjYBzGmwVs8MNkSXHiindkPNas1T3a8WNPmHpkCk6JSCrzTTZ5e9FrLT77lr+Fd3vXKn+PhLaXIKLlD+Vb5kda6f5yLf1Us+4dnsZ7P9M+/Z75b73Zk+WttlqrFynFvsu2UrFU/Ig0ZvoZMkReuQoUwQebDIIOB2O4mPGDX1UC3TEgKbZ/vu+n6DFNZD25r5OCJWwbfjEVSfXUoHpIqT/AWvNHlmJlSL1Mz5OQsb6tb6+9g5X6Epj8iwKaDL+Kk96DC2uImunZMG0D+cjN72A3rYKJgLvMeZ5VEM6wgErb9GE1JGa7WrXQ3Wj6gznpdedKLEvxoefYtf4Lkw6/pHCz4KRm0X6bRBzhZeriaSNmgRsMmAeFzBwN5A2vaCkV7A/yzV1WAfFmjTtjCufly9MROYIFDzATpk= lucas@lucas-Lenovo-Product"

func TestParseKeyAcceptsRealKeys(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"ed25519", sampleKey},
		{"rsa", rsaKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, err := ParseKey(tc.line)
			if err != nil {
				t.Fatalf("ParseKey: %v", err)
			}
			if k.Algorithm != strings.Fields(tc.line)[0] {
				t.Errorf("algorithm = %q", k.Algorithm)
			}
			if !strings.HasPrefix(k.Fingerprint, "SHA256:") {
				t.Errorf("fingerprint = %q", k.Fingerprint)
			}
			if k.Comment == "" {
				t.Error("comment was dropped")
			}
			if got := k.Text(); got != tc.line {
				t.Errorf("round trip changed the line:\n got %q\nwant %q", got, tc.line)
			}
		})
	}
}

func TestParseKeyRejectsNonKeys(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"empty", ""},
		{"whitespace", "   "},
		{"comment", "# this is a note"},
		{"prose", "lucas's laptop, added 2026-09-10"},
		{"one field", "ssh-ed25519"},
		{"unknown type", "ssh-dss AAAAB3NzaC1kc3MAAACB"},
		{"bad base64", "ssh-ed25519 !!!not base64!!!"},
		// A blob cut off after the header: it decodes, but declares a length
		// longer than what follows, so sshd would load it and never match it.
		{"truncated blob", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA"},
		// The header length runs past the end of the blob.
		{"header overruns blob", "ssh-ed25519 AAAAHssh-ed25519"},
		{"algorithm mismatch", "ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAIKI+Z+pWFHlo1bnYEv+DA/RABRQfuDO37FSBJyVFVyx5 c"},
		// An option prefix must not be mistaken for the algorithm.
		{"with options", `command="/bin/false" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIL2nQ0DGwFCFCFCFCFCFCFCFCFCFCFCFCFCFCFCFCFCFCFa c`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseKey(tc.line); err == nil {
				t.Fatalf("expected a rejection for %q", tc.line)
			}
		})
	}
}

// TestAddAllIsIdempotent covers the case that matters most in practice:
// pressing "Add" twice, or adding a key that is already installed.
func TestAddAllIsIdempotent(t *testing.T) {
	existing := parseKeys(sampleKey + "\n")

	content, added, err := AddAll(existing, sampleKey+"\n", []Key{mustParse(t, sampleKey)})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Errorf("re-added an existing key: %d added", len(added))
	}
	if content != sampleKey+"\n" {
		t.Errorf("content changed on a no-op add:\n%q", content)
	}
}

// Adding the same key twice in one paste must produce one line, not two.
func TestAddAllDeduplicatesWithinTheSameCall(t *testing.T) {
	content, added, err := AddAll(nil, "", []Key{
		mustParse(t, sampleKey),
		mustParse(t, sampleKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 {
		t.Fatalf("added = %d, want 1", len(added))
	}
	if n := strings.Count(content, sampleKey); n != 1 {
		t.Errorf("the key appears %d times: %q", n, content)
	}
}

func TestAddAllAppendsNewKeys(t *testing.T) {
	existing := parseKeys(sampleKey + "\n")

	content, added, err := AddAll(existing, sampleKey+"\n", []Key{mustParse(t, rsaKey)})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 {
		t.Fatalf("added = %d, want 1", len(added))
	}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), content)
	}
	if lines[0] != sampleKey || lines[1] != rsaKey {
		t.Error("order or content changed")
	}
}

// TestAddAllNormalisesLineEndings is the bug that shows up as an intermittent
// authentication failure: a key written with CRLF is rejected by sshd, and the
// error it gives points at the key rather than at the line ending.
func TestAddAllNormalisesLineEndings(t *testing.T) {
	crlf := sampleKey + "\r\n"

	content, _, err := AddAll(parseKeys(crlf), crlf, []Key{mustParse(t, rsaKey)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "\r") {
		t.Errorf("CR survived into the written file: %q", content)
	}
	if !strings.HasSuffix(content, "\n") {
		t.Errorf("file does not end with a newline: %q", content)
	}
}

// A file whose last line has no newline would otherwise have the next key
// glued onto it.
func TestAddAllRepairsMissingFinalNewline(t *testing.T) {
	noNewline := sampleKey // no trailing \n

	content, _, err := AddAll(parseKeys(noNewline), noNewline, []Key{mustParse(t, rsaKey)})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("keys were merged: %q", content)
	}
}

// Comments and blank lines in the file are somebody's notes; the editor must
// not silently drop them.
func TestAddAllPreservesForeignLines(t *testing.T) {
	original := "# keys added 2026-09-10\n\n" + sampleKey + "\n"

	content, _, err := AddAll(parseKeys(original), original, []Key{mustParse(t, rsaKey)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "# keys added 2026-09-10") {
		t.Errorf("comment was dropped: %q", content)
	}
	if !strings.Contains(content, sampleKey) || !strings.Contains(content, rsaKey) {
		t.Errorf("a key went missing: %q", content)
	}
}

func TestRemove(t *testing.T) {
	original := sampleKey + "\n" + rsaKey + "\n"
	victim := mustParse(t, sampleKey)

	got := Remove(original, victim.Fingerprint)
	if strings.Contains(got, victim.Blob) {
		t.Error("the victim key is still present")
	}
	if !strings.Contains(got, mustParse(t, rsaKey).Blob) {
		t.Error("an unrelated key was removed")
	}
}

// Removing by fingerprint rather than by line number is what makes this safe
// when the file changed between load and save.
func TestRemoveIgnoresUnknownFingerprint(t *testing.T) {
	original := sampleKey + "\n"
	got := Remove(original, "SHA256:does-not-exist")
	if got != original {
		t.Errorf("unrelated file changed: %q", got)
	}
}

func TestRemoveKeepsComments(t *testing.T) {
	original := "# note\n" + sampleKey + "\n"
	got := Remove(original, mustParse(t, sampleKey).Fingerprint)
	if !strings.Contains(got, "# note") {
		t.Errorf("comment was dropped: %q", got)
	}
}

// TestFingerprintMatchesSSHKeygen pins the fingerprint to a value produced by
// OpenSSH itself. The GUI shows this string and operators compare it against
// what their own ssh-keygen prints, so a mismatch would make a correct key look
// wrong.
func TestFingerprintMatchesSSHKeygen(t *testing.T) {
	k := mustParse(t, sampleKey)
	if k.Fingerprint != sampleKeyFP {
		t.Errorf("fingerprint = %q, want %q (from ssh-keygen -lf)", k.Fingerprint, sampleKeyFP)
	}
	// Same for the RSA key, which exercises a much longer blob.
	rsa := mustParse(t, rsaKey)
	if rsa.Fingerprint != "SHA256:X8A69RzhE4rAXgGiWCy5f8wyid/bTFOjPwK7tUQoVC0" {
		t.Errorf("RSA fingerprint = %q", rsa.Fingerprint)
	}
}

func TestLabelFallsBackToAlgorithm(t *testing.T) {
	noComment := strings.Join(strings.Fields(sampleKey)[:2], " ")
	k := mustParse(t, noComment)
	if k.Comment != "" {
		t.Fatalf("expected no comment, got %q", k.Comment)
	}
	if k.Label() == "" {
		t.Error("Label returned an empty string")
	}
}

func TestSortedByCommentIsStable(t *testing.T) {
	keys := []Key{
		mustParse(t, rsaKey),
		mustParse(t, sampleKey),
	}
	a := SortedByComment(keys)
	b := SortedByComment(keys)
	for i := range a {
		if a[i].Fingerprint != b[i].Fingerprint {
			t.Fatal("ordering is not stable across calls")
		}
	}
}

func mustParse(t *testing.T, line string) Key {
	t.Helper()
	k, err := ParseKey(line)
	if err != nil {
		t.Fatalf("ParseKey(%q): %v", line, err)
	}
	return k
}
