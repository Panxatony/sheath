package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A root filesystem as it comes out of an image: the unit exists, nothing has
// enabled it yet, and the host keys are not there because the first boot has
// not happened.
func fakeRoot(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestEnableSSHWritesTheSymlinkSystemdReads(t *testing.T) {
	root := fakeRoot(t, "usr/lib/systemd/system/ssh.service")
	msg, err := enableSSH(root)
	if err != nil {
		t.Fatalf("enableSSH: %v", err)
	}
	if !strings.Contains(msg, "ssh.service enabled") {
		t.Errorf("message was %q", msg)
	}
	if !strings.Contains(msg, "0 host keys") {
		t.Errorf("a fresh image has no host keys, message was %q", msg)
	}
	link := filepath.Join(root, "etc/systemd/system/multi-user.target.wants/ssh.service")
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("no symlink: %v", err)
	}
	// Absolute and as it will read on the running system, not as it reads
	// from here — the mount point is ours and does not exist after the reboot.
	if got != "/usr/lib/systemd/system/ssh.service" {
		t.Errorf("symlink points at %q", got)
	}
}

func TestEnableSSHCountsTheHostKeysItFinds(t *testing.T) {
	root := fakeRoot(t,
		"lib/systemd/system/sshd.service",
		"etc/ssh/ssh_host_ed25519_key", "etc/ssh/ssh_host_ed25519_key.pub",
		"etc/ssh/ssh_host_rsa_key", "etc/ssh/sshd_config")
	msg, err := enableSSH(root)
	if err != nil {
		t.Fatalf("enableSSH: %v", err)
	}
	if !strings.Contains(msg, "sshd.service enabled") || !strings.Contains(msg, "2 host keys") {
		t.Errorf("message was %q", msg)
	}
}

func TestEnableSSHLeavesSocketActivationAlone(t *testing.T) {
	root := fakeRoot(t,
		"usr/lib/systemd/system/ssh.service",
		"etc/systemd/system/sockets.target.wants/ssh.socket")
	msg, err := enableSSH(root)
	if err != nil {
		t.Fatalf("enableSSH: %v", err)
	}
	if !strings.Contains(msg, "left alone") {
		t.Errorf("message was %q", msg)
	}
	// Both would then fight over port 22.
	if _, err := os.Lstat(filepath.Join(root, "etc/systemd/system/multi-user.target.wants/ssh.service")); err == nil {
		t.Error("the service was enabled next to the socket")
	}
}

func TestEnableSSHSaysSoWhenItIsAlreadyOn(t *testing.T) {
	root := fakeRoot(t, "usr/lib/systemd/system/ssh.service")
	wants := filepath.Join(root, "etc/systemd/system/multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/lib/systemd/system/ssh.service", filepath.Join(wants, "ssh.service")); err != nil {
		t.Fatal(err)
	}
	msg, err := enableSSH(root)
	if err != nil {
		t.Fatalf("enableSSH: %v", err)
	}
	if !strings.Contains(msg, "already enabled") {
		t.Errorf("message was %q", msg)
	}
}

func TestEnableSSHRefusesAnImageWithoutOne(t *testing.T) {
	if _, err := enableSSH(fakeRoot(t, "etc/os-release")); err == nil {
		t.Error("an image with no ssh unit should say so")
	}
}
