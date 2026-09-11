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
//
// The client area is painted by hand. Nothing in this package uses a Static,
// Button or ListView control except where a real edit field or message box is
// needed: the sidebar, the table and the buttons are all GDI, because that is
// what allows the hover and transition animations, and because a stock
// ListView cannot draw a rounded capsule or fade a row.
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
	appTitle     = "sercon — serial console capture"
	classNameID  = "serconGuiWindow"
	keyClassName = "serconKeyWindow"

	// Command ids. They are shared by the sidebar entries and the action
	// buttons, which is why the sidebar ids start well clear of the buttons'.
	idOpenLogs = 1001
	idCopyCmd  = 1002
	idRefresh  = 1003
	idKeys     = 1004

	// Sidebar section ids. Only "Ports" is implemented; the others are visible
	// as the shape of what this window is for, and are inert.
	idRailPorts    = 1101
	idRailLogs     = 1102
	idRailKeys     = 1103
	idRailSettings = 1104

	timerRefresh = 1

	refreshEvery = 1000 // milliseconds
)

// wndProcCallback is package-level so the callback trampoline is never
// collected while the window class still points at it.
var wndProcCallback = syscall.NewCallback(wndProc)

// keyPanelProcCallback is the same arrangement for the key panel's class.
var keyPanelProcCallback = syscall.NewCallback(keyPanelProc)

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

type guiState struct {
	hwnd uintptr

	fontsReady bool

	// lyt is recomputed on every resize and read by the paint and hit-test
	// paths. Nothing else computes control positions.
	lyt layout

	// surf is the off-screen buffer every repaint is composed into.
	surf surface

	manager  *hub.Manager
	listener net.Listener
	sock     string
	logDir   string

	// ports mirrors the manager's table for the paint path, so a repaint never
	// takes the manager lock.
	ports []proto.PortInfo

	// selected is the port reference the operator has chosen, kept by
	// reference rather than by row index so it survives rows moving.
	selected string

	// flash is a transient message shown in the footer, so that pressing a
	// button produces visible feedback even though the live summary refreshes
	// every second.
	flashMsg   string
	flashUntil time.Time

	// remoteStop is set when a client asked the daemon to shut down. Such a
	// request is deliberate, so the close path skips the confirmation prompt
	// that a human clicking the X would get.
	remoteStop atomic.Bool

	painting bool
	once     sync.Once
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
	case wmEraseBkgnd:
		// Claiming the erase stops the window from painting its background
		// before every repaint, which is what would otherwise show as a flash
		// of the old contents.
		return 1
	case wmPaint:
		onPaint(hwnd)
		return 0
	case wmSize:
		onSize()
		return 0
	case wmTimer:
		onTimer(wParam)
		return 0
	case wmMouseMove:
		onMouseMove(lParam)
		return 0
	case wmMouseLeave:
		onMouseLeave()
		return 0
	case wmLButtonDown:
		onMouseDown(lParam)
		return 0
	case wmLButtonUp:
		onMouseUp(lParam)
		return 0
	case wmSetCursor:
		// A cursor over a clickable area is the cheapest affordance there is.
		if gui != nil && gui.hitTestable(lParam) {
			pLoadCursorW.Call(0, idcHand)
			return 1
		}
		return 0
	case wmGetMinMaxInfo:
		info := (*minMaxInfo)(uptrToPtr(lParam))
		info.MinTrackSize = point{X: 720, Y: 460}
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
		anim.stop()
		gui.surf.release()
		shutdown()
		pPostQuitMessage.Call(0)
		return 0
	}
	return defWindowProc(hwnd, m, wParam, lParam)
}

func onCreate(hwnd uintptr) {
	gui.hwnd = hwnd

	initFonts()
	gui.fontsReady = true

	onSize()

	// The refresh timer fires every second regardless of whether anything
	// changed, which is what keeps the window live when a client attaches. The
	// animation timer is separate and only runs while something moves.
	pSetTimer.Call(hwnd, timerRefresh, refreshEvery, 0)

	refresh()
	paint()
	startDemo()
}

func onSize() {
	if gui == nil || gui.hwnd == 0 {
		return
	}
	r := clientRect(gui.hwnd)
	w, h := r.Right-r.Left, r.Bottom-r.Top
	if w <= 0 || h <= 0 {
		return
	}
	gui.lyt = buildLayout(w, h)
	syncRail(&gui.lyt)
	paint()
}

// onPaint composes the whole window off-screen and blits it in one go.
func onPaint(hwnd uintptr) {
	var ps paintStruct
	dc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	if gui == nil || !gui.fontsReady {
		return
	}
	r := clientRect(hwnd)
	w, h := r.Right-r.Left, r.Bottom-r.Top

	// A repaint can be triggered from inside a paint — the row fades call back
	// into invalidate — so a second entry is dropped rather than drawing into
	// the surface the first one is still composing.
	if gui.painting {
		return
	}
	gui.painting = true
	defer func() { gui.painting = false }()

	if !gui.surf.ensure(hwnd, w, h) {
		// Falling back to direct drawing keeps the window usable if the
		// bitmap could not be allocated, at the cost of the flicker the
		// surface exists to avoid.
		paintInto(dc, gui.lyt, nowMS())
		return
	}

	now := nowMS()
	paintInto(gui.surf.dc, gui.lyt, now)
	gui.surf.flush(hwnd)
}

// paintInto renders the whole window into a device context.
func paintInto(dc uintptr, l layout, now int64) {
	fillRect(dc, l.client, colBackground)
	paintContent(dc, &l, gui.ports, gui.sock, gui.flashText(), now)
	paintRail(dc, &l)
}

// paint schedules a repaint.
func paint() {
	if gui == nil || gui.hwnd == 0 {
		return
	}
	pInvalidateRect.Call(gui.hwnd, 0, 0)
}

func onTimer(id uintptr) {
	switch id {
	case timerRefresh:
		refresh()
	case animTimerID:
		now := nowMS()
		// A demo stage that changes a value has to keep changing it, because
		// the refresh tick re-seeds the counters from the real port table.
		demoHold(now)
		moving := anim.tick(now)
		pruneRows()
		traceAnim("frame")
		paint()
		if !moving && !demoHolding() {
			anim.stop()
		}
	case demoTimerID:
		advanceDemo()
	}
}

// demoHolding reports whether the animation clock must keep running for a demo
// stage's sake. Outside a demo it is always false, so the timer stops the
// moment nothing is moving.
func demoHolding() bool {
	if !demoOn {
		return false
	}
	stages := demoStages()
	if demoIndex < 0 || demoIndex >= len(stages) {
		return false
	}
	st := stages[demoIndex]
	return st.hold != nil
}

// --- input -----------------------------------------------------------------

// hitTestable reports whether a point is over something clickable.
func (g *guiState) hitTestable(lParam uintptr) bool {
	p := point{X: int32(int16(lParam & 0xFFFF)), Y: int32(int16((lParam >> 16) & 0xFFFF))}
	if _, ok := g.lyt.railAt(p); ok {
		return true
	}
	if g.lyt.buttonAt(p) >= 0 {
		return true
	}
	return g.lyt.rowAt(p, len(rows.items)) >= 0
}

// onMouseMove and the other input handlers all resolve the client-area layout
// from gui.lyt, so the trace records both what arrived and what it resolved to.
// Without the resolved point, a hover index that never changes cannot be told
// apart from a message that never arrived.
func onMouseMove(lParam uintptr) {
	if gui == nil {
		return
	}
	x := int32(int16(lParam & 0xFFFF))
	y := int32(int16((lParam >> 16) & 0xFFFF))
	gui.pointerMoved(point{X: x, Y: y})
}

func onMouseLeave() {
	if gui == nil {
		return
	}
	gui.pointerMoved(point{X: -1, Y: -1})
}

// pointerMoved updates the hover state for the rail, the buttons and the rows,
// then starts the animation clock if anything changed.
func (g *guiState) pointerMoved(p point) {
	now := nowMS()
	changed := false

	idx := -1
	if _, ok := g.lyt.railAt(p); ok {
		for i := range rail.entries {
			if pointIn(rail.entries[i].item.rect, p) {
				idx = i
				break
			}
		}
	}
	// The hover setter has to be visible in the trace, because the interesting
	// case is a pointer move that leaves the hover index alone: that is the
	// common case, and it is the one that must not schedule frames.
	prevRail, prevBtn, prevRow := rail.hover, btnHoverIndex, hover.index

	if idx != rail.hover {
		rail.hover = idx
		for i := range rail.entries {
			v := 0.0
			if i == idx {
				v = 1
			}
			rail.entries[i].hover.setTarget(v, hoverDur, now)
		}
		changed = true
	}

	bi := g.lyt.buttonAt(p)
	if bi != btnHoverIndex {
		btnHoverIndex = bi
		for i := range buttonHover {
			v := 0.0
			if i == bi {
				v = 1
			}
			buttonHover[i].setTarget(v, hoverDur, now)
		}
		changed = true
	}

	ri := g.lyt.rowAt(p, len(rows.items))
	if ri != hover.index {
		hover.index = ri
		v := 0.0
		if ri >= 0 {
			v = 1
		}
		hover.row.setTarget(v, hoverDurContent, now)
		changed = true
	}

	if animTracing && guiLog != nil && (prevRail != rail.hover || prevBtn != btnHoverIndex || prevRow != hover.index) {
		fmt.Fprintf(guiLog, "hover moved to (%d,%d): rail %d->%d btn %d->%d row %d->%d\n",
			p.X, p.Y, prevRail, rail.hover, prevBtn, btnHoverIndex, prevRow, hover.index)
	}
	// The window's own geometry is printed on the first traced move, so a
	// coordinate mismatch between what was sent and what the window resolved
	// shows up in the same file as the events.
	if animTracing && guiLog != nil && !traceRectLogged {
		traceRectLogged = true
		r := gui.lyt
		fmt.Fprintf(guiLog, "layout rail=(%d,%d)-(%d,%d) body=(%d,%d)-(%d,%d) table=(%d,%d)-(%d,%d) items=%d\n",
			r.rail.Left, r.rail.Top, r.rail.Right, r.rail.Bottom,
			r.body.Left, r.body.Top, r.body.Right, r.body.Bottom,
			r.table.Left, r.table.Top, r.table.Right, r.table.Bottom,
			len(r.items))
		for i := range r.items {
			it := &r.items[i]
			fmt.Fprintf(guiLog, "  item %-10q (%d,%d)-(%d,%d)\n",
				it.label, it.rect.Left, it.rect.Top, it.rect.Right, it.rect.Bottom)
		}
	}

	g.wake(changed)
}

var traceRectLogged bool

// wake starts the animation clock if any relevant element is still moving.
func (g *guiState) wake(changed bool) {
	if !changed {
		return
	}
	elems := railElems()
	elems = append(elems, rowElems()...)
	elems = append(elems, counterElems()...)
	for i := range buttonHover {
		elems = append(elems, &buttonHover[i])
	}
	anim.attach(g.hwnd, elems...)
}

func onMouseDown(lParam uintptr) {
	if gui == nil {
		return
	}
	p := point{X: int32(int16(lParam & 0xFFFF)), Y: int32(int16((lParam >> 16) & 0xFFFF))}

	// Repaint on press so the click has immediate feedback, and keep the
	// message flowing to learn about the release.
	if _, ok := gui.lyt.railAt(p); ok {
		railPress = true
		paint()
	}
	// A click on a row selects it.
	if ri := gui.lyt.rowAt(p, len(rows.items)); ri >= 0 && ri < len(rows.items) {
		gui.selected = rows.items[ri].ref
		paint()
	}
	pSetCapture.Call(gui.hwnd)
}

var railPress bool

func onMouseUp(lParam uintptr) {
	if gui == nil {
		return
	}
	pReleaseCapture.Call()
	railPress = false

	p := point{X: int32(int16(lParam & 0xFFFF)), Y: int32(int16((lParam >> 16) & 0xFFFF))}

	// Release outside the window must not fire the command; the press has to
	// have started on the same control.
	if it, ok := gui.lyt.railAt(p); ok {
		dbg("rail click %q", it.label)
		// Only Ports is wired up; the others are the shape of the window.
		if it.id == idRailKeys {
			openKeyPanel()
		}
		return
	}
	switch bi := gui.lyt.buttonAt(p); bi {
	case 0:
		openLogFolder()
	case 1:
		copyAttachCommand()
	case 2:
		openKeyPanel()
	case 3:
		refresh()
		gui.flash("refreshed", 4*time.Second)
	}
	paint()
}

// --- data ------------------------------------------------------------------

// refresh pulls the port table and reconciles the painted rows.
//
// It repaints only when something actually changed. The timer fires once a
// second whether or not anything moved, and a full repaint of the window on
// every tick is a percent of a core doing nothing — measurable on a laptop, and
// pointless on a window whose contents are identical to the last frame.
func refresh() {
	if gui == nil || gui.hwnd == 0 {
		return
	}

	ports := gui.manager.Ports()
	changed := !samePorts(gui.ports, ports)
	gui.ports = ports

	now := nowMS()
	if syncRows(ports, now) {
		changed = true
	}

	online, _, _ := tally(ports)
	footer := railFooterFor(version.Build(), len(ports), online, loggedSummary(ports))
	if footer != railFooterText {
		setRailFooter(footer)
		changed = true
	}

	// The clock is only started when an animation is actually pending. The
	// flash message counts as one, because it has to appear and then go away.
	if changed || gui.flashActive() {
		gui.wake(true)
		paint()
	}
}

// samePorts reports whether two snapshots describe the same ports with the
// same values. It compares the fields the table actually draws, so a change to
// something invisible does not trigger a repaint.
func samePorts(a, b []proto.PortInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Ref != b[i].Ref || a[i].Online != b[i].Online ||
			a[i].Owner != b[i].Owner || a[i].Observers != b[i].Observers ||
			a[i].LastErr != b[i].LastErr || a[i].Log != b[i].Log ||
			a[i].Dev != b[i].Dev || a[i].Baud != b[i].Baud {
			return false
		}
	}
	return true
}

// loggedSummary is the third line of the rail footer.
func loggedSummary(ports []proto.PortInfo) string {
	if gui != nil && gui.logDir != "" {
		return "logging to disk"
	}
	return "not logging"
}

// flashActive reports whether a transient message is currently on screen, or
// has just expired and needs one more frame to be cleared.
func (g *guiState) flashActive() bool {
	return g.flashMsg != ""
}

func (g *guiState) flashText() string {
	if g.flashMsg != "" && time.Now().Before(g.flashUntil) {
		return g.flashMsg
	}
	g.flashMsg = ""
	return ""
}

func (g *guiState) flash(msg string, d time.Duration) {
	g.flashMsg = msg
	g.flashUntil = time.Now().Add(d)
}

// --- commands --------------------------------------------------------------

func openLogFolder() {
	if err := os.MkdirAll(gui.logDir, 0o700); err != nil {
		messageBox(appTitle, fmt.Sprintf("Cannot create %s:\n%v", gui.logDir, err), mbOK|mbIconError)
		return
	}
	if err := shellOpen(gui.logDir); err != nil {
		messageBox(appTitle, err.Error(), mbOK|mbIconError)
		return
	}
	gui.flash("opened "+gui.logDir, 6*time.Second)
}

func copyAttachCommand() {
	cmd, ok := attachCommand()
	if !ok {
		messageBox(appTitle, "Select a port in the table first.", mbOK|mbIconInformation)
		return
	}
	if err := setClipboardText(cmd); err != nil {
		messageBox(appTitle, err.Error(), mbOK|mbIconError)
		return
	}
	gui.flash("copied: "+cmd, 8*time.Second)
}

// attachCommand builds the line the operator would run on their own machine,
// which is the single most useful thing this window can hand over.
func attachCommand() (string, bool) {
	if gui.selected == "" {
		return "", false
	}
	user := firstNonEmpty(os.Getenv("USERNAME"), os.Getenv("USER"))
	host, _ := os.Hostname()
	return fmt.Sprintf("sercon attach -t %s@%s %s", user, host, shellQuote(gui.selected)), true
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

// versionText is the footer's right-hand label. It is the compact form, because
// the footer's single row already carries the socket path on the left.
func versionText() string { return version.Build() }

// rowIsSelected reports whether a row is the operator's current selection.
func rowIsSelected(ref string) bool { return gui != nil && gui.selected == ref }

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
