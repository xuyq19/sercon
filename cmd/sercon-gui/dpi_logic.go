//go:build windows

package main

// dpiScale rounds a 96-DPI device-pixel metric for a target monitor DPI.
func dpiScale(value int32, dpi uint32) int32 {
	if dpi == 0 {
		dpi = 96
	}
	return int32((int64(value)*int64(dpi) + 48) / 96)
}

// suggestedRectSize returns the non-negative dimensions encoded by a suggested
// WM_DPICHANGED rectangle. It is independent of Win32 calls for unit testing.
func suggestedRectSize(r rect) (int32, int32) {
	width := r.Right - r.Left
	height := r.Bottom - r.Top
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	return width, height
}
