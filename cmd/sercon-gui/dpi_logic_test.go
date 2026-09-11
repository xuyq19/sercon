//go:build windows

package main

import "testing"

func TestDpiScale(t *testing.T) {
	cases := []struct {
		value int32
		dpi   uint32
		want  int32
	}{{96, 96, 96}, {96, 144, 144}, {10, 120, 13}, {20, 0, 20}}
	for _, test := range cases {
		if got := dpiScale(test.value, test.dpi); got != test.want {
			t.Fatalf("dpiScale(%d, %d) = %d; want %d", test.value, test.dpi, got, test.want)
		}
	}
}

func TestSuggestedRectSizeClampsInvalidBounds(t *testing.T) {
	width, height := suggestedRectSize(rect{Left: 40, Top: 30, Right: 20, Bottom: 10})
	if width != 0 || height != 0 {
		t.Fatalf("size = (%d,%d); want (0,0)", width, height)
	}
}
