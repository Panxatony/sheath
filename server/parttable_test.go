package main

import "testing"

// The two tables as they actually are on the images in the catalogue, read
// off the first kilobyte of each file.
func head(sig bool, gpt bool, types ...byte) []byte {
	h := make([]byte, 1024)
	for i, t := range types {
		h[446+16*i+4] = t
	}
	if sig {
		h[510], h[511] = 0x55, 0xAA
	}
	if gpt {
		copy(h[512:520], "EFI PART")
	}
	return h
}

func TestPartTableTellsTheTwoApart(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
		want string
	}{
		// Debian 13 arm64: a GPT with a hybrid MBR — the FAT entry is there,
		// and the blade still boots from nowhere off an eMMC.
		{"debian, hybrid", head(true, true, 0x0c, 0xEE, 0xEE), "gpt"},
		// DietPi: a plain MBR, boot flag on the FAT partition.
		{"dietpi", head(true, false, 0x0c, 0x83), "mbr"},
		{"ubuntu", head(true, false, 0x0c, 0x83), "mbr"},
		// A GPT with nothing but the protective entry.
		{"protective only", head(true, true, 0xEE), "gpt"},
		// Not a partition table at all: say nothing rather than guess, or an
		// image nobody could read becomes an image nobody may install.
		{"no signature", head(false, false, 0x0c, 0x83), ""},
		{"empty table", head(true, false), ""},
		{"too short", make([]byte, 64), ""},
	} {
		if got := partTableOf(c.in); got != c.want {
			t.Errorf("%s: partTableOf = %q, want %q", c.name, got, c.want)
		}
	}
}
