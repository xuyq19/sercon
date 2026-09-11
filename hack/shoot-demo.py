"""Photograph each stage of the sercon GUI's animation demo.

Runs the window with SERCON_ANIM_DEMO=1, which makes it walk its own animations
on a timer. Each stage lasts a couple of seconds, so a capture taken partway
through lands inside the transition rather than after it has settled.

Launch, wait, capture and clean up all happen in one process. A window started
from a shell command is torn down when that command's job closes, so splitting
this across commands would leave nothing to photograph.

Stdlib only.

    python hack/shoot-demo.py [outdir]
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

    blob = (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw, 6))
            + chunk(b"IEND", b""))
    with open(path, "wb") as fh:
        fh.write(blob)


def find_pids():
    TH32CS_SNAPPROCESS = 2

    class PE(ctypes.Structure):
        _fields_ = [
            ("dwSize", w.DWORD), ("cntUsage", w.DWORD),
            ("th32ProcessID", w.DWORD), ("th32DefaultHeapID", ctypes.c_void_p),
            ("th32ModuleID", w.DWORD), ("cntThreads", w.DWORD),
            ("th32ParentProcessID", w.DWORD), ("pcPriClassBase", ctypes.c_long),
            ("dwFlags", w.DWORD), ("szExeFile", ctypes.c_char * 260),
        ]

    snap = kernel32.CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
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


def launch(exe, env_flag):
    class STARTUPINFO(ctypes.Structure):
        _fields_ = [
            ("cb", w.DWORD), ("lpReserved", w.LPWSTR), ("lpDesktop", w.LPWSTR),
            ("lpTitle", w.LPWSTR), ("dwX", w.DWORD), ("dwY", w.DWORD),
            ("dwXSize", w.DWORD), ("dwYSize", w.DWORD),
            ("dwXCountChars", w.DWORD), ("dwYCountChars", w.DWORD),
            ("dwFillAttribute", w.DWORD), ("dwFlags", w.DWORD),
            ("wShowWindow", w.WORD), ("cbReserved2", w.WORD),
            ("lpReserved2", ctypes.POINTER(ctypes.c_byte)),
            ("hStdInput", w.HANDLE), ("hStdOutput", w.HANDLE),
            ("hStdError", w.HANDLE),
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


def capture(hwnd, path):
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
    got = gdi32.GetDIBits(hdc_mem, hbmp, 0, height, buf,
                          ctypes.byref(info), DIB_RGB_COLORS)
    if got != height:
        raise RuntimeError("GetDIBits returned %d of %d" % (got, height))

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
    write_png(path, width, height, rows)

    gdi32.DeleteObject(hbmp)
    gdi32.DeleteDC(hdc_mem)
    user32.ReleaseDC(hwnd, hdc_window)
    return width, height


def main():
    outdir = sys.argv[1] if len(sys.argv) > 1 else "dist/demo"
    os.makedirs(outdir, exist_ok=True)
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    exe = os.path.join(root, "dist", "sercon-gui.exe")

    for pid in find_pids():
        h = kernel32.OpenProcess(PROCESS_TERMINATE, False, pid)
        if h:
            kernel32.TerminateProcess(h, 0)
            kernel32.CloseHandle(h)
    time.sleep(0.5)

    # The socket file survives a killed process, and a stale one makes the new
    # window report "another daemon owns this socket". Renaming rather than
    # removing keeps the trick inside one filesystem call that cannot fail on a
    # read-only or locked file.
    sock = os.path.join(os.environ.get("LOCALAPPDATA", ""), "sercon", "run", "s.sock")
    for n, p in enumerate((sock, sock + ".lock")):
        try:
            os.rename(p, "%s.stale%d" % (p, n))
        except OSError:
            pass

    os.environ["SERCON_ANIM_DEMO"] = "1"
    pi = launch(exe, "SERCON_ANIM_DEMO")
    print("started pid %d" % pi.dwProcessId)

    hwnd = None
    for _ in range(100):
        hwnd = user32.FindWindowW(CLASS_NAME, None)
        if hwnd:
            break
        time.sleep(0.1)
    if not hwnd:
        raise RuntimeError("window never appeared")

    # The demo advances every 2200 ms. Capture just after each boundary so the
    # frame lands inside the transition rather than on the settled value.
    stages = ["01-rest", "02-rail-ports", "03-rail-keys", "04-row-hover",
              "05-button-hover", "06-counters", "07-row-fade", "08-done"]
    time.sleep(1.2)
    for i, name in enumerate(stages):
        # Two shots per stage: one early in the transition, one nearly settled.
        for tag, extra in (("a", 0.12), ("b", 0.55)):
            time.sleep(extra if tag == "a" else 0.0)
            w_, h_ = capture(hwnd, os.path.join(outdir, "%s-t%s.png" % (name, tag)))
            print("%s-t%s.png  %dx%d" % (name, tag, w_, h_))
            if tag == "a":
                time.sleep(0.35)
        # Wait out the rest of the interval before the next stage.
        time.sleep(2.2 - 1.02)

    kernel32.TerminateProcess(pi.hProcess, 0)
    kernel32.CloseHandle(pi.hProcess)
    kernel32.CloseHandle(pi.hThread)
    print("done")


if __name__ == "__main__":
    main()
