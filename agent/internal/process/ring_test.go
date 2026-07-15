package process

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

func TestRingMonotonicCursor(t *testing.T) {
	r := newRing(16)
	r.write([]byte("hello"))

	data, next, lost := r.readFrom(0)
	if lost {
		t.Fatalf("unexpected loss")
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want hello", data)
	}
	if next != 5 {
		t.Fatalf("next = %d, want 5", next)
	}

	// Reading from the returned cursor yields nothing (caught up).
	data, next2, lost := r.readFrom(next)
	if len(data) != 0 || lost || next2 != 5 {
		t.Fatalf("caught-up read returned data=%q next=%d lost=%v", data, next2, lost)
	}

	// New bytes appear only after the cursor.
	r.write([]byte("world"))
	data, next3, lost := r.readFrom(next2)
	if string(data) != "world" || lost || next3 != 10 {
		t.Fatalf("incremental read: data=%q next=%d lost=%v", data, next3, lost)
	}
}

func TestRingOverwriteReportsLoss(t *testing.T) {
	r := newRing(8)
	r.write([]byte("0123456789ABCDEF")) // 16 bytes into an 8-byte ring

	// Cursor 0 has fallen far behind; only the last 8 bytes remain.
	data, next, lost := r.readFrom(0)
	if !lost {
		t.Fatalf("expected loss=true when cursor fell behind the ring")
	}
	if string(data) != "89ABCDEF" {
		t.Fatalf("data = %q, want 89ABCDEF", data)
	}
	if next != 16 {
		t.Fatalf("next = %d, want 16", next)
	}
}

func TestRingStaleFutureCursor(t *testing.T) {
	r := newRing(8)
	r.write([]byte("abc"))
	// A cursor ahead of the head (never legitimately produced, but defended
	// against) is treated as caught up rather than panicking.
	data, next, lost := r.readFrom(999)
	if len(data) != 0 || lost || next != 3 {
		t.Fatalf("future cursor: data=%q next=%d lost=%v", data, next, lost)
	}
}

func TestRingExactWrap(t *testing.T) {
	r := newRing(4)
	r.write([]byte("abcd")) // exactly fills
	data, _, lost := r.readFrom(0)
	if lost || string(data) != "abcd" {
		t.Fatalf("exact fill: data=%q lost=%v", data, lost)
	}
	r.write([]byte("ef")) // wraps; oldest two dropped
	data, next, lost := r.readFrom(0)
	if !lost || string(data) != "cdef" || next != 6 {
		t.Fatalf("wrap: data=%q next=%d lost=%v", data, next, lost)
	}
}

// TestPollReportsTruncationOnOverrun drives a real process whose output
// overruns a tiny ring, then asserts poll reports OUTPUT_TRUNCATED with a
// dropped-bytes gap.
func TestPollReportsTruncationOnOverrun(t *testing.T) {
	cfg := testConfig()
	cfg.RingSize = 64 // tiny ring to force overrun
	m := New(cfg)
	t.Cleanup(func() { _ = m.Close() })

	start, err := m.Process(context.Background(), api.ProcessInput{
		Action:  api.ProcessStart,
		Command: "for i in $(seq 1 500); do printf 'XXXXXXXXXX'; done",
	})
	if err != nil || start.Code != "" {
		t.Fatalf("start failed: %v %q", err, start.Code)
	}

	// Wait for the process to finish producing, then poll once — the ring is
	// far smaller than the ~5000 bytes emitted, so bytes were dropped.
	var sawTruncated bool
	var buf bytes.Buffer
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := m.Process(context.Background(), api.ProcessInput{
			Action: api.ProcessPoll, ProcessID: start.ProcessID, TimeoutMs: ptr(200),
		})
		if err != nil {
			t.Fatalf("poll error: %v", err)
		}
		buf.WriteString(p.Stdout)
		if p.Truncated && p.Code == api.CodeOutputTruncated {
			sawTruncated = true
		}
		if !p.Running {
			break
		}
	}
	if !sawTruncated {
		t.Fatalf("expected a poll to report OUTPUT_TRUNCATED for an overrun ring")
	}
}
