package main

// keyJobState tracks the one outstanding key-panel worker. It is intentionally
// free of Win32 types so generation rules can be tested on every platform.
type keyJobState struct {
	generation uint64
	busy       bool
}

// begin reserves the next generation for a worker started by the UI thread.
func (s *keyJobState) begin() uint64 {
	s.busy = true
	s.generation++
	return s.generation
}

// accepts reports whether a worker result belongs to the current job.
func (s keyJobState) accepts(generation uint64) bool {
	return s.busy && generation == s.generation
}
