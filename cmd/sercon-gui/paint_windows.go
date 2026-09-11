//go:build windows

// The content area renderer: the heading, the counters, the port table and the
// footer bar.
//
// It is all custom paint. The previous version used a SysListView32, which is
// the right choice when the contents are plain text — the control handles
// scrolling, selection and keyboard navigation for free. It is the wrong choice
// here for two reasons: the state column wants a capsule rather than text, and
// the row animations need to know where every row is, which the control does
// not expose. The cost is that selection, hover and scrolling become this
// file's job.
package main

import (
	"fmt"
	"time"

	"sercon/internal/proto"
)

// contentHover tracks which row the cursor is over and its fade value.
type contentHover struct {
	row   tween
	index int
}

var hover contentHover

// hoverDurContent is deliberately a little longer than the rail's, because a
// table row is a much larger area and a fast flash across a large surface
// reads as a glitch.
const hoverDurContent = 130 * time.Millisecond

// rowFade animates a row in and out when the set of ports changes.
//
// A row that has just appeared fades up from zero; one that has gone is drawn
// for as long as its fade lasts and then dropped. That is what stops the table
// from snapping when an adapter is plugged in.
type rowFade struct {
	ref   string
	t     tween
	dying bool
	data  proto.PortInfo
}

type rowsState struct {
	items []rowFade
}

var rows rowsState

func (r *rowFade) step(now int64) bool { return r.t.step(now) }
func (h *contentHover) step(now int64) bool {
	return h.row.step(now)
}

// rowElems returns the animated row elements plus the hover.
func rowElems() []animatable {
	out := make([]animatable, 0, len(rows.items)+1)
	for i := range rows.items {
		out = append(out, &rows.items[i])
	}
	out = append(out, &hover)
	return out
}

// syncRows reconciles the painted rows with the daemon's port table.
//
// Existing rows keep their identity and therefore their fade value; new ports
// start invisible and fade up; ports that have disappeared are kept for the
// length of a fade-out so they can be animated away rather than blinking out.
//
// It reports whether the row set changed, which is what decides if a frame is
// needed at all.
func syncRows(ports []proto.PortInfo, now int64) bool {
	incoming := make(map[string]proto.PortInfo, len(ports))
	for _, p := range ports {
		incoming[p.Ref] = p
	}

	changed := false

	// Update the rows that survive, and retire the ones that did not.
	kept := rows.items[:0]
	for i := range rows.items {
		r := &rows.items[i]
		if p, ok := incoming[r.ref]; ok {
			r.data = p
			r.dying = false
			kept = append(kept, *r)
			delete(incoming, r.ref)
		} else if !r.dying {
			// The fade-out is started from the paint path, not here.
			//
			// syncRows runs on the one-second refresh tick as well as on the
			// animation frames, and setTarget restarts the clock from the
			// value it is called with. Starting the fade here means every
			// refresh tick resets it back to a full row — a disappearing port
			// would sit at full height indefinitely and the fade would never
			// run. beginRowFade is idempotent and runs every frame instead.
			r.dying = true
			kept = append(kept, *r)
			changed = true
		} else {
			kept = append(kept, *r)
		}
	}
	rows.items = kept

	// Append the ports that were not already on screen, in the order the
	// daemon reported them so the table does not reshuffle as adapters appear.
	for _, p := range ports {
		if _, ok := incoming[p.Ref]; !ok {
			continue
		}
		rf := rowFade{ref: p.Ref, data: p}
		rf.t = newTween(0)
		rf.t.setTarget(1, fadeInDur, now)
		rows.items = append(rows.items, rf)
		delete(incoming, p.Ref)
		changed = true
	}

	return changed
}

// pruneRows drops rows whose fade-out has finished.
func pruneRows() {
	kept := rows.items[:0]
	for i := range rows.items {
		r := &rows.items[i]
		// Both conditions are needed. The tween's value can only reach zero
		// on a frame that the tween itself has stepped, and the last such
		// frame is the one where step reports that it has stopped.
		if r.dying && !r.t.active && r.t.value <= 0.001 {
			continue
		}
		kept = append(kept, *r)
	}
	rows.items = kept
}

// beginRowFade starts a dying row's fade-out. Calling it again on a row that
// is already fading does nothing, which is what lets it run from the paint
// path — the only place that runs on animation frames rather than on the
// once-a-second refresh.
func (r *rowFade) beginRowFade(now int64) {
	if r.dying && !r.t.active && r.t.to != 0 {
		r.t.setTarget(0, fadeOutDur, now)
	}
}

// fadeOutDur and fadeInDur are how long a row takes to leave and arrive.
const (
	fadeOutDur = 160 * time.Millisecond
	fadeInDur  = 180 * time.Millisecond
)

// rowCount is how many live rows the table is showing.
func rowCount() int {
	n := 0
	for i := range rows.items {
		if !rows.items[i].dying {
			n++
		}
	}
	return n
}

// rowHeightOf returns a row's drawn height, which eases in with its fade so a
// new row grows into place instead of shoving the rows below it down.
//
// A 0.6 floor rather than 0: at zero the row would have no area, its columns
// would be computed against a zero-width rect, and the row would be invisible
// for the first frames anyway. Starting at just over half height and easing to
// full reads as the row settling rather than as a slide.
func (r *rowFade) height() int32 {
	v := r.t.value
	if r.dying {
		return int32(float64(rowH) * v)
	}
	return int32(float64(rowH) * (0.6 + 0.4*v))
}

// rowAlpha is how opaque the row's contents are drawn.
//
// The height floor above means the row box is already visible at v=0, so the
// contents have to fade separately or a new row would appear at over half its
// height with all of its text at full strength and no fade at all.
func (r *rowFade) alpha() float64 {
	return r.t.value
}

// paintContent draws everything to the right of the rail.
func paintContent(hdc uintptr, l *layout, ports []proto.PortInfo, sock string, flash string, now int64) {
	fillRect(hdc, l.body, colBackground)

	paintHeading(hdc, l, ports, now)
	paintCounters(hdc, l, ports, now)
	paintTableHead(hdc, l)
	paintRows(hdc, l, now)
	paintButtons(hdc, l)
	paintFooter(hdc, l, sock, flash)
}

func paintHeading(hdc uintptr, l *layout, ports []proto.PortInfo, now int64) {
	old, _, _ := pSelectObject.Call(hdc, fonts.title)
	textAt(hdc, l.title.Left, l.title.Top+3, "Serial console capture", colText)
	pSelectObject.Call(hdc, old)

	online, held, observing := tally(ports)

	pSelectObject.Call(hdc, fonts.subtitle)
	if len(ports) == 0 {
		textAt(hdc, l.subtitle.Left, l.subtitle.Top+3,
			"capturing · no serial ports found", colTextDim)
	} else {
		s := fmt.Sprintf("%d online · %d in use · %d observing", online, held, observing)
		textAt(hdc, l.subtitle.Left, l.subtitle.Top+3, s, colTextDim)
	}
	pSelectObject.Call(hdc, old)
}

func tally(ports []proto.PortInfo) (online, held, observing int) {
	for _, p := range ports {
		if p.Online {
			online++
		}
		if p.Owner != "" {
			held++
		}
		observing += p.Observers
	}
	return
}

// paintCounters draws the four statistics. Each is a number in the monospace
// face with a caption above it, and the number is the only large text in the
// window besides the title — which is what makes the strip readable at a
// glance rather than being a line of small print.
func paintCounters(hdc uintptr, l *layout, ports []proto.PortInfo, now int64) {
	online, held, observing := tally(ports)

	vals := []struct {
		label string
		value int
		color uint32
	}{
		{"PORTS", len(ports), colText},
		{"ONLINE", online, colOnline},
		{"WRITABLE", held, colBusy},
		{"OBSERVING", observing, colText},
	}

	cells := l.counterRects()
	for i, v := range vals {
		if i >= len(cells) {
			break
		}
		c := cells[i]

		pSelectObject.Call(hdc, fonts.counterLabel)
		textAt(hdc, c.Left, c.Top, v.label, colHint)

		// The number rolls towards its new value rather than jumping. It is
		// read from the animation state rather than the live value so the drawn
		// value and the animation stay consistent between frames.
		n := counterValue(i, float64(v.value), now)

		col := v.color
		if v.value == 0 {
			col = colHint
		}

		pSelectObject.Call(hdc, fonts.counter)
		textAt(hdc, c.Left, c.Top+15, fmt.Sprintf("%d", n), col)
	}
	pSelectObject.Call(hdc, fonts.body)
}

// counters holds the animated values behind paintCounters.
var counters [4]tween
var countersInit bool

// counterElems exposes the counters as animatables so the shared clock drives
// their roll.
func counterElems() []animatable {
	out := make([]animatable, 0, len(counters))
	for i := range counters {
		out = append(out, &counters[i])
	}
	return out
}

// counterValue advances a counter towards v and returns the value to draw.
func counterValue(slot int, v float64, now int64) int {
	if !countersInit {
		for i := range counters {
			counters[i] = newTween(0)
		}
		countersInit = true
	}
	c := &counters[slot]
	if !c.active && c.to != v {
		c.setTarget(v, 320*time.Millisecond, now)
	}
	return int(c.value + 0.5)
}

// paintTableHead draws the column captions and the rule beneath them.
func paintTableHead(hdc uintptr, l *layout) {
	old, _, _ := pSelectObject.Call(hdc, fonts.colHead)
	for _, c := range l.cols {
		x := l.table.Left + c.x
		if c.right {
			textRight(hdc, x+c.w, l.head.Top+8, c.title, colHint)
		} else {
			textAt(hdc, x, l.head.Top+8, c.title, colHint)
		}
	}
	pSelectObject.Call(hdc, old)
	hLine(hdc, l.head.Left, l.head.Right, l.head.Bottom-1, colRule)
}

// paintRows draws the port table.
//
// Everything is clipped to the table's rect. A row's box is the full row height
// even while it is fading, so without the clip a table shorter than its rect
// would paint rows over the button row and the rail footer.
func paintRows(hdc uintptr, l *layout, now int64) {
	clip := clipTo(hdc, l.table)
	defer clip.release()

	y := l.table.Top
	for i := range rows.items {
		r := &rows.items[i]
		r.beginRowFade(now)
		h := rowH
		if r.dying {
			h = r.height()
		}
		if y >= l.table.Bottom {
			break
		}
		paintRow(hdc, l, r, y, h, i == hover.index)
		y += h
	}

	if len(rows.items) == 0 {
		pSelectObject.Call(hdc, fonts.body)
		textAt(hdc, l.table.Left, l.table.Top+16,
			"Waiting for a serial adapter…", colHint)
	}
}

// paintRow draws a single port row.
//
// alpha is the row's own opacity, which is separate from its fade tween because
// a row that is arriving also grows in height: its tween value drives both the
// box and the contents, so the two have to be applied together rather than the
// tween being read twice from different places.
func paintRow(hdc uintptr, l *layout, r *rowFade, y, h int32, hovered bool) {
	alpha := r.alpha()
	if alpha <= 0.01 {
		return
	}

	full := rect{l.table.Left, y, l.table.Right, y + rowH}

	// Row background. Only the hover highlight is drawn here; selection is a
	// property of the whole row and is painted by the caller of this function
	// when it exists.
	if hovered && hover.row.value > 0.01 {
		fillRectBlend(hdc, full, colBackground, colRowSel, hover.row.value*alpha)
	} else if rowIsSelected(r.ref) {
		fillRect(hdc, full, colRowSel)
	}

	textY := y + (rowH-15)/2
	if h < rowH {
		// A disappearing row's text rides up with it rather than being clipped
		// in place, which is what makes the fade read as a departure.
		textY = y + (h-15)/2
	}

	p := r.data
	for _, c := range l.cols {
		x := l.table.Left + c.x
		switch c.title {
		case "port":
			old, _, _ := pSelectObject.Call(hdc, fonts.bodyMedium)
			textAt(hdc, x, textY, p.Ref, lerp(colBackground, colText, alpha))
			pSelectObject.Call(hdc, old)

		case "state":
			paintStateCapsule(hdc, x, textY-3, p, alpha)

		case "owner":
			pSelectObject.Call(hdc, fonts.body)
			textAt(hdc, x, textY, orDash(p.Owner), lerp(colBackground, colTextDim, alpha))

		case "device":
			pSelectObject.Call(hdc, fonts.body)
			textAt(hdc, x, textY, p.Dev, lerp(colBackground, colTextDim, alpha))

		case "baud":
			pSelectObject.Call(hdc, fonts.bodyMedium)
			textAt(hdc, x, textY, fmt.Sprintf("%d", p.Baud), lerp(colBackground, colTextDim, alpha))

		case "observers":
			pSelectObject.Call(hdc, fonts.bodyMedium)
			s := "-"
			if p.Observers > 0 {
				s = fmt.Sprintf("%d", p.Observers)
			}
			textAt(hdc, x, textY, s, lerp(colBackground, colTextDim, alpha))

		case "log file":
			// Only the file name is shown, and only if the column has room.
			// The directory is the same for every port, so the full path would
			// repeat itself down the column and push the width elsewhere.
			pSelectObject.Call(hdc, fonts.body)
			if c.w <= 4 {
				continue
			}
			s := logBase(p.Log)
			if s == "" {
				s = "-"
			}
			textRight(hdc, x+c.w, textY,
				clampText(hdc, s, c.w), lerp(colBackground, colTextDim, alpha))
		}
	}
}

// paintStateCapsule draws the online/offline pill.
func paintStateCapsule(hdc uintptr, x, y int32, p proto.PortInfo, alpha float64) {
	label := "offline"
	var fg, bg uint32 = colOffline, colOfflineBG
	if p.Online {
		label = "online"
		fg, bg = colOnline, colOnlineBG
	}

	pSelectObject.Call(hdc, fonts.colHead)
	w := textWidth(hdc, label) + 26

	r := rectAt(x, y, w, 20)
	// The capsule fades with the row so a row appearing does not have its
	// strongest element pop in first.
	roundRectBlend(hdc, r, 10, colBackground, bg, alpha, 0)

	// The dot carries the state as well as the word, which is what makes the
	// column scannable without reading.
	dot := rectAt(r.Left+9, r.Top+7, 6, 6)
	roundRectBlend(hdc, dot, 3, colBackground, fg, alpha, 0)

	textAt(hdc, r.Left+20, r.Top+3, label, lerp(colBackground, fg, alpha))
}

// paintButtons draws the four action buttons.
func paintButtons(hdc uintptr, l *layout) {
	labels := []string{"Open log folder", "Copy attach command", "SSH keys", "Refresh"}
	for i, r := range l.buttons {
		if i >= len(labels) {
			break
		}
		h := btnHoverAt(i)
		if h <= 0.01 {
			roundRectFilled(hdc, r, 8, colBackground, colRule)
		} else {
			roundRectBlend(hdc, r, 8, colBackground, colRowSel, h, colRule)
		}
		pSelectObject.Call(hdc, fonts.body)
		tw := textWidth(hdc, labels[i])
		textAt(hdc, r.Left+(r.Right-r.Left-tw)/2, r.Top+10, labels[i], colText)
	}
}

// buttonHover holds the fade value per button.
var buttonHover [4]tween
var buttonsInit bool

// btnHoverAt is wired to the hover index by the window's mouse handler.
var btnHoverIndex = -1

func btnHoverAt(i int) float64 {
	if !buttonsInit {
		for j := range buttonHover {
			buttonHover[j] = newTween(0)
		}
		buttonsInit = true
	}
	return buttonHover[i].value
}

// paintFooter draws the bottom bar: the socket path on the left, the version on
// the right, and any transient message in place of the path.
func paintFooter(hdc uintptr, l *layout, sock string, flash string) {
	fillRect(hdc, l.footer, colRailBG)

	old, _, _ := pSelectObject.Call(hdc, fonts.footer)
	left := sock
	var col uint32 = colRailDim
	if flash != "" {
		left = flash
		col = colRailText
	}
	textAt(hdc, l.footer.Left+contentPadX, l.footer.Top+9, left, col)

	right := versionText()
	textRight(hdc, l.footer.Right-contentPadX, l.footer.Top+9, right, colRailDim)
	pSelectObject.Call(hdc, old)
}

// logBase returns the file name part of a log path, tolerating both separators.
func logBase(p string) string {
	if p == "" {
		return ""
	}
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '\\' || p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// clampText trims a string with an ellipsis so it fits in the given width.
func clampText(hdc uintptr, s string, w int32) string {
	if w <= 0 || textWidth(hdc, s) <= w {
		return s
	}
	runes := []rune(s)
	for len(runes) > 1 {
		runes = runes[:len(runes)-1]
		if textWidth(hdc, string(runes)+"…") <= w {
			return string(runes) + "…"
		}
	}
	return ""
}
