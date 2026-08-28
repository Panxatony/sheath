package main

import (
	"encoding/json"
	"testing"
)

// A reset is the middle ground between "in service" and "this hardware is
// gone": everything that says where the blade was and what it was for goes,
// everything that says what it is stays.
func TestResetKeepsWhatTheModuleIs(t *testing.T) {
	a := testApp(t)
	rackID := mustRack(t, a, 1, "hall")
	facts := `{"board":"Compute Module 4","ram_mb":8192,"boot_order":"0xf54162",
		"nvme_bytes":500107862016,"os_name":"Debian GNU/Linux 13","kernel":"6.12.0",
		"agent_version":"sheath-agent/v1","ssh_listening":true}`
	if _, err := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created,facts_json,
		health_json,image,install_state,config_applied)
		VALUES('s1','s1','online','tok',?,?,'{"soc_temp_c":44}','debian-13-arm64','done','v9')`,
		now(), facts); err != nil {
		t.Fatal(err)
	}
	slot := 3
	if err := a.placeBlade("s1", &rackID, &slot); err != nil {
		t.Fatal(err)
	}
	if err := a.resetBlade("s1"); err != nil {
		t.Fatalf("resetBlade: %v", err)
	}

	b, err := a.getBlade("s1")
	if err != nil {
		t.Fatal(err)
	}
	if b.RackID != nil || b.Slot != nil {
		t.Error("it is still in a slot")
	}
	for name, got := range map[string]string{
		"image": b.Image, "hostname": b.Hostname, "config_applied": b.ConfigApp,
	} {
		if got != "" {
			t.Errorf("%s survived the reset: %q", name, got)
		}
	}
	if b.Stored == "" {
		t.Error("it is not marked as being in storage")
	}

	var f map[string]any
	if err := json.Unmarshal(b.Facts, &f); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"board", "ram_mb", "boot_order", "nvme_bytes"} {
		if _, ok := f[k]; !ok {
			t.Errorf("%s is gone, and a module does not change by being unplugged", k)
		}
	}
	for _, k := range []string{"os_name", "kernel", "agent_version", "ssh_listening"} {
		if _, ok := f[k]; ok {
			t.Errorf("%s survived, and it describes what was installed, not the module", k)
		}
	}

	// The two things Forget takes away and this must not.
	var tok string
	if err := a.db.QueryRow(`SELECT token FROM blades WHERE serial='s1'`).Scan(&tok); err != nil {
		t.Fatal(err)
	}
	if tok != "tok" {
		t.Errorf("the token changed to %q — an installed system could never talk again", tok)
	}
	if lvl, _ := evalHealthWith(b, defaultPolicy()); lvl != hUnknown {
		t.Errorf("a blade in storage should raise nothing, got %v", lvl)
	}
}

// And it is in service again the moment it is put back in a slot — not the
// moment it reports, because a blade reset while still running keeps
// reporting until somebody pulls it.
func TestASlotEndsStorage(t *testing.T) {
	a := testApp(t)
	rackID := mustRack(t, a, 1, "hall")
	if _, err := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created,stored)
		VALUES('s1','s1','new','tok',?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	slot := 1
	if err := a.placeBlade("s1", &rackID, &slot); err != nil {
		t.Fatal(err)
	}
	b, err := a.getBlade("s1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Stored != "" {
		t.Error("it is in a slot and still counted as being in storage")
	}
}
