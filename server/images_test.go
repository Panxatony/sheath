package main

import "testing"

func TestMatchRecipe(t *testing.T) {
	cases := []struct{ url, os, kernel string }{
		{"https://cdimage.ubuntu.com/releases/24.04/release/ubuntu-24.04.3-preinstalled-server-arm64+raspi.img.xz", "ubuntu", "downstream"},
		{"https://dietpi.com/downloads/images/DietPi_RPi5-ARMv8-Trixie.img.xz", "dietpi", "downstream"},
		{"https://cloud.debian.org/images/cloud/trixie/20260819-2575/debian-13-nocloud-arm64-20260819-2575.tar.xz", "debian", "upstream"},
	}
	for _, c := range cases {
		r, ok := matchRecipe("", c.url)
		if !ok {
			t.Fatalf("%s: no recipe", c.url)
		}
		if r.OSID != c.os || r.Kernel != c.kernel {
			t.Errorf("%s: got %s/%s, want %s/%s", c.url, r.OSID, r.Kernel, c.os, c.kernel)
		}
	}
	if _, ok := matchRecipe("", "https://example.com/alpine-3.20-aarch64.img.xz"); ok {
		t.Error("alpine should not match a recipe")
	}
}

func TestSuggestID(t *testing.T) {
	cases := map[string]string{
		"https://cdimage.ubuntu.com/…/ubuntu-24.04.3-preinstalled-server-arm64+raspi.img.xz": "ubuntu-24.04.3-arm64",
		"https://dietpi.com/downloads/images/DietPi_RPi5-ARMv8-Trixie.img.xz":                "dietpi-trixie-arm64",
		"https://cloud.debian.org/…/debian-13-nocloud-arm64-20260819-2575.tar.xz":            "debian-13-arm64",
	}
	for url, want := range cases {
		if got := suggestID(url); got != want {
			t.Errorf("%s: got %q, want %q", url, got, want)
		}
	}
}
