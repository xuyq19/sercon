//go:build windows

// A scripted tour of the window's animations.
//
// It exists because the animations cannot always be exercised from outside the
// process. A locked desktop or a session without pointer access refuses
// GetCursorPos, mouse_event produces no WM_MOUSEMOVE, and the only way left to
// see a hover highlight is to ask the program to produce one itself.
//
// Enabled with SERCON_ANIM_DEMO=1. It moves the cursor-equivalent state through
// each animated element on a timer, holds each stage long enough to be
// photographed, and leaves the window in its normal resting state. Nothing here
// runs unless the variable is set.
package main

import (
	"os"
	"time"
)

const (
	demoStepEvery = 2200 // milliseconds between stages
	demoTimerID   = 3
	demoRollDur   = 320 * time.Millisecond
)

var demoOn = os.Getenv("SERCON_ANIM_DEMO") != ""

// demoStage is one step of the tour.
type demoStage struct {
	name string
	// at is a point in client coordinates, or a point outside the window to
	// mean "nowhere".
	at point
	// run is an extra action for stages that change something other than the
	// pointer, such as the counters.
	run func()
	// hold, when set, is applied on every animation frame for as long as the
	// stage lasts. The paint path drives the counters from the live tally, so
	// a value written once is overwritten by the next refresh tick — a stage
	// that wants a sustained value has to keep re-asserting it.
	hold func(now int64)
}

func demoStages() []demoStage {
	l := gui.lyt
	var railPorts, railKeys point
	if len(l.items) >= 3 {
		railPorts = centre(l.items[0].rect)
		railKeys = centre(l.items[2].rect)
	}
	var row1 point
	if len(rows.items) > 0 {
		row1 = point{X: l.table.Left + 60, Y: l.table.Top + rowH/2}
	}
	var btn3 point
	if len(l.buttons) >= 4 {
		btn3 = centre(l.buttons[3])
	}
	away := point{X: -50, Y: -50}

	return []demoStage{
		{"rest", away, nil, nil},
		{"rail-hover-ports", railPorts, nil, nil},
		{"rail-hover-keys", railKeys, nil, nil},
		{"row-hover", row1, nil, nil},
		{"button-hover", btn3, nil, nil},
		{"counters", away, nil, func(now int64) {
			// Drive the counters somewhere other than the live tally so the
			// numbers visibly roll, and keep driving them: the refresh tick
			// re-seeds them from the real port table every second.
			for i := range counters {
				counters[i].setTarget(float64(i+7), demoRollDur, now)
			}
		}},
		{"row-fade", away, func() {
			// Mark one row as leaving so its fade-out plays.
			for i := range rows.items {
				if !rows.items[i].dying {
					rows.items[i].dying = true
					rows.items[i].t.setTarget(0, fadeOutDur, nowMS())
					break
				}
			}
		}, func(now int64) {
			// Keep the row disappearing. The refresh tick sees the port is
			// still present and revives it, which would cut the fade short.
			for i := range rows.items {
				if !rows.items[i].dying {
					rows.items[i].dying = true
					rows.items[i].t.setTarget(0, fadeOutDur, now)
					return
				}
			}
		}},
		{"done", away, nil, nil},
	}
}

// centre returns the middle of a rect.
func centre(r rect) point {
	return point{X: (r.Left + r.Right) / 2, Y: (r.Top + r.Bottom) / 2}
}

// startDemo schedules the first stage of the tour.
func startDemo() {
	if !demoOn {
		return
	}
	dbg("animation demo enabled: %d stages, one per %dms",
		len(demoStages()), demoStepEvery)
	pSetTimer.Call(gui.hwnd, demoTimerID, demoStepEvery, 0)
	// Run the first stage immediately so the window does not sit still for a
	// full interval before anything happens.
	advanceDemo()
}

// demoIndex is the stage currently on screen.
var demoIndex = -1

// advanceDemo moves to the next stage and animates it.
func advanceDemo() {
	if gui == nil || gui.hwnd == 0 {
		return
	}
	stages := demoStages()
	demoIndex++
	if demoIndex >= len(stages) {
		if demoIndex > len(stages) {
			return
		}
		gui.pointerMoved(point{X: -50, Y: -50})
		dbg("animation demo complete")
		pKillTimer.Call(gui.hwnd, demoTimerID)
		return
	}

	st := stages[demoIndex]
	dbg("demo stage %d/%d: %s", demoIndex+1, len(stages), st.name)
	if st.run != nil {
		st.run()
	}
	gui.pointerMoved(st.at)
	// A stage that holds a value needs the frame clock running even when the
	// pointer has settled, so the clock is started explicitly.
	anim.attach(gui.hwnd, demoElems()...)
}

// demoElems is everything the clock should advance during a demo stage.
func demoElems() []animatable {
	out := railElems()
	out = append(out, rowElems()...)
	out = append(out, counterElems()...)
	return out
}

// demoHold is called from every animation frame while the tour is running.
func demoHold(now int64) {
	if !demoOn {
		return
	}
	stages := demoStages()
	if demoIndex < 0 || demoIndex >= len(stages) {
		return
	}
	if hold := stages[demoIndex].hold; hold != nil {
		hold(now)
	}
}
