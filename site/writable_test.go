package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The point of the check is that it notices, not how it phrases it. A
// directory that is not there cannot be written to either — and that is worth
// reporting rather than passing over, because a site whose images directory
// has vanished is in exactly the trouble this looks for.
func TestWriteTroubleNamesWhatRefuses(t *testing.T) {
	good := t.TempDir()
	s := &site{cfg: config{
		ImagesDir: good, TFTPDir: good, AgentDir: good,
		StateDir: good, HostsDir: good,
	}}
	if got := s.writeTrouble(); got != "" {
		t.Errorf("a writable site reported %q", got)
	}

	s.cfg.ImagesDir = filepath.Join(good, "gone")
	got := s.writeTrouble()
	if !strings.Contains(got, "images") {
		t.Errorf("the missing directory is not named: %q", got)
	}
	if strings.Contains(got, "tftp") {
		t.Errorf("a directory that is fine was named too: %q", got)
	}

	// A directory nobody may write to, which is the same answer as a
	// read-only mount for everyone but root.
	if os.Geteuid() != 0 {
		shut := filepath.Join(good, "shut")
		if err := os.Mkdir(shut, 0o500); err != nil {
			t.Fatal(err)
		}
		s.cfg.ImagesDir, s.cfg.TFTPDir = good, shut
		if got := s.writeTrouble(); !strings.Contains(got, "tftp") {
			t.Errorf("a directory that refuses writes was not named: %q", got)
		}
	}

	// An empty path is a part of the site that is not configured, not a
	// fault.
	s.cfg = config{ImagesDir: good}
	if got := s.writeTrouble(); got != "" {
		t.Errorf("unset directories were counted as trouble: %q", got)
	}
}

// A payload that still points at the address this site used to have is a
// payload every netbooting blade follows into a void: it takes a lease, it
// answers a ping, and nothing reaches it. The stamp cannot see that — it says
// which payload is here, not where it points.
func TestAimedHereReadsWhereThePayloadPoints(t *testing.T) {
	dir := t.TempDir()
	s := &site{cfg: config{TFTPDir: dir, RelayURL: "http://10.0.0.9:8081"}}

	if !(&site{cfg: config{TFTPDir: dir}}).aimedHere() {
		t.Error("a site with no relay URL has nothing to be wrong about")
	}
	if s.aimedHere() {
		t.Error("no cmdline.txt at all counted as aimed here")
	}
	write := func(line string) {
		if err := os.WriteFile(filepath.Join(dir, "cmdline.txt"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("console=tty1 ip=dhcp sheath_server=http://10.0.0.7:8081\n")
	if s.aimedHere() {
		t.Error("a payload aimed at the old address counted as current")
	}
	write("console=tty1 ip=dhcp sheath_server=http://10.0.0.9:8081\n")
	if !s.aimedHere() {
		t.Error("a payload aimed at this site was not recognised")
	}
	// A trailing slash on the configured URL is the same address.
	s.cfg.RelayURL = "http://10.0.0.9:8081/"
	if !s.aimedHere() {
		t.Error("a trailing slash made the same address look different")
	}
}
