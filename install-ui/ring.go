package main

import "strings"

// ring is a fixed-capacity FIFO of log lines backing the log viewport. Installs
// can emit tens of thousands of lines; the cap bounds memory on a live system
// whose /var is a tmpfs overlay.
type ring struct {
	buf []string
	max int
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) add(line string) {
	r.buf = append(r.buf, line)
	if len(r.buf) > r.max {
		copy(r.buf, r.buf[len(r.buf)-r.max:])
		r.buf = r.buf[:r.max]
	}
}

func (r *ring) lines() []string { return r.buf }

func (r *ring) text() string { return strings.Join(r.buf, "\n") }
