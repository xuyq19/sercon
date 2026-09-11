"""Profile the sercon GUI's CPU use over time in short slices.

A single average over a long window hides the shape of the cost. This samples
consecutive short windows, so a burst of work during startup is told apart from
a steady drain that would still be running an hour later — which is the
difference between an acceptable cost and a bug.

Pass --demo to run with the animation tour enabled. That is what shows the
animation clock's cost, and more importantly that it stops again: after the
tour ends the process must return to doing nothing between refreshes.

Stdlib only.

    python hack/cpu-slices.py [total-seconds] [slice-seconds] [exe] [--demo]
"""

import ctypes
import ctypes.wintypes as w
import os
import subprocess
import sys
import time

kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
DETACHED_PROCESS = 0x00000008
CREATE_NEW_PROCESS_GROUP = 0x00000200
PROCESS_TERMINATE = 0x0001
PROCESS_QUERY_LIMITED_INFORMATION = 0x1000

kernel32.CreateToolhelp32Snapshot.restype = w.HANDLE
kernel32.CreateToolhelp32Snapshot.argtypes = [w.DWORD, w.DWORD]
kernel32.OpenProcess.restype = w.HANDLE
kernel32.OpenProcess.argtypes = [w.DWORD, w.BOOL, w.DWORD]
kernel32.GetProcessTimes.argtypes = [w.HANDLE, ctypes.c_void_p, ctypes.c_void_p,
                                     ctypes.c_void_p, ctypes.c_void_p]


class FILETIME(ctypes.Structure):
    _fields_ = [("lo", w.DWORD), ("hi", w.DWORD)]


def ft(x):
    return (x.hi << 32) | x.lo


def cpu_time(h):
    c, e, a, b = FILETIME(), FILETIME(), FILETIME(), FILETIME()
    ok = kernel32.GetProcessTimes(h, ctypes.byref(c), ctypes.byref(e),
                                 ctypes.byref(a), ctypes.byref(b))
    if not ok:
        raise OSError("GetProcessTimes failed: %d" % ctypes.get_last_error())
    return ft(a) + ft(b)


def kill_existing():
    class PE(ctypes.Structure):
        _fields_ = [
            ("dwSize", w.DWORD), ("cntUsage", w.DWORD),
            ("th32ProcessID", w.DWORD), ("th32DefaultHeapID", ctypes.c_void_p),
            ("th32ModuleID", w.DWORD), ("cntThreads", w.DWORD),
            ("th32ParentProcessID", w.DWORD), ("pcPriClassBase", ctypes.c_long),
            ("dwFlags", w.DWORD), ("szExeFile", ctypes.c_char * 260),
        ]

    snap = kernel32.CreateToolhelp32Snapshot(2, 0)
    e = PE()
    e.dwSize = ctypes.sizeof(PE)
    ok = kernel32.Process32First(snap, ctypes.byref(e))
    n = 0
    while ok:
        if b"sercon-gui" in e.szExeFile:
            h = kernel32.OpenProcess(PROCESS_TERMINATE, False, e.th32ProcessID)
            if h:
                kernel32.TerminateProcess(h, 0)
                kernel32.CloseHandle(h)
                n += 1
        ok = kernel32.Process32Next(snap, ctypes.byref(e))
    kernel32.CloseHandle(snap)
    return n


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    demo = "--demo" in sys.argv
    total = float(args[0]) if len(args) > 0 else 60.0
    slice_s = float(args[1]) if len(args) > 1 else 2.0

    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    exe = os.path.abspath(args[2]) if len(args) > 2 else os.path.join(root, "dist", "sercon-gui.exe")
    if not os.path.exists(exe):
        sys.exit("not found: %s" % exe)
    print("binary: %s%s" % (exe, "  (animation demo on)" if demo else ""))

    n = kill_existing()
    if n:
        print("stopped %d existing instance(s)" % n)
    time.sleep(0.4)
    sock = os.path.join(os.environ.get("LOCALAPPDATA", ""), "sercon", "run", "s.sock")
    try:
        os.rename(sock, sock + ".stale")
    except OSError:
        pass

    env = dict(os.environ)
    if demo:
        env["SERCON_ANIM_DEMO"] = "1"
    proc = subprocess.Popen([exe], cwd=os.path.dirname(exe), env=env,
                            creationflags=DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP)
    h = None
    for _ in range(100):
        # OpenProcess needs the process to exist; retry briefly since the
        # handle cannot be taken from the Popen object once it is detached.
        h = kernel32.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, False, proc.pid)
        if h:
            break
        time.sleep(0.1)
    if not h:
        sys.exit("could not open pid %d: %d" % (proc.pid, ctypes.get_last_error()))

    # Confirm the handle is real before trusting any number it produces. A
    # failed OpenProcess leaves a zero handle, and every reading off it would
    # be a plausible-looking zero.
    probe = cpu_time(h)
    print("pid %d, cpu time at start: %.2f ms" % (proc.pid, probe / 1e4))

    print("%8s  %10s  %10s  %s" % ("t (s)", "cpu (ms)", "% of core", "note"))
    elapsed = 0.0
    total_ms = 0.0
    busy = 0
    prev_c, prev_t = probe, time.time()
    while elapsed < total:
        time.sleep(slice_s)
        now, t = cpu_time(h), time.time()
        wall = t - prev_t
        ms = (now - prev_c) / 1e4
        pct = ms / 1000 / wall * 100
        total_ms += ms
        if pct > 0.5:
            note = "busy"
        elif pct > 0.01:
            note = "light"
        else:
            note = "idle"
        if pct > 0.01:
            busy += 1
        elapsed += wall
        print("%8.1f  %10.2f  %10.4f  %s" % (elapsed, ms, pct, note))
        prev_c, prev_t = now, t

    avg = total_ms / 1000 / elapsed * 100
    print()
    print("total %.2f ms over %.1fs: %.4f%% of one core; %d of %d slices above 0.01%%"
          % (total_ms, elapsed, avg, busy, int(elapsed / slice_s)))

    kernel32.CloseHandle(h)
    proc.terminate()
    proc.wait(timeout=5)

    # The window must not keep a frame clock running once nothing is moving.
    # With the demo tour on, the last third of the run is past the end of the
    # tour, so a process still busy there has a timer that never stopped.
    if demo:
        if busy <= 2:
            print("PASS: the frame clock stopped when the tour finished")
        else:
            print("CHECK: %d of %d slices were busy after the tour started; "
                  "confirm those are the daemon's serial polling and not the clock"
                  % (busy, int(elapsed / slice_s)))


if __name__ == "__main__":
    main()
