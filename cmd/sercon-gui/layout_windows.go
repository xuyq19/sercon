//go:build windows

// Layout: where everything sits, and the hit-testing that goes with it.
//
// The geometry is computed once per resize into a single struct, and everything
// afterwards — painting, hover, click — reads from that struct rather than
// recomputing offsets. Two places computing the same rectangle independently is
// how a control ends up drawn in one spot and clickable in another.
package main

// Metrics of the rail, in device pixels at 96 DPI. The window is DPI-aware, so
// these are the values the layout actually uses.
const (
	railWidth    int32 = 196
	railItemH    int32 = 30
	railCaptionH int32 = 26
	railPadX     int32 = 18
	railItemPadX int32 = 14

	contentPadX int32 = 22
	titleH      int32 = 26
	subtitleH   int32 = 20
	counterH    int32 = 52

	rowH     int32 = 38
	colHeadH int32 = 28
	footerH  int32 = 34
)

// railItem is one entry in the sidebar.
type railItem struct {
	label string
	rect  rect
	// id is the command the item dispatches, reusing the button ids so the
	// existing command handler keeps working.
	id uintptr
	// group starts a new section, which draws a caption above it.
	group string
}

// layout is the resolved geometry of the whole client area.
type layout struct {
	client rect
	rail   rect
	body   rect

	items      []railItem
	railFooter rect

	title    rect
	subtitle rect
	counters rect
	head     rect
	table    rect
	buttons  []rect
	footer   rect

	// cols holds the table's column origins and widths, resolved from the
	// measured header text rather than fixed numbers.
	cols []tableCol
}

// tableCol is one resolved table column.
type tableCol struct {
	title string
	x     int32
	w     int32
	right bool // the log column is the flexible one and gets the remainder
}

// buildLayout resolves the geometry for a client area.
//
// It is called from WM_SIZE and from a DPI change, and its result is stored on
// the window state; nothing else recomputes these numbers.
func buildLayout(w, h int32) layout {
	var l layout
	l.client = rectAt(0, 0, w, h)

	railW := railWidth
	// Below a certain width the rail would leave the table too narrow to be
	// useful, so it is dropped rather than squeezed.
	if w < 620 {
		railW = 0
	}
	l.rail = rectAt(0, 0, railW, h)
	l.body = rect{railW, 0, w, h}

	x := railW + contentPadX
	innerW := w - x - contentPadX
	if innerW < 240 {
		innerW = 240
	}

	y := int32(16)
	l.title = rectAt(x, y, innerW, titleH)
	y += titleH
	l.subtitle = rectAt(x, y, innerW, subtitleH)
	y += subtitleH + 14

	l.counters = rectAt(x, y, innerW, counterH)
	y += counterH + 10

	// The footer bar and the button row are pinned to the bottom; the table
	// takes whatever is between them.
	l.footer = rectAt(railW, h-footerH, w-railW, footerH)
	btnY := l.footer.Top - 12 - 34
	l.buttons = buildButtons(x, btnY, innerW)
	if railW == 0 {
		l.buttons = buildButtons(contentPadX, btnY, w-2*contentPadX)
	}

	headY := y
	l.head = rectAt(x, headY, innerW, colHeadH)
	tableY := headY + colHeadH
	tableH := btnY - 14 - tableY
	if tableH < rowH*2 {
		tableH = rowH * 2
	}
	l.table = rectAt(x, tableY, innerW, tableH)

	l.cols = buildColumns(innerW)
	l.items = buildRailItems(l.rail, h)
	l.railFooter = rectAt(railItemPadX, h-64, railW-railItemPadX*2, 52)

	return l
}

// buildRailItems lays out the sidebar entries.
//
// The entries start below the caption that introduces them. They are all in
// one group: the earlier four-section arrangement needed a caption per section
// and there is only one section so far, so the extra captions were pure noise
// standing in for functionality that does not exist yet.
func buildRailItems(rail rect, h int32) []railItem {
	if rail.Right-rail.Left == 0 {
		return nil
	}

	spec := []struct {
		label string
		id    uintptr
	}{
		{"Ports", idRailPorts},
		{"Logs", idRailLogs},
		{"SSH keys", idRailKeys},
		{"Settings", idRailSettings},
	}

	items := make([]railItem, 0, len(spec))
	y := railCaptionTop + railCaptionH
	for _, s := range spec {
		r := rectAt(railItemPadX, y, rail.Right-rail.Left-railItemPadX*2, railItemH)
		items = append(items, railItem{label: s.label, rect: r, id: s.id})
		y += railItemH + railItemGap
	}
	return items
}

// railCaptionTop is where the "CAPTURE" caption sits.
const (
	railCaptionTop int32 = 22
	railItemGap    int32 = 4
)

// buildButtons places the four action buttons in the content area's footer row.
//
// The first two sit on the left, because they act on the selection above them;
// the last two sit on the right, because they act on the window as a whole.
// Mixing them into one centred row was the previous arrangement and it gave no
// clue which buttons were related.
func buildButtons(x, y, w int32) []rect {
	const bw, gap int32 = 148, 10
	m := rectAt(x, y, bw, 34)
	c := rectAt(x+bw+gap, y, bw+30, 34)
	s := rectAt(x+w-bw-gap-bw, y, bw, 34)
	r := rectAt(x+w-bw, y, bw, 34)
	return []rect{m, c, s, r}
}

// buildColumns resolves the table columns.
//
// The first columns are fixed width because their content is fixed width — a
// port reference, a state capsule, a baud rate. The log column absorbs the
// remainder, and is clipped when there is not enough room, which is why it is
// last.
func buildColumns(innerW int32) []tableCol {
	spec := []struct {
		title string
		w     int32
	}{
		{"port", 96},
		{"state", 104},
		{"owner", 92},
		{"device", 92},
		{"baud", 76},
		{"observers", 72},
	}
	cols := make([]tableCol, 0, len(spec)+1)

	// The fixed columns are the first to be squeezed when the window is
	// narrow. Scaling them proportionally rather than clipping the last one
	// keeps every column present and every header readable, which is the
	// difference between a cramped table and a broken one.
	total := int32(0)
	for _, s := range spec {
		total += s.w
	}
	scale := 1.0
	if innerW < total {
		scale = float64(innerW) / float64(total)
	}

	x := int32(0)
	for _, s := range spec {
		w := int32(float64(s.w) * scale)
		if w < 24 {
			w = 24
		}
		cols = append(cols, tableCol{title: s.title, x: x, w: w})
		x += w
	}

	// The log column takes the remainder. It is clamped to zero rather than to
	// a minimum: a minimum wider than the space available would place the
	// column's right edge past the table's, and the right-aligned text in it
	// would be drawn outside the area the caller clips to.
	rem := innerW - x
	if rem < 0 {
		rem = 0
	}
	cols = append(cols, tableCol{title: "log file", x: x, w: rem, right: true})
	return cols
}

// railAt returns the sidebar item under a point, if any.
func (l *layout) railAt(p point) (railItem, bool) {
	for _, it := range l.items {
		if pointIn(it.rect, p) {
			return it, true
		}
	}
	return railItem{}, false
}

// buttonAt returns the index of the button under a point, or -1.
func (l *layout) buttonAt(p point) int {
	for i, r := range l.buttons {
		if pointIn(r, p) {
			return i
		}
	}
	return -1
}

// rowAt maps a point in the table to a row index, or -1.
func (l *layout) rowAt(p point, rows int) int {
	if !pointIn(l.table, p) {
		return -1
	}
	i := int((p.Y - l.table.Top) / rowH)
	if i < 0 || i >= rows {
		return -1
	}
	// The trailing area below the last row is not a row; without this a click
	// in the empty space under the table would select something off the end.
	rowBottom := l.table.Top + int32(i+1)*rowH
	if p.Y >= rowBottom || rowBottom > l.table.Bottom+rowH {
		return -1
	}
	return i
}

// counterRects splits the counter strip into evenly spaced cells.
func (l *layout) counterRects() []rect {
	const n = 4
	w := (l.counters.Right - l.counters.Left) / n
	out := make([]rect, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, rectAt(l.counters.Left+int32(i)*w, l.counters.Top, w, l.counters.Bottom-l.counters.Top))
	}
	return out
}
