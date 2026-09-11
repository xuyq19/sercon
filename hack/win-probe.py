"""Liveness probe for the sercon GUI.

Reports the process id, the window rectangle and the window's client origin in
screen coordinates. Everything here needs explicit restypes and argtypes: a
Window handle is pointer-sized, and ctypes defaults an undeclared return to a
32-bit int, which silently truncates it to nothing on a 64-bit build.

Stdlib only.

    python hack/win-probe.py
"""

import ctypes
import ctypes.wintypes as w

user32 = ctypes.WinDLL("user32", use_last_error=True)
kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)

kernel32.CreateToolhelp32Snapshot.restype = w.HANDLE
kernel32.CreateToolhelp32Snapshot.argtypes = [w.DWORD, w.DWORD]
kernel32.Process32First.argtypes = [w.HANDLE, ctypes.c_void_p]
kernel32.Process32Next.argtypes = [w.HANDLE, ctypes.c_void_p]
kernel32.OpenProcess.restype = w.HANDLE
kernel32.OpenProcess.argtypes = [w.DWORD, w.BOOL, w.DWORD]

user32.FindWindowW.restype = w.HWND
user32.FindWindowW.argtypes = [w.LPCWSTR, w.LPCWSTR]
user32.GetWindowRect.argtypes = [w.HWND, ctypes.POINTER(w.RECT)]
user32.GetClientRect.argtypes = [w.HWND, ctypes.POINTER(w.RECT)]
user32.ClientToScreen.argtypes = [w.HWND, ctypes.POINTER(w.POINT)]
user32.GetCursorPos.argtypes = [ctypes.POINTER(w.POINT)]
user32.GetWindowThreadProcessId.argtypes = [w.HWND, ctypes.POINTER(w.DWORD)]

TH32CS_SNAPPROCESS = 0x00000002
PROCESS_QUERY_LIMITED_INFORMATION = 0x1000


class PROCESSENTRY32(ctypes.Structure):
    _fields_ = [
        ("dwSize", w.DWORD),
        ("cntUsage", w.DWORD),
        ("th32ProcessID", w.DWORD),
        ("th32DefaultHeapID", ctypes.POINTER(ctypes.c_ulong)),
        ("th32ModuleID", w.DWORD),
        ("cntThreads", w.DWORD),
        ("th32ParentProcessID", w.DWORD),
        ("pcPriClassBase", ctypes.c_long),
        ("dwFlags", w.DWORD),
        ("szExeFile", ctypes.c_char * 260),
    ]


def procs():
    snap = kernel32.CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
    if snap == w.HANDLE(-1).value or not snap:
        return []
    out = []
    e = PROCESSENTRY32()
    e.dwSize = ctypes.sizeof(PROCESSENTRY32)
    ok = kernel32.Process32First(snap, ctypes.byref(e))
    while ok:
        out.append((e.th32ProcessID, e.szExeFile.decode(errors="replace")))
        ok = kernel32.Process32Next(snap, ctypes.byref(e))
    kernel32.CloseHandle(snap)
    return out


def main():
    hits = [(pid, nm) for pid, nm in procs() if "sercon" in nm.lower()]
    print("matching processes: %d" % len(hits))
    for pid, nm in hits:
        print("  pid %-8d %s" % (pid, nm))

    hwnd = user32.FindWindowW("serconGuiWindow", None)
    if not hwnd:
        print("main window: not found")
        return
    owner = w.DWORD(0)
    user32.GetWindowThreadProcessId(hwnd, ctypes.byref(owner))
    print("main window: hwnd=0x%X owner pid=%d" % (hwnd, owner.value))

    rect = w.RECT()
    user32.GetWindowRect(hwnd, ctypes.byref(rect))
    print("  window rect  (%d,%d)-(%d,%d)  %dx%d"
          % (rect.left, rect.top, rect.right, rect.bottom,
             rect.right - rect.left, rect.bottom - rect.top))

    origin = w.POINT(0, 0)
    user32.ClientToScreen(hwnd, ctypes.byref(origin))
    print("  client origin (screen) %d,%d" % (origin.x, origin.y))

    client = w.RECT()
    user32.GetClientRect(hwnd, ctypes.byref(client))
    print("  client size  %dx%d" % (client.right, client.bottom))

    cur = w.POINT()
    user32.GetCursorPos(ctypes.byref(cur))
    print("  cursor screen %d,%d -> client %d,%d"
          % (cur.x, cur.y, cur.x - origin.x, cur.y - origin.y))


if __name__ == "__main__":
    main()
