"""Capture the sercon GUI window to a PNG.

Uses PrintWindow with PW_RENDERFULLCONTENT so only this window's own content is
rendered into the bitmap. Nothing else on the desktop ends up in the image, and
an occluded window still captures correctly.

Stdlib only: ctypes for the Win32 calls, zlib for PNG compression.
"""

import ctypes
import ctypes.wintypes as w
import struct
import sys
import zlib

user32 = ctypes.WinDLL("user32", use_last_error=True)
gdi32 = ctypes.WinDLL("gdi32", use_last_error=True)

SRCCOPY = 0x00CC0020
PW_RENDERFULLCONTENT = 0x00000002
BI_RGB = 0
DIB_RGB_COLORS = 0

CLASS_NAME = "serconGuiWindow"


class BITMAPINFOHEADER(ctypes.Structure):
    _fields_ = [
        ("biSize", w.DWORD),
        ("biWidth", w.LONG),
        ("biHeight", w.LONG),
        ("biPlanes", w.WORD),
        ("biBitCount", w.WORD),
        ("biCompression", w.DWORD),
        ("biSizeImage", w.DWORD),
        ("biXPelsPerMeter", w.LONG),
        ("biYPelsPerMeter", w.LONG),
        ("biClrUsed", w.DWORD),
        ("biClrImportant", w.DWORD),
    ]


class BITMAPINFO(ctypes.Structure):
    _fields_ = [
        ("bmiHeader", BITMAPINFOHEADER),
        ("bmiColors", w.DWORD * 3),
    ]


def write_png(path, width, height, rows):
    """rows: list of bytes, each row already RGB (3 bytes per pixel)."""
    raw = b"".join(b"\x00" + r for r in rows)

    def chunk(tag, data):
        return (
            struct.pack(">I", len(data))
            + tag
            + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    blob = (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(raw, 6))
        + chunk(b"IEND", b"")
    )
    with open(path, "wb") as fh:
        fh.write(blob)


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    out = args[0] if args else "gui-shot.png"
    # The key panel is a second window of the same process, so the capture
    # needs to be told which one to look at.
    class_name = args[1] if len(args) > 1 else CLASS_NAME

    hwnd = user32.FindWindowW(class_name, None)
    if not hwnd:
        sys.exit("window class %r not found" % class_name)

    rect = w.RECT()
    if not user32.GetWindowRect(hwnd, ctypes.byref(rect)):
        sys.exit("GetWindowRect failed")

    x, y = rect.left, rect.top
    width = rect.right - rect.left
    height = rect.bottom - rect.top
    if width <= 0 or height <= 0:
        sys.exit("window has no area")

    hdc_window = user32.GetWindowDC(hwnd)
    hdc_mem = gdi32.CreateCompatibleDC(hdc_window)
    hbmp = gdi32.CreateCompatibleBitmap(hdc_window, width, height)
    gdi32.SelectObject(hdc_mem, hbmp)

    # PrintWindow renders the window into our DC. Using it rather than BitBlt
    # off the screen means the capture is of the window itself, not of whatever
    # happens to be on top of it.
    rendered = user32.PrintWindow(hwnd, hdc_mem, PW_RENDERFULLCONTENT)
    if not rendered:
        gdi32.BitBlt(hdc_mem, 0, 0, width, height, hdc_window, 0, 0, SRCCOPY)

    info = BITMAPINFO()
    info.bmiHeader.biSize = ctypes.sizeof(BITMAPINFOHEADER)
    info.bmiHeader.biWidth = width
    # Negative height asks for a top-down DIB, which saves flipping the rows.
    info.bmiHeader.biHeight = -height
    info.bmiHeader.biPlanes = 1
    info.bmiHeader.biBitCount = 32
    info.bmiHeader.biCompression = BI_RGB
    info.bmiHeader.biSizeImage = width * height * 4

    buf = ctypes.create_string_buffer(width * height * 4)
    got = gdi32.GetDIBits(hdc_mem, hbmp, 0, height, buf, ctypes.byref(info), DIB_RGB_COLORS)
    if got != height:
        sys.exit("GetDIBits returned %d of %d rows" % (got, height))

    raw = buf.raw
    stride = width * 4
    rows = []
    for r in range(height):
        row = raw[r * stride:(r + 1) * stride]
        # BGRA -> RGB. Slicing lane by lane and interleaving stays in C; a
        # per-pixel Python loop over three quarters of a megapixel does not.
        blue = row[0::4]
        green = row[1::4]
        red = row[2::4]
        rgb = bytearray(len(blue) * 3)
        rgb[0::3] = red
        rgb[1::3] = green
        rgb[2::3] = blue
        rows.append(bytes(rgb))

    write_png(out, width, height, rows)

    if "--histogram" in sys.argv:
        counts = {}
        for row in rows:
            for i in range(0, len(row), 3):
                key = (row[i], row[i + 1], row[i + 2])
                counts[key] = counts.get(key, 0) + 1
        print("%-16s %s" % ("#RRGGBB", "pixels"))
        for color, n in sorted(counts.items(), key=lambda kv: -kv[1])[:14]:
            print("%-16s %d" % ("#%02X%02X%02X" % color, n))

    gdi32.DeleteObject(hbmp)
    gdi32.DeleteDC(hdc_mem)
    user32.ReleaseDC(hwnd, hdc_window)

    print("captured %dx%d from (%d,%d) -> %s" % (width, height, x, y, out))


if __name__ == "__main__":
    main()
