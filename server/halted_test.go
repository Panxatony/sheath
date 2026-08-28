package main

import "testing"

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
