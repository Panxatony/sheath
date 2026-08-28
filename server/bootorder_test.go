package main

import "testing"

func TestBootOrderIsReadFromTheRight(t *testing.T) {
	for _, c := range []struct {
		order string
		want  string
	}{
		// What the sites are set to during bring-up.
		{"0xf162", "network → NVMe → SD/eMMC → start over"},
		// The stock order of a Raspberry Pi that has never been touched.
		{"0xf25416", "NVMe → SD/eMMC → USB → USB → network → start over"},
		// A 0 ends the sequence: nothing to the left of it is ever tried.
		{"0x21", "SD/eMMC → network"},
		{"0xe1", "SD/eMMC → stop"},
		// Behind a restart there is no next device either.
		{"0x6f21", "SD/eMMC → network → start over"},
		{"", ""},
		{"0x0", ""},
		{"nonsense", ""},
	} {
		if got := bootOrderText(LangEN, c.order); got != c.want {
			t.Errorf("bootOrderText(%q) = %q, want %q", c.order, got, c.want)
		}
	}
}

// The point of keeping the number: it says whether an image written to a
// device would ever be started.
func TestBootOrderReachesTheInstallTarget(t *testing.T) {
	for _, c := range []struct {
		order, kind string
		want        bool
	}{
		{"0xf162", "nvme", true},
		{"0xf162", "emmc", true},
		{"0xf162", "sd", true},
		{"0xf26", "emmc", false}, // network, NVMe, and nothing else
		{"0xf26", "nvme", true},
		{"0xf21", "nvme", false},
		{"0x6f21", "nvme", false}, // behind the restart, so never reached
		{"", "emmc", false},
	} {
		if got := bootOrderReaches(c.order, c.kind); got != c.want {
			t.Errorf("bootOrderReaches(%q, %q) = %v, want %v", c.order, c.kind, got, c.want)
		}
	}
}

// Two blades with a 64 GB and a 32 GB microSD in them read as "Lite (no
// eMMC)" for a day, and the card they were being installed on was nowhere in
// the inventory. eMMC and card are one interface, and that is exactly why
// they have to be named apart.
func TestTheInventoryTellsACardFromAnEMMC(t *testing.T) {
	a := testApp(t)
	for _, c := range []struct {
		serial, facts, want string
	}{
		{"s1", `{"mmc_kind":"emmc","emmc_bytes":7818182656,"emmc_model":"8GTF4R"}`,
			"eMMC 7.8 GB · 8GTF4R"},
		{"s2", `{"mmc_kind":"sd","sd_bytes":63864569856,"emmc_bytes":0,"mmc_model":"SN64G"}`,
			"SD 63.9 GB · SN64G"},
		{"s3", `{"emmc_bytes":0}`, "no eMMC, no card"},
		{"s4", `{"board":"Compute Module 4"}`, ""},
	} {
		if _, err := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created,facts_json)
			VALUES(?,?,'online','t',?,?)`, c.serial, c.serial, now(), c.facts); err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err := a.inventory(LangEN)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Serial] = r.EMMC
	}
	for _, c := range []struct{ serial, want string }{
		{"s1", "eMMC 7.8 GB · 8GTF4R"},
		{"s2", "SD 63.9 GB · SN64G"},
		{"s3", "no eMMC, no card"},
		{"s4", ""},
	} {
		if got[c.serial] != c.want {
			t.Errorf("%s: %q, want %q", c.serial, got[c.serial], c.want)
		}
	}
}
