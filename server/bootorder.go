package main

import (
	"fmt"
	"strings"
)

// The order the bootloader tries devices in.
//
// One number in the module's EEPROM, read from the right: the lowest digit is
// tried first, an f starts the sequence over, and a 0 ends it. Nothing on a
// running system shows it — Linux is already past it by then — so a blade has
// to read it out through the firmware and say so, and this is where the
// number is turned back into the sentence it stands for.
//
// It is worth the trouble because of what it explains: a blade whose order
// does not name the device its image was written to boots nowhere, and says
// nothing at all while doing it. That is not a symptom anyone diagnoses from
// the outside; the rack looks like a blade that will not come up.

// bootOrderSteps turns the number into the sequence it means, first tried
// first. Everything behind a restart is unreachable, and so is everything
// behind a 0 — both end the list rather than being a device in it. What is
// not a hexadecimal number is not an order, and yields none.
func bootOrderSteps(order string) []byte {
	s := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(order)), "0x")
	var out []byte
	for i := len(s) - 1; i >= 0; i-- {
		var d byte
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		default:
			// Not a number at all. Part of one is worse than nothing here: it
			// would be read out as a boot order no module has.
			return nil
		}
		if d == 0 {
			break
		}
		out = append(out, d)
		if d == 0xf {
			break
		}
	}
	return out
}

// bootOrderText reads the number out loud: "network → NVMe → SD/eMMC → start
// over".
func bootOrderText(l Lang, order string) string {
	steps := bootOrderSteps(order)
	if len(steps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(steps))
	for _, d := range steps {
		key := bootStepKey(d)
		if key == "bo.other" {
			parts = append(parts, T(l, key, fmt.Sprintf("%x", d)))
			continue
		}
		parts = append(parts, T(l, key))
	}
	return strings.Join(parts, " → ")
}

// bootOrderReaches says whether the order names a kind of device at all. A
// Compute Module has one digit for the card interface, because the eMMC and a
// card in the slot are the same interface — so a module with eMMC and a Lite
// with an SD card are asking the same question here.
func bootOrderReaches(order, kind string) bool {
	for _, d := range bootOrderSteps(order) {
		switch d {
		case 1:
			if kind == "sd" || kind == "emmc" {
				return true
			}
		case 6:
			if kind == "nvme" {
				return true
			}
		case 4, 5:
			if kind == "usb" {
				return true
			}
		}
	}
	return false
}

func bootStepKey(d byte) string {
	switch d {
	case 1:
		return "bo.card"
	case 2:
		return "bo.network"
	case 3:
		return "bo.rpiboot"
	case 4, 5:
		return "bo.usb"
	case 6:
		return "bo.nvme"
	case 7:
		return "bo.http"
	case 0xe:
		return "bo.stop"
	case 0xf:
		return "bo.restart"
	}
	return "bo.other"
}
