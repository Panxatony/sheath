package main

import (
	"testing"
	"time"
)

// A site buffers what it sees while the link is down and sends it when the
// link comes back. The time it carries is the time the thing happened, and
// that is the whole reason the buffering exists.
func TestAnEventKeepsTheTimeItHappened(t *testing.T) {
	a := testApp(t)
	happened := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	a.logEventAt("s1", "warn", "site: the link went away", happened)

	var ts, received string
	if err := a.db.QueryRow(`SELECT ts,received FROM events WHERE serial='s1'`).
		Scan(&ts, &received); err != nil {
		t.Fatal(err)
	}
	if ts != happened {
		t.Errorf("stored %q, want the time the site gave: %q", ts, happened)
	}
	if received == "" {
		t.Error("a line that arrived late does not say when it arrived")
	}
}

// And a line that arrives as it happens carries no delivery time at all —
// "arrived when it happened" is noise on every ordinary line.
func TestAPromptEventCarriesNoDeliveryTime(t *testing.T) {
	a := testApp(t)
	a.logEventAt("s2", "info", "site: something", now())
	var received string
	if err := a.db.QueryRow(`SELECT received FROM events WHERE serial='s2'`).Scan(&received); err != nil {
		t.Fatal(err)
	}
	if received != "" {
		t.Errorf("a prompt line was marked as delayed: %q", received)
	}
}

// A clock that is out by a week is a thing sites have. An event dated next
// month sorts above everything for ever, so what cannot be believed is not
// believed.
func TestAnImpossibleStampIsNotTaken(t *testing.T) {
	arrived := now()
	for _, c := range []struct {
		name, when string
		want       string
	}{
		{"empty", "", arrived},
		{"not a time", "yesterday", arrived},
		{"next week", time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339), arrived},
		{"a year ago", time.Now().UTC().Add(-365 * 24 * time.Hour).Format(time.RFC3339), arrived},
		{"a minute into the future, which is a clock, not a lie",
			time.Now().UTC().Add(time.Minute).Format(time.RFC3339), ""},
		{"two hours ago", time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339), ""},
	} {
		got := usableStamp(c.when, arrived)
		if c.want == arrived && got != arrived {
			t.Errorf("%s: %q was believed, want the arrival time", c.name, c.when)
		}
		if c.want == "" && got == arrived {
			t.Errorf("%s: %q was not believed and should have been", c.name, c.when)
		}
	}
}
