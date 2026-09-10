//go:build windows

// Command sercon-gui is the Windows capture daemon with a window.
//
// It does the same job as "sercond capture" — owns the serial devices, writes
// the per-port logs, serves the socket — but shows what it is doing. On a lab
// machine that someone actually sits at, an invisible background process is the
// wrong shape: there is no way to see whether an adapter is alive, no way to
// find the log, and no obvious way to stop it.
//
// The window must be started in the interactive session. An SSH session and the
// desktop belong to different window stations, so a process spawned from SSH
// could run this code and never be able to draw it.
package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"sercon/internal/audit"
	"sercon/internal/config"
	"sercon/internal/hub"
	"sercon/internal/ipc"
	"sercon/internal/proto"
	"sercon/internal/version"
)

const (
	// The title stays fixed. Putting the port count in it would make the window
	// jump around as adapters come and go, and it makes the window impossible
	// to find by name from a script.
	appTitle    = "sercond — serial console capture"
	classNameID = "sercondGuiWindow"

	idList     = 1001
	idOpenLogs = 1002
	idCopyCmd  = 1003
	idRefresh  = 1004

	timerRefresh = 1
	refreshEvery = 1000 // milliseconds

	rowHeight = 26
)

// Palette. COLORREF is 0x00BBGGRR, which is why these do not read like hex
// colour codes.
const (
	colBackground = 0x00FFFFFF
	colText       = 0x002A2C2C
	colMuted      = 0x00686B6B
	colOnline     = 0x00759E1D
	colOffline    = 0x002D2DA3
	colBusy       = 0x001775BA
)

// Column indices, used by the renderer and the custom-draw colouring.
const (
	colRef = iota
	colState
	colOwner
	colDev
	colBaud
	colObs
	colLog
	colErr
	colCount
)

// wndProcCallback is package-level so the callback trampoline is never
// collected while the window class still points at it.
var wndProcCallback = syscall.NewCallback(wndProc)

// init pins the main goroutine to its OS thread for the life of the process.
//
// This is not optional. Win32 delivers a window's messages to the thread that
// created it, and a Go goroutine is free to resume on a different OS thread at
// any scheduling point. Without the lock, CreateWindowExW runs on one thread
// and the message loop ends up on another, with two consequences that both look
// like a broken app: synchronous messages sent during creation (WM_CREATE) work
// fine, while everything posted afterwards — WM_SIZE, WM_TIMER, WM_CLOSE —
// lands in a queue nobody is reading. The window appears, and then nothing in
// it ever updates or responds.
func init() {
	runtime.LockOSThread()
}

type column struct {
	title string
	width int32
}

// columns defines the list layout, left to right in the order an operator
// checks things: which port, whether it is alive, who is on it, then the
// reference material.
//
// Widths total under the default client width on purpose. A horizontal
// scrollbar in a two-row table is pure friction.
var columns = []column{
	{"port", 260},
	{"state", 70},
	{"owner", 150},
	{"device", 90},
	{"baud", 60},
	{"obs", 45},
	{"log", 280},
	{"last error", 200},
}

type guiState struct {
	hwnd     uintptr
	header   uintptr
	subtitle uintptr
	list     uintptr
	status   uintptr
	btns     []uintptr

	fontUI      uintptr
	fontHeading uintptr
	bgBrush     uintptr

	manager  *hub.Manager
	listener net.Listener
	sock     string
	logDir   string

	// rows is the port references currently in the list, in order. It is what
	// lets refresh tell "same ports, changed values" apart from "the set of
	// ports changed", which decides whether the operator's selection can be
	// preserved.
	rows []string
	// snapshot is the same table the list is showing, kept so the custom-draw
	// handler can colour a cell without querying the manager per paint.
	snapshot []proto.PortInfo

	// flash is a transient message shown in place of the socket path, so that
	// pressing a button produces visible feedback even though the live summary
	// refreshes every second.
	flash      string
	flashUntil time.Time

	// remoteStop is set when a client asked the daemon to shut down. Such a
	// request is deliberate, so the close path skips the confirmation prompt
	// that a human clicking the X would get.
	remoteStop atomic.Bool

	once sync.Once
}

var gui *guiState

// guiLog receives lifecycle diagnostics.
//
// A GUI-subsystem binary has no console, so stderr goes nowhere. Writing to a
// file next to the socket keeps the information reachable when the window
// exists but something in it misbehaves.
var guiLog *os.File

func dbg(format string, args ...any) {
	if guiLog == nil {
		return
	}
	fmt.Fprintf(guiLog, time.Now().Format("2006-01-02 15:04:05.000 ")+format+"\n", args...)
}

func main() {
	// DPI awareness has to be set before any window exists, or Windows scales
	// the result and the text goes soft.
	pSetProcessDPIAware.Call()

	openLog()

	if err := runGUI(); err != nil {
		messageBox(appTitle, err.Error(), mbOK|mbIconError)
		os.Exit(1)
	}
}

func openLog() {
	dir, _, err := ipc.Endpoint()
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "gui.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		guiLog = f
	}
}

func runGUI() error {
	icc := initCommonControlsEx{
		Size: uint32(unsafe.Sizeof(initCommonControlsEx{})),
		ICC:  iccListViewClasses,
	}
	if r, _, errno := pInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&icc))); r == 0 {
		return fmt.Errorf("InitCommonControlsEx: %v", errno)
	}

	if err := becomeDaemon(); err != nil {
		return err
	}

	if err := registerClass(classNameID, wndProcCallback); err != nil {
		return err
	}

	class := className(classNameID)
	// Explicit coordinates rather than CW_USEDEFAULT: the constant is a
	// negative int32, and sign-extending it through a uintptr argument is a
	// needless way to be at the mercy of how the callee reads its parameters.
	hwnd := createWindow(class, appTitle,
		wsOverlappedWindow, 0,
		120, 80, 1240, 620,
		0, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW: %v", syscall.GetLastError())
	}

	pShowWindow.Call(hwnd, swShowNormal)
	pUpdateWindow.Call(hwnd)

	// A client running "sercond stop" must actually stop this window, not just
	// get an acknowledgement. Without this the daemon keeps running and the
	// caller times out wondering why.
	go func() {
		<-gui.manager.ShutdownRequested()
		dbg("shutdown requested by client, closing the window")
		gui.remoteStop.Store(true)
		pPostMessageW.Call(hwnd, wmClose, 0, 0)
	}()

	messageLoop()
	return nil
}

// becomeDaemon claims the socket and starts capturing.
//
// Binding is the single-instance gate. Losing the race is worth reporting
// clearly: the likely cause is that a headless "sercond capture" is already
// running, and the operator needs to know that rather than wonder why the
// window shows nothing.
func becomeDaemon() error {
	dir, sock, err := ipc.Endpoint()
	if err != nil {
		return err
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}

	m, err := hub.New(hub.OptionsFrom(cfg))
	if err != nil {
		return err
	}

	ln, err := ipc.Listen(dir, sock)
	if err != nil {
		return fmt.Errorf("%w\n\nAnother capture daemon already owns this machine's socket.\n"+
			"Stop it first ('sercond stop'), or close its window.", err)
	}
	if err := m.Start(); err != nil {
		ln.Close()
		return err
	}

	m.Audit().Log(audit.Event{
		Event:  audit.EventDaemonStart,
		Detail: fmt.Sprintf("pid=%d gui socket=%s", os.Getpid(), sock),
	})
	dbg("daemon started %s pid=%d socket=%s logdir=%s",
		version.Short(), os.Getpid(), sock, cfg.LogDir)

	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() { _ = m.ServeConn(c, "local") }()
		}
	}()

	gui = &guiState{
		manager:  m,
		listener: ln,
		sock:     sock,
		logDir:   cfg.LogDir,
	}
	return nil
}

func messageLoop() {
	var m msg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func wndProc(hwnd uintptr, m uint32, wParam, lParam uintptr) uintptr {
	switch m {
	case wmCreate:
		onCreate(hwnd)
		return 0
	case wmSize:
		onSize()
		return 0
	case wmTimer:
		if wParam == timerRefresh {
			refresh()
		}
		return 0
	case wmNotify:
		return onNotify(lParam)
	case wmCtlColorStatic:
		return onCtlColor(wParam, lParam)
	case wmCommand:
		onCommand(uint16(wParam & 0xFFFF))
		return 0
	case wmGetMinMaxInfo:
		info := (*minMaxInfo)(uptrToPtr(lParam))
		info.MinTrackSize = point{X: 820, Y: 400}
		return 0
	case wmClose:
		dbg("WM_CLOSE")
		if !gui.remoteStop.Load() && !confirmExit() {
			return 0
		}
		// DestroyWindow delivers WM_DESTROY, which does the real teardown and
		// ends the message loop.
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		dbg("WM_DESTROY")
		shutdown()
		pPostQuitMessage.Call(0)
		return 0
	}
	return defWindowProc(hwnd, m, wParam, lParam)
}

func onCreate(hwnd uintptr) {
	gui.hwnd = hwnd
	gui.fontUI = createUIFont()
	gui.fontHeading = createHeadingFont()
	gui.bgBrush = solidBrush(colBackground)

	// Two lines of hierarchy at the top. Without them the window reads as a
	// bare table with buttons bolted underneath.
	gui.header = createWindow(className("Static"), "Serial console capture · "+version.Full(),
		wsChild|wsVisible, 0, 12, 12, 100, 24, hwnd, 0)
	setFont(gui.header, gui.fontHeading)

	gui.subtitle = createWindow(className("Static"), "starting…",
		wsChild|wsVisible, 0, 12, 38, 100, 18, hwnd, 0)
	setFont(gui.subtitle, gui.fontUI)

	gui.list = createWindow(className("SysListView32"), "",
		wsChild|wsVisible|wsTabStop|lvsReport|lvsSingleSel|lvsShowSelAlways,
		wsExClientEdge,
		0, 0, 100, 100, hwnd, idList)
	setFont(gui.list, gui.fontUI)

	// No gridlines. A grid of hairlines through every cell is the single
	// biggest reason a stock ListView looks dated; full-row selection plus a
	// little vertical padding reads far better and costs nothing.
	ext := uintptr(lvsExFullRowSelect | lvsExDoubleBuffer)
	pSendMessageW.Call(gui.list, lvmSetExtendedListViewStyle, ext, ext)

	// Rows are sized to the small image list, so an empty one is the only
	// supported way to get padding inside a report row.
	setImageList(gui.list, createImageList(rowHeight))

	for i, col := range columns {
		c := lvColumn{
			Mask:     lvcfText | lvcfFmt | lvcfWidth | lvcfSubItem,
			Fmt:      lvcfmtLeft,
			Cx:       col.width,
			ISubItem: int32(i),
			PszText:  utf16Ptr(col.title),
		}
		pSendMessageW.Call(gui.list, lvmInsertColumnW, uintptr(i), uintptr(unsafe.Pointer(&c)))
	}

	labels := []string{"Open log folder", "Copy attach command", "Refresh"}
	ids := []uintptr{idOpenLogs, idCopyCmd, idRefresh}
	for i, label := range labels {
		b := createWindow(className("Button"), label,
			wsChild|wsVisible|wsTabStop|wsGroup,
			0, 0, 0, 170, 32, hwnd, ids[i])
		setFont(b, gui.fontUI)
		gui.btns = append(gui.btns, b)
	}

	gui.status = createWindow(className("Static"), "",
		wsChild|wsVisible, 0, 0, 0, 100, 20, hwnd, 0)
	setFont(gui.status, gui.fontUI)

	pSetTimer.Call(hwnd, timerRefresh, refreshEvery, 0)
	refresh()
}

func onSize() {
	r := clientRect(gui.hwnd)
	w := r.Right - r.Left
	h := r.Bottom - r.Top

	const (
		margin  int32 = 12
		headerH int32 = 24
		subH    int32 = 18
		gap     int32 = 8
		btnH    int32 = 32
		btnW    int32 = 170
		statusH int32 = 20
	)

	headerY := margin
	subY := headerY + headerH
	listY := subY + subH + gap

	statusY := h - margin - statusH
	btnY := statusY - gap - btnH
	listH := btnY - gap - listY

	// A window dragged small enough to invert the layout should still produce a
	// sane control geometry rather than negative widths.
	if listH < 60 {
		listH = 60
	}
	if w-2*margin < 200 {
		w = 2*margin + 200
	}

	moveWindow(gui.header, margin, headerY, w-2*margin, headerH)
	moveWindow(gui.subtitle, margin, subY, w-2*margin, subH)
	moveWindow(gui.list, margin, listY, w-2*margin, listH)

	// Right-align the button row so the controls stay put as the window
	// resizes, instead of drifting with the left edge.
	x := w - margin
	for i := len(gui.btns) - 1; i >= 0; i-- {
		x -= btnW
		moveWindow(gui.btns[i], x, btnY, btnW, btnH)
		x -= gap
	}

	moveWindow(gui.status, margin, statusY, w-2*margin, statusH)
}

// onCtlColor renders the header, subtitle and status line in flat colours on
// the window background. Without it the Static controls paint themselves with
// the default system face colour and the window looks like a settings dialog
// from 2001.
func onCtlColor(hdc, control uintptr) uintptr {
	setBkMode(hdc, transparent)
	if control == gui.subtitle || control == gui.status {
		setTextColor(hdc, colMuted)
	} else {
		setTextColor(hdc, colText)
	}
	return gui.bgBrush
}

// onNotify colours the state and owner cells.
//
// This is where the list stops being plain text: green for a live port, red for
// one that is gone, amber for one somebody else is holding. Those three are the
// questions an operator actually asks when glancing at the window.
//
// The three-stage dance is not optional. In a report ListView the CDDS_ITEMPREPAINT
// callback is per *row*, and iSubItem is always zero there — colouring from it
// would tint the whole row and could never reach the state column. Per-cell
// colouring only happens after asking for subitem callbacks, which is what
// returning CDRF_NOTIFYSUBITEMDRAW from the item stage does.
func onNotify(lParam uintptr) uintptr {
	hdr := (*nmhdr)(uptrToPtr(lParam))
	if hdr.HwndFrom != gui.list || hdr.Code != nmCustomDraw {
		return 0
	}

	cd := (*nmlvCustomDraw)(uptrToPtr(lParam))
	switch cd.Nmcd.DwDrawStage {
	case cddsPrePaint:
		return cdrfNotifyItemDraw

	case cddsItemPrePaint:
		return cdrfNotifySubItemDraw

	case cddsSubItemPrePaint:
		row := int(int32(cd.Nmcd.DwItemSpec))
		if row < 0 || row >= len(gui.snapshot) {
			return cdrfDoDefault
		}
		if color, ok := stateColor(gui.snapshot[row], int(cd.ISubItem)); ok {
			cd.ClrText = color
		}
		return cdrfNewFont
	}
	return cdrfDoDefault
}

func stateColor(p proto.PortInfo, col int) (uint32, bool) {
	switch col {
	case colState:
		if p.Online {
			return colOnline, true
		}
		return colOffline, true
	case colOwner:
		if p.Owner != "" {
			return colBusy, true
		}
	}
	return 0, false
}

// refresh reconciles the list with the daemon's port table.
//
// Rebuilding the rows on every tick would clear the operator's selection once a
// second, which makes "select a port, copy its attach command" unusable. So the
// list is only rebuilt when the set of ports actually changes; otherwise the
// cells are updated in place and the selection survives.
func refresh() {
	if gui == nil || gui.list == 0 {
		return
	}

	ports := gui.manager.Ports()
	refs := make([]string, len(ports))
	for i, p := range ports {
		refs[i] = p.Ref
	}

	if !sameRefs(refs, gui.rows) {
		pSendMessageW.Call(gui.list, lvmSetRedraw, 0, 0)
		pSendMessageW.Call(gui.list, lvmDeleteAllItems, 0, 0)
		for i := range ports {
			item := lvItem{Mask: lvifText, IItem: int32(i), ISubItem: 0}
			item.PszText = utf16Ptr(ports[i].Ref)
			pSendMessageW.Call(gui.list, lvmInsertItemW, 0, uintptr(unsafe.Pointer(&item)))
		}
		pSendMessageW.Call(gui.list, lvmSetRedraw, 1, 0)
		gui.rows = refs
	}
	gui.snapshot = ports

	for i := range ports {
		for col := range columns {
			item := lvItem{Mask: lvifText, IItem: int32(i), ISubItem: int32(col)}
			item.PszText = utf16Ptr(cell(ports[i], col))
			pSendMessageW.Call(gui.list, lvmSetItemW, 0, uintptr(unsafe.Pointer(&item)))
		}
	}

	online, held, observing := 0, 0, 0
	for _, p := range ports {
		if p.Online {
			online++
		}
		if p.Owner != "" {
			held++
		}
		observing += p.Observers
	}

	summary := fmt.Sprintf("%d ports · %d online · %d writable · %d observing",
		len(ports), online, held, observing)
	if len(ports) == 0 {
		summary = "capturing · no serial ports found"
	}
	setText(gui.subtitle, summary)

	if gui.flash != "" && time.Now().Before(gui.flashUntil) {
		setText(gui.status, gui.flash)
	} else {
		gui.flash = ""
		setText(gui.status, gui.sock)
	}
}

// flash shows a transient message in the status line.
func flash(msg string) {
	gui.flash = msg
	gui.flashUntil = time.Now().Add(6 * time.Second)
	setText(gui.status, msg)
}

func cell(p proto.PortInfo, col int) string {
	switch col {
	case colRef:
		return p.Ref
	case colState:
		if p.Online {
			return "online"
		}
		return "offline"
	case colOwner:
		return orDash(p.Owner)
	case colDev:
		return p.Dev
	case colBaud:
		return fmt.Sprintf("%d", p.Baud)
	case colObs:
		if p.Observers > 0 {
			return fmt.Sprintf("%d", p.Observers)
		}
		return "-"
	case colLog:
		return orDash(p.Log)
	case colErr:
		return orDash(p.LastErr)
	}
	return ""
}

func onCommand(id uint16) {
	switch id {
	case idOpenLogs:
		if err := os.MkdirAll(gui.logDir, 0o700); err != nil {
			messageBox(appTitle, fmt.Sprintf("Cannot create %s:\n%v", gui.logDir, err), mbOK|mbIconError)
			return
		}
		if err := shellOpen(gui.logDir); err != nil {
			messageBox(appTitle, err.Error(), mbOK|mbIconError)
			return
		}
		flash("opened " + gui.logDir)

	case idCopyCmd:
		cmd, ok := attachCommand()
		if !ok {
			messageBox(appTitle, "Select a port in the list first.", mbOK|mbIconInformation)
			return
		}
		if err := setClipboardText(cmd); err != nil {
			messageBox(appTitle, err.Error(), mbOK|mbIconError)
			return
		}
		flash("copied: " + cmd)

	case idRefresh:
		refresh()
		flash("refreshed")
	}
}

// attachCommand builds the line the operator would run on their own machine,
// which is the single most useful thing this window can hand over.
func attachCommand() (string, bool) {
	idx, _, _ := pSendMessageW.Call(gui.list, lvmGetNextItem, ^uintptr(0), lvniSelected)
	i := int(int32(idx))
	if i < 0 || i >= len(gui.rows) {
		return "", false
	}

	user := firstNonEmpty(os.Getenv("USERNAME"), os.Getenv("USER"))
	host, _ := os.Hostname()
	return fmt.Sprintf("sercon attach -t %s@%s %s", user, host, shellQuote(gui.rows[i])), true
}

func confirmExit() bool {
	ports := gui.manager.Ports()
	attached := 0
	for _, p := range ports {
		if p.Owner != "" {
			attached++
		}
		attached += p.Observers
	}
	if attached == 0 {
		return true
	}
	return messageBox(appTitle,
		fmt.Sprintf("%d client session(s) are attached to a serial port.\n\n"+
			"Closing this window stops the daemon and disconnects them.\n\nContinue?",
			attached),
		mbYesNo|mbIconQuestion) == idYes
}

// shutdown is idempotent: WM_CLOSE leads to WM_DESTROY, and the message loop
// can also end on its own, so it may be reached more than once.
func shutdown() {
	if gui == nil {
		return
	}
	gui.once.Do(func() {
		if gui.listener != nil {
			_ = gui.listener.Close()
		}
		if gui.manager != nil {
			_ = gui.manager.Close()
		}
		if gui.sock != "" {
			_ = os.Remove(gui.sock)
		}
		dbg("shutdown complete")
	})
}

func sameRefs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return "user"
}

// shellQuote protects a port reference that contains something the remote shell
// would otherwise interpret. COM names and by-id names never need it, but the
// function is the difference between "works" and "works for the refs I thought
// of".
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		safe := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '/' || r == '.' || r == '_' || r == '-' || r == '@' ||
			r == ':' || r == ',' || r == '+' || r == '='
		if !safe {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
