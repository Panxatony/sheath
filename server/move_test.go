package main

import (
	"path/filepath"
	"testing"
)

// A blade is the module, not the slot it sits in.
//
// Carry one from one site to another and the serial is the same serial: the
// address changes, the name changes with the site it now stands in, and
// everything that was ever recorded about it stays recorded. The alternative
// — retiring the old entry and enrolling a new one — would make the history
// of a module end every time somebody picked it up.
func TestBladeKeepsItselfWhenItMovesBetweenSites(t *testing.T) {
	a := testApp(t)

	if _, err := a.db.Exec(`INSERT INTO sites(id,name,net_base,gateway,dns,domain,
		pool_from,pool_to,offset_base,offset_step,local,token,last_seen,created,host_prefix)
		VALUES(2,'Annexe','10.0.1','10.0.1.1','10.0.1.1','',210,240,100,20,0,'','','2026-01-01T00:00:00Z','an')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE sites SET host_prefix='mh', net_base='10.0.0' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	here := mustRack(t, a, 1, "hall")
	there := mustRack(t, a, 2, "annexe")

	const serial = "10000000deadbeef"
	if _, err := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created)
		VALUES(?,?,'new','tok','2026-01-01T00:00:00Z')`, serial, "deadbeef"); err != nil {
		t.Fatal(err)
	}
	slot := 3
	if err := a.placeBlade(serial, &here, &slot); err != nil {
		t.Fatalf("placing it: %v", err)
	}
	a.logEvent(serial, "info", "installed here")

	before, err := a.getBlade(serial)
	if err != nil {
		t.Fatal(err)
	}
	if before.Hostname != "blade-mh-r1s03" {
		t.Errorf("named %q at the first site", before.Hostname)
	}

	// Somebody unplugs it and screws it in at the other site.
	if err := a.placeBlade(serial, &there, &slot); err != nil {
		t.Fatalf("moving it: %v", err)
	}
	after, err := a.getBlade(serial)
	if err != nil {
		t.Fatalf("the blade is gone after the move: %v", err)
	}

	if after.Serial != before.Serial {
		t.Fatalf("a different blade came out: %s", after.Serial)
	}
	if after.Hostname != "blade-an-r1s03" {
		t.Errorf("name did not follow the site: %q", after.Hostname)
	}
	if after.IP == before.IP || after.IP == "" {
		t.Errorf("address did not follow the site: %q → %q", before.IP, after.IP)
	}
	if after.SiteID != 2 {
		t.Errorf("still counted at the old site: %d", after.SiteID)
	}
	if after.Created != before.Created {
		t.Errorf("the record was replaced rather than moved")
	}

	rows, err := a.rackEvents(there, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range rows {
		if e.Serial == serial && e.Msg == "installed here" {
			found = true
		}
	}
	if !found {
		t.Error("what happened at the first site is no longer readable at the second")
	}
}

func testApp(t *testing.T) *App {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := &App{db: db, sess: newSessions()}
	if err := a.ensureDefaultSite("10.0.0"); err != nil {
		t.Fatal(err)
	}
	return a
}

func mustRack(t *testing.T, a *App, siteID int64, name string) int64 {
	t.Helper()
	off, err := a.nextRackOffset(siteID)
	if err != nil {
		t.Fatalf("no address block free at site %d: %v", siteID, err)
	}
	res, err := a.db.Exec(
		`INSERT INTO racks(site_id,name,size,ip_offset,location,created) VALUES(?,?,?,?,'',?)`,
		siteID, name, 10, off, now())
	if err != nil {
		t.Fatalf("creating a BladeRunner at site %d: %v", siteID, err)
	}
	id, _ := res.LastInsertId()
	return id
}
