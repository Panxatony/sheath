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
