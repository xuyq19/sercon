package main

import "testing"

func TestKeyJobStateRejectsStaleCompletion(t *testing.T) {
	state := keyJobState{}
	generation := state.begin()
	if !state.accepts(generation) {
		t.Fatal("current completion was rejected")
	}
	if state.accepts(generation - 1) {
		t.Fatal("stale completion was accepted")
	}
}

func TestKeyJobStateGenerationAdvances(t *testing.T) {
	state := keyJobState{}
	if got := state.begin(); got != 1 {
		t.Fatalf("first generation = %d; want 1", got)
	}
	state.busy = false
	if got := state.begin(); got != 2 {
		t.Fatalf("second generation = %d; want 2", got)
	}
}
