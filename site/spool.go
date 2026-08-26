package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Keeping the buffers across a restart.
//
// Both queues in this program exist for the same reason: while the centre is
// unreachable, what happened here is what nobody else saw. They were kept in
// memory, which meant a restart of this service during an outage — an upgrade,
// a reboot, a crash — threw away exactly the part of the story that could not
// be reconstructed from anywhere else.
//
// So they are written down. Not on every change: a netboot produces a burst of
// lines and a provisioning run a steady trickle, and fsyncing each one buys
// nothing. A dirty flag and a one-second tick coalesce them, the file is
// replaced atomically, and the buffer is written once more on the way out.
//
// The blade reports carry the blades' own tokens, so the files are 0600.

type spool struct {
	path  string
	snap  func() any // a copy of what should be on disk, taken under the owner's lock
	mu    sync.Mutex
	dirty bool
}

func newSpool(dir, name string, snap func() any) *spool {
	return &spool{path: filepath.Join(dir, name), snap: snap}
}

// touch says the buffer changed. It never writes — the ticker does that, so a
// burst of events costs one write and not fifty.
func (sp *spool) touch() {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	sp.dirty = true
	sp.mu.Unlock()
}

func (sp *spool) run(every time.Duration, stop <-chan struct{}) {
	if sp == nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			sp.writeIfDirty()
		case <-stop:
			sp.writeIfDirty()
			return
		}
	}
}

func (sp *spool) writeIfDirty() {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	dirty := sp.dirty
	sp.dirty = false
	sp.mu.Unlock()
	if !dirty {
		return
	}
	if err := writeJSONFile(sp.path, sp.snap()); err != nil {
		log.Printf("spool %s: %v", filepath.Base(sp.path), err)
		sp.touch() // try again on the next tick
	}
}

func (sp *spool) load(into any) {
	if sp == nil {
		return
	}
	b, err := os.ReadFile(sp.path)
	if err != nil || len(b) == 0 {
		return
	}
	if err := json.Unmarshal(b, into); err != nil {
		log.Printf("spool %s unreadable, starting empty: %v", filepath.Base(sp.path), err)
	}
}

// writeJSONFile replaces a file in one step. A half-written buffer is worse
// than none: it would be read back as a parse error and dropped anyway, and
// the crash that truncated it is exactly when the contents mattered.
func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
