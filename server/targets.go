package main

import (
	"encoding/json"
	"fmt"
)

// Where an installation may be written.
//
// The device was a text field with `/dev/nvme0n1` behind it. That is the right
// answer for a blade with an NVMe and no answer at all for a Lite with a card
// in it, or for anyone who wants to try an image on 8 GB of eMMC before
// spending a 500 GB disk on it.
//
// The blade already says what it has — the mini OS reads it before an image is
// even chosen — so the choice can be offered as the devices that exist rather
// than as a path somebody has to remember.

type installDevice struct {
	Path  string `json:"path"`
	Kind  string `json:"kind"` // nvme | emmc | sd
	Label string `json:"label"`
	Bytes int64  `json:"bytes"`
}

// installDevices lists what this blade reported. Empty means it has never
// said — a blade that has not booted since this existed, which is a reason to
// leave the choice alone rather than to refuse everything.
func (a *App) installDevices(b *Blade) []installDevice {
	var f map[string]any
	if len(b.Facts) > 0 {
		_ = json.Unmarshal(b.Facts, &f)
	}
	var out []installDevice
	if n, ok := num(f["nvme_bytes"]); ok && n > 0 {
		d := installDevice{Path: "/dev/nvme0n1", Kind: "nvme", Bytes: int64(n), Label: "NVMe"}
		if m := str(f, "nvme_model"); m != "" {
			d.Label += " · " + m
		}
		out = append(out, d)
	}
	kind, _ := f["mmc_kind"].(string)
	if n, ok := num(f["emmc_bytes"]); ok && n > 0 {
		d := installDevice{Path: "/dev/mmcblk0", Kind: "emmc", Bytes: int64(n), Label: "eMMC"}
		if m := str(f, "emmc_model"); m != "" {
			d.Label += " · " + m
		}
		out = append(out, d)
	} else if n, ok := num(f["sd_bytes"]); ok && n > 0 {
		d := installDevice{Path: "/dev/mmcblk0", Kind: "sd", Bytes: int64(n), Label: "SD card"}
		if m := str(f, "mmc_model"); m != "" {
			d.Label += " · " + m
		}
		out = append(out, d)
	} else if kind != "" {
		// It said there is a card device and no size, which is odd enough to
		// pass on rather than to hide.
		out = append(out, installDevice{Path: "/dev/mmcblk0", Kind: kind, Label: kind})
	}
	return out
}

// checkTarget answers whether this blade can be installed to the device it is
// pointed at, with the image it is assigned. Both halves are worth asking
// before a blade reboots into an installer that will find out the hard way.
func (a *App) checkTarget(b *Blade) error {
	target := a.installTarget(b)
	devs := a.installDevices(b)
	if len(devs) == 0 {
		// Never reported. The installer will use what it finds; refusing here
		// would make a blade uninstallable for want of a fact.
		return nil
	}
	var chosen *installDevice
	for i := range devs {
		if devs[i].Path == target {
			chosen = &devs[i]
			break
		}
	}
	if chosen == nil {
		return me("err.nodevice", bladeName(b), target, deviceList(devs))
	}
	if b.Image == "" {
		return nil
	}
	var min int64
	if err := a.db.QueryRow(`SELECT min_disk FROM images WHERE id=?`, b.Image).Scan(&min); err != nil {
		return nil
	}
	if min > 0 && chosen.Bytes > 0 && chosen.Bytes < min {
		return me("err.toosmall", bladeName(b), chosen.Label, human(chosen.Bytes),
			b.Image, human(min))
	}
	// The card interface is the one place where the table on the image
	// decides whether anything boots afterwards. Refusing here costs a
	// sentence; finding out costs an hour of writing and a blade that goes
	// silent without saying anything at all.
	if chosen.Kind == "emmc" || chosen.Kind == "sd" {
		if a.imageTable(b.Image) == "gpt" {
			return me("err.gptoncard", bladeName(b), b.Image, chosen.Label)
		}
	}
	return nil
}

func deviceList(devs []installDevice) string {
	out := ""
	for i, d := range devs {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s (%s)", d.Path, d.Label)
	}
	return out
}

// bladeName is what to call a blade in a sentence: the name if it has one,
// and the serial if nobody has named it yet. A message about "this blade" is
// no use on a page listing ten of them.
func bladeName(b *Blade) string {
	if b.Hostname != "" {
		return b.Hostname
	}
	return b.Serial
}
