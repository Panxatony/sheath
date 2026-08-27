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
