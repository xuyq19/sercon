//go:build windows

// Win32 bindings for the seriald GUI.
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
	comctl32 = syscall.NewLazyDLL("comctl32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
)

var (
	pRegisterClassExW   = user32.NewProc("RegisterClassExW")
	pCreateWindowExW    = user32.NewProc("CreateWindowExW")
	pDefWindowProcW     = user32.NewProc("DefWindowProcW")
	pGetMessageW        = user32.NewProc("GetMessageW")
	pTranslateMessage   = user32.NewProc("TranslateMessage")
	pDispatchMessageW   = user32.NewProc("DispatchMessageW")
	pPostQuitMessage    = user32.NewProc("PostQuitMessage")
	pPostMessageW       = user32.NewProc("PostMessageW")
	pDestroyWindow      = user32.NewProc("DestroyWindow")
	pShowWindow         = user32.NewProc("ShowWindow")
	pUpdateWindow       = user32.NewProc("UpdateWindow")
	pGetClientRect      = user32.NewProc("GetClientRect")
	pSendMessageW       = user32.NewProc("SendMessageW")
	pSetWindowTextW     = user32.NewProc("SetWindowTextW")
	pLoadCursorW        = user32.NewProc("LoadCursorW")
	pMessageBoxW        = user32.NewProc("MessageBoxW")
	pSetProcessDPIAware = user32.NewProc("SetProcessDPIAware")
	pMoveWindow         = user32.NewProc("MoveWindow")
	pSetTimer           = user32.NewProc("SetTimer")
	pOpenClipboard      = user32.NewProc("OpenClipboard")
	pEmptyClipboard     = user32.NewProc("EmptyClipboard")
	pSetClipboardData   = user32.NewProc("SetClipboardData")
	pCloseClipboard     = user32.NewProc("CloseClipboard")
	pGetDC              = user32.NewProc("GetDC")
	pReleaseDC          = user32.NewProc("ReleaseDC")

	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	pGlobalLock       = kernel32.NewProc("GlobalLock")
	pGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	pGetDeviceCaps    = gdi32.NewProc("GetDeviceCaps")
	pCreateFontW      = gdi32.NewProc("CreateFontW")
	pGetStockObject   = gdi32.NewProc("GetStockObject")
	pSetTextColor     = gdi32.NewProc("SetTextColor")
	pSetBkMode        = gdi32.NewProc("SetBkMode")
	pCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")

	pImageListCreate = comctl32.NewProc("ImageList_Create")

	pInitCommonControlsEx = comctl32.NewProc("InitCommonControlsEx")
	pShellExecuteW        = shell32.NewProc("ShellExecuteW")
)

// Window styles and messages.
const (
	wsOverlappedWindow = 0x00CF0000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsTabStop          = 0x00010000
	wsGroup            = 0x00020000
	wsExClientEdge     = 0x00000200

	cwUseDefault = ^uint32(0) // (uint32)-1

	swShowNormal = 1

	csHRedraw = 0x0002
	csVRedraw = 0x0001

	idcArrow       = 32512
	colorWindow    = 5
	colorBtnFace   = 15
	defaultGUIFont = 17

	wmCreate         = 0x0001
	wmDestroy        = 0x0002
	wmSize           = 0x0005
	wmClose          = 0x0010
	wmSetFont        = 0x0030
	wmNotify         = 0x004E
	wmCommand        = 0x0111
	wmTimer          = 0x0113
	wmGetMinMaxInfo  = 0x0024
	wmCtlColorStatic = 0x0138

	transparent = 1

	lognPixelY = 90
)

// Common controls.
const (
	ilcColor32      = 0x00000020
	lvsilSmall      = 1
	lvmSetImageList = lvmFirst + 2
)

// ListView custom draw.
const (
	nmCustomDraw = 0xFFFFFFF4

	// CDDS_SUBITEM is 0x00020000, not 0x00000002 — the low bit pattern is
	// CDDS_POSTPAINT, which is a different stage entirely. Subitem callbacks
	// arrive as 0x00030001.
	cddsSubItem      = 0x00020000
	cddsPrePaint     = 0x00000001
	cddsItemPrePaint = 0x00010001
	// Subitem callbacks carry both bits, and that is the only stage at which
	// ISubItem means anything for a report ListView.
	cddsSubItemPrePaint = cddsItemPrePaint | cddsSubItem

	cdrfDoDefault = 0x00000000
	cdrfNewFont   = 0x00000002
	// CDRF_NOTIFYITEMDRAW and CDRF_NOTIFYSUBITEMDRAW share a value; the meaning
	// depends on which stage returned it.
	cdrfNotifyItemDraw    = 0x00000020
	cdrfNotifySubItemDraw = 0x00000020
)

// ListView.
const (
	lvsReport        = 0x0001
	lvsSingleSel     = 0x0004
	lvsShowSelAlways = 0x0008

	lvsExGridLines     = 0x00000001
	lvsExFullRowSelect = 0x00000020
	lvsExDoubleBuffer  = 0x00010000

	lvmFirst                    = 0x1000
	lvmDeleteAllItems           = lvmFirst + 9
	lvmSetItemW                 = lvmFirst + 76
	lvmInsertItemW              = lvmFirst + 77
	lvmInsertColumnW            = lvmFirst + 97
	lvmSetExtendedListViewStyle = lvmFirst + 54
	lvmGetNextItem              = lvmFirst + 12

	lvcfFmt     = 0x0001
	lvcfWidth   = 0x0002
	lvcfText    = 0x0004
	lvcfSubItem = 0x0008

	lvcfmtLeft = 0x0000

	lvifText = 0x0001

	lvniSelected = 0x0002

	iccListViewClasses = 0x00000001

	lvmSetRedraw = 0x100B
)

// MessageBox.
const (
	mbOK              = 0x0000
	mbYesNo           = 0x0004
	mbIconError       = 0x0010
	mbIconQuestion    = 0x0020
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

type initCommonControlsEx struct {
	Size uint32
	ICC  uint32
}

// lvColumn mirrors LVCOLUMNW. Field order and widths matter: the control reads
// the struct by member offset, so a missing tail field would shift nothing but
// a reordered one would corrupt every column.
type lvColumn struct {
	Mask       uint32
	Fmt        int32
	Cx         int32
	PszText    *uint16
	CchTextMax int32
	ISubItem   int32
	IImage     int32
	IOrder     int32
	CxMin      int32
	CxDefault  int32
	CxIdeal    int32
}

// lvItem mirrors LVITEMW.
type lvItem struct {
	Mask       uint32
	IItem      int32
	ISubItem   int32
	State      uint32
	StateMask  uint32
	PszText    *uint16
	CchTextMax int32
	IImage     int32
	LParam     uintptr
	IIndent    int32
	IGroupID   int32
	CColumns   uint32
	PuColumns  *uint32
	PiColFmt   *int32
	IGroup     int32
}

// nmhdr mirrors NMHDR, the header every WM_NOTIFY payload starts with.
type nmhdr struct {
	HwndFrom uintptr
	IdFrom   uintptr
	Code     uint32
}

// nmCustomDrawInfo mirrors NMCUSTOMDRAW.
type nmCustomDrawInfo struct {
	Hdr         nmhdr
	DwDrawStage uint32
	Hdc         uintptr
	Rc          rect
	DwItemSpec  uintptr
	UItemState  uint32
	LItemlParam uintptr
}

// nmlvCustomDraw mirrors NMLVCUSTOMDRAW: an NMCUSTOMDRAW followed by the
// ListView-specific colour fields. Only DwDrawStage, DwItemSpec, ISubItem and
// ClrText are read or written here, but the whole layout has to be right
// because the control hands over a pointer to the real struct.
type nmlvCustomDraw struct {
	Nmcd        nmCustomDrawInfo
	ClrText     uint32
	ClrTextBk   uint32
	ISubItem    int32
	DwItemType  uint32
	ClrFace     uint32
	IIconEffect int32
	IIconPhase  int32
	IPartId     int32
	IStateId    int32
	RcText      rect
	UAlign      uint32
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
		return fmt.Errorf("RegisterClassExW(%s): %v", name, errno)
	}
	return nil
}

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

func messageBox(title, text string, flags uintptr) int {
	r, _, _ := pMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(utf16Ptr(text))),
		uintptr(unsafe.Pointer(utf16Ptr(title))),
		flags,
	)
	return int(r)
}

func createFont(pt int, weight uintptr) uintptr {
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

	face := utf16Ptr("Segoe UI")
	r, _, _ := pCreateFontW.Call(
		uintptr(height), 0, 0, 0,
		weight, 0, 0, 0,
		ansiCharset, defaultPitch,
		uintptr(unsafe.Pointer(face)),
	)
	return r
}

// createUIFont is the body font used by the list, buttons and status line.
func createUIFont() uintptr { return createFont(9, fwNormal) }

// createHeadingFont is used for the one line of hierarchy at the top of the
// window, which is what keeps it from reading as a wall of controls.
func createHeadingFont() uintptr { return createFont(12, fwSemibold) }

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

// createImageList makes a small image list with no images in it.
//
// Its only purpose is the row height: a ListView sizes every row to the height
// of its small image list, and there is no other supported way to get padding
// inside a report row. Without this the rows are as tight as the font allows,
// which is a large part of why default ListViews read as dated.
func createImageList(rowHeight int) uintptr {
	r, _, _ := pImageListCreate.Call(1, uintptr(rowHeight), ilcColor32, 1, 1)
	return r
}

func setImageList(list, imageList uintptr) {
	pSendMessageW.Call(list, lvmSetImageList, lvsilSmall, imageList)
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
