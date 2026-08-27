package main

import (
	"testing"
	"time"
)

// An arming that nobody notices must not outlive its reason: a blade that was
// off when it was armed would otherwise land in the installer weeks later,
// for a request nobody remembers making.
func TestProbeArmingExpires(t *testing.T) {
	for _, c := range []struct {
		name  string
		probe string
		want  bool
	}{
		{"never armed", "", false},
		{"just now", time.Now().UTC().Format(time.RFC3339), true},
		{"a few minutes ago", time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339), true},
		{"an hour ago", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), false},
		{"nonsense in the column", "yesterday", false},
	} {
		if got := probeArmed(&Blade{Probe: c.probe}); got != c.want {
			t.Errorf("%s: probeArmed = %v, want %v", c.name, got, c.want)
		}
	}
}
