package mcp

import (
	"testing"
	"time"
)

// Each wait is between half and all of the doubled step, capped at Max.
func TestBackoffWaits(t *testing.T) {
	b := Backoff{Attempts: 8, Base: time.Second, Max: 30 * time.Second}
	steps := []time.Duration{1, 2, 4, 8, 16, 30, 30} // seconds, before jitter
	for i, step := range steps {
		step *= time.Second
		for range 1000 {
			if w := b.wait(i + 1); w < step/2 || w > step {
				t.Fatalf("wait(%d) = %s, want between %s and %s", i+1, w, step/2, step)
			}
		}
	}
	// A shift far past 64 bits must not wrap round to a tiny or negative wait.
	if w := b.wait(200); w < 15*time.Second {
		t.Errorf("wait(200) = %s, want the 30s cap", w)
	}
}

// The default retries must outlast Quantic's one-minute window, however the
// jitter falls: a client refused at the start of a window must wait all of
// it. Each wait is at least half its step, so the halves must add up to a
// minute.
func TestDefaultBackoffOutlastsAMinute(t *testing.T) {
	var shortest time.Duration
	for n := 1; n < DefaultBackoff.Attempts; n++ {
		shortest += min(DefaultBackoff.Base<<(n-1), DefaultBackoff.Max) / 2
	}
	if shortest < time.Minute {
		t.Errorf("the retries can give up after %s of waiting; a refused client may need a minute", shortest)
	}
}
