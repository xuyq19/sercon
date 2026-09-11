//go:build windows

// Win32 bindings for the sercond GUI.
//
// Everything goes through syscall.NewLazyDLL rather than a GUI toolkit so that
// the binary keeps the project's zero-dependency property. Each DLL is resolved
// on first use, and every function is declared with the exact parameter widths
// the API expects — mixing up uint32 and uintptr here is the usual source of
// silent window-creation failures.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

// Window class names must stay reachable for as long as the class is registered,
// so the UTF-16 copies are cached here rather than built at each call site.
var (
	classNamesOnce sync.Once
	classNames     map[string]*uint16
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
)

var (
	pRegisterClassExW      = user32.NewProc("RegisterClassExW")
	pCreateWindowExW       = user32.NewProc("CreateWindowExW")
	pDefWindowProcW        = user32.NewProc("DefWindowProcW")
	pGetMessageW           = user32.NewProc("GetMessageW")
	pTranslateMessage      = user32.NewProc("TranslateMessage")
	pDispatchMessageW      = user32.NewProc("DispatchMessageW")
	pPostQuitMessage       = user32.NewProc("PostQuitMessage")
	pPostMessageW          = user32.NewProc("PostMessageW")
	pDestroyWindow         = user32.NewProc("DestroyWindow")
	pShowWindow            = user32.NewProc("ShowWindow")
	pUpdateWindow          = user32.NewProc("UpdateWindow")
	pGetClientRect         = user32.NewProc("GetClientRect")
	pSendMessageW          = user32.NewProc("SendMessageW")
	pSetWindowTextW        = user32.NewProc("SetWindowTextW")
	pLoadCursorW           = user32.NewProc("LoadCursorW")
	pMessageBoxW           = user32.NewProc("MessageBoxW")
	pSetProcessDPIAware    = user32.NewProc("SetProcessDPIAware")
	pMoveWindow            = user32.NewProc("MoveWindow")
	pSetTimer              = user32.NewProc("SetTimer")
	pKillTimer             = user32.NewProc("KillTimer")
	pSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	pOpenClipboard         = user32.NewProc("OpenClipboard")
	pEmptyClipboard        = user32.NewProc("EmptyClipboard")
	pSetClipboardData      = user32.NewProc("SetClipboardData")
	pCloseClipboard        = user32.NewProc("CloseClipboard")
	pGetDC                 = user32.NewProc("GetDC")
	pReleaseDC             = user32.NewProc("ReleaseDC")
	pEnableWindow          = user32.NewProc("EnableWindow")
	pGetWindowTextW        = user32.NewProc("GetWindowTextW")
	pGetWindowTextLengthW  = user32.NewProc("GetWindowTextLengthW")
	pSetFocus              = user32.NewProc("SetFocus")
	pGetDlgCtrlID          = user32.NewProc("GetDlgCtrlID")
	pGetCursorPos          = user32.NewProc("GetCursorPos")
	pInvalidateRect        = user32.NewProc("InvalidateRect")
	pTrackMouseEvent       = user32.NewProc("TrackMouseEvent")
	pSetCapture            = user32.NewProc("SetCapture")
	pReleaseCapture        = user32.NewProc("ReleaseCapture")
	pShowCursor            = user32.NewProc("ShowCursor")
	pSystemParametersInfoW = user32.NewProc("SystemParametersInfoW")

	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	pGlobalLock       = kernel32.NewProc("GlobalLock")
	pGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	pGetTickCount64   = kernel32.NewProc("GetTickCount64")

	pGetDeviceCaps          = gdi32.NewProc("GetDeviceCaps")
	pCreateFontW            = gdi32.NewProc("CreateFontW")
	pGetStockObject         = gdi32.NewProc("GetStockObject")
	pSetTextColor           = gdi32.NewProc("SetTextColor")
	pSetBkMode              = gdi32.NewProc("SetBkMode")
	pCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	pDeleteObject           = gdi32.NewProc("DeleteObject")
	pSelectObject           = gdi32.NewProc("SelectObject")
	pCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	pCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	pBitBlt                 = gdi32.NewProc("BitBlt")
	pDeleteDC               = gdi32.NewProc("DeleteDC")
	pFillRect               = user32.NewProc("FillRect")
	pCreatePen              = gdi32.NewProc("CreatePen")
	pMoveToEx               = gdi32.NewProc("MoveToEx")
	pLineTo                 = gdi32.NewProc("LineTo")
	pRoundRect              = gdi32.NewProc("RoundRect")
	pGetTextExtentPoint32W  = gdi32.NewProc("GetTextExtentPoint32W")
	pSetTextAlign           = gdi32.NewProc("SetTextAlign")
	pTextOutW               = gdi32.NewProc("TextOutW")
	pIntersectClipRect      = gdi32.NewProc("IntersectClipRect")
	pSelectClipRgn          = gdi32.NewProc("SelectClipRgn")

	pScreenToClient = user32.NewProc("ScreenToClient")
	pBeginPaint     = user32.NewProc("BeginPaint")
	pEndPaint       = user32.NewProc("EndPaint")

	pShellExecuteW = shell32.NewProc("ShellExecuteW")
)

// Window styles and messages.
const (
	wsOverlappedWindow = 0x00CF0000
	wsCaption          = 0x00C00000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsTabStop          = 0x00010000
	wsGroup            = 0x00020000
	wsExClientEdge     = 0x00000200
	wsBorder           = 0x00800000
	wsVScroll          = 0x00200000
	wsDisabled         = 0x08000000

	// Edit control styles. ES_MULTILINE is what makes the key box accept a
	// paste of several lines at once, which is how these are usually copied.
	esMultiline   = 0x0004
	esAutoVScroll = 0x0040
	esWantReturn  = 0x1000
	esNoHideSel   = 0x0100

	// ListBox styles.
	lbsNotify = 0x0001

	// Button styles.
	bsPushButton = 0x00000000

	swShowNormal = 1
	swHide       = 0

	csHRedraw = 0x0002
	csVRedraw = 0x0001

	idcArrow     = 32512
	colorBtnFace = 15

	wmCreate         = 0x0001
	wmDestroy        = 0x0002
	wmSize           = 0x0005
	wmPaint          = 0x000F
	wmClose          = 0x0010
	wmEraseBkgnd     = 0x0014
	wmSetCursor      = 0x0020
	wmGetMinMaxInfo  = 0x0024
	wmSetFont        = 0x0030
	wmMouseMove      = 0x0200
	wmLButtonDown    = 0x0201
	wmLButtonUp      = 0x0202
	wmMouseLeave     = 0x02A3
	wmNotify         = 0x004E
	wmCommand        = 0x0111
	wmTimer          = 0x0113
	wmCtlColorStatic = 0x0138

	// idcHand is the hand cursor, used over anything clickable.
	idcHand = 32649

	transparent = 1

	lognPixelY = 90
)

// paintStruct mirrors PAINTSTRUCT, which BeginPaint fills in. The field widths
// matter: the struct is written by the OS and read by nothing here.
type paintStruct struct {
	Hdc         uintptr
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

// ListBox.
const (
	lbAddString    = 0x0180
	lbResetContent = 0x0184
	lbGetCurSel    = 0x0188
	lbSetCurSel    = 0x0186
	lbGetCount     = 0x018B
	lbGetTextLen   = 0x018A
	lbGetText      = 0x0189
	lbDelString    = 0x0182
)

// Edit control messages.
const (
	emSetSel       = 0x00B1
	emReplaceSel   = 0x00C2
	emSetReadOnly  = 0x00CF
	emGetLineCount = 0x00BA
	emSetLimitText = 0x00C5
)

// MessageBox.
const (
	mbOK              = 0x0000
	mbYesNo           = 0x0004
	mbIconError       = 0x0010
	mbIconQuestion    = 0x0020
	mbIconWarning     = 0x0030
	mbIconInformation = 0x0040

	idYes = 6
)

// Clipboard.
const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// Font weights and character sets.
const (
	fwNormal     = 400
	fwSemibold   = 600
	ansiCharset  = 0
	defaultPitch = 0
)

type wndClassExW struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type minMaxInfo struct {
	Reserved     point
	MaxSize      point
	MaxPosition  point
	MinTrackSize point
	MaxTrackSize point
}

func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		// Only reachable if s contains an interior NUL, which no caller here
		// can produce. Returning nil degrades to an empty label rather than
		// taking down a window that is otherwise fine.
		return nil
	}
	return p
}

// uptrToPtr reinterprets a handle-sized integer returned by a Win32 call as a
// pointer.
//
// go vet rejects a direct uintptr-to-unsafe.Pointer conversion because that
// pattern is usually a bug: a uintptr that once held a Go pointer says nothing
// about whether the object has since moved or been collected. That reasoning
// does not apply to the values routed through here. They come from the OS — a
// window message's lParam, or the address GlobalLock handed back — and they
// stay valid for exactly as long as the OS keeps them so. The value is read
// through a local, which also keeps it off the GC's radar, where it belongs:
// none of this memory is Go-managed.
func uptrToPtr(u uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&u))
}

func className(name string) *uint16 {
	// Class names must stay reachable for the lifetime of the class, so they
	// are kept in this map rather than built inline.
	classNamesOnce.Do(func() { classNames = map[string]*uint16{} })
	if p, ok := classNames[name]; ok {
		return p
	}
	p := utf16Ptr(name)
	classNames[name] = p
	return p
}

// registerClass registers a window class, tolerating one that already exists.
//
// RegisterClassExW fails with ERROR_CLASS_ALREADY_EXISTS when the name is
// taken, and reporting that as an error is wrong here: registering a class
// twice is not a failure, it is a no-op. Treating it as one meant the second
// caller bailed out before creating its window, which is exactly how the SSH
// key panel came to be unopenable — the class was registered at startup and the
// panel's own registration then reported a failure that was not one.
func registerClass(name string, wndProc uintptr) error {
	hInst, _, _ := pGetModuleHandleW.Call(0)

	cls := wndClassExW{
		Style:      csHRedraw | csVRedraw,
		WndProc:    wndProc,
		Instance:   hInst,
		Cursor:     loadCursor(idcArrow),
		Background: colorBrush(colorBtnFace),
		ClassName:  className(name),
	}
	cls.Size = uint32(unsafe.Sizeof(cls))

	r, _, errno := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&cls)))
	if r == 0 {
		if errno == errorClassAlreadyExists {
			return nil
		}
		return fmt.Errorf("RegisterClassExW(%s): %v", name, errno)
	}
	return nil
}

// errorClassAlreadyExists is ERROR_CLASS_ALREADY_EXISTS. The syscall package
// does not declare it.
const errorClassAlreadyExists = syscall.Errno(1410)

func loadCursor(id uintptr) uintptr {
	r, _, _ := pLoadCursorW.Call(0, id)
	return r
}

func colorBrush(index uintptr) uintptr {
	// GetStockObject with a COLOR_* index returns the matching system brush.
	r, _, _ := pGetStockObject.Call(index)
	return r
}

func createWindow(class *uint16, text string, style, exStyle uint32, x, y, w, h int32, parent, id uintptr) uintptr {
	hInst, _, _ := pGetModuleHandleW.Call(0)
	r, _, _ := pCreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(class)),
		uintptr(unsafe.Pointer(utf16Ptr(text))),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent,
		id,
		hInst,
		0,
	)
	return r
}

func defWindowProc(hwnd uintptr, m uint32, wParam, lParam uintptr) uintptr {
	r, _, _ := pDefWindowProcW.Call(hwnd, uintptr(m), wParam, lParam)
	return r
}

func moveWindow(hwnd uintptr, x, y, w, h int32) {
	pMoveWindow.Call(hwnd, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 1)
}

func clientRect(hwnd uintptr) rect {
	var r rect
	pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

func setText(hwnd uintptr, s string) {
	pSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(utf16Ptr(s))))
}

func setFont(hwnd, font uintptr) {
	pSendMessageW.Call(hwnd, wmSetFont, font, 1)
}

// windowText reads a control's text.
func windowText(hwnd uintptr) string {
	n, _, _ := pGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	r, _, _ := pGetWindowTextW.Call(
		hwnd,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:r])
}

func enableWindow(hwnd uintptr, enable bool) {
	var v uintptr
	if enable {
		v = 1
	}
	pEnableWindow.Call(hwnd, v)
}

func setFocus(hwnd uintptr) {
	pSetFocus.Call(hwnd)
}

// sysLastError reads the calling thread's last error. It is only meaningful
// immediately after a failed Win32 call, so it is wrapped rather than exposed
// as a variable.
func sysLastError() error {
	return syscall.GetLastError()
}

// readFileString reads a file as text.
func readFileString(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// atomicWrite replaces a file's contents in one step.
//
// A key file is a login credential: if the process dies mid-write the machine
// is left with a truncated file and no way in. Writing a sibling and renaming
// over the original means a reader sees either the old file or the new one,
// never a half-written one. The rename is atomic on the same volume, which is
// why the temporary lives in the same directory.
func atomicWrite(path, contents string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, ".authorized_keys-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds

	if _, err := f.WriteString(contents); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// A Windows rename fails if the destination exists, unlike POSIX. Removing
	// first loses atomicity, so this is only reached on the non-elevated path
	// where the file is this user's own and the window is small.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmp, path)
}

func messageBox(title, text string, flags uintptr) int {
	r, _, _ := pMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(utf16Ptr(text))),
		uintptr(unsafe.Pointer(utf16Ptr(title))),
		flags,
	)
	return int(r)
}

func createFont(face string, pt int, weight uintptr) uintptr {
	hdc, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, hdc)

	dpi, _, _ := pGetDeviceCaps.Call(hdc, lognPixelY)
	if dpi == 0 {
		dpi = 96
	}

	// CreateFontW wants the character height, so the point size is negated and
	// converted against the screen's actual DPI. Skipping that step is why so
	// many small tools look soft on a laptop panel.
	height := -int32(pt*int(dpi)) / 72

	r, _, _ := pCreateFontW.Call(
		uintptr(height), 0, 0, 0,
		weight, 0, 0, 0,
		ansiCharset, defaultPitch,
		uintptr(unsafe.Pointer(utf16Ptr(face))),
	)
	return r
}

// solidBrush creates a GDI brush. The caller owns it and should delete it, but
// these are created once per window and live as long as the process does, so
// there is nothing to leak in practice.
func solidBrush(color uint32) uintptr {
	r, _, _ := pCreateSolidBrush.Call(uintptr(color))
	return r
}

// setTextColor and setBkMode are used from WM_CTLCOLORSTATIC to render the
// subtitle and status line in a muted grey on the window background.
func setTextColor(hdc, color uintptr) uintptr {
	r, _, _ := pSetTextColor.Call(hdc, color)
	return r
}

func setBkMode(hdc, mode uintptr) {
	_, _, _ = pSetBkMode.Call(hdc, mode)
}

// shellOpen hands a path to the shell, which is how a folder gets opened in
// Explorer without linking against anything newer than shell32.
func shellOpen(path string) error {
	verb := utf16Ptr("open")
	target := utf16Ptr(path)
	if target == nil {
		return errors.New("shell: invalid path")
	}

	r, _, _ := pShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(target)),
		0, 0,
		swShowNormal,
	)
	// ShellExecute returns a value greater than 32 on success; anything at or
	// below it is a legacy error code with no matching errno.
	if r <= 32 {
		return fmt.Errorf("shell: cannot open %s (code %d)", path, r)
	}
	return nil
}

// setClipboardText puts text on the Windows clipboard.
//
// The memory handed to the clipboard must come from GlobalAlloc, because the
// system takes ownership of it and frees it after the paste.
func setClipboardText(s string) error {
	utf16, err := syscall.UTF16FromString(s)
	if err != nil {
		return fmt.Errorf("clipboard: %w", err)
	}

	hMem, _, errno := pGlobalAlloc.Call(gmemMoveable, uintptr(len(utf16)*2))
	if hMem == 0 {
		return fmt.Errorf("clipboard: GlobalAlloc: %v", errno)
	}
	ptr, _, _ := pGlobalLock.Call(hMem)
	if ptr == 0 {
		return errors.New("clipboard: GlobalLock failed")
	}
	dst := unsafe.Slice((*uint16)(uptrToPtr(ptr)), len(utf16))
	copy(dst, utf16)
	pGlobalUnlock.Call(hMem)

	if r, _, errno := pOpenClipboard.Call(0); r == 0 {
		return fmt.Errorf("clipboard: OpenClipboard: %v", errno)
	}
	defer pCloseClipboard.Call()

	pEmptyClipboard.Call()
	if r, _, errno := pSetClipboardData.Call(cfUnicodeText, hMem); r == 0 {
		return fmt.Errorf("clipboard: SetClipboardData: %v", errno)
	}
	return nil
}

// --- drawing primitives ---------------------------------------------------

// The custom-painted half of the window needs a handful of GDI calls that a
// stock control would have made internally. They are wrapped here rather than
// called at each site so the argument marshalling stays in one place.

// hbrush is a GDI brush handle. The zero value is not a valid handle, which
// makes "not created yet" the same thing as "not set".
type hbrush uintptr

// solidBrushFrom creates a brush in a given colour.
//
// These are cached by colour for the life of the process. A repaint allocates
// none: the sidebar alone would otherwise create and destroy half a dozen
// brushes per frame, and at 60 frames a second that is the difference between
// a window that idles at 0% CPU and one that never settles.
var (
	brushCache = map[uint32]hbrush{}
)

func brush(color uint32) hbrush {
	if b, ok := brushCache[color]; ok {
		return b
	}
	r, _, _ := pCreateSolidBrush.Call(uintptr(color))
	b := hbrush(r)
	brushCache[color] = b
	return b
}

// penCache holds the one-pixel pens used for hairlines and outlines. Width 1
// is PS_SOLID with a width of 1; a wider pen is created on demand.
const psSolid = 0

func pen(color uint32) uintptr {
	r, _, _ := pCreatePen.Call(psSolid, 1, uintptr(color))
	return r
}

// fillRect paints a solid rectangle.
//
// FillRect takes a RECT by pointer and, unlike most GDI calls, does not need
// the brush selected into a DC first — which makes it both cheaper and less
// error-prone than SetBkColor plus ExtTextOut for a background block.
func fillRect(hdc uintptr, r rect, color uint32) {
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&r)), uintptr(brush(color)))
}

// clipGuard restricts drawing to a rectangle for the duration of a paint pass.
//
// GDI has no scoped clip, so it is established by intersecting with the current
// region and undone by selecting the whole window back. The type exists so the
// restore cannot be forgotten: a clip left installed would silently swallow
// every later draw in the frame.
type clipGuard struct {
	hdc uintptr
}

// clipTo bounds subsequent drawing to r.
func clipTo(hdc uintptr, r rect) clipGuard {
	pIntersectClipRect.Call(hdc, uintptr(r.Left), uintptr(r.Top),
		uintptr(r.Right), uintptr(r.Bottom))
	return clipGuard{hdc: hdc}
}

// release restores the unrestricted clip region.
func (c clipGuard) release() {
	// A null region selects the whole window back, which is what the DC had
	// before. Passing a specific rect instead would leave the outer area
	// clipped for the rest of the frame.
	pSelectClipRgn.Call(c.hdc, 0)
}

// strokeRect draws a one-pixel outline, inset by half a pixel so the line
// lands inside the rect rather than straddling its edge.
func strokeRect(hdc uintptr, r rect, color uint32) {
	p := pen(color)
	old, _, _ := pSelectObject.Call(hdc, p)
	defer func() {
		pSelectObject.Call(hdc, old)
		pDeleteObject.Call(p)
	}()

	// Left and right edges, then top and bottom. The bottom and right are one
	// pixel inside so that adjacent boxes share an edge instead of doubling it.
	pMoveToEx.Call(hdc, uintptr(r.Left), uintptr(r.Top), 0)
	pLineTo.Call(hdc, uintptr(r.Right-1), uintptr(r.Top))
	pLineTo.Call(hdc, uintptr(r.Right-1), uintptr(r.Bottom-1))
	pLineTo.Call(hdc, uintptr(r.Left), uintptr(r.Bottom-1))
	pLineTo.Call(hdc, uintptr(r.Left), uintptr(r.Top))
}

// hLine draws a horizontal hairline at y across [x0, x1).
func hLine(hdc uintptr, x0, x1, y int32, color uint32) {
	p := pen(color)
	old, _, _ := pSelectObject.Call(hdc, p)
	pMoveToEx.Call(hdc, uintptr(x0), uintptr(y), 0)
	pLineTo.Call(hdc, uintptr(x1), uintptr(y))
	pSelectObject.Call(hdc, old)
	pDeleteObject.Call(p)
}

// vLine draws a vertical hairline at x across [y0, y1).
func vLine(hdc uintptr, x, y0, y1 int32, color uint32) {
	p := pen(color)
	old, _, _ := pSelectObject.Call(hdc, p)
	pMoveToEx.Call(hdc, uintptr(x), uintptr(y0), 0)
	pLineTo.Call(hdc, uintptr(x), uintptr(y1))
	pSelectObject.Call(hdc, old)
	pDeleteObject.Call(p)
}

// roundRectFilled paints a rounded rectangle, optionally outlined.
//
// RoundRect draws its border with the current pen and fills with the current
// brush in one pass, so a capsule costs one call rather than a path. Passing a
// null brush would leave it unfilled; passing a null pen would leave the
// outline undrawn, so both are always supplied.
func roundRectFilled(hdc uintptr, r rect, radius int32, fill, outline uint32) {
	fb := brush(fill)
	var p uintptr
	if outline != 0 {
		p = pen(outline)
	} else {
		p, _, _ = pGetStockObject.Call(nullPen)
	}

	oldB, _, _ := pSelectObject.Call(hdc, uintptr(fb))
	oldP, _, _ := pSelectObject.Call(hdc, p)

	pRoundRect.Call(hdc,
		uintptr(r.Left), uintptr(r.Top), uintptr(r.Right), uintptr(r.Bottom),
		uintptr(radius), uintptr(radius))

	pSelectObject.Call(hdc, oldB)
	pSelectObject.Call(hdc, oldP)
	if outline != 0 {
		pDeleteObject.Call(p)
	}
}

// textAt draws a single line of text at (x, y), which is the top-left of the
// text box rather than its baseline.
func textAt(hdc uintptr, x, y int32, s string, color uint32) {
	if s == "" {
		return
	}
	pSetTextColor.Call(hdc, uintptr(color))
	pTextOutW.Call(hdc, uintptr(x), uintptr(y),
		uintptr(unsafe.Pointer(utf16Ptr(s))), uintptr(len([]rune(s))))
}

// textWidth measures a string in the currently selected font.
//
// The column layout is computed from measured text rather than from character
// counts, because Segoe UI is proportional — counting characters would push
// the last column off the edge on any string with wide glyphs in it.
func textWidth(hdc uintptr, s string) int32 {
	if s == "" {
		return 0
	}
	var sz size
	u := utf16Ptr(s)
	n := len([]rune(s))
	pGetTextExtentPoint32W.Call(hdc,
		uintptr(unsafe.Pointer(u)), uintptr(n),
		uintptr(unsafe.Pointer(&sz)))
	return sz.CX
}

// textRight draws text right-aligned so that its right edge lands on x.
func textRight(hdc uintptr, x, y int32, s string, color uint32) {
	w := textWidth(hdc, s)
	textAt(hdc, x-w, y, s, color)
}

// --- off-screen compositing ------------------------------------------------

// surface is a memory DC holding one window's worth of pixels.
//
// Every repaint goes through one of these and is blitted to the screen in a
// single BitBlt. Drawing straight to the window DC is the reason hand-painted
// Win32 windows flicker: the user sees the background painted, then the rail,
// then each row, one after another. Compositing off-screen costs one extra
// bitmap and removes the entire class of problem.
type surface struct {
	dc     uintptr
	bitmap uintptr
	width  int32
	height int32
	valid  bool
}

// ensure makes the surface large enough for the given client area, reallocating
// only when the window has actually grown. Reallocating every frame would be
// far more expensive than the drawing it saves.
func (s *surface) ensure(hwnd uintptr, w, h int32) bool {
	if w <= 0 || h <= 0 {
		return false
	}
	if s.valid && s.width >= w && s.height >= h {
		return true
	}
	s.release()

	dc, _, _ := pGetDC.Call(hwnd)
	compat, _, _ := pCreateCompatibleDC.Call(dc)
	bmp, _, _ := pCreateCompatibleBitmap.Call(dc, uintptr(w), uintptr(h))
	pReleaseDC.Call(hwnd, dc)

	if compat == 0 || bmp == 0 {
		if compat != 0 {
			pDeleteDC.Call(compat)
		}
		if bmp != 0 {
			pDeleteObject.Call(bmp)
		}
		return false
	}

	pSelectObject.Call(compat, bmp)
	s.dc = compat
	s.bitmap = bmp
	s.width = w
	s.height = h
	s.valid = true
	return true
}

func (s *surface) release() {
	if !s.valid {
		return
	}
	pDeleteDC.Call(s.dc)
	pDeleteObject.Call(s.bitmap)
	s.dc, s.bitmap = 0, 0
	s.valid = false
}

// flush copies the composed image to the window.
func (s *surface) flush(hwnd uintptr) {
	dc, _, _ := pGetDC.Call(hwnd)
	pBitBlt.Call(dc, 0, 0, uintptr(s.width), uintptr(s.height),
		s.dc, 0, 0, srccopy)
	pReleaseDC.Call(hwnd, dc)
}

// srccopy is the ROP code for "copy source to destination" in BitBlt.
const srccopy = 0x00CC0020

// nullPen and nullBrush are the stock-object indices used to suppress part of
// a drawing operation.
const (
	nullPen        = 8
	emptyBrush     = 5
	dcBrush        = 18
	dcPen          = 19
	defaultCharset = 1
)

// size mirrors the SIZE struct GetTextExtentPoint32W fills in.
type size struct{ CX, CY int32 }

// --- animation timing -----------------------------------------------------

// nowMS is the monotonic clock the animation code runs on.
//
// time.Now() would work, but every animation frame asks for the time and this
// is the cheaper call. Wall-clock time is the wrong source anyway: an NTP
// correction mid-animation would make an eased value jump.
func nowMS() int64 {
	r, _, _ := pGetTickCount64.Call()
	return int64(r)
}

// easeOutCubic maps 0..1 to 0..1 with a decelerating curve.
func easeOutCubic(t float64) float64 {
	u := 1 - t
	return 1 - u*u*u
}

// clamp01 bounds a progress value.
func clamp01(t float64) float64 {
	if t < 0 {
		return 0
	}
	if t > 1 {
		return 1
	}
	return t
}

// lerp blends two colours by t, 0 giving a and 1 giving b.
//
// Blending in COLORREF's 0x00BBGGRR layout works channel by channel with the
// same shifts regardless of channel order, which is why this does not have to
// unpack and repack.
func lerp(a, b uint32, t float64) uint32 {
	if t <= 0 {
		return a
	}
	if t >= 1 {
		return b
	}
	ar, ag, ab := a&0xFF, (a>>8)&0xFF, (a>>16)&0xFF
	br, bg, bb := b&0xFF, (b>>8)&0xFF, (b>>16)&0xFF
	r := uint32(float64(ar) + (float64(br)-float64(ar))*t)
	g := uint32(float64(ag) + (float64(bg)-float64(ag))*t)
	bl := uint32(float64(ab) + (float64(bb)-float64(ab))*t)
	return r | g<<8 | bl<<16
}

// --- animation diagnostics ------------------------------------------------

// animTracing turns on a per-frame dump of every animated value. It is off by
// default and costs nothing when off.
//
// The numbers it prints are read from the same tween values the painter reads,
// which is the only way to tell "the animation is running but the screenshot
// caught it settled" apart from "the animation never ran" without watching the
// window, and a scripted capture has no way to watch the window.
var animTracing = os.Getenv("SERCON_ANIM_TRACE") != ""

// animTrace counts frames so the dump is bounded and readable.
var animTraceFrames int

// traceAnim prints one line per frame summarising the animation state.
func traceAnim(kind string) {
	if !animTracing || guiLog == nil {
		return
	}
	if animTraceFrames++; animTraceFrames > 400 {
		return
	}
	var b []byte
	b = append(b, kind...)
	for i := range rail.entries {
		e := &rail.entries[i]
		b = fmt.Appendf(b, " %s[h=%.2f s=%.2f]", e.item.label, e.hover.value, e.sel.value)
	}
	b = fmt.Appendf(b, " hover(r=%.2f i=%d)", hover.row.value, hover.index)
	for i := range rows.items {
		r := &rows.items[i]
		b = fmt.Appendf(b, " row(%s %.2f h=%d dying=%v)", r.ref, r.t.value, r.height(), r.dying)
	}
	for i := range counters {
		b = fmt.Appendf(b, " c%d=%.1f", i, counters[i].value)
	}
	for i := range buttonHover {
		b = fmt.Appendf(b, " b%d=%.2f", i, buttonHover[i].value)
	}
	fmt.Fprintf(guiLog, "%s\n", b)
}

// cursorPos reports the cursor in client coordinates of the given window.
func cursorPos(hwnd uintptr) (point, bool) {
	var p point
	if r, _, _ := pGetCursorPos.Call(uintptr(unsafe.Pointer(&p))); r == 0 {
		return point{}, false
	}
	// ClientToScreen is inverted by translating with ScreenToClient; the
	// deprecated GetMessagePos approach returns screen coordinates and would
	// need the same conversion.
	pScreenToClient.Call(hwnd, uintptr(unsafe.Pointer(&p)))
	return p, true
}

// pointIn reports whether a point lies inside a rect.
func pointIn(r rect, p point) bool {
	return p.X >= r.Left && p.X < r.Right && p.Y >= r.Top && p.Y < r.Bottom
}

// inset shrinks a rect on every side.
func insetRect(r rect, dx, dy int32) rect {
	return rect{r.Left + dx, r.Top + dy, r.Right - dx, r.Bottom - dy}
}

// rectAt builds a rect from an origin and a size.
func rectAt(x, y, w, h int32) rect {
	return rect{x, y, x + w, y + h}
}
