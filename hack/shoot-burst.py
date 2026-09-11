"""Sample one demo stage repeatedly to catch a tween mid-flight.

The stages that change a value do so over a few hundred milliseconds, which is
short compared to the interval between stages. Capturing once per stage lands on
whatever the value happened to be at that instant; this samples the same stage
many times in quick succession so the roll itself is on record.

Stdlib only.

    python hack/shoot-burst.py <outdir> <stage-index> <shots> <gap-ms>
"""

import ctypes
import ctypes.wintypes as w
import os
import struct
import sys
import time
import zlib

user32 = ctypes.WinDLL("user32", use_last_error=True)
gdi32 = ctypes.WinDLL("gdi32", use_last_error=True)
kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)

SRCCOPY = 0x00CC0020
PW_RENDERFULLCONTENT = 0x00000002
BI_RGB = 0
DIB_RGB_COLORS = 0
DETACHED_PROCESS = 0x00000008
CREATE_NEW_PROCESS_GROUP = 0x00000200
PROCESS_TERMINATE = 0x0001
CLASS_NAME = "serconGuiWindow"

kernel32.CreateToolhelp32Snapshot.restype = w.HANDLE
kernel32.CreateToolhelp32Snapshot.argtypes = [w.DWORD, w.DWORD]
kernel32.OpenProcess.restype = w.HANDLE
kernel32.OpenProcess.argtypes = [w.DWORD, w.BOOL, w.DWORD]
user32.FindWindowW.restype = w.HWND
user32.FindWindowW.argtypes = [w.LPCWSTR, w.LPCWSTR]
user32.GetWindowRect.argtypes = [w.HWND, ctypes.POINTER(w.RECT)]
user32.PrintWindow.argtypes = [w.HWND, w.HDC, w.UINT]
user32.GetWindowDC.restype = w.HDC
user32.GetWindowDC.argtypes = [w.HWND]
user32.ReleaseDC.argtypes = [w.HWND, w.HDC]
gdi32.CreateCompatibleDC.restype = w.HDC
gdi32.CreateCompatibleDC.argtypes = [w.HDC]
gdi32.CreateCompatibleBitmap.restype = w.HANDLE
gdi32.CreateCompatibleBitmap.argtypes = [w.HDC, ctypes.c_int, ctypes.c_int]
gdi32.SelectObject.restype = w.HANDLE
gdi32.SelectObject.argtypes = [w.HDC, w.HANDLE]
gdi32.GetDIBits.argtypes = [w.HDC, w.HANDLE, w.UINT, w.UINT,
                            ctypes.c_void_p, ctypes.c_void_p, w.UINT]
gdi32.DeleteObject.argtypes = [w.HANDLE]
gdi32.DeleteDC.argtypes = [w.HDC]


class BITMAPINFOHEADER(ctypes.Structure):
    _fields_ = [
        ("biSize", w.DWORD), ("biWidth", w.LONG), ("biHeight", w.LONG),
        ("biPlanes", w.WORD), ("biBitCount", w.WORD), ("biCompression", w.DWORD),
        ("biSizeImage", w.DWORD), ("biXPelsPerMeter", w.LONG),
        ("biYPelsPerMeter", w.LONG), ("biClrUsed", w.DWORD),
        ("biClrImportant", w.DWORD),
    ]


class BITMAPINFO(ctypes.Structure):
    _fields_ = [("bmiHeader", BITMAPINFOHEADER), ("bmiColors", w.DWORD * 3)]


def write_png(path, width, height, rows):
    raw = b"".join(b"\x00" + r for r in rows)

    def chunk(tag, data):
        return (struct.pack(">I", len(data)) + tag + data
                + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF))

    with open(path, "wb") as fh:
        fh.write(b"\x89PNG\r\n\x1a\n"
                 + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
                 + chunk(b"IDAT", zlib.compress(raw, 6))
                 + chunk(b"IEND", b""))


def find_pids():
    class PE(ctypes.Structure):
        _fields_ = [
            ("dwSize", w.DWORD), ("cntUsage", w.DWORD),
            ("th32ProcessID", w.DWORD), ("th32DefaultHeapID", ctypes.c_void_p),
            ("th32ModuleID", w.DWORD), ("cntThreads", w.DWORD),
            ("th32ParentProcessID", w.DWORD), ("pcPriClassBase", ctypes.c_long),
            ("dwFlags", w.DWORD), ("szExeFile", ctypes.c_char * 260),
        ]

    snap = kernel32.CreateToolhelp32Snapshot(2, 0)
    out = []
    e = PE()
    e.dwSize = ctypes.sizeof(PE)
    ok = kernel32.Process32First(snap, ctypes.byref(e))
    while ok:
        if b"sercon-gui" in e.szExeFile:
            out.append(e.th32ProcessID)
        ok = kernel32.Process32Next(snap, ctypes.byref(e))
    kernel32.CloseHandle(snap)
    return out


def launch(exe):
    class STARTUPINFO(ctypes.Structure):
        _fields_ = [
            ("cb", w.DWORD), ("lpReserved", w.LPWSTR), ("lpDesktop", w.LPWSTR),
            ("lpTitle", w.LPWSTR), ("dwX", w.DWORD), ("dwY", w.DWORD),
            ("dwXSize", w.DWORD), ("dwYSize", w.DWORD),
            ("dwXCountChars", w.DWORD), ("dwYCountChars", w.DWORD),
            ("dwFillAttribute", w.DWORD), ("dwFlags", w.DWORD),
            ("wShowWindow", w.WORD), ("cbReserved2", w.WORD),
            ("lpReserved2", ctypes.POINTER(ctypes.c_byte)),
            ("hStdInput", w.HANDLE), ("hStdOutput", w.HANDLE), ("hStdError", w.HANDLE),
        ]

    class PROCESS_INFORMATION(ctypes.Structure):
        _fields_ = [("hProcess", w.HANDLE), ("hThread", w.HANDLE),
                    ("dwProcessId", w.DWORD), ("dwThreadId", w.DWORD)]

    kernel32.CreateProcessW.restype = w.BOOL
    si = STARTUPINFO()
    si.cb = ctypes.sizeof(STARTUPINFO)
    pi = PROCESS_INFORMATION()
    cmd = ctypes.create_unicode_buffer('"%s"' % exe)
    ok = kernel32.CreateProcessW(None, cmd, None, None, False,
                                 DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP,
                                 None, os.path.dirname(exe),
                                 ctypes.byref(si), ctypes.byref(pi))
    if not ok:
        raise OSError("CreateProcessW failed: %d" % ctypes.get_last_error())
    return pi


def grab(hwnd):
    """Return the window pixels as (width, height, rows of RGB bytes)."""
    rect = w.RECT()
    user32.GetWindowRect(hwnd, ctypes.byref(rect))
    width, height = rect.right - rect.left, rect.bottom - rect.top
    hdc_window = user32.GetWindowDC(hwnd)
    hdc_mem = gdi32.CreateCompatibleDC(hdc_window)
    hbmp = gdi32.CreateCompatibleBitmap(hdc_window, width, height)
    gdi32.SelectObject(hdc_mem, hbmp)
    if not user32.PrintWindow(hwnd, hdc_mem, PW_RENDERFULLCONTENT):
        gdi32.BitBlt(hdc_mem, 0, 0, width, height, hdc_window, 0, 0, SRCCOPY)

    info = BITMAPINFO()
    info.bmiHeader.biSize = ctypes.sizeof(BITMAPINFOHEADER)
    info.bmiHeader.biWidth = width
    info.bmiHeader.biHeight = -height
    info.bmiHeader.biPlanes = 1
    info.bmiHeader.biBitCount = 32
    info.bmiHeader.biCompression = BI_RGB
    info.bmiHeader.biSizeImage = width * height * 4

    buf = ctypes.create_string_buffer(width * height * 4)
    gdi32.GetDIBits(hdc_mem, hbmp, 0, height, buf, ctypes.byref(info), DIB_RGB_COLORS)
    raw = buf.raw
    stride = width * 4
    rows = []
    for r in range(height):
        row = raw[r * stride:(r + 1) * stride]
        rgb = bytearray(width * 3)
        rgb[0::3] = row[2::4]
        rgb[1::3] = row[1::4]
        rgb[2::3] = row[0::4]
        rows.append(bytes(rgb))

    gdi32.DeleteObject(hbmp)
    gdi32.DeleteDC(hdc_mem)
    user32.ReleaseDC(hwnd, hdc_window)
    return width, height, rows


def region_hash(rows, x0, y0, x1, y1):
    """Sum the bytes of one region, which is enough to tell frames apart."""
    total = 0
    for y in range(y0, y1):
        total = (total + sum(rows[y][x0 * 3:x1 * 3])) & 0xFFFFFFFF
    return total


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    outdir = sys.argv[1]
    stage = int(sys.argv[2]) if len(sys.argv) > 2 else 6
    shots = int(sys.argv[3]) if len(sys.argv) > 3 else 8
    gap = int(sys.argv[4]) if len(sys.argv) > 4 else 120
    os.makedirs(outdir, exist_ok=True)

    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    exe = os.path.join(root, "dist", "sercon-gui.exe")
    # An offset in milliseconds from the nominal stage start. A negative value
    # starts the burst just before the boundary, which is what catches a
    # transition that is shorter than the sampling gap.
    offset = int(sys.argv[5]) if len(sys.argv) > 5 else 0

    for pid in find_pids():
        h = kernel32.OpenProcess(PROCESS_TERMINATE, False, pid)
        if h:
            kernel32.TerminateProcess(h, 0)
            kernel32.CloseHandle(h)
    time.sleep(0.4)
    sock = os.path.join(os.environ.get("LOCALAPPDATA", ""), "sercon", "run", "s.sock")
    for n, p in enumerate((sock, sock + ".lock")):
        try:
            os.rename(p, "%s.stale%d" % (p, n))
        except OSError:
            pass

    os.environ["SERCON_ANIM_DEMO"] = "1"
    pi = launch(exe)
    hwnd = None
    for _ in range(100):
        hwnd = user32.FindWindowW(CLASS_NAME, None)
        if hwnd:
            break
        time.sleep(0.1)
    if not hwnd:
        raise RuntimeError("window never appeared")

    # Wait for the stage under test to begin. Stage n starts at n * 2200 ms,
    # and the first stage runs at startup. The window takes a moment to appear
    # and draw, so the wait is measured from the point the window was found.
    target = stage * 2.2 + offset / 1000.0
    t0 = time.time()
    while time.time() - t0 < target - 0.35:
        time.sleep(0.02)

    w_, h_ = grab(hwnd)[:2]
    print("burst on stage %d: %d shots, %dms apart" % (stage, shots, gap))
    seen = []
    for i in range(shots):
        width, height, rows = grab(hwnd)
        # The counters live under the title, in the top-left of the content
        # area. Sampling that band is enough to see a roll.
        sig = region_hash(rows, 200, 88, 700, 145)
        seen.append(sig)
        write_png(os.path.join(outdir, "burst-%02d.png" % i), width, height, rows)
        print("  burst-%02d.png  counters=%d" % (i, sig))
        time.sleep(gap / 1000.0)

    distinct = len(set(seen))
    print("distinct counter signatures: %d of %d" % (distinct, shots))
    kernel32.TerminateProcess(pi.hProcess, 0)
    kernel32.CloseHandle(pi.hProcess)
    kernel32.CloseHandle(pi.hThread)


if __name__ == "__main__":
    main()
