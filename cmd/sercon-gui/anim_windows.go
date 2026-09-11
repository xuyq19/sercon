//go:build windows

// The animation clock.
//
// One timer drives every animation in the window. The alternative — a timer
// per animated element — costs a WM_TIMER round trip each and makes the
// message queue the bottleneck long before the drawing is.
//
// The timer only runs while something is actually moving. An idle window
// schedules no frames at all, which is what keeps a daemon that may sit on
// screen for days from showing up in Task Manager.
package main

import (
	"sync"
	"time"
)

const (
	// animFPS targets a frame roughly every 16 milliseconds. A UI timer is
	// not a frame clock: Windows coalesces WM_TIMER to the system tick, so
	// this is a floor on latency and not a guarantee.
	animFrameMS = 16

	// animTimerID is the shared animation timer, distinct from the one-second
	// refresh timer the window already runs.
	animTimerID = 2
)

// animatable is anything that can report whether it is still moving and can
// advance to a point in time.
type animatable interface {
	// step advances the animation to now (milliseconds) and reports whether
	// the element wants another frame. Returning false for everything stops
	// the timer.
	step(now int64) bool
}

// tween is a value that eases towards a target over a fixed duration.
//
// Transitions are driven from the wall clock rather than by accumulating a
// per-frame delta. Accumulating drifts: if a frame is late, or the window is
// dragged and stops receiving timers for a moment, the error compounds and the
// animation either finishes early or never quite settles.
type tween struct {
	from     float64
	to       float64
	value    float64
	startMS  int64
	duration int64
	active   bool
}

// newTween returns a settled tween at v.
func newTween(v float64) tween {
	return tween{from: v, to: v, value: v}
}

// setTarget starts a transition towards v over d. A zero duration snaps.
func (t *tween) setTarget(v float64, d time.Duration, now int64) {
	if t.to == v && t.active {
		return
	}
	if d <= 0 || t.value == v {
		t.from, t.to, t.value = v, v, v
		t.active = false
		return
	}
	t.from = t.value
	t.to = v
	t.startMS = now
	t.duration = int64(d / time.Millisecond)
	if t.duration <= 0 {
		t.duration = 1
	}
	t.active = true
}

// step advances the tween and reports whether it is still moving.
func (t *tween) step(now int64) bool {
	if !t.active {
		return false
	}
	elapsed := now - t.startMS
	if elapsed >= t.duration {
		t.value = t.to
		t.active = false
		return false
	}
	p := float64(elapsed) / float64(t.duration)
	t.value = t.from + (t.to-t.from)*easeOutCubic(clamp01(p))
	return true
}

// --- the shared clock ------------------------------------------------------

// animator owns the frame timer and the set of animations attached to it.
type animator struct {
	mu      sync.Mutex
	hwnd    uintptr
	running bool

	// elems is the set of elements that want frames. It is rebuilt by the
	// caller each frame from the elements that are currently animating, so an
	// element that is destroyed stops being asked without any explicit
	// deregistration step — the usual source of a dangling animation.
	elems []animatable
}

var anim animator

// attach registers the animations to be driven and starts the timer.
//
// It is called whenever something changes state, not every frame. A duplicate
// start is harmless: the timer is only installed if one is not already
// running.
func (a *animator) attach(hwnd uintptr, elems ...animatable) {
	a.mu.Lock()
	a.hwnd = hwnd
	a.elems = elems
	wasRunning := a.running
	a.running = true
	a.mu.Unlock()

	if !wasRunning && hwnd != 0 {
		pSetTimer.Call(hwnd, animTimerID, animFrameMS, 0)
	}
}

// stop ends the frame loop. Called from the frame handler once nothing is
// moving, and from window teardown.
func (a *animator) stop() {
	a.mu.Lock()
	hwnd := a.hwnd
	a.running = false
	a.mu.Unlock()

	if hwnd != 0 {
		pKillTimer.Call(hwnd, animTimerID)
	}
}

// tick advances every registered animation and reports whether any is still
// moving. The caller repaints once if so, which is the whole point: one
// repaint per frame regardless of how many elements changed.
func (a *animator) tick(now int64) bool {
	a.mu.Lock()
	elems := a.elems
	a.mu.Unlock()

	moving := false
	for _, e := range elems {
		if e.step(now) {
			moving = true
		}
	}
	return moving
}
