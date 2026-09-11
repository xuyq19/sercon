//go:build windows

// The sidebar renderer.
//
// The rail is painted in one pass: background, entries, then the footer
// statistics. Each entry carries its own hover and selection values so the
// animation is per-entry rather than global — a rising highlight on the item
// the cursor just left should not be reset by a highlight starting on the one
// it entered.
package main

import (
	"fmt"
	"strings"
	"time"
)

// railEntry is one sidebar row plus its animation state.
type railEntry struct {
	item railItem

	hover tween // 0..1 highlight strength
	sel   tween // 0..1 selection strength
	// press is set between mouse-down and mouse-up, which is what makes the
	// click feel connected to the cursor.
	press bool
}

// hoverDur and selDur are the highlight timings. They are short on purpose:
// a hover highlight that takes longer than about a fifth of a second stops
// reading as feedback and starts reading as lag.
const (
	hoverDur = 110 * time.Millisecond
	selDur   = 160 * time.Millisecond
)

// railState is the sidebar's live state.
type railState struct {
	entries []railEntry
	hover   int // index under the cursor, -1 when none
	selID   uintptr
}

var rail = railState{selID: idRailPorts}

// syncRail rebuilds the entry list when the layout changes, preserving the
// animation state of entries that survive so a resize does not restart every
// highlight.
func syncRail(l *layout) {
	if len(rail.entries) == len(l.items) {
		same := true
		for i := range l.items {
			if rail.entries[i].item.label != l.items[i].label {
				same = false
				break
			}
		}
		if same {
			for i := range l.items {
				rail.entries[i].item = l.items[i]
			}
			applyRailSelection()
			return
		}
	}

	old := rail.entries
	rail.entries = make([]railEntry, len(l.items))
	for i, it := range l.items {
		e := railEntry{item: it}
		// Carry over the animation of a matching entry.
		for j := range old {
			if old[j].item.label == it.label {
				e.hover, e.sel, e.press = old[j].hover, old[j].sel, old[j].press
				break
			}
		}
		rail.entries[i] = e
	}
	applyRailSelection()
}

// applyRailSelection settles each entry's selection value against the current
// section.
//
// It sets the value outright rather than animating it. The selection only
// changes when the operator clicks, and a click already produces a response —
// the fill, the accent bar and the panel swapping. Sliding the highlight from
// one item to another on top of that reads as the interface catching up.
func applyRailSelection() {
	for i := range rail.entries {
		v := 0.0
		if rail.entries[i].item.id == rail.selID {
			v = 1
		}
		rail.entries[i].sel = newTween(v)
	}
}

// railElems returns the entries as animatables, which is how the shared clock
// drives them.
func railElems() []animatable {
	out := make([]animatable, 0, len(rail.entries))
	for i := range rail.entries {
		out = append(out, &rail.entries[i])
	}
	return out
}

// step advances one entry's highlights.
func (e *railEntry) step(now int64) bool {
	moving := e.hover.step(now)
	if e.sel.step(now) {
		moving = true
	}
	return moving
}

// paintRail draws the entire sidebar.
//
// Everything is clipped to the rail. The rail is painted last, over the content
// area, so anything that overflows its column lands on top of the buttons
// rather than being hidden — which is what the build line did once the version
// gained a timestamp. The clip makes the rail's width a hard boundary instead
// of something every line of text has to be trusted to respect.
func paintRail(hdc uintptr, l *layout) {
	if l.rail.Right-l.rail.Left == 0 {
		return
	}

	clip := clipTo(hdc, l.rail)
	defer clip.release()

	fillRect(hdc, l.rail, colRailBG)

	// The text colour has to be set before every draw, because the content
	// area is painted first and leaves its own colour selected. Leaving this
	// out is what made every sidebar label come out in the content area's
	// near-black on the dark rail — invisible rather than missing.
	oldFont, _, _ := pSelectObject.Call(hdc, fonts.rail)
	pSetTextColor.Call(hdc, uintptr(colRailText))
	pSetBkMode.Call(hdc, transparent)

	textAt(hdc, railItemPadX, railCaptionTop+6, "CAPTURE", colRailDim)

	for i := range rail.entries {
		paintRailEntry(hdc, &rail.entries[i])
	}

	pSelectObject.Call(hdc, oldFont)
	paintRailFooter(hdc, l, l.railFooter)
}

// paintRailEntry draws one sidebar row.
func paintRailEntry(hdc uintptr, e *railEntry) {
	r := e.item.rect
	if r.Right <= r.Left {
		return
	}

	// The selection and hover fills are independent: a selected item can also
	// be hovered, and the two are layered rather than one replacing the other.
	if h := e.hover.value; h > 0.01 {
		fillRect(hdc, r, lerp(colRailBG, colRailHover, h))
	}
	if s := e.sel.value; s > 0.01 {
		fillRect(hdc, r, lerp(colRailBG, colRailSel, s))
	}

	// The accent bar grows out of the left edge with the selection rather than
	// appearing at full height, so the movement tracks the fill.
	if s := e.sel.value; s > 0.01 {
		barH := int32(float64(r.Bottom-r.Top) * s)
		barY := r.Top + (r.Bottom-r.Top-barH)/2
		fillRect(hdc, rect{r.Left, barY, r.Left + 2, barY + barH}, colRailSelBar)
	}

	// Text colour moves with whichever highlight is stronger.
	t := e.hover.value
	if e.sel.value > t {
		t = e.sel.value
	}
	color := lerp(colRailText, colRailSelText, t)

	oldFont, _, _ := pSelectObject.Call(hdc, fonts.rail)
	textAt(hdc, r.Left+railItemPadX+6, r.Top+7, e.item.label, color)
	pSelectObject.Call(hdc, oldFont)
}

// paintRailFooter draws the build and volume summary at the bottom of the rail.
func paintRailFooter(hdc uintptr, l *layout, r rect) {
	if r.Bottom <= r.Top {
		return
	}

	ruleY := r.Top - 16
	hLine(hdc, railItemPadX, l.rail.Right-railItemPadX, ruleY, colRailRule)

	old, _, _ := pSelectObject.Call(hdc, fonts.railFooter)
	lines := railFooterLines()
	for i, s := range lines {
		textAt(hdc, r.Left, r.Top+int32(i)*16, s, colRailDim)
	}
	pSelectObject.Call(hdc, old)
}

// railFooterText is the footer as a single comparable string, so the refresh
// tick can tell whether anything needs repainting without walking the lines.
var railFooterText string

var railFooterCache []string

func railFooterLines() []string { return railFooterCache }

// railFooterFor renders the footer block.
//
// The version is the compact form, not version.Short(): the rail is 168px wide
// and the full identity does not fit. The commit is what identifies the build.
func railFooterFor(version string, ports, online int, logged string) string {
	lines := []string{
		fmt.Sprintf("build %s", version),
		fmt.Sprintf("%d ports, %d online", ports, online),
		logged,
	}
	railFooterCache = lines
	return strings.Join(lines, "\n")
}

func setRailFooter(text string) {
	railFooterText = text
	railFooterCache = strings.Split(text, "\n")
}
