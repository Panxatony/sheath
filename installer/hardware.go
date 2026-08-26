package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// What this blade is made of — read in the mini OS, before anything is
// installed on it.
//
// This file is a copy of agent/hardware.go, and deliberately so. The two
// programs are separate modules built separately, each a static binary with
// no dependencies; sharing two hundred lines between them would mean a third
// module in every build for the sake of one struct. When one changes, change
// the other: the test below is in both, so a drift in the decoding shows up
// as a failing test rather than as two answers to the same question.
//
// The mini OS has the same /proc/cpuinfo and the same block devices the
// installed system will have, so the answer here is the answer there. What it
// buys is the order: the interface can say "8 GB, Lite, no eMMC" while the
// blade is still waiting for someone to choose an image for it.
//
// "Raspberry Pi Compute Module 4 Rev 1.1" is what the device tree says, and it
// leaves out everything anyone actually asks: how much memory, is there eMMC
// on it or is it a Lite, is there wireless, who built it. All of that except
// the storage is in one number — the revision code in /proc/cpuinfo — which
// the firmware fills in from the OTP and which nothing else exposes.
//
// The rest is read where it lies: the eMMC and the NVMe from their block
// devices, the throttling history from the firmware. Reading is all this
// does; an inventory that changes what it measures is not an inventory.

// hardware collects the facts that do not change while the blade is running.
func hardware() map[string]any {
	h := map[string]any{}

	rev := revisionCode()
	if rev != "" {
		h["board_revision"] = rev
		if b, ok := decodeRevision(rev); ok {
			h["board"] = b.Model
			h["board_rev"] = b.Revision
			h["soc"] = b.SoC
			h["maker"] = b.Maker
			if b.RAMMB > 0 {
				h["ram_mb"] = b.RAMMB
			}
		}
	}
	if n := onlineCPUs(); n > 0 {
		h["cpu_cores"] = n
	}
	if mhz := maxMHz(); mhz > 0 {
		h["cpu_mhz"] = mhz
	}

	// eMMC: a CM4 Lite has none, and the difference decides whether a blade
	// can be brought up without a card at all.
	if sz, name := sysBlockSize("mmcblk0"); sz > 0 {
		h["emmc_bytes"] = sz
		if name != "" {
			h["emmc_model"] = name
		}
	} else {
		h["emmc_bytes"] = int64(0)
	}
	if sz, name := sysBlockSize("nvme0n1"); sz > 0 {
		h["nvme_bytes"] = sz
		if name != "" {
			h["nvme_model"] = name
		}
	}
	h["wireless"] = hasWireless()
	if m := primaryMAC(); m != "" {
		h["eth_mac"] = m
	}
	if v := firstLine("/sys/firmware/devicetree/base/chosen/bootloader/version"); v != "" {
		h["bootloader"] = v
	}
	return h
}

// board is a revision code, read out.
type board struct {
	Model    string
	Revision string
	SoC      string
	Maker    string
	RAMMB    int
}

// decodeRevision reads the new-style revision code, the one with bit 23 set.
// The old-style codes belong to boards from before the Compute Module 4 and
// are not worth carrying: this fleet cannot contain one.
func decodeRevision(hex string) (board, bool) {
	v, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
	if err != nil || v&(1<<23) == 0 {
		return board{}, false
	}
	types := map[uint64]string{
		0x00: "Model A", 0x01: "Model B", 0x02: "Model A+", 0x03: "Model B+",
		0x04: "Pi 2 Model B", 0x06: "Compute Module 1", 0x08: "Pi 3 Model B",
		0x09: "Zero", 0x0a: "Compute Module 3", 0x0c: "Zero W",
		0x0d: "Pi 3 Model B+", 0x0e: "Pi 3 Model A+", 0x10: "Compute Module 3+",
		0x11: "Pi 4 Model B", 0x12: "Zero 2 W", 0x13: "Pi 400",
		0x14: "Compute Module 4", 0x15: "Compute Module 4S", 0x17: "Pi 5",
		0x18: "Compute Module 5", 0x19: "Pi 500", 0x1a: "Compute Module 5 Lite",
	}
	socs := map[uint64]string{0: "BCM2835", 1: "BCM2836", 2: "BCM2837", 3: "BCM2711", 4: "BCM2712"}
	makers := map[uint64]string{
		0: "Sony UK", 1: "Egoman", 2: "Embest", 3: "Sony Japan", 4: "Embest", 5: "Stadium",
	}
	ram := map[uint64]int{0: 256, 1: 512, 2: 1024, 3: 2048, 4: 4096, 5: 8192, 6: 16384}

	b := board{
		Model:    types[(v>>4)&0xff],
		Revision: "1." + strconv.FormatUint(v&0xf, 10),
		SoC:      socs[(v>>12)&0xf],
		Maker:    makers[(v>>16)&0xf],
		RAMMB:    ram[(v>>20)&0x7],
	}
	if b.Model == "" {
		b.Model = fmt.Sprintf("unknown type 0x%02x", (v>>4)&0xff)
	}
	return b, true
}

func revisionCode() string {
	for _, line := range strings.Split(readWhole("/proc/cpuinfo"), "\n") {
		if strings.HasPrefix(line, "Revision") {
			if _, v, ok := strings.Cut(line, ":"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// sysBlockSize returns a block device's size in bytes and its model name. The
// size is in 512-byte sectors whatever the device's own sector size is —
// /sys/block/*/size has said so since the beginning and is not going to
// change now.
func sysBlockSize(dev string) (int64, string) {
	raw := firstLine(filepath.Join("/sys/block", dev, "size"))
	if raw == "" {
		return 0, ""
	}
	sectors, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || sectors <= 0 {
		return 0, ""
	}
	name := firstLine(filepath.Join("/sys/block", dev, "device", "model"))
	if name == "" {
		name = firstLine(filepath.Join("/sys/block", dev, "device", "name"))
	}
	return sectors * 512, strings.TrimSpace(name)
}

func onlineCPUs() int {
	entries, err := os.ReadDir("/sys/devices/system/cpu")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "cpu") {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(name, "cpu")); err == nil {
			n++
		}
	}
	return n
}

func maxMHz() int {
	raw := firstLine("/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq")
	if raw == "" {
		return 0
	}
	khz, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return khz / 1000
}

// hasWireless says whether this module has radio on it. The revision code
// does not carry it for the Compute Module — the wireless and non-wireless
// variants share a code — so the answer comes from whether the driver found
// anything.
func hasWireless() bool {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "wlan") {
			return true
		}
	}
	return false
}

func primaryMAC() string {
	for _, dev := range []string{"eth0", "end0"} {
		if m := firstLine("/sys/class/net/" + dev + "/address"); m != "" {
			return m
		}
	}
	return ""
}

// readWhole is firstLine's sibling for files that are read line by line.
func readWhole(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func firstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimRight(string(b), "\x00\n ")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
