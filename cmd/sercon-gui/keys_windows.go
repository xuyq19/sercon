//go:build windows

// The SSH key panel installs and removes public keys used by sshd.
package main

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"sercon/internal/sshauth"
)

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

	// This timer is private to the key panel and must not collide with the
	// main window's refresh and animation timers.
	keyPanelTimerID       = 41
	keyPanelPollInterval  = 50
	keyCompletionCapacity = 8
)

type keyJobKind uint8

const (
	keyJobReload keyJobKind = iota
	keyJobAdd
	keyJobRemove
)

// keyCompletion contains only Go values produced by a worker. It is consumed
// by keyPanelProc on the window-owning UI thread before any control is touched.
type keyCompletion struct {
	generation uint64
	kind       keyJobKind
	path       string
	keys       []sshauth.Key
	raw        string
	added      int
	rejected   int
	err        error
	hint       string
}

// keyPanel owns all key window model state. Except for completions, every field
// is accessed only by the UI thread.
type keyPanel struct {
	hwnd        uintptr
	list        uintptr
	input       uintptr
	pathLabel   uintptr
	hintLabel   uintptr
	inputHint   uintptr
	btns        []uintptr
	path        string
	keys        []sshauth.Key
	raw         string
	job         keyJobState
	ready       bool
	destroyed   bool
	completions chan keyCompletion
}

var keyWin *keyPanel

func keyPanelReady() bool { return keyWin != nil && keyWin.ready && !keyWin.destroyed }

// openKeyPanel opens an existing panel without performing a privileged read on
// the message thread. The initial view is always a non-elevated background job.
func openKeyPanel() {
	if keyWin != nil && keyWin.hwnd != 0 && !keyWin.destroyed {
		pShowWindow.Call(keyWin.hwnd, swShowNormal)
		pSetForegroundWindow.Call(keyWin.hwnd)
		keyPanelStartReload(keyWin, false)
		return
	}
	createKeyPanel()
}

func createKeyPanel() {
	if err := registerClass(keyClassName, keyPanelProcCallback); err != nil {
		messageBox(appTitle, err.Error(), mbOK|mbIconError)
		return
	}
	keyWin = &keyPanel{completions: make(chan keyCompletion, keyCompletionCapacity)}
	hwnd := createWindow(className(keyClassName), "SSH keys", wsOverlappedWindow|wsCaption, 0, 260, 180, 760, 600, 0, 0)
	if hwnd == 0 {
		messageBox(appTitle, fmt.Sprintf("cannot create the key window: %v", sysLastError()), mbOK|mbIconError)
		keyWin = nil
		return
	}
	pShowWindow.Call(hwnd, swShowNormal)
	pUpdateWindow.Call(hwnd)
}

func keyPanelProc(hwnd uintptr, m uint32, wParam, lParam uintptr) uintptr {
	defer guardCallback("keyPanelProc", msgName(m), fmt.Sprintf("wParam=%#x lParam=%#x", wParam, lParam))
	switch m {
	case wmCreate:
		keyPanelCreate(hwnd)
		return 0
	case wmSize:
		if keyPanelReady() {
			keyPanelLayout()
		}
		return 0
	case wmDpiChanged:
		keyPanelOnDpiChanged(hwnd, (*rect)(uptrToPtr(lParam)))
		return 0
	case wmGetMinMaxInfo:
		(*minMaxInfo)(uptrToPtr(lParam)).MinTrackSize = point{X: 560, Y: 440}
		return 0
	case wmCtlColorStatic:
		return onKeyPanelCtlColor(wParam, lParam)
	case wmCommand:
		keyPanelCommand(uint16(wParam & 0xffff))
		return 0
	case wmTimer:
		if wParam == keyPanelTimerID {
			keyPanelDrainCompletions()
		}
		return 0
	case wmClose:
		pShowWindow.Call(hwnd, swHide)
		return 0
	case wmDestroy:
		if keyWin != nil && keyWin.hwnd == hwnd {
			pKillTimer.Call(hwnd, keyPanelTimerID)
			keyWin.destroyed = true
			keyWin.hwnd = 0
			keyWin = nil
		}
		return 0
	}
	return defWindowProc(hwnd, m, wParam, lParam)
}

func keyPanelOnDpiChanged(hwnd uintptr, suggested *rect) {
	if suggested != nil {
		setWindowPos(hwnd, *suggested)
	}
	// Fonts are process-wide. Rebuild them before reassigning control handles;
	// the main surface is also recreated lazily on its next paint.
	fontDPI = updateFontDPI(hwnd)
	fonts.release()
	initFonts()
	if gui != nil {
		gui.surf.release()
		paint()
	}
	kp := keyWin
	if kp == nil || kp.destroyed {
		return
	}
	for _, control := range append([]uintptr{kp.pathLabel, kp.hintLabel, kp.list, kp.inputHint, kp.input}, kp.btns...) {
		setFont(control, fonts.body)
	}
	keyPanelLayout()
}

func keyPanelCreate(hwnd uintptr) {
	kp := keyWin
	kp.hwnd = hwnd
	path, err := sshauth.KeyFile("")
	if err == nil {
		kp.path = path
	}
	kp.pathLabel = createWindow(className("Static"), "", wsChild|wsVisible, 0, 12, 12, 100, 18, hwnd, idKeyPathLabel)
	kp.hintLabel = createWindow(className("Static"), "", wsChild|wsVisible, 0, 12, 32, 100, 32, hwnd, idKeyHintLabel)
	kp.list = createWindow(className("ListBox"), "", wsChild|wsVisible|wsTabStop|wsBorder|wsVScroll|lbsNotify, wsExClientEdge, 12, 70, 100, 120, hwnd, idKeyList)
	kp.inputHint = createWindow(className("Static"), "Paste the client's public key (the line from id_ed25519.pub or id_rsa.pub):", wsChild|wsVisible, 0, 12, 178, 100, 18, hwnd, idKeyInputHint)
	kp.input = createWindow(className("Edit"), "", wsChild|wsVisible|wsTabStop|wsBorder|wsVScroll|esMultiline|esAutoVScroll|esWantReturn|esNoHideSel, wsExClientEdge, 12, 200, 100, 100, hwnd, idKeyInput)
	for _, button := range []struct {
		label string
		id    uintptr
	}{{"Add key", idKeyAdd}, {"Remove selected", idKeyRemove}, {"Reload", idKeyReload}, {"Close", idKeyClose}} {
		h := createWindow(className("Button"), button.label, wsChild|wsVisible|wsTabStop|bsPushButton, 0, 0, 0, 130, 30, hwnd, button.id)
		kp.btns = append(kp.btns, h)
		setFont(h, fonts.body)
	}
	for _, control := range []uintptr{kp.pathLabel, kp.hintLabel, kp.list, kp.inputHint, kp.input} {
		setFont(control, fonts.body)
	}
	kp.ready = true
	keyPanelLayout()
	pSetTimer.Call(hwnd, keyPanelTimerID, keyPanelPollInterval, 0)
	keyPanelStartReload(kp, false)
}

func keyPanelLayout() {
	kp := keyWin
	if kp == nil || kp.hwnd == 0 || kp.destroyed {
		return
	}
	r := clientRect(kp.hwnd)
	w, h := r.Right-r.Left, r.Bottom-r.Top
	const margin, rowH, gap, btnW, btnH, listMin, inputMin int32 = 14, 18, 6, 130, 30, 70, 70
	inner := w - 2*margin
	if inner < 300 {
		inner = 300
	}
	y := margin
	moveWindow(kp.pathLabel, margin, y, inner, rowH)
	y += rowH
	moveWindow(kp.hintLabel, margin, y, inner, 2*rowH)
	y += 2*rowH + gap
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
	x := w - margin
	for i := len(kp.btns) - 1; i >= 0; i-- {
		x -= btnW
		moveWindow(kp.btns[i], x, btnY, btnW, btnH)
		x -= gap
	}
}

func onKeyPanelCtlColor(hdc, control uintptr) uintptr {
	setBkMode(hdc, transparent)
	if keyWin != nil && (control == keyWin.hintLabel || control == keyWin.pathLabel) {
		setTextColor(hdc, colTextDim)
	} else {
		setTextColor(hdc, colText)
	}
	return bgBrush
}

func keyPanelCommand(id uint16) {
	kp := keyWin
	if kp == nil || kp.destroyed || kp.job.busy {
		return
	}
	switch id {
	case idKeyReload:
		keyPanelStartReload(kp, true)
	case idKeyAdd:
		keyPanelStartAdd(kp)
	case idKeyRemove:
		keyPanelStartRemove(kp)
	case idKeyClose:
		pShowWindow.Call(kp.hwnd, swHide)
	}
}

func keyPanelBegin(kp *keyPanel) uint64 { return kp.job.begin() }

func keyPanelStartReload(kp *keyPanel, mayElevate bool) {
	if kp == nil || kp.job.busy || kp.destroyed {
		return
	}
	generation, path := keyPanelBegin(kp), kp.path
	keySetHint(kp, map[bool]string{true: "Waiting for the elevation prompt…", false: "Loading keys…"}[mayElevate])
	go func() { kp.completions <- loadKeyCompletion(generation, path, mayElevate) }()
}

func loadKeyCompletion(generation uint64, path string, mayElevate bool) keyCompletion {
	result := keyCompletion{generation: generation, kind: keyJobReload, path: path}
	if path == "" {
		result.err = fmt.Errorf("cannot determine where sshd keeps its keys")
		return result
	}
	contents, err := readFileString(path)
	if err != nil && !os.IsNotExist(err) && mayElevate && sshauth.NeedsElevation(path) {
		contents, err = sshauth.ReadElevated(path)
	}
	if err != nil && sshauth.NeedsElevation(path) && !mayElevate {
		result.hint = `Press "Reload" to read it (needs elevation). ` + keyHintFor(path)
		return result
	}
	if err != nil {
		result.err = err
		return result
	}
	result.raw, result.keys, result.hint = contents, parseKeyContents(contents), keyHintFor(path)
	return result
}

func keyPanelStartAdd(kp *keyPanel) {
	text := windowText(kp.input)
	if strings.TrimSpace(text) == "" {
		messageBoxAt(kp.hwnd, "Paste a public key first.", mbOK|mbIconInformation)
		return
	}
	candidates, rejected := parseKeyLines(text)
	if len(candidates) == 0 {
		messageBoxAt(kp.hwnd, "That does not look like a public key. Paste a complete ssh-ed25519 or ssh-rsa public-key line.", mbOK|mbIconWarning)
		return
	}
	generation, path, raw := keyPanelBegin(kp), kp.path, kp.raw
	keySetHint(kp, "Waiting for the elevation prompt…")
	go func() { kp.completions <- addKeyCompletion(generation, path, raw, candidates, len(rejected)) }()
}

func addKeyCompletion(generation uint64, path, raw string, candidates []sshauth.Key, rejected int) keyCompletion {
	result := keyCompletion{generation: generation, kind: keyJobAdd, path: path, rejected: rejected}
	updated, added, err := sshauth.AddAll(parseKeyContents(raw), raw, candidates)
	if err != nil {
		result.err = err
		return result
	}
	if len(added) == 0 {
		result.raw, result.keys, result.hint = raw, parseKeyContents(raw), "Already installed — nothing to do."
		return result
	}
	if err = writeKeyFile(path, updated); err != nil {
		result.err = err
		return result
	}
	result.raw, result.keys, result.added = updated, parseKeyContents(updated), len(added)
	result.hint = fmt.Sprintf("Installed %d key(s).", len(added))
	if rejected > 0 {
		result.hint += fmt.Sprintf(" %d line(s) ignored.", rejected)
	}
	return result
}

func keyPanelStartRemove(kp *keyPanel) {
	idx := int(int32(selectedKeyIndex(kp.list)))
	if idx < 0 || idx >= len(kp.keys) {
		messageBoxAt(kp.hwnd, "Select a key in the list first.", mbOK|mbIconInformation)
		return
	}
	victim := kp.keys[idx]
	if messageBoxAt(kp.hwnd, fmt.Sprintf("Remove this key?\n\n%s\n%s\n\nAnyone using it will no longer be able to log in to this machine.", victim.Label(), victim.Fingerprint), mbYesNo|mbIconQuestion) != idYes {
		return
	}
	generation, path, raw := keyPanelBegin(kp), kp.path, kp.raw
	keySetHint(kp, "Waiting for the elevation prompt…")
	go func() { kp.completions <- removeKeyCompletion(generation, path, raw, victim) }()
}

func removeKeyCompletion(generation uint64, path, raw string, victim sshauth.Key) keyCompletion {
	result := keyCompletion{generation: generation, kind: keyJobRemove, path: path}
	updated := sshauth.Remove(raw, victim.Fingerprint)
	if updated == raw {
		result.raw, result.keys, result.hint = raw, parseKeyContents(raw), "That key was not in the file — nothing changed."
		return result
	}
	if err := writeKeyFile(path, updated); err != nil {
		result.err = err
		return result
	}
	result.raw, result.keys, result.hint = updated, parseKeyContents(updated), "Removed "+victim.Label()+"."
	return result
}

func keyPanelDrainCompletions() {
	kp := keyWin
	if kp == nil || kp.destroyed {
		return
	}
	for {
		select {
		case result := <-kp.completions:
			keyPanelApplyCompletion(kp, result)
		default:
			return
		}
	}
}

func keyPanelApplyCompletion(kp *keyPanel, result keyCompletion) {
	if kp.destroyed || !kp.job.accepts(result.generation) {
		return
	}
	kp.job.busy = false
	keySetStatus(kp.pathLabel, result.path)
	if result.err != nil {
		keySetHint(kp, "Not completed: "+result.err.Error())
		messageBoxAt(kp.hwnd, "Could not update SSH keys:\n\n"+result.err.Error(), mbOK|mbIconError)
		return
	}
	keySetRows(kp, result.keys, result.raw)
	keySetHint(kp, result.hint)
	if result.kind == keyJobAdd && result.added > 0 {
		setText(kp.input, "")
	}
}

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
		return "This account is an administrator, so sshd reads the key file in ProgramData. Writing it needs elevation."
	}
	return "sshd reads this account's own key file."
}

func keySetRows(kp *keyPanel, keys []sshauth.Key, raw string) {
	kp.keys, kp.raw = sshauth.SortedByComment(keys), raw
	if kp.list == 0 {
		return
	}
	pSendMessageW.Call(kp.list, lbResetContent, 0, 0)
	if len(kp.keys) == 0 {
		pSendMessageW.Call(kp.list, lbAddString, 0, uintptr(unsafe.Pointer(utf16Ptr("(no keys installed)"))))
		return
	}
	for _, key := range kp.keys {
		line := fmt.Sprintf("%-32s  %s  %s", truncate(key.Label(), 32), key.Algorithm, key.Fingerprint)
		pSendMessageW.Call(kp.list, lbAddString, 0, uintptr(unsafe.Pointer(utf16Ptr(line))))
	}
}

func keySetStatus(control uintptr, text string) {
	if control != 0 {
		setText(control, text)
	}
}
func keySetHint(kp *keyPanel, text string) { keySetStatus(kp.hintLabel, text) }
func selectedKeyIndex(list uintptr) uintptr {
	if list == 0 {
		return ^uintptr(0)
	}
	value, _, _ := pSendMessageW.Call(list, lbGetCurSel, 0, 0)
	return value
}
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if limit <= 1 {
		return text[:limit]
	}
	return text[:limit-1] + "…"
}
func messageBoxAt(owner uintptr, text string, flags uintptr) int {
	value, _, _ := pMessageBoxW.Call(owner, uintptr(unsafe.Pointer(utf16Ptr(text))), uintptr(unsafe.Pointer(utf16Ptr(appTitle))), flags)
	return int(value)
}
func parseKeyContents(contents string) []sshauth.Key {
	keys, _ := sshauth.ParseKeys(contents)
	return keys
}
func parseKeyLines(text string) (keys []sshauth.Key, rejected []string) {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, err := sshauth.ParseKey(line)
		if err != nil {
			rejected = append(rejected, line)
			continue
		}
		keys = append(keys, key)
	}
	return keys, rejected
}
