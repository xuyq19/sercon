"""Drive the sercon GUI from a script.

Two operations, both of which the development loop needs: click a control, and
read or write a control's text. They exist so a panel can be exercised and
screenshotted without a human in the loop, which matters for the SSH key panel
because most of its paths go through a UAC prompt.

Stdlib only.

    python hack/gui-drive.py serconKeyWindow 2011 set "ssh-ed25519 AAAA..."
    python hack/gui-drive.py serconKeyWindow 2011 get
    python hack/gui-drive.py serconKeyWindow 2012 click
    python hack/gui-drive.py serconKeyWindow --list
"""

import ctypes
import ctypes.wintypes as w
import sys

user32 = ctypes.WinDLL("user32", use_last_error=True)
user32.FindWindowW.restype = w.HWND
user32.GetDlgItem.restype = w.HWND
user32.SendMessageW.restype = ctypes.c_ssize_t

BM_CLICK = 0x00F5
WM_SETTEXT = 0x000C
WM_GETTEXT = 0x000D
WM_GETTEXTLENGTH = 0x000E
LB_SETCURSEL = 0x0186
LB_GETCURSEL = 0x0188
LB_GETCOUNT = 0x018B
LB_GETTEXT = 0x0189
LB_GETTEXTLEN = 0x018A
GWL_ID = -12

ENUM_CALLBACK = ctypes.WINFUNCTYPE(w.BOOL, w.HWND, w.LPARAM)


def find_window(class_name):
    hwnd = user32.FindWindowW(class_name, None)
    if not hwnd:
        sys.exit("window class %r not found" % class_name)
    return hwnd


def get_text(hwnd):
    n = user32.SendMessageW(hwnd, WM_GETTEXTLENGTH, 0, 0)
    buf = ctypes.create_unicode_buffer(n + 1)
    user32.SendMessageW(hwnd, WM_GETTEXT, n + 1, buf)
    return buf.value


def list_children(parent):
    """Print every child control with its id, class and current text."""
    def cb(child, _):
        buf = ctypes.create_unicode_buffer(256)
        user32.GetWindowTextW(child, buf, 256)
        cbuf = ctypes.create_unicode_buffer(256)
        user32.GetClassNameW(child, cbuf, 256)
        cid = user32.GetWindowLongW(child, GWL_ID)
        print("  id=%-6d %-16s %r" % (cid, cbuf.value, buf.value))
        return True

    user32.EnumChildWindows(parent, ENUM_CALLBACK(cb), 0)


def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)

    class_name = sys.argv[1]
    parent = find_window(class_name)

    if sys.argv[2] == "--list":
        print("%s:" % class_name)
        list_children(parent)
        return

    ctrl_id = int(sys.argv[2])
    action = sys.argv[3]

    # GetDlgItem only finds immediate children, which is all this needs: every
    # control in these windows is a direct child.
    child = user32.GetDlgItem(parent, ctrl_id)
    if not child:
        sys.exit("control %d not found in %s" % (ctrl_id, class_name))

    if action == "click":
        # BM_CLICK makes the button send WM_COMMAND to its parent, which is
        # what a real click produces.
        user32.SendMessageW(child, BM_CLICK, 0, 0)
        print("clicked %d" % ctrl_id)
    elif action == "get":
        print(get_text(child))
    elif action == "set":
        user32.SendMessageW(child, WM_SETTEXT, 0, sys.argv[4])
        print("set %d" % ctrl_id)
    elif action == "select":
        # ListBox selection. GetWindowText on a ListBox returns nothing useful,
        # so --list cannot show the items; this reads one back instead.
        idx = int(sys.argv[4])
        user32.SendMessageW(child, LB_SETCURSEL, idx, 0)
        got = user32.SendMessageW(child, LB_GETCURSEL, 0, 0)
        print("selected index %d" % got)
    elif action == "items":
        # ListBox contents, read one item at a time.
        count = user32.SendMessageW(child, LB_GETCOUNT, 0, 0)
        for i in range(count):
            n = user32.SendMessageW(child, LB_GETTEXTLEN, i, 0)
            buf = ctypes.create_unicode_buffer(n + 1)
            user32.SendMessageW(child, LB_GETTEXT, i, buf)
            print("  [%d] %s" % (i, buf.value))
    else:
        sys.exit("unknown action %r" % action)


if __name__ == "__main__":
    main()
