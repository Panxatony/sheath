package main

import "testing"

// The codes are from real boards: what the firmware puts in /proc/cpuinfo on
// the modules in this rack, and a few others to make sure the fields are not
// being read out of the wrong bits.
func TestDecodeRevision(t *testing.T) {
	cases := []struct {
		code  string
		model string
		rev   string
		ram   int
		soc   string
		maker string
	}{
		{"a03140", "Compute Module 4", "1.0", 1024, "BCM2711", "Sony UK"},
		{"b03140", "Compute Module 4", "1.0", 2048, "BCM2711", "Sony UK"},
		{"c03141", "Compute Module 4", "1.1", 4096, "BCM2711", "Sony UK"},
		{"d03141", "Compute Module 4", "1.1", 8192, "BCM2711", "Sony UK"},
		{"c03111", "Pi 4 Model B", "1.1", 4096, "BCM2711", "Sony UK"},
		{"902120", "Zero 2 W", "1.0", 512, "BCM2837", "Sony UK"},
	}
	for _, c := range cases {
		b, ok := decodeRevision(c.code)
		if !ok {
			t.Errorf("%s: not decoded", c.code)
			continue
		}
		if b.Model != c.model || b.Revision != c.rev || b.RAMMB != c.ram ||
			b.SoC != c.soc || b.Maker != c.maker {
			t.Errorf("%s: got %+v", c.code, b)
		}
	}
	// Old-style codes predate every board this can run on; saying "I cannot
	// read this" beats reading the wrong fields out of it.
	if _, ok := decodeRevision("0002"); ok {
		t.Error("an old-style code was decoded as if it were new-style")
	}
	if _, ok := decodeRevision("not a number"); ok {
		t.Error("nonsense was decoded")
	}
}
