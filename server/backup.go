package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backing the database up onto the same machine.
//
// The machine itself is backed up from outside — Proxmox, Borgmatic. That
// covers the disk and not the database: SQLite writes through a WAL, and a
// file-level copy taken while the server is writing captures a torn state
// that restores as "database disk image is malformed". What the outside
// backup needs is a file that is already consistent when it arrives.
//
// VACUUM INTO produces exactly that — a complete copy with the WAL folded in,
// taken while everyone keeps working, and compacted on the way out. It is
// the same thing `.backup` does in the shell, without needing a shell.
//
// The copies carry every token in the system. They belong to their owner and
// to nobody else: the directory is 0700, the files 0600, and they live
// outside anything the server serves over HTTP.

const backupPrefix = "sheath-"
const backupStamp = "20060102T150405Z"

// backupNow writes one copy and returns its path and size.
func (a *App) backupNow() (string, int64, error) {
	if a.backupDir == "" {
		return "", 0, fmt.Errorf("no backup directory configured")
	}
	if err := os.MkdirAll(a.backupDir, 0o700); err != nil {
		return "", 0, err
	}
	name := backupPrefix + time.Now().UTC().Format(backupStamp) + ".db"
	path := filepath.Join(a.backupDir, name)

	// VACUUM INTO takes a string literal, not a placeholder. The path comes
	// from a flag on this server and never from a request; doubling the
	// quote is what keeps it a path and not a statement.
	lit := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	if _, err := a.db.Exec(`VACUUM INTO ` + lit); err != nil {
		_ = os.Remove(path)
		return "", 0, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		log.Printf("backup mode: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}

	// A stable name beside the timestamped ones, so a restore does not begin
	// with reading a directory listing.
	link := filepath.Join(a.backupDir, backupPrefix+"latest.db")
	_ = os.Remove(link)
	if err := os.Symlink(name, link); err != nil {
		log.Printf("backup link: %v", err)
	}
	return path, st.Size(), nil
}

// pruneBackups keeps the newest `keep` copies and removes the rest. Only
// files this server wrote itself — whatever else somebody put in that
// directory is not ours to delete.
func (a *App) pruneBackups(keep int) int {
	if keep <= 0 {
		return 0
	}
	names := a.backupList()
	if len(names) <= keep {
		return 0
	}
	dropped := 0
	for _, n := range names[keep:] {
		if err := os.Remove(filepath.Join(a.backupDir, n)); err == nil {
			dropped++
		}
	}
	return dropped
}

// backupList returns the copies, newest first. The names sort by time
// because the timestamp in them is written that way round.
func (a *App) backupList() []string {
	entries, err := os.ReadDir(a.backupDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, backupPrefix) || !strings.HasSuffix(n, ".db") {
			continue
		}
		if n == backupPrefix+"latest.db" {
			continue
		}
		names = append(names, n)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

// backupInfo describes the state of the copies for the interface.
type backupInfo struct {
	Dir   string
	Count int
	Last  time.Time
	Size  int64
	Keep  int
	At    string
}

func (a *App) backupInfo() backupInfo {
	info := backupInfo{Dir: a.backupDir, Keep: a.backupKeep, At: a.backupAt}
	names := a.backupList()
	info.Count = len(names)
	if len(names) == 0 {
		return info
	}
	if st, err := os.Stat(filepath.Join(a.backupDir, names[0])); err == nil {
		info.Size = st.Size()
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(names[0], backupPrefix), ".db")
	if t, err := time.Parse(backupStamp, stamp); err == nil {
		info.Last = t
	}
	return info
}

// runBackups writes a copy every day at the configured time, and one at
// startup when the newest is older than a day — a machine that is switched
// off at night would otherwise never reach its hour.
func (a *App) runBackups() {
	if a.backupDir == "" {
		log.Printf("backups switched off (--backup empty)")
		return
	}
	log.Printf("backups: %s daily at %s, keeping %d", a.backupDir, a.backupAt, a.backupKeep)

	if info := a.backupInfo(); info.Last.IsZero() || time.Since(info.Last) > 24*time.Hour {
		a.backupAndLog("catching up")
	}
	for {
		time.Sleep(time.Until(nextDaily(a.backupAt)))
		a.backupAndLog("")
	}
}

func (a *App) backupAndLog(why string) {
	path, size, err := a.backupNow()
	if err != nil {
		log.Printf("backup failed: %v", err)
		a.logEvent("", "warn", "backup failed: "+err.Error())
		return
	}
	dropped := a.pruneBackups(a.backupKeep)
	msg := fmt.Sprintf("backup written: %s (%s)", filepath.Base(path), human(size))
	if dropped > 0 {
		msg += fmt.Sprintf(", %d older one(s) removed", dropped)
	}
	if why != "" {
		msg += " — " + why
	}
	log.Print(msg)
	a.logEvent("", "info", msg)
}

// nextDaily is the next occurrence of "HH:MM" in local time. Local and not
// UTC: somebody who sets 03:30 means the quiet hour where the machine
// stands, and wants it to stay there when the clocks change.
func nextDaily(hhmm string) time.Time {
	h, m := 3, 30
	if _, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		h, m = 3, 30
	}
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func (a *App) hBackupNow(w http.ResponseWriter, r *http.Request) {
	path, size, err := a.backupNow()
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	dropped := a.pruneBackups(a.backupKeep)
	a.logEvent("", "info", fmt.Sprintf("backup written: %s (%s)", filepath.Base(path), human(size)))
	writeJSON(w, 201, map[string]any{
		"file": filepath.Base(path), "bytes": size, "removed": dropped,
	})
}
