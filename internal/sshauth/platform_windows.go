//go:build windows

package sshauth

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// elevatedTimeout bounds how long to wait for the UAC helper to write its
// result. It is generous because the wait includes the time the user spends
// reading the prompt.
const elevatedTimeout = 90 * time.Second

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	netapi32 = syscall.NewLazyDLL("netapi32.dll")

	pCheckTokenMembership  = advapi32.NewProc("CheckTokenMembership")
	pShellExecuteW         = shell32.NewProc("ShellExecuteW")
	pNetUserGetLocalGroups = netapi32.NewProc("NetUserGetLocalGroups")
	pGetTokenInformation   = advapi32.NewProc("GetTokenInformation")
	pOpenProcessToken      = advapi32.NewProc("OpenProcessToken")
)

const (
	// currentProcess is the pseudo-handle for the running process.
	currentProcess = ^uintptr(0) // (HANDLE)-1
	tokenQuery     = 0x0008
	// tokenGroups is TOKEN_INFORMATION_CLASS 2.
	tokenGroups = 2
)

// AdministratorsSID is the well-known SID of the built-in Administrators group.
//
// CheckTokenMembership takes it in binary form, which is why it is written out
// rather than looked up by name: the group's *name* is localised, so on a
// Chinese or German install "Administrators" does not exist and a name-based
// check silently answers the wrong thing.
var administratorsSID = []byte{
	0x01, 0x02, 0x00, 0x00, // revision 1, 2 sub-authorities
	0x00, 0x00, 0x00, 0x00, 0x00, 0x05, // SECURITY_NT_AUTHORITY
	0x20, 0x00, 0x00, 0x00, // SECURITY_BUILTIN_DOMAIN_RID
	0x20, 0x02, 0x00, 0x00, // DOMAIN_ALIAS_RID_ADMINS
}

type sidAndAttributes struct {
	Sid        uintptr
	Attributes uint32
}

// KeyFile returns the authorized_keys path sshd will actually read.
//
// This is the whole reason the package exists. Stock sshd_config has:
//
//	AuthorizedKeysFile .ssh/authorized_keys
//	Match Group administrators
//	       AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys
//
// so for an administrator the per-user file is not read at all. Writing a key
// there looks right and fails with "Permission denied (publickey)", which
// sends people looking at the key instead of at the path.
//
// Membership is per-token, not per-account: an elevated process and a normal
// one share the answer here, but a token with the group disabled (a symptom of
// UAC's filtered token) must still resolve to the administrator path, or the
// file would move depending on how the GUI was launched. Both branches are
// therefore checked.
func KeyFile(username string) (string, error) {
	if IsAdmin(username) {
		dir := os.Getenv("ProgramData")
		if dir == "" {
			dir = `C:\ProgramData`
		}
		return filepath.Join(dir, "ssh", "administrators_authorized_keys"), nil
	}

	home, err := homeDir(username)
	if err != nil {
		return "", err
	}
	return DefaultKeyFile(home), nil
}

// IsAdmin reports whether the account belongs to the Administrators group.
//
// It answers about the *account*, not the current token: an un-elevated
// process belonging to an administrator still gets the administrator key file,
// which is what sshd will use when that person logs in.
//
// This distinction is the whole reason the function is more than one API call.
// UAC hands an administrator a "filtered" token with the Administrators group
// present but marked deny-only, and CheckTokenMembership reports such a group
// as absent. Asking only that would answer "not an admin" for the common case
// of the GUI launched normally, and the panel would then point at
// ~/.ssh/authorized_keys — the file sshd does not read for this account. The
// name-based lookup does not go through the token, so it gets the right
// answer.
func IsAdmin(username string) bool {
	if username == "" {
		username = currentUser()
	}
	if username == "" {
		return tokenInAdministrators() || tokenIsElevatable()
	}

	// NetUserGetLocalGroups is the direct question — "is this account in that
	// group" — and unlike NetUserGetInfo it does not need a logon.
	if inAdminsByGroupLookup(username) {
		return true
	}
	// A domain account can be in Administrators through domain group nesting,
	// which the local enumeration does not expand. Fall back to comparing the
	// name to the current user and asking the token.
	return sameUserAsCurrent(username) && (tokenInAdministrators() || tokenIsElevatable())
}

// tokenIsElevatable reports whether the token carries a deny-only
// Administrators group, which is what an un-elevated administrator has.
//
// CheckTokenMembership with a nil handle answers "is this group enabled". To
// see a deny-only group the call has to be made against the token directly
// with the group still in its list, so the token's group list is walked
// instead of asking the simpler question.
func tokenIsElevatable() bool {
	return tokenHasGroup(administratorsSID)
}

// tokenInAdministrators asks whether the current token carries Administrators
// as an enabled group.
//
// CheckTokenMembership with a nil token handle inspects the calling thread's
// effective token. For an un-elevated administrator, UAC has removed the group
// from the effective token, so this returns false — which is why IsAdmin does
// not rely on it alone.
func tokenInAdministrators() bool {
	var isMember int32
	r, _, _ := pCheckTokenMembership.Call(
		0,
		uintptr(unsafe.Pointer(&administratorsSID[0])),
		uintptr(unsafe.Pointer(&isMember)),
	)
	if r == 0 {
		return false
	}
	return isMember != 0
}

// tokenHasGroup reports whether the process token lists the given SID at all,
// regardless of whether the group is enabled.
//
// This is what distinguishes "not an administrator" from "an administrator who
// has not elevated": UAC leaves the group in the token as SE_GROUP_USE_FOR_DENY_ONLY
// rather than removing it. GetTokenInformation(TOKEN_GROUPS) returns the raw
// list, so the denied entry is visible here even though CheckTokenMembership
// says no.
func tokenHasGroup(want []byte) bool {
	var tok syscall.Token
	if err := openProcessToken(&tok); err != nil {
		return false
	}
	defer tok.Close()

	// First call sizes the buffer. TOKEN_GROUPS is a count followed by
	// inline SID_AND_ATTRIBUTES entries, so the size is not fixed.
	var size uint32
	pGetTokenInformation.Call(
		uintptr(tok),
		tokenGroups,
		0, 0,
		uintptr(unsafe.Pointer(&size)),
	)
	if size == 0 {
		return false
	}

	buf := make([]byte, size)
	r, _, _ := pGetTokenInformation.Call(
		uintptr(tok),
		tokenGroups,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(size),
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return false
	}

	// The struct is a DWORD count followed by that many SID_AND_ATTRIBUTES.
	// The SIDs live inside the same allocation, which is why buf is kept alive
	// across the walk.
	count := *(*uint32)(unsafe.Pointer(&buf[0]))
	entries := unsafe.Slice(
		(*sidAndAttributes)(unsafe.Pointer(&buf[4])),
		int(count),
	)
	for _, e := range entries {
		if e.Sid == 0 {
			continue
		}
		if sidEqual(e.Sid, want) {
			return true
		}
	}
	return false
}

// sidEqual compares a SID in memory against a byte-form SID.
//
// The length has to be read from the SID itself first: a SID is a header plus
// a variable-length sub-authority array, so the two cannot be compared as byte
// slices without knowing how long the first one is.
func sidEqual(ptr uintptr, want []byte) bool {
	const sidMaxLen = 68 // SECURITY_MAX_SID_SIZE
	got := unsafe.Slice((*byte)(uptrToPtr(ptr)), sidMaxLen)
	n := int(got[1])*4 + 8
	if n > len(want) || n > sidMaxLen {
		return false
	}
	return string(got[:n]) == string(want[:n])
}

func openProcessToken(tok *syscall.Token) error {
	r, _, errno := pOpenProcessToken.Call(
		uintptr(currentProcess),
		uintptr(tokenQuery),
		uintptr(unsafe.Pointer(tok)),
	)
	if r == 0 {
		return errno
	}
	return nil
}

// currentUser returns the name of the account running this process.
func currentUser() string {
	return firstNonEmpty(os.Getenv("USERNAME"), os.Getenv("USER"))
}

// uptrToPtr reinterprets a handle-sized integer as a pointer.
//
// The values routed through here come from the OS — a pointer inside a buffer
// Windows filled in — so the usual objection to this conversion (that a
// uintptr says nothing about whether a Go object moved) does not apply. The
// value is read through a local to keep it off the GC's radar, where it
// belongs: none of this memory is Go-managed.
func uptrToPtr(u uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&u))
}

// inAdminsByGroupLookup enumerates a local account's group memberships.
func inAdminsByGroupLookup(username string) bool {
	name, err := syscall.UTF16PtrFromString(username)
	if err != nil {
		return false
	}

	var buf *byte
	var entriesRead, totalEntries uint32
	const level = 0

	r, _, _ := pNetUserGetLocalGroups.Call(
		0,
		uintptr(unsafe.Pointer(name)),
		level,
		0, // no flags: returns the groups the account is directly in
		uintptr(unsafe.Pointer(&buf)),
		0xFFFFFFFF, // MAX_PREFERRED_LENGTH
		uintptr(unsafe.Pointer(&entriesRead)),
		uintptr(unsafe.Pointer(&totalEntries)),
	)
	if r != 0 || buf == nil {
		return false
	}
	defer netApiBufferFree(buf)

	// LOCALGROUP_USERS_INFO_0 is a single LPWSTR, so the buffer is an array of
	// UTF-16 pointers and the stride is the pointer width.
	type groupInfo struct{ Name *uint16 }
	items := unsafe.Slice((*groupInfo)(unsafe.Pointer(buf)), int(entriesRead))
	for _, g := range items {
		if g.Name == nil {
			continue
		}
		if isAdministratorsName(syscall.UTF16ToString(unsafe.Slice(g.Name, 256))) {
			return true
		}
	}
	return false
}

// isAdministratorsName matches the localised name of the Administrators group.
//
// The name is localised ("Administratoren", "Administrateurs", "Administrators")
// so both the English spelling and the SID-resolved name are needed. The
// well-known SID is resolved once and cached; before that, only the English
// form is recognised, which is correct on an English install and merely
// incomplete elsewhere — the token check covers the rest.
func isAdministratorsName(name string) bool {
	if strings.EqualFold(name, "Administrators") {
		return true
	}
	if adminsGroupName == "" {
		adminsGroupName = lookupNameBySID(administratorsSID)
	}
	return adminsGroupName != "" && strings.EqualFold(name, adminsGroupName)
}

var adminsGroupName string

// TightenACL restricts path to SYSTEM and Administrators.
//
// sshd refuses to read an administrators_authorized_keys that any other
// principal can reach — the file is a login credential, and an account that
// can rewrite it can log in as anyone. The stock Windows install creates the
// file with the right ACL, but anything this package creates needs it set, and
// a file copied in from elsewhere (which is exactly what happens when someone
// follows advice written for Linux) will not have it.
//
// icacls is used rather than SetNamedSecurityInfo because the latter needs a
// correctly ordered ACL built out of binary ACEs, and getting that subtly wrong
// produces a file sshd rejects for reasons that never surface in a log.
func TightenACL(path string) error {
	// /inheritance:r drops inherited entries, and the two grants are the whole
	// permitted set. Anything not named here is removed as a side effect of
	// removing inheritance before granting.
	cmd := exec.Command("icacls", path,
		"/inheritance:r",
		"/grant", `*S-1-5-18:(F)`, // SYSTEM, by SID so it survives localisation
		"/grant", `*S-1-5-32-544:(F)`, // BUILTIN\Administrators
	)
	hide(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("icacls %s: %s", path, msg)
	}
	return nil
}

// NeedsElevation reports whether writing path needs a privilege this process
// lacks.
//
// The question is only interesting for the ProgramData file, which is owned by
// Administrators. An un-elevated administrator can read it and not write it,
// so the GUI has to notice before it tries, in order to offer a UAC prompt
// rather than fail with "Access is denied".
func NeedsElevation(path string) bool {
	dir := filepath.Dir(path)
	f, err := os.OpenFile(filepath.Join(dir, ".ssh-write-probe"),
		os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return os.IsPermission(err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return false
}

// ReadElevated reads path through a UAC prompt.
//
// An un-elevated administrator can be denied even read access to the
// ProgramData key file, because its ACL names only SYSTEM and Administrators
// and the filtered token is in neither. Without this the panel could not show
// what is already installed, and the user would be editing blind.
func ReadElevated(path string) (string, error) {
	tmp, err := os.MkdirTemp("", "sercon-sshread-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	outPath := filepath.Join(tmp, "contents.txt")
	scriptPath := filepath.Join(tmp, "read.bat")

	script := "@echo off\r\n" +
		"cmd /c type \"" + path + "\" > \"" + outPath + "\" 2>&1\r\n" +
		"echo EXIT=%ERRORLEVEL% >> \"" + outPath + "\"\r\n"

	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return "", err
	}
	if err := runElevatedScript(scriptPath, outPath); err != nil {
		return "", err
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return "", err
	}
	return stripExitMarker(string(data)), nil
}

// RunElevated performs a file write through a UAC prompt.
//
// The parent has already decided what to write; this only carries it out with
// the rights to do so. It waits for the helper to finish so the caller can
// report the real outcome instead of optimistically claiming success the
// moment the prompt is dismissed.
//
// A batch file is generated rather than invoking icacls directly because a
// single elevated action has to include the write, the ACL, and a way to
// report back — three steps that need to run in one prompt.
func RunElevated(path, content string) error {
	tmp, err := os.MkdirTemp("", "sercon-sshkey-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	payload := filepath.Join(tmp, "key.txt")
	if err := os.WriteFile(payload, []byte(content), 0o600); err != nil {
		return err
	}

	resultPath := filepath.Join(tmp, "result.txt")
	scriptPath := filepath.Join(tmp, "apply.bat")

	// The grants are given by SID, not by name: the group names are localised
	// and this script runs under whatever locale the machine uses.
	//
	// Each helper is invoked with cmd /c and its own redirection so that one
	// failing does not hide the other's output. Quoting everything up to the
	// redirect is deliberate — cmd splits on the first unquoted space.
	script := "@echo off\r\n" +
		"cmd /c copy /Y \"" + payload + "\" \"" + path + "\" > \"" + resultPath + "\" 2>&1\r\n" +
		"cmd /c icacls \"" + path + "\" /inheritance:r" +
		" /grant *S-1-5-18:(F) /grant *S-1-5-32-544:(F)" +
		" >> \"" + resultPath + "\" 2>&1\r\n" +
		"echo EXIT=%ERRORLEVEL% >> \"" + resultPath + "\"\r\n"

	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return err
	}

	if err := runElevatedScript(scriptPath, resultPath); err != nil {
		return err
	}

	data, err := os.ReadFile(resultPath)
	if err != nil {
		return err
	}
	out := string(data)
	if !strings.Contains(out, "EXIT=0") {
		return fmt.Errorf("%s", strings.TrimSpace(stripExitMarker(out)))
	}
	return nil
}

// runElevatedScript launches a batch file with the runas verb and waits for a
// result file to appear.
//
// ShellExecuteW returns as soon as the process starts, so the result file is
// the only evidence the elevated half actually finished — and its absence is
// how a dismissed UAC prompt is detected.
func runElevatedScript(scriptPath, resultPath string) error {
	verb := syscall.StringToUTF16Ptr("runas")
	file := syscall.StringToUTF16Ptr(scriptPath)

	// The working directory is left to the default (%SystemRoot%\System32).
	// Handing an elevated command a writable directory is a way to turn a
	// prompt into a privilege escalation.
	r, _, errno := pShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		0,
		0,
		1, // SW_SHOWNORMAL
	)
	if r <= 32 {
		// ShellExecute reports a refused prompt as ERROR_CANCELLED (1223).
		if errno == syscall.Errno(1223) {
			return errors.New("cancelled at the UAC prompt")
		}
		return fmt.Errorf("cannot elevate: %v", errno)
	}

	if !waitForFile(resultPath, elevatedTimeout) {
		return errors.New("the elevated helper did not report back")
	}
	return nil
}

// stripExitMarker removes the trailing EXIT=n line the helper appends, so the
// caller sees only the file's own contents.
func stripExitMarker(s string) string {
	lines := strings.Split(s, "\n")
	var kept []string
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "EXIT=") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// waitForFile reports whether path appeared within timeout.
func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			// Give the redirect a moment to be flushed and closed, or the
			// read that follows can catch a partial file.
			time.Sleep(50 * time.Millisecond)
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func homeDir(username string) (string, error) {
	if username == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return h, nil
	}
	// os/user on Windows resolves USERPROFILE without needing a domain
	// controller, which is the failure mode of the cgo-free lookup in an
	// offline environment.
	if u := os.Getenv("USERPROFILE"); sameUserAsCurrent(username) && u != "" {
		return u, nil
	}
	return profileDirFor(username)
}

// sameUserAsCurrent compares a name against the running account, accepting the
// several ways Windows spells the same identity (bare name, DOMAIN\name,
// UPN).
func sameUserAsCurrent(username string) bool {
	current := firstNonEmpty(os.Getenv("USERNAME"), os.Getenv("USER"))
	if current == "" {
		return false
	}
	if strings.EqualFold(username, current) {
		return true
	}
	// DOMAIN\user and user@domain both end up comparing the bare part.
	if i := strings.LastIndexAny(username, `\`); i >= 0 {
		return strings.EqualFold(username[i+1:], current)
	}
	if i := strings.Index(username, "@"); i >= 0 {
		return strings.EqualFold(username[:i], current)
	}
	return false
}

// profileDirFor returns an account's profile directory from the registry.
//
// USERPROFILE is right for the running account, but this package also answers
// for other accounts — the operator may want to install a key for a colleague
// — and ProfileImagePath is the only reliable source then. os/user would need
// the directory service in some configurations, which fails on a machine that
// is not domain-joined.
func profileDirFor(username string) (string, error) {
	base := `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`

	root, err := registryOpenKey(base)
	if err != nil {
		return "", fmt.Errorf("cannot locate %s: %w", username, err)
	}
	defer root.Close()

	for _, sub := range root.Subkeys() {
		subkey, err := registryOpenKey(base + `\` + sub)
		if err != nil {
			continue
		}
		path, err := subkey.StringValue("ProfileImagePath")
		subkey.Close()
		if err != nil {
			continue
		}
		if profileMatchesUser(path, username) {
			return filepath.Clean(path), nil
		}
	}
	return "", fmt.Errorf("cannot locate %s: no profile directory", username)
}

// profileMatchesUser compares a profile path (typically C:\Users\lucas)
// against a username, allowing for the DOMAIN\name form.
func profileMatchesUser(profilePath, username string) bool {
	base := filepath.Base(filepath.Clean(profilePath))
	if base == "" || base == "." {
		return false
	}
	if strings.EqualFold(base, username) {
		return true
	}
	if i := strings.LastIndexAny(username, `\`); i >= 0 {
		return strings.EqualFold(base, username[i+1:])
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
