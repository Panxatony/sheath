package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Whether this site can still write where it has to.
//
// The disk holding /srv/sheath at one site lost its capacity: the kernel saw
// zero sectors, every write landed beyond the end of the device, ext4 aborted
// its journal and remounted read-only. For five hours after that the centre
// showed the site as well — the relay answered, the heartbeat arrived, the
// payload checksum matched. All of that is reading, and reading still worked.
//
// A site that cannot write cannot cache an image, cannot take a new payload,
// and cannot let dnsmasq append to the log it watches for blades netbooting.
// It goes on serving what it already holds, which is exactly why it looks
// fine and is not. So this is asked once a pass, and the answer travels with
// the heartbeat.

// writeTrouble names the directories that refuse a write, or "" when all of
// them take one.
//
// A real write, not access(2). access looked right — it returns EROFS for a
// read-only mount and leaves nothing behind — and it was wrong for exactly
// the case this exists for. When ext4 hits an I/O error it does not remount
// the way a person does; it sets its own shutdown flag, and the mount still
// reads `rw,noatime,emergency_ro`. The superblock is not marked read-only, so
// access says the directory is writable while every write returns EROFS. It
// was measured saying "no trouble" four minutes after the disk had stopped
// taking writes.
//
// So: a file is created and removed in each directory, once a pass. Five
// inodes every thirty seconds is a cost; being wrong about whether a site can
// work at all is the larger one.
func (s *site) writeTrouble() string {
	dirs := map[string]string{
		"images":       s.cfg.ImagesDir,
		"tftp":         s.cfg.TFTPDir,
		"binaries":     s.cfg.AgentDir,
		"state":        s.cfg.StateDir,
		"reservations": s.cfg.HostsDir,
	}
	var bad []string
	for what, path := range dirs {
		if path == "" {
			continue
		}
		if err := probeWrite(path); err != nil {
			bad = append(bad, what+" ("+err.Error()+")")
		}
	}
	if len(bad) == 0 {
		return ""
	}
	// Sorted, because this string is compared with the last one to decide
	// whether anything changed, and a map has no order.
	sort.Strings(bad)
	return "cannot write to " + strings.Join(bad, ", ")
}

// probeWrite creates a file and removes it again. O_EXCL so a leftover from a
// crash is never taken for success, and the name says what it is for anybody
// who finds one.
func probeWrite(dir string) error {
	name := filepath.Join(dir, ".sheath-writable")
	_ = os.Remove(name)
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	// Closing is where a deferred write reports itself, so its error counts.
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}
