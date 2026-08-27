package main

import "testing"

// A blade is offered the devices it has, and refused the ones it has not.
func TestInstallTargetsFollowWhatTheBladeReported(t *testing.T) {
	a := testApp(t)
	const lite = "10000000000000aa" // an NVMe and nothing else
	const both = "10000000000000bb" // an NVMe and eMMC
	const card = "10000000000000cc" // a Lite with a card in the slot

	facts := map[string]string{
		lite: `{"nvme_bytes":500107862016,"nvme_model":"CT500P310SSD8","emmc_bytes":0}`,
		both: `{"nvme_bytes":500107862016,"emmc_bytes":7818182656,"mmc_kind":"emmc","emmc_model":"8GTF4R"}`,
		card: `{"nvme_bytes":500107862016,"sd_bytes":31914983424,"mmc_kind":"sd","emmc_bytes":0}`,
	}
	for serial, f := range facts {
		if _, err := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created,facts_json)
			VALUES(?,?,'online','t','2026-01-01T00:00:00Z',?)`, serial, serial[8:], f); err != nil {
			t.Fatal(err)
		}
	}

	b, _ := a.getBlade(lite)
	if d := a.installDevices(b); len(d) != 1 || d[0].Kind != "nvme" {
		t.Errorf("a blade with only an NVMe was offered %+v", d)
	}
	b, _ = a.getBlade(both)
	d := a.installDevices(b)
	if len(d) != 2 || d[1].Kind != "emmc" || d[1].Bytes != 7818182656 {
		t.Errorf("the eMMC was not offered: %+v", d)
	}
	b, _ = a.getBlade(card)
	d = a.installDevices(b)
	if len(d) != 2 || d[1].Kind != "sd" {
		t.Errorf("a card was not recognised as a card: %+v", d)
	}
}

// The eMMC in this rack holds 7.3 GB and Ubuntu asks for 8. That is a thing
// worth saying before a blade reboots into an installer.
func TestATargetTooSmallIsRefused(t *testing.T) {
	a := testApp(t)
	const serial = "10000000000000dd"
	if _, err := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created,facts_json,image)
		VALUES(?,'dd','online','t','2026-01-01T00:00:00Z',
		'{"nvme_bytes":500107862016,"emmc_bytes":7818182656,"mmc_kind":"emmc"}','ubuntu-24.04-arm64')`,
		serial); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO images(id,url,created,min_disk) VALUES('ubuntu-24.04-arm64','http://x',?,?)`,
		now(), int64(8)<<30); err != nil {
		t.Fatal(err)
	}
	if err := a.putConfig("blade:"+serial, map[string]any{
		"install": map[string]any{"install_target": "/dev/mmcblk0"},
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := a.getBlade(serial)
	if err := a.checkTarget(b); err == nil {
		t.Error("an 8 GB image was accepted onto a 7.3 GB eMMC")
	}

	// The same blade, pointed at its NVMe, is fine.
	if err := a.putConfig("blade:"+serial, map[string]any{
		"install": map[string]any{"install_target": "/dev/nvme0n1"},
	}); err != nil {
		t.Fatal(err)
	}
	b, _ = a.getBlade(serial)
	if err := a.checkTarget(b); err != nil {
		t.Errorf("the NVMe was refused: %v", err)
	}

	// A device this blade does not have is refused whatever the image.
	if err := a.putConfig("blade:"+serial, map[string]any{
		"install": map[string]any{"install_target": "/dev/sda"},
	}); err != nil {
		t.Fatal(err)
	}
	b, _ = a.getBlade(serial)
	if err := a.checkTarget(b); err == nil {
		t.Error("a device the blade does not have was accepted")
	}
}

// A module with one possible answer should not have to be told the answer.
func TestTheTargetDefaultsToWhatTheBladeHas(t *testing.T) {
	a := testApp(t)
	add := func(serial, facts string) *Blade {
		if _, err := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created,facts_json)
			VALUES(?,?,'online','t','2026-01-01T00:00:00Z',?)`, serial, serial[8:], facts); err != nil {
			t.Fatal(err)
		}
		b, err := a.getBlade(serial)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Only an eMMC: that is the target, without anybody setting it.
	only := add("10000000000000e1", `{"emmc_bytes":31914983424,"mmc_kind":"emmc","emmc_model":"SP32G"}`)
	if got := a.installTarget(only); got != "/dev/mmcblk0" {
		t.Errorf("a blade with only an eMMC defaults to %q", got)
	}
	if err := a.checkTarget(only); err != nil {
		t.Errorf("and it is refused: %v", err)
	}

	// An NVMe beside an eMMC keeps the old answer: the disk, not the card.
	both := add("10000000000000e2", `{"nvme_bytes":500107862016,"emmc_bytes":7818182656,"mmc_kind":"emmc"}`)
	if got := a.installTarget(both); got != "/dev/nvme0n1" {
		t.Errorf("a blade with both defaults to %q", got)
	}

	// A blade that has said nothing keeps the constant — the installer finds
	// out what is there, and refusing here would strand it.
	quiet := add("10000000000000e3", `{}`)
	if got := a.installTarget(quiet); got != "/dev/nvme0n1" {
		t.Errorf("a silent blade defaults to %q", got)
	}

	// What somebody set always wins.
	if err := a.putConfig("blade:"+only.Serial, map[string]any{
		"install": map[string]any{"install_target": "/dev/mmcblk0"},
	}); err != nil {
		t.Fatal(err)
	}
	only, _ = a.getBlade(only.Serial)
	if got := a.installTarget(only); got != "/dev/mmcblk0" {
		t.Errorf("the setting was ignored: %q", got)
	}
}
