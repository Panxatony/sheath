package main

import (
	"os"
	"strings"
	"testing"
)

// Against the payload that is actually in the rack, not one this test made.
func TestPatchRealPayload(t *testing.T) {
	src := os.Getenv("REAL_BOOTIMG")
	if src == "" {
		t.Skip("REAL_BOOTIMG not set")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skip(err)
	}
	img := t.TempDir() + "/boot.img"
	if err := os.WriteFile(img, data, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := readFATFile(img, "cmdline.txt")
	if err != nil {
		t.Fatalf("reading the original: %v", err)
	}
	t.Logf("was: %s", strings.TrimSpace(before))

	want := "console=tty1 console=serial0,115200 sheath_server=http://192.168.99.10:8081"
	if err := patchCmdline(img, want); err != nil {
		t.Fatalf("patch: %v", err)
	}
	got, err := readFATFile(img, "cmdline.txt")
	if err != nil || strings.TrimSpace(got) != want {
		t.Fatalf("read back %q (%v)", got, err)
	}
	if err := os.WriteFile(os.Getenv("REAL_BOOTIMG")+".patched", mustRead(t, img), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("now: %s", strings.TrimSpace(got))
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
