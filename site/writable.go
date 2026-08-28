package main

import (
	"sort"
	"strings"
	"syscall"
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

// writeTrouble names the directories that would refuse a write, or "" when
// all of them would take one.
//
// access(2) rather than a probe file: it reports EROFS for a read-only mount,
// which is the failure this exists for, and it leaves nothing behind. Five
// files created and deleted every thirty seconds on a flash device is a cost
// with no return.
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
		if err := syscall.Access(path, wOK); err != nil {
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

// wOK is access(2)'s W_OK. Spelled out rather than pulled from x/sys, which
// this program does not otherwise need.
const wOK = 0x2
