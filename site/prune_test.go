package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneKeepsWhatIsWantedAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	s := &site{cfg: config{ImagesDir: dir}}

	files := []string{
		"ubuntu-24.04.3-arm64.img.xz",
		"ubuntu-24.04.3-arm64.img.xz.verified",
		"DietPi_RPi234-ARMv8-Trixie.img.xz",
		"DietPi_RPi234-ARMv8-Bookworm.img.xz",
		"DietPi_RPi234-ARMv8-Bookworm.img.xz.verified",
		"half-fetched.img.xz.part",
		"notes.txt", // somebody's file, in the wrong place
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s.pruneImages(map[string]bool{"DietPi_RPi234-ARMv8-Bookworm.img.xz": true})

	left := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left[e.Name()] = true
	}
	for _, want := range []string{
		"DietPi_RPi234-ARMv8-Bookworm.img.xz",
		"DietPi_RPi234-ARMv8-Bookworm.img.xz.verified",
		"notes.txt",
	} {
		if !left[want] {
			t.Errorf("%s was removed and should not have been", want)
		}
	}
	for _, gone := range []string{
		"ubuntu-24.04.3-arm64.img.xz",
		"ubuntu-24.04.3-arm64.img.xz.verified",
		"DietPi_RPi234-ARMv8-Trixie.img.xz",
		"half-fetched.img.xz.part",
	} {
		if left[gone] {
			t.Errorf("%s is still there", gone)
		}
	}

	// An empty list means the centre said nothing, not that nothing is needed.
	before, _ := os.ReadDir(dir)
	s.pruneImages(nil)
	after, _ := os.ReadDir(dir)
	if len(after) != len(before) {
		t.Error("an empty list emptied the cache")
	}
}
