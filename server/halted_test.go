package main

import (
	"testing"
	"time"
)

// A blade that was switched off is not a blade that failed. Both stop
// answering; only one is worth a notification at three in the morning.
func TestASwitchedOffBladeIsNotCritical(t *testing.T) {
	p := defaultPolicy()

	gone := &Blade{State: "offline"}
	if lvl, _ := evalHealthWith(gone, p); lvl != hCrit {
		t.Errorf("a blade that stopped answering on its own should be critical, got %v", lvl)
	}

	off := &Blade{State: "offline", Halted: "2026-08-28T05:46:00Z"}
	if lvl, reasons := evalHealthWith(off, p); lvl != hUnknown || len(reasons) != 0 {
		t.Errorf("a blade that was switched off should raise nothing, got %v %v", lvl, reasons)
	}
}

// And the mail that would follow it: an alert standing from before the
// decision must not turn into a "recovered" notice for a failure that never
// happened.
func TestSwitchingOffClearsAStandingAlert(t *testing.T) {
	a := testApp(t)
	if _, err := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created)
		VALUES('s1','s1','online','t',?)`, now()); err != nil {
		t.Fatal(err)
	}
	if err := a.raiseAlert(alert{Serial: "s1", Level: "crit", Reason: "no heartbeat",
		Since: time.Now().UTC(), Notified: "crit"}); err != nil {
		t.Fatal(err)
	}
	if err := a.requestShutdown("s1"); err != nil {
		t.Fatalf("requestShutdown: %v", err)
	}
	open, err := a.openAlerts()
	if err != nil {
		t.Fatal(err)
	}
	if _, still := open["s1"]; still {
		t.Error("the alert from before the shutdown is still standing")
	}
	b, err := a.getBlade("s1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Halted == "" {
		t.Error("the blade is not marked as switched off")
	}
}
