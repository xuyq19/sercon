//go:build windows

// The SSH key panel: the window that installs a client's public key so it can
// reach this machine.
//
// It exists because doing this by hand on Windows is easy to get wrong in a
// way that produces a misleading error. Two things must both be true, and
// getting either wrong gives the same "Permission denied (publickey)":
//
//   - the key has to be in the file sshd reads for this account, which for an
//     administrator is %ProgramData%\ssh\administrators_authorized_keys and
//     not ~/.ssh/authorized_keys
//   - that file's ACL has to name only SYSTEM and Administrators, because sshd
//     refuses to read a key file anyone else can write
//
// The panel decides both, and raises a UAC prompt for the write, since neither
// the file nor its directory is writable by a normally launched process.
package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"unsafe"

	"sercon/internal/sshauth"
)

// Control IDs for the key window. They share the parent window's command
// dispatch, so they must not collide with the main window's.
const (
	idKeyEntryOpen = 2001

	idKeyList      = 2010
	idKeyInput     = 2011
	idKeyAdd       = 2012
	idKeyRemove    = 2013
	idKeyClose     = 2014
	idKeyReload    = 2015
	idKeyPathLabel = 2016
	idKeyHintLabel = 2017
	idKeyInputHint = 2018
)

// state holds the key window's controls and the keys currently listed. It is
// package-level so the window procedure can reach it from the callback, in the
// same way the main window's state is.
type keyPanel struct {
	hwnd      uintptr
	list      uintptr
	input     uintptr
	pathLabel uintptr
	hintLabel uintptr
	inputHint uintptr
	btns      []uintptr

	path string
	// keys is the list as displayed, kept so a selection can be mapped back to
	// a fingerprint without re-reading the file.
	keys []sshauth.Key
	// raw is the file's contents as last read. Add and Remove edit this
	// in-memory copy and write the result, so a change the panel cannot read
	// back (because it lacks the rights) is still not lost.
	raw string

	// busy guards against a second UAC prompt while one is open, and stops the
	// panel being dismissed mid-write.
	busy bool
	mu   sync.Mutex

	// ready is set once every control exists. WM_SIZE arrives during creation,
	// so layout has to be suppressed until the last control is in place.
	ready bool
}

// keyPanelReady reports whether the panel's controls all exist.
func keyPanelReady() bool {
	kp := keyWin
	return kp != nil && kp.ready
}

var keyWin *keyPanel

// openKeyPanel shows the window, creating it the first time.
//
// It is a separate top-level window rather than a modal dialog because the
// main window keeps refreshing behind it, and because a modal loop would need
// its own message pump, which is where this kind of code usually goes wrong.
func openKeyPanel() {
	if keyWin != nil && keyWin.hwnd != 0 {
		pSetForegroundWindow.Call(keyWin.hwnd)
		keyReload()
		return
	}
	createKeyPanel()
}

func createKeyPanel() {
	class := className(keyClassName)
	if err := registerClass(keyClassName, keyPanelProcCallback); err != nil {
		messageBox(appTitle, err.Error(), mbOK|mbIconError)
		return
	}

	kp := &keyPanel{}
	keyWin = kp

	hwnd := createWindow(class, "SSH keys",
		wsOverlappedWindow|wsCaption, 0,
		260, 180, 760, 600, 0, 0)
	if hwnd == 0 {
		messageBox(appTitle, fmt.Sprintf("cannot create the key window: %v", sysLastError()), mbOK|mbIconError)
		keyWin = nil
		return
	}
	pShowWindow.Call(hwnd, swShowNormal)
	pUpdateWindow.Call(hwnd)
}

// keyPanelProc handles the key window's messages.
func keyPanelProc(hwnd uintptr, m uint32, wParam, lParam uintptr) uintptr {
	switch m {
	case wmCreate:
		keyPanelandCreate(hwnd)
		return 0

	case wmSize:
		// Controls are laid out once they all exist. WM_SIZE arrives while
		// WM_CREATE is still running, and moving handles that have not been
		// created yet would place the ones that have at the wrong offsets.
		// keyPanelLayout is called at the end of creation instead.
		if keyPanelReady() {
			keyPanelLayout()
		}
		return 0

	case wmGetMinMaxInfo:
		info := (*minMaxInfo)(uptrToPtr(lParam))
		info.MinTrackSize = point{X: 560, Y: 440}
		return 0

	case wmCtlColorStatic:
		return onKeyPanelCtlColor(wParam, lParam)

	case wmCommand:
		keyPanelCommand(uint16(wParam & 0xFFFF))
		return 0

	case wmClose:
		// Hiding rather than destroying keeps the list and any half-typed key
		// around if the operator closes the window to look something up.
		// HideWindow is what CloseWindow does; DestroyWindow would force the
		// panel to be rebuilt on every open.
		pShowWindow.Call(hwnd, swHide)
		return 0
	}
	return defWindowProc(hwnd, m, wParam, lParam)
}

func keyPanelandCreate(hwnd uintptr) {
	kp := keyWin
	kp.hwnd = hwnd

	// resolvePath is done once, at creation: it depends only on the account,
	// not on anything the user does in this window.
	path, err := sshauth.KeyFile("")
	if err != nil {
		kp.path = ""
	} else {
		kp.path = path
	}

	kp.pathLabel = createWindow(className("Static"), "…",
		wsChild|wsVisible, 0, 12, 12, 100, 18, hwnd, idKeyPathLabel)
	setFont(kp.pathLabel, gui.fontUI)

	kp.hintLabel = createWindow(className("Static"), "…",
		wsChild|wsVisible, 0, 12, 32, 100, 32, hwnd, idKeyHintLabel)
	setFont(kp.hintLabel, gui.fontUI)

	kp.list = createWindow(className("ListBox"), "",
		wsChild|wsVisible|wsTabStop|wsBorder|wsVScroll|lbsNotify,
		wsExClientEdge,
		12, 70, 100, 120, hwnd, idKeyList)
	setFont(kp.list, gui.fontUI)

	// A multiline edit rather than a single line: a public key is a long
	// string and people paste it, often with the trailing newline intact.
	kp.input = createWindow(className("Edit"), "",
		wsChild|wsVisible|wsTabStop|wsBorder|wsVScroll|
			esMultiline|esAutoVScroll|esWantReturn|esNoHideSel,
		wsExClientEdge,
		12, 200, 100, 100, hwnd, idKeyInput)
	setFont(kp.input, gui.fontUI)

	kp.inputHint = createWindow(className("Static"),
		"Paste the client's public key (the line from id_ed25519.pub or id_rsa.pub):",
		wsChild|wsVisible, 0, 12, 178, 100, 18, hwnd, idKeyInputHint)
	setFont(kp.inputHint, gui.fontUI)

	buttons := []struct {
		label string
		id    uintptr
	}{
		{"Add key", idKeyAdd},
		{"Remove selected", idKeyRemove},
		{"Reload", idKeyReload},
		{"Close", idKeyClose},
	}
	for _, b := range buttons {
		h := createWindow(className("Button"), b.label,
			wsChild|wsVisible|wsTabStop|bsPushButton,
			0, 0, 0, 130, 30, hwnd, b.id)
		setFont(h, gui.fontUI)
		kp.btns = append(kp.btns, h)
	}

	// Every control now exists, so WM_SIZE may lay them out. Then lay them out
	// once explicitly: the WM_SIZE that mattered already arrived and was
	// ignored, and the window would otherwise keep its creation-time geometry
	// until the user resized it.
	kp.ready = true
	keyPanelLayout()

	go keyReloadQuiet()
}

// keyPanelLayout places the controls. Called from WM_SIZE, so the geometry has
// to come from the client rect rather than from fixed numbers.
func keyPanelLayout() {
	kp := keyWin
	if kp == nil || kp.hwnd == 0 {
		return
	}
	r := clientRect(kp.hwnd)
	w := r.Right - r.Left
	h := r.Bottom - r.Top

	const (
		margin   int32 = 14
		rowH     int32 = 18
		gap      int32 = 6
		btnW     int32 = 130
		btnH     int32 = 30
		listMin  int32 = 70
		inputMin int32 = 70
	)

	inner := w - 2*margin
	if inner < 300 {
		inner = 300
	}

	y := margin
	moveWindow(kp.pathLabel, margin, y, inner, rowH)
	y += rowH

	moveWindow(kp.hintLabel, margin, y, inner, 2*rowH)
	y += 2*rowH + gap

	// The button row is pinned to the bottom, and the list and input box split
	// what is left. Giving each a minimum keeps a short window usable.
	btnY := h - margin - btnH
	available := btnY - margin - y - 2*gap

	listH := available/2 - rowH - gap
	inputH := available - listH - rowH - 2*gap
	if listH < listMin {
		listH = listMin
	}
	if inputH < inputMin {
		inputH = inputMin
	}

	moveWindow(kp.list, margin, y, inner, listH)
	y += listH + gap

	moveWindow(kp.inputHint, margin, y, inner, rowH)
	y += rowH

	moveWindow(kp.input, margin, y, inner, inputH)

	// Right-align the buttons.
	x := w - margin
	for i := len(kp.btns) - 1; i >= 0; i-- {
		x -= btnW
		moveWindow(kp.btns[i], x, btnY, btnW, btnH)
		x -= gap
	}
}

func onKeyPanelCtlColor(hdc, control uintptr) uintptr {
	setBkMode(hdc, transparent)
	if control == keyWin.hintLabel || control == keyWin.pathLabel {
		setTextColor(hdc, colMuted)
	} else {
		setTextColor(hdc, colText)
	}
	return gui.bgBrush
}

func keyPanelCommand(id uint16) {
	kp := keyWin
	if kp == nil {
		return
	}
	kp.mu.Lock()
	busy := kp.busy
	kp.mu.Unlock()
	if busy {
		return
	}

	switch id {
	case idKeyAdd:
		go keyAdd()
	case idKeyRemove:
		go keyRemove()
	case idKeyReload:
		go keyReload()
	case idKeyClose:
		pShowWindow.Call(kp.hwnd, swHide)
	}
}

// keyReload refreshes the panel from the file on disk.
//
// The read is done on a goroutine because the elevated path blocks on a UAC
// prompt, and running that on the UI thread would freeze the window before it
// had even painted.
func keyReload() { keyReloadWith(true) }

// keyReloadQuiet refreshes without offering to elevate.
//
// This is what happens when the panel first opens. Popping a UAC prompt the
// moment a window appears, before the user has asked for anything, is the kind
// of behaviour that makes people stop using a tool — so the first look is
// best-effort and the prompt comes when a key is actually added.
func keyReloadQuiet() { keyReloadWith(false) }

func keyReloadWith(mayElevate bool) {
	kp := keyWin
	if kp == nil {
		return
	}

	path := kp.path
	if path == "" {
		keySetStatus(kp.pathLabel, "Cannot determine where sshd keeps its keys.")
		return
	}

	contents, err := readFileString(path)
	if err != nil && !os.IsNotExist(err) {
		if mayElevate && sshauth.NeedsElevation(path) {
			contents, err = sshauth.ReadElevated(path)
		} else if sshauth.NeedsElevation(path) {
			// Expected on the administrator path: the file is readable only
			// with elevation, so the panel starts empty and says why.
			keySetRows(kp, nil, "")
			keySetStatus(kp.pathLabel, path)
			keySetHint(kp, `Press "Reload" to read it (needs elevation). `+keyHintFor(path))
			return
		}
	}
	if err != nil {
		keySetRows(kp, nil, "")
		keySetStatus(kp.pathLabel, path)
		keySetHint(kp, "Cannot read the file: "+err.Error())
		return
	}

	keys := parseKeyContents(contents)
	keySetRows(kp, keys, contents)

	keySetStatus(kp.pathLabel, path)
	keySetHint(kp, keyHintFor(path))
}

// keyAdd validates what was pasted and writes it.
//
// Several keys may be pasted at once — the common case is copying a whole
// authorized_keys file off another machine — so every line is parsed and the
// unrecognised ones are reported rather than silently dropped.
func keyAdd() {
	kp := keyWin
	if kp == nil {
		return
	}
	if !keyTryBegin(kp) {
		return
	}
	defer keyEnd(kp)

	text := windowText(kp.input)
	if strings.TrimSpace(text) == "" {
		messageBoxAt(kp.hwnd, "Paste a public key first.", mbOK|mbIconInformation)
		return
	}

	candidates, rejected := parseKeyLines(text)
	if len(candidates) == 0 {
		messageBoxAt(kp.hwnd,
			"That does not look like a public key.\n\n"+
				"A key is one line, starting with the type:\n\n"+
				"  ssh-ed25519 AAAA… user@host\n\n"+
				"Make sure the whole line was copied, including the\n"+
				"'ssh-ed25519' or 'ssh-rsa' part at the front.",
			mbOK|mbIconWarning)
		return
	}

	existing := parseKeyContents(kp.raw)
	updated, added, err := sshauth.AddAll(existing, kp.raw, candidates)
	if err != nil {
		messageBoxAt(kp.hwnd, err.Error(), mbOK|mbIconError)
		return
	}
	if len(added) == 0 {
		keySetHint(kp, "Already installed — nothing to do.")
		keySetRows(kp, existing, kp.raw)
		return
	}

	keySetHint(kp, "Waiting for the elevation prompt…")
	if err := writeKeyFile(kp.path, updated); err != nil {
		keySetHint(kp, "Not written: "+err.Error())
		messageBoxAt(kp.hwnd, "Could not install the key:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}

	// Show the contents that was just written rather than reading the file
	// back. For the administrator path the file cannot be read without
	// elevating again, so a read-back would mean a second UAC prompt for
	// information the panel already has. The write either succeeded — in which
	// case the file holds exactly this — or it reported an error above.
	keySetRows(kp, parseKeyContents(updated), updated)

	summary := fmt.Sprintf("Installed %d key(s).", len(added))
	if len(rejected) > 0 {
		summary += fmt.Sprintf(" %d line(s) ignored.", len(rejected))
	}
	keySetHint(kp, summary)
	setText(kp.input, "")
	flashKeyPanel(summary)
}

// keyRemove deletes the selected key.
func keyRemove() {
	kp := keyWin
	if kp == nil {
		return
	}

	idx := int(int32(selectedKeyIndex(kp.list)))
	if idx < 0 || idx >= len(kp.keys) {
		messageBoxAt(kp.hwnd, "Select a key in the list first.", mbOK|mbIconInformation)
		return
	}
	victim := kp.keys[idx]

	// Removing the last way in would lock the operator out, so it is worth
	// naming the key they are about to delete.
	if messageBoxAt(kp.hwnd,
		fmt.Sprintf("Remove this key?\n\n%s\n%s\n\n"+
			"Anyone using it will no longer be able to log in to this machine.",
			victim.Label(), victim.Fingerprint),
		mbYesNo|mbIconQuestion) != idYes {
		return
	}

	if !keyTryBegin(kp) {
		return
	}
	defer keyEnd(kp)

	updated := sshauth.Remove(kp.raw, victim.Fingerprint)
	if updated == kp.raw {
		keySetHint(kp, "That key was not in the file — nothing changed.")
		return
	}
	if err := writeKeyFile(kp.path, updated); err != nil {
		keySetHint(kp, "Not written: "+err.Error())
		messageBoxAt(kp.hwnd, "Could not remove the key:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}

	// As with Add: the contents already in hand is what the file now holds, and
	// reading it back would raise a second UAC prompt for nothing.
	keySetRows(kp, parseKeyContents(updated), updated)
	keySetHint(kp, "Removed "+victim.Label()+".")
}

// --- helpers -------------------------------------------------------------

// writeKeyFile writes the key file, raising a UAC prompt if it must.
func writeKeyFile(path, contents string) error {
	if !sshauth.NeedsElevation(path) {
		if err := atomicWrite(path, contents); err != nil {
			return err
		}
		return sshauth.TightenACL(path)
	}
	return sshauth.RunElevated(path, contents)
}

func keyHintFor(path string) string {
	if strings.Contains(strings.ToLower(path), "programdata") {
		return "This account is an administrator, so sshd reads the key file in " +
			"ProgramData rather than ~/.ssh. Writing it needs elevation, and " +
			"Windows will ask when you add a key."
	}
	return "sshd reads this account's own key file."
}

// keySetRows replaces the list contents.
func keySetRows(kp *keyPanel, keys []sshauth.Key, raw string) {
	kp.mu.Lock()
	kp.keys = sshauth.SortedByComment(keys)
	kp.raw = raw
	kp.mu.Unlock()

	if kp.list == 0 {
		return
	}
	pSendMessageW.Call(kp.list, lbResetContent, 0, 0)
	if len(keys) == 0 {
		pSendMessageW.Call(kp.list, lbAddString, 0,
			uintptr(unsafe.Pointer(utf16Ptr("(no keys installed)"))))
		return
	}
	for _, k := range kp.keys {
		line := fmt.Sprintf("%-32s  %s  %s", truncate(k.Label(), 32), k.Algorithm, k.Fingerprint)
		pSendMessageW.Call(kp.list, lbAddString, 0,
			uintptr(unsafe.Pointer(utf16Ptr(line))))
	}
}

func keySetStatus(ctrl uintptr, s string) {
	if ctrl != 0 {
		setText(ctrl, s)
	}
}

func keySetHint(kp *keyPanel, s string) {
	keySetStatus(kp.hintLabel, s)
}

func selectedKeyIndex(list uintptr) uintptr {
	if list == 0 {
		return ^uintptr(0)
	}
	r, _, _ := pSendMessageW.Call(list, lbGetCurSel, 0, 0)
	return r
}

func keyTryBegin(kp *keyPanel) bool {
	kp.mu.Lock()
	defer kp.mu.Unlock()
	if kp.busy {
		return false
	}
	kp.busy = true
	return true
}

func keyEnd(kp *keyPanel) {
	kp.mu.Lock()
	kp.busy = false
	kp.mu.Unlock()
}

func flashKeyPanel(msg string) {
	_ = msg // surfaced through the hint label; the main window's status line is elsewhere
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// messageBoxAt shows a message owned by a specific window, so the prompt
// appears over the key panel rather than over the main window.
func messageBoxAt(owner uintptr, text string, flags uintptr) int {
	r, _, _ := pMessageBoxW.Call(
		owner,
		uintptr(unsafe.Pointer(utf16Ptr(text))),
		uintptr(unsafe.Pointer(utf16Ptr(appTitle))),
		flags,
	)
	return int(r)
}

func parseKeyContents(contents string) []sshauth.Key {
	keys, _ := sshauth.ParseKeys(contents)
	return keys
}

// parseKeyLines parses pasted text into keys, returning the accepted keys and
// the lines that were not understood so they can be reported.
func parseKeyLines(text string) (keys []sshauth.Key, rejected []string) {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, err := sshauth.ParseKey(line)
		if err != nil {
			rejected = append(rejected, line)
			continue
		}
		keys = append(keys, k)
	}
	return keys, rejected
}
