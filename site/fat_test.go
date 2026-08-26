package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The payload the blades boot is a FAT16 image built by mkfs.vfat. This test
// builds one the same way, rewrites a file in it, and reads the result back
// with mtools — the thing we are deliberately not depending on at runtime is
// exactly the right thing to check against.
func TestPatchCmdline(t *testing.T) {
	for _, tool := range []string{"mkfs.vfat", "mcopy", "mtype"} {
		if _, err := exec.LookPath(tool); err != nil {
			if _, err := os.Stat("/usr/sbin/" + tool); err != nil {
				t.Skipf("%s not available", tool)
			}
		}
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "boot.img")
	if err := os.Truncate(img, 0); err != nil {
		_ = os.WriteFile(img, nil, 0o644)
	}
	if err := os.Truncate(img, 27262976); err != nil {
		t.Fatal(err)
	}
	run(t, "mkfs.vfat", "-F", "16", "-n", "BOOT", img)

	src := filepath.Join(dir, "cmdline.txt")
	old := "console=tty1 console=serial0,115200 sheath_server=http://10.0.0.10:8080\n"
	if err := os.WriteFile(src, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "mcopy", "-i", img, src, "::cmdline.txt")
	// A second file, so the test would notice a patch that hits the wrong one.
	other := filepath.Join(dir, "config.txt")
	if err := os.WriteFile(other, []byte("arm_64bit=1\nenable_uart=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "mcopy", "-i", img, other, "::config.txt")

	want := "console=tty1 console=serial0,115200 sheath_server=http://192.168.99.10:8081"
	if err := patchCmdline(img, want); err != nil {
		t.Fatalf("patch: %v", err)
	}

	got := strings.TrimSpace(run(t, "mtype", "-i", img, "::cmdline.txt"))
	if got != want {
		t.Errorf("mtools reads %q, want %q", got, want)
	}
	if mine, err := readFATFile(img, "cmdline.txt"); err != nil || strings.TrimSpace(mine) != want {
		t.Errorf("read back %q (%v), want %q", mine, err, want)
	}
	if cfg := strings.TrimSpace(run(t, "mtype", "-i", img, "::config.txt")); cfg != "arm_64bit=1\nenable_uart=1" {
		t.Errorf("the other file was disturbed: %q", cfg)
	}
}

func TestPatchRefusesToGrowPastACluster(t *testing.T) {
	// A line longer than one cluster would need the chain extended, which
	// this deliberately does not do.
	f := &fat16{data: make([]byte, 4096), bytesPerSec: 512, secPerClus: 1, rootEntries: 16}
	f.rootOff, f.dataOff = 512, 1024
	copy(f.data[512:], fat83("cmdline.txt"))
	f.data[512+26] = 2 // first cluster
	if err := f.replace("cmdline.txt", make([]byte, 600)); err == nil {
		t.Error("a file larger than its cluster was accepted")
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		name = "/usr/sbin/" + name
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}
