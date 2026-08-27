package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// What this blade is made of.
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

	// The card device, and which of the two things it is. A Compute Module
	// with eMMC and a Lite with an SD card in the slot both show up as
	// mmcblk0, and calling a card "eMMC" would be a lie the interface then
	// repeats — the kernel says which it is, and eMMC additionally brings
	// boot partitions a card does not have.
	if sz, name := blockSize("mmcblk0"); sz > 0 {
		kind := mmcKind()
		h["mmc_kind"], h["mmc_bytes"] = kind, sz
		if name != "" {
			h["mmc_model"] = name
		}
		if kind == "sd" {
			h["sd_bytes"], h["emmc_bytes"] = sz, int64(0)
		} else {
			h["emmc_bytes"] = sz
			if name != "" {
				h["emmc_model"] = name
			}
		}
	} else {
		h["emmc_bytes"] = int64(0)
	}
	if sz, name := blockSize("nvme0n1"); sz > 0 {
		h["nvme_bytes"] = sz
		if name != "" {
			h["nvme_model"] = name
		}
	}
	h["wireless"] = hasWireless()
	for k, v := range firmware() {
		h[k] = v
	}
	if m := primaryMAC(); m != "" {
		h["eth_mac"] = m
	}
	return h
}

// firmware is what runs before Linux does: the bootloader in the EEPROM, the
// day it was built, how this blade came up this time, and the VideoCore
// firmware that started the kernel. All of it is written into the device tree
// by the firmware itself, so it is read rather than asked for — except the
// VideoCore version, which only vcgencmd knows and only on systems that ship
// it.
func firmware() map[string]any {
	out := map[string]any{}
	if v := dtString("/proc/device-tree/chosen/bootloader/version"); v != "" {
		out["bootloader"] = v
		if len(v) > 8 {
			out["bootloader_short"] = v[:8]
		}
	}
	if ts, ok := dtUint32("/proc/device-tree/chosen/bootloader/build-timestamp"); ok && ts > 0 {
		out["bootloader_built"] = time.Unix(int64(ts), 0).UTC().Format("2006-01-02")
	}
	if m, ok := dtUint32("/proc/device-tree/chosen/bootloader/boot-mode"); ok {
		out["boot_mode"] = bootModeName(m)
	}
	if v := vcgencmdVersion(); v != "" {
		out["vc_firmware"] = v
	}
	return out
}

// bootModeName translates the number the bootloader recorded into the thing
// it means. Same numbering as BOOT_ORDER, which is where anyone who cares
// about this will look next.
func bootModeName(m uint32) string {
	switch m {
	case 1:
		return "sd"
	case 2:
		return "network"
	case 3:
		return "rpiboot"
	case 4:
		return "usb-msd"
	case 5:
		return "usb-bcm"
	case 6:
		return "nvme"
	case 7:
		return "http"
	}
	return fmt.Sprintf("mode %d", m)
}

// vcgencmdVersion asks the VideoCore for its firmware date. It is the one
// piece here that runs a program, because there is no file that holds it, and
// it is skipped where the tool is not installed — Debian ships without it.
func vcgencmdVersion() string {
	path, err := exec.LookPath("vcgencmd")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return ""
	}
	// Three lines: a date, a copyright, and "version <hash> (clean) ...".
	date, hash := "", ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "version "):
			f := strings.Fields(line)
			if len(f) > 1 && len(f[1]) >= 8 {
				hash = f[1][:8]
			}
		case date == "" && line != "" && !strings.HasPrefix(line, "Copyright"):
			date = line
		}
	}
	if t, err := time.Parse("Jan _2 2006 15:04:05", date); err == nil {
		date = t.Format("2006-01-02")
	}
	switch {
	case date != "" && hash != "":
		return date + " (" + hash + ")"
	case date != "":
		return date
	}
	return hash
}

// dtString reads a device tree property that holds text: NUL terminated, and
// the NUL is part of the property rather than the string.
func dtString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\x00\n ")
}

// dtUint32 reads a device tree property that holds one number, big endian as
// the device tree always is.
func dtUint32(path string) (uint32, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 4 {
		return 0, false
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), true
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

// revisionCode asks the two places it is written down. A Raspberry Pi kernel
// puts it in /proc/cpuinfo; an upstream kernel does not, and the firmware's
// own copy in the device tree is the only one left. A blade running Debian
// answered "no revision" until this looked in the second place, and its
// memory then had to be guessed from what the kernel had left over — 7.6 GB
// for a module with 8 on it.
func revisionCode() string {
	for _, line := range strings.Split(readFileStr("/proc/cpuinfo"), "\n") {
		if strings.HasPrefix(line, "Revision") {
			if _, v, ok := strings.Cut(line, ":"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	// Four bytes, big endian, as the firmware wrote them.
	if b, err := os.ReadFile("/proc/device-tree/system/linux,revision"); err == nil && len(b) >= 4 {
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		if v != 0 {
			return fmt.Sprintf("%x", v)
		}
	}
	return ""
}

// blockSize returns a block device's size in bytes and its model name. The
// size is in 512-byte sectors whatever the device's own sector size is —
// /sys/block/*/size has said so since the beginning and is not going to
// change now.
func blockSize(dev string) (int64, string) {
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

// mmcKind tells eMMC from an SD card. The kernel names the type outright, and
// where it does not, the boot partitions do: eMMC has mmcblk0boot0 and boot1,
// a card has neither.
func mmcKind() string {
	switch strings.ToUpper(firstLine("/sys/block/mmcblk0/device/type")) {
	case "MMC":
		return "emmc"
	case "SD":
		return "sd"
	}
	if _, err := os.Stat("/sys/block/mmcblk0boot0"); err == nil {
		return "emmc"
	}
	return "sd"
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
