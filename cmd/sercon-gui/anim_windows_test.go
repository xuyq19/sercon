//go:build windows

// Tests for the GUI's animation and layout logic.
//
// These exist because the animations cannot be verified from outside the
// process. Driving the window with synthetic input is not available in every
// environment — a locked or non-interactive desktop refuses GetCursorPos,
// mouse_event produces no WM_MOUSEMOVE, and PrintWindow then captures a
// different desktop — so "the animation works" would otherwise rest on someone
// watching the window. The tween values and the layout geometry are ordinary
// data, and asserting on them directly is both stronger evidence and something
// that keeps holding as the code changes.
//
// The window handle is left at zero throughout. The paint code is not exercised
// here; what is exercised is the state the paint code reads.
package main

import (
	"syscall"
	"testing"
	"time"
)

// --- tween -----------------------------------------------------------------

func TestTweenSnapsWithZeroDuration(t *testing.T) {
	var tw tween = newTween(0)
	tw.setTarget(1, 0, 1000)
	if tw.value != 1 {
		t.Fatalf("value = %v, want 1", tw.value)
	}
	if tw.active {
		t.Fatal("active = true, want false: a zero duration must settle immediately")
	}
}

func TestTweenEasesTowardsTarget(t *testing.T) {
	var tw = newTween(0)
	tw.setTarget(1, 100*time.Millisecond, 1000)

	// Quarter of the way through. An ease-out curve is ahead of linear, so the
	// value must be past 0.25 but not yet at 1.
	tw.step(1025)
	if tw.value <= 0.25 {
		t.Fatalf("value = %v at 25%%; ease-out should be ahead of linear", tw.value)
	}
	if tw.value >= 1 {
		t.Fatalf("value = %v at 25%%; should not have arrived yet", tw.value)
	}
	if !tw.step(1025) {
		t.Fatal("step = false mid-flight, want true")
	}

	// Halfway.
	tw.step(1050)
	mid := tw.value
	if mid <= 0.25 || mid >= 1 {
		t.Fatalf("value = %v at 50%%, want a value strictly between", mid)
	}

	// Past the end settles exactly on the target and stops asking for frames.
	if tw.step(1101) {
		t.Fatal("step = true after the duration elapsed, want false")
	}
	if tw.value != 1 {
		t.Fatalf("value = %v after settling, want exactly 1", tw.value)
	}
}

func TestTweenIsMonotonic(t *testing.T) {
	var tw = newTween(0)
	tw.setTarget(1, 200*time.Millisecond, 5000)

	prev := -1.0
	for now := int64(5000); now <= 5200; now += 16 {
		tw.step(now)
		if tw.value < prev {
			t.Fatalf("value went backwards at t=%d: %v then %v", now, prev, tw.value)
		}
		prev = tw.value
	}
}

func TestTweenReversalStartsFromCurrentValue(t *testing.T) {
	// A hover that is interrupted mid-fade and sent back must ease from where
	// it currently is, not from the value it was heading towards. Starting
	// from the previous target is what makes a half-finished highlight jump.
	var tw = newTween(0)
	tw.setTarget(1, 100*time.Millisecond, 0)
	tw.step(50)
	mid := tw.value
	if mid <= 0 || mid >= 1 {
		t.Fatalf("value = %v, wanted a mid-flight value", mid)
	}

	tw.setTarget(0, 100*time.Millisecond, 50)
	if tw.from != mid {
		t.Fatalf("from = %v, want %v (the value at the moment of reversal)", tw.from, mid)
	}
	tw.step(75)
	if tw.value >= mid {
		t.Fatalf("value = %v after reversing; should have moved down from %v", tw.value, mid)
	}
}

func TestTweenDoesNotRestartWhenAlreadyHeadingToTarget(t *testing.T) {
	// The animation clock re-attaches the same elements every frame. If
	// setTarget restarted the clock on an already-correct target, nothing would
	// ever finish.
	var tw = newTween(0)
	tw.setTarget(1, 100*time.Millisecond, 0)
	tw.step(40)
	if !tw.active {
		t.Fatal("expected the tween to be mid-flight")
	}
	start := tw.startMS

	tw.setTarget(1, 100*time.Millisecond, 40)
	if tw.startMS != start {
		t.Fatalf("startMS moved %d -> %d; a redundant setTarget restarted the tween", start, tw.startMS)
	}
}

// --- row fades -------------------------------------------------------------

// resetRows clears the package-level row state between tests.
func resetRows() {
	rows = rowsState{}
	hover = contentHover{}
}

func TestRowFadeInStartsAtZero(t *testing.T) {
	resetRows()
	r := rowFade{ref: "COM1"}
	r.t = newTween(0)
	if r.alpha() != 0 {
		t.Fatalf("alpha = %v on a fresh row, want 0", r.alpha())
	}
	// A non-dying row keeps a height floor so it is never a zero-area rect.
	if h := r.height(); h <= 0 {
		t.Fatalf("height = %d on a fresh row, want > 0", h)
	}
}

func TestRowFadeOutShrinksToNothing(t *testing.T) {
	r := rowFade{ref: "COM1", dying: true}
	r.t = newTween(1)
	if h := r.height(); h != rowH {
		t.Fatalf("height = %d at the start of a fade-out, want %d", h, rowH)
	}

	r.t.setTarget(0, fadeOutDur, 0)
	r.t.step(int64(fadeOutDur / time.Millisecond))
	if h := r.height(); h != 0 {
		t.Fatalf("height = %d at the end of a fade-out, want 0", h)
	}
}

func TestBeginRowFadeIsIdempotent(t *testing.T) {
	// beginRowFade runs from the paint path, which runs on every animation
	// frame. If it restarted the tween each time, the fade would reset to a
	// full row 60 times a second and never complete.
	r := rowFade{ref: "COM1", dying: true}
	r.t = newTween(1)

	r.beginRowFade(0)
	if !r.t.active {
		t.Fatal("beginRowFade did not start the fade")
	}
	r.t.step(40)
	start := r.t.startMS
	value := r.t.value

	for i := 0; i < 10; i++ {
		r.beginRowFade(40 + int64(i))
	}
	if r.t.startMS != start {
		t.Fatalf("startMS moved %d -> %d; beginRowFade restarted a running fade", start, r.t.startMS)
	}
	if r.t.value != value {
		t.Fatalf("value moved %v -> %v; beginRowFade disturbed a running fade", value, r.t.value)
	}
}

func TestBeginRowFadeIgnoresLiveRows(t *testing.T) {
	r := rowFade{ref: "COM1"}
	r.t = newTween(1)
	r.beginRowFade(0)
	if r.t.active {
		t.Fatal("beginRowFade started a fade on a row that is not dying")
	}
}

func TestPruneRowsKeepsAFadeInFlightAndDropsAFinishedOne(t *testing.T) {
	resetRows()

	// A row that has only just started fading must survive the prune, or the
	// fade would never be seen.
	dying := rowFade{ref: "COM1", dying: true}
	dying.t = newTween(1)
	dying.beginRowFade(0)
	dying.t.step(20)

	live := rowFade{ref: "COM2"}
	live.t = newTween(1)

	rows.items = []rowFade{dying, live}
	pruneRows()
	if len(rows.items) != 2 {
		t.Fatalf("len = %d after pruning a fade in flight, want 2", len(rows.items))
	}

	// Once it has run to completion it is dropped.
	rows.items[0].t.step(int64(fadeOutDur/time.Millisecond) + 1)
	pruneRows()
	if len(rows.items) != 1 {
		t.Fatalf("len = %d after pruning a finished fade, want 1", len(rows.items))
	}
	if rows.items[0].ref != "COM2" {
		t.Fatalf("kept %q, want the live row COM2", rows.items[0].ref)
	}
}

func TestRowCountExcludesDyingRows(t *testing.T) {
	resetRows()
	a := rowFade{ref: "COM1"}
	a.t = newTween(1)
	b := rowFade{ref: "COM2", dying: true}
	b.t = newTween(1)
	rows.items = []rowFade{a, b}
	if n := rowCount(); n != 1 {
		t.Fatalf("rowCount = %d, want 1", n)
	}
}

// --- layout ----------------------------------------------------------------

func TestLayoutRailHidesWhenNarrow(t *testing.T) {
	wide := buildLayout(1240, 620)
	if wide.rail.Right-wide.rail.Left != railWidth {
		t.Fatalf("rail width = %d at 1240, want %d", wide.rail.Right-wide.rail.Left, railWidth)
	}
	if len(wide.items) == 0 {
		t.Fatal("no rail items at 1240")
	}

	narrow := buildLayout(600, 620)
	if w := narrow.rail.Right - narrow.rail.Left; w != 0 {
		t.Fatalf("rail width = %d at 600, want 0", w)
	}
	if len(narrow.items) != 0 {
		t.Fatalf("got %d rail items at 600, want none", len(narrow.items))
	}
}

func TestLayoutRegionsDoNotOverlap(t *testing.T) {
	for _, size := range []struct{ w, h int32 }{
		{1240, 620}, {720, 460}, {1920, 1080}, {620, 500}, {4000, 2200},
	} {
		l := buildLayout(size.w, size.h)

		if l.head.Bottom > l.table.Top {
			t.Errorf("%dx%d: table head (%d) overlaps the table top (%d)",
				size.w, size.h, l.head.Bottom, l.table.Top)
		}
		if l.table.Bottom > l.buttons[0].Top {
			t.Errorf("%dx%d: table bottom (%d) overlaps the button row (%d)",
				size.w, size.h, l.table.Bottom, l.buttons[0].Top)
		}
		if l.footer.Top < l.buttons[0].Bottom {
			t.Errorf("%dx%d: footer (%d) overlaps the buttons (%d)",
				size.w, size.h, l.footer.Top, l.buttons[0].Bottom)
		}
		if l.counters.Bottom > l.head.Top {
			t.Errorf("%dx%d: counters (%d) overlap the table head (%d)",
				size.w, size.h, l.counters.Bottom, l.head.Top)
		}
	}
}

func TestLayoutStaysInsideTheClientArea(t *testing.T) {
	for _, size := range []struct{ w, h int32 }{
		{1240, 620}, {720, 460}, {1920, 1080},
	} {
		l := buildLayout(size.w, size.h)
		for name, r := range map[string]rect{
			"title": l.title, "subtitle": l.subtitle, "counters": l.counters,
			"head": l.head, "table": l.table, "footer": l.footer,
		} {
			if r.Left < 0 || r.Top < 0 || r.Right > size.w || r.Bottom > size.h {
				t.Errorf("%dx%d: %s = %v escapes the client area", size.w, size.h, name, r)
			}
		}
		for i, r := range l.buttons {
			if r.Left < 0 || r.Right > size.w {
				t.Errorf("%dx%d: button %d = %v escapes the client area", size.w, size.h, i, r)
			}
		}
	}
}

func TestButtonsDoNotOverlapAndStayOrdered(t *testing.T) {
	for _, size := range []struct{ w, h int32 }{
		{1240, 620}, {900, 620}, {1920, 1080},
	} {
		l := buildLayout(size.w, size.h)
		if len(l.buttons) != 4 {
			t.Fatalf("%dx%d: got %d buttons, want 4", size.w, size.h, len(l.buttons))
		}
		// The first two sit on the left, the last two on the right, and the
		// left pair must not run into the right pair.
		if l.buttons[0].Right > l.buttons[2].Left {
			t.Errorf("%dx%d: the left and right button groups overlap: %v vs %v",
				size.w, size.h, l.buttons[0], l.buttons[2])
		}
		for i := 0; i+1 < len(l.buttons); i += 2 {
			if l.buttons[i].Right > l.buttons[i+1].Left {
				t.Errorf("%dx%d: buttons %d and %d overlap", size.w, size.h, i, i+1)
			}
		}
	}
}

func TestColumnsAreOrderedAndFitTheTable(t *testing.T) {
	for _, w := range []int32{600, 900, 1196, 2000} {
		cols := buildColumns(w)
		if len(cols) == 0 {
			t.Fatalf("inner width %d produced no columns", w)
		}
		last := cols[len(cols)-1]
		if !last.right {
			t.Errorf("inner width %d: the last column is not the flexible one", w)
		}
		if end := last.x + last.w; end > w {
			t.Errorf("inner width %d: columns end at %d, past the edge", w, end)
		}
		for i := 1; i < len(cols); i++ {
			if cols[i].x != cols[i-1].x+cols[i-1].w {
				t.Errorf("inner width %d: column %q starts at %d, wanted %d (a gap or overlap follows %q)",
					w, cols[i].title, cols[i].x, cols[i-1].x+cols[i-1].w, cols[i-1].title)
			}
		}
	}
}

func TestRailItemsAreOrderedAndInsideTheRail(t *testing.T) {
	l := buildLayout(1240, 620)
	prev := rect{Right: -1}
	for i, it := range l.items {
		if it.rect.Top < prev.Bottom && i > 0 {
			t.Errorf("item %q overlaps the previous item", it.label)
		}
		if it.rect.Left < l.rail.Left || it.rect.Right > l.rail.Right {
			t.Errorf("item %q = %v escapes the rail %v", it.label, it.rect, l.rail)
		}
		if it.rect.Bottom > l.railFooter.Top {
			t.Errorf("item %q = %v runs into the rail footer", it.label, it.rect)
		}
		prev = it.rect
	}
}

// --- hit testing -----------------------------------------------------------

func TestRailAtFindsEveryItem(t *testing.T) {
	l := buildLayout(1240, 620)
	for _, it := range l.items {
		centre := point{
			X: (it.rect.Left + it.rect.Right) / 2,
			Y: (it.rect.Top + it.rect.Bottom) / 2,
		}
		got, ok := l.railAt(centre)
		if !ok {
			t.Errorf("railAt(%v) found nothing, want %q", centre, it.label)
			continue
		}
		if got.id != it.id {
			t.Errorf("railAt(%v) = %q, want %q", centre, got.label, it.label)
		}
	}
}

func TestRailAtMissesOutsideTheRail(t *testing.T) {
	l := buildLayout(1240, 620)
	// Well inside the content area.
	if it, ok := l.railAt(point{X: 800, Y: 300}); ok {
		t.Errorf("railAt found %q in the content area", it.label)
	}
	// In the rail's own footer strip, which is not an item.
	if it, ok := l.railAt(point{X: 30, Y: l.railFooter.Top + 4}); ok {
		t.Errorf("railAt found %q in the rail footer", it.label)
	}
}

func TestButtonAtFindsEveryButton(t *testing.T) {
	l := buildLayout(1240, 620)
	for i, r := range l.buttons {
		centre := point{X: (r.Left + r.Right) / 2, Y: (r.Top + r.Bottom) / 2}
		if got := l.buttonAt(centre); got != i {
			t.Errorf("buttonAt(%v) = %d, want %d", centre, got, i)
		}
	}
	if got := l.buttonAt(point{X: 600, Y: 120}); got != -1 {
		t.Errorf("buttonAt in the title area = %d, want -1", got)
	}
}

func TestRowAtMapsTheTable(t *testing.T) {
	l := buildLayout(1240, 620)
	for i := 0; i < 3; i++ {
		// Sample the vertical centre of the nth row.
		y := l.table.Top + int32(i)*rowH + rowH/2
		p := point{X: l.table.Left + 40, Y: y}
		if got := l.rowAt(p, 3); got != i {
			t.Errorf("rowAt(%v) = %d, want %d", p, got, i)
		}
	}
	// Below the last row there is no row, even though the table's rect has
	// height left over.
	below := point{X: l.table.Left + 40, Y: l.table.Top + 5*rowH}
	if got := l.rowAt(below, 3); got != -1 {
		t.Errorf("rowAt(%v) = %d below the last row, want -1", below, got)
	}
	// Outside the table entirely.
	if got := l.rowAt(point{X: l.table.Left - 10, Y: l.table.Top + 4}, 3); got != -1 {
		t.Errorf("rowAt outside the table = %d, want -1", got)
	}
}

func TestHitTestingIsStableAcrossResizes(t *testing.T) {
	// A window resized down and back up must hit-test the same way. This is
	// what would break if the layout were cached rather than rebuilt.
	first := buildLayout(1240, 620)
	buildLayout(700, 500)
	again := buildLayout(1240, 620)

	for i := range first.items {
		a, b := first.items[i].rect, again.items[i].rect
		if a != b {
			t.Errorf("item %q moved between identical layouts: %v then %v",
				first.items[i].label, a, b)
		}
	}
	if first.table != again.table {
		t.Errorf("table moved between identical layouts: %v then %v", first.table, again.table)
	}
}

// --- animation clock -------------------------------------------------------

func TestAnimatorReportsWhenNothingMoves(t *testing.T) {
	var a animator
	settled := newTween(1)
	a.elems = []animatable{&settled}
	if a.tick(1000) {
		t.Fatal("tick = true with only a settled element, want false")
	}
}

func TestAnimatorReportsWhileMovingAndThenStops(t *testing.T) {
	var a animator
	tw := newTween(0)
	tw.setTarget(1, 100*time.Millisecond, 0)

	a.elems = []animatable{&tw}
	if !a.tick(20) {
		t.Fatal("tick = false mid-flight, want true")
	}
	if a.tick(500) {
		t.Fatal("tick = true after the tween settled, want false: the timer would never stop")
	}
}

// --- colour ----------------------------------------------------------------

func TestLerpEndpointsAndMidpoint(t *testing.T) {
	black, white := uint32(0x00000000), uint32(0x00FFFFFF)
	if got := lerp(black, white, 0); got != black {
		t.Errorf("lerp(t=0) = %#x, want %#x", got, black)
	}
	if got := lerp(black, white, 1); got != white {
		t.Errorf("lerp(t=1) = %#x, want %#x", got, white)
	}
	if got := lerp(black, white, 0.5); got != 0x007F7F7F {
		t.Errorf("lerp(t=0.5) = %#x, want 0x007f7f7f", got)
	}
	// Out-of-range values clamp rather than extrapolating into a nonsense
	// colour.
	if got := lerp(black, white, -1); got != black {
		t.Errorf("lerp(t=-1) = %#x, want %#x", got, black)
	}
	if got := lerp(black, white, 2); got != white {
		t.Errorf("lerp(t=2) = %#x, want %#x", got, white)
	}
}

func TestLerpMovesEachChannelIndependently(t *testing.T) {
	// COLORREF is 0x00BBGGRR, so this checks the byte order as well as the
	// blend: a red-to-blue ramp must move red and blue and leave green alone.
	red, blue := uint32(0x000000FF), uint32(0x00FF0000)
	got := lerp(red, blue, 0.5)
	if got&0xFF == 0 || (got>>16)&0xFF == 0 {
		t.Errorf("lerp(red, blue, 0.5) = %#x; both red and blue should be present", got)
	}
	if (got>>8)&0xFF != 0 {
		t.Errorf("lerp(red, blue, 0.5) = %#x; green should be untouched", got)
	}
}

func TestEaseOutCubicIsBoundedAndDecelerating(t *testing.T) {
	if got := easeOutCubic(0); got != 0 {
		t.Errorf("easeOutCubic(0) = %v, want 0", got)
	}
	if got := easeOutCubic(1); got != 1 {
		t.Errorf("easeOutCubic(1) = %v, want 1", got)
	}

	// The curve must be ahead of linear throughout and never overshoot.
	for i := 1; i < 10; i++ {
		x := float64(i) / 10
		y := easeOutCubic(x)
		if y <= x {
			t.Errorf("easeOutCubic(%v) = %v; ease-out should be ahead of linear", x, y)
		}
		if y > 1 {
			t.Errorf("easeOutCubic(%v) = %v; overshoots 1", x, y)
		}
	}

	// Later equal steps must produce smaller changes than earlier ones.
	prevDelta := 1.0
	for i := 0; i < 10; i++ {
		a := easeOutCubic(float64(i) / 10)
		b := easeOutCubic(float64(i+1) / 10)
		if d := b - a; d > prevDelta+1e-9 {
			t.Errorf("delta grew from %v to %v at step %d; the curve is not decelerating",
				prevDelta, d, i)
		} else {
			prevDelta = d
		}
	}
}

// --- window class registration ---------------------------------------------

// TestRegisterClassIsIdempotent covers a regression that made the SSH key panel
// impossible to open.
//
// RegisterClassExW fails with ERROR_CLASS_ALREADY_EXISTS on a second
// registration of the same name, and registerClass reported that as an error.
// The main window registered the key panel's class at startup, so the panel's
// own registration then failed and it returned before creating its window —
// with no visible symptom beyond the panel never appearing, because the failure
// path is a message box on a window that was never created.
//
// The check is that the error is recognised as benign. It cannot call
// registerClass twice from here and observe the difference, because the second
// call in-process would be the first real registration; what it pins down is
// that 1410 maps to the constant the code compares against.
func TestRegisterClassTreatsAlreadyExistsAsSuccess(t *testing.T) {
	// 1410 is ERROR_CLASS_ALREADY_EXISTS. Asserting the value keeps the
	// constant honest: if it were wrong, registerClass would silently go back
	// to failing and nothing else in the package would notice.
	if errorClassAlreadyExists != syscall.Errno(1410) {
		t.Fatalf("errorClassAlreadyExists = %d, want 1410", errorClassAlreadyExists)
	}
	// ERROR_ACCESS_DENIED is a different failure and must still be reported.
	if errorClassAlreadyExists == syscall.Errno(5) {
		t.Fatal("the already-exists code must not collide with access denied")
	}
}

// TestRegisteredClassNamesAreDistinct guards the other half: the main window and
// the key panel must not share a class name, because they have different window
// procedures. Sharing one would make the second window subclass the first.
func TestRegisteredClassNamesAreDistinct(t *testing.T) {
	if classNameID == keyClassName {
		t.Fatalf("both windows use class %q", classNameID)
	}
	if classNameID == "" || keyClassName == "" {
		t.Fatal("window class names must not be empty")
	}
}
