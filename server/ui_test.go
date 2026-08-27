package main

import "testing"

// A disk is sold in powers of ten and remembered in powers of two, and only
// one of those is printed on the drive.
func TestHumanReadsLikeTheSticker(t *testing.T) {
	cases := map[int64]string{
		500107862016: "500.1 GB", // the SSD in these blades
		7818182656:   "7.8 GB",   // the eMMC on a CM4 with 8 GB
		1232556432:   "1.2 GB",   // an Ubuntu image
		425984:       "426 KB",
		0:            "—",
	}
	for n, want := range cases {
		if got := human(n); got != want {
			t.Errorf("%d → %q, want %q", n, got, want)
		}
	}
	// Memory keeps its own unit: 8 GB of RAM is 8 GiB and says so.
	if got := ramText(8192 << 20); got != "8 GB" {
		t.Errorf("memory reads %q", got)
	}
}
