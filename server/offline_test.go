package main

import (
	"testing"
	"time"
)

// How long a blade may be silent is a per-site number. The health verdict has
// always read it from the site; the sweep that flips a blade to offline read
// the global one, so a site allowed a longer silence had its blades marked
// offline early and called unhealthy late — two answers to the same question.
func TestOfflineSweepUsesEachSitesOwnPatience(t *testing.T) {
	a := testApp(t)

	// Site 1 is the default site from testApp. A second one, told to wait an
	// hour before giving up on a blade.
	patient, err := a.createSite(Site{Name: "far", NetBase: "10.9.9", PoolFrom: 210, PoolTo: 240,
		OffsetBase: 100, OffsetStep: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.setSitePolicy(patient, Policy{OfflineMin: 60}); err != nil {
		t.Fatal(err)
	}

	near := mustRack(t, a, 1, "near")
	far := mustRack(t, a, patient, "far")

	// Both blades last spoke twenty minutes ago: past the default patience,
	// well inside the hour the far site is allowed.
	quiet := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
	for _, c := range []struct {
		serial string
		rack   int64
		slot   int
	}{{"near1", near, 1}, {"far1", far, 1}} {
		if _, err := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created,
			rack_id,slot,last_seen) VALUES(?,?,'online','t',?,?,?,?)`,
			c.serial, c.serial, now(), c.rack, c.slot, quiet); err != nil {
			t.Fatal(err)
		}
	}

	a.sweepOffline()

	state := func(serial string) string {
		b, err := a.getBlade(serial)
		if err != nil {
			t.Fatal(err)
		}
		return b.State
	}
	if got := state("near1"); got != "offline" {
		t.Errorf("the blade past its site's patience is %q, want offline", got)
	}
	if got := state("far1"); got != "online" {
		t.Errorf("the blade at the patient site is %q, want online", got)
	}
}

// A blade in no BladeRunner belongs to no site, and is judged by the global
// number rather than being skipped.
func TestABladeInNoRackIsStillSwept(t *testing.T) {
	a := testApp(t)
	quiet := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
	if _, err := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created,last_seen)
		VALUES('loose','loose','online','t',?,?)`, now(), quiet); err != nil {
		t.Fatal(err)
	}
	a.sweepOffline()
	b, err := a.getBlade("loose")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != "offline" {
		t.Errorf("a blade in no rack is %q, want offline", b.State)
	}
}
