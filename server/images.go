package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Bringing an image in.
//
// Adding an image used to mean logging in to the server and running two
// scripts by hand: one to mirror it, one to prepare it. Both are good scripts
// and both stay — this runs them, one image at a time, and writes down where
// it got to. What a person types is a URL; what comes out is an entry blades
// can be installed from.
//
// The work is not done in Go. Mirroring means unpacking a tarball and
// checksumming a gigabyte; preparing means a loop mount and a chroot. The
// shell does that well, the scripts are tested, and rewriting them here would
// buy nothing but a second place for the same mistakes.

const (
	imgQueued  = "queued"
	imgWorking = "working"
	imgReady   = "ready"
	imgError   = "error"
)

// imageWork is one job: fetch this URL, and prepare it with these packages.
type imageWork struct {
	ID       string
	URL      string
	OSID     string
	Packages []string
}

// imageQueue runs one job at a time. One, deliberately: two chroots and two
// gigabyte downloads at once on a Raspberry Pi is how a server stops
// answering.
type imageQueue struct {
	mu      sync.Mutex
	pending []imageWork
	running bool
}

func (a *App) enqueueImage(w imageWork) {
	a.imgQ.mu.Lock()
	a.imgQ.pending = append(a.imgQ.pending, w)
	start := !a.imgQ.running
	a.imgQ.running = true
	a.imgQ.mu.Unlock()
	if start {
		go a.runImageQueue()
	}
}

func (a *App) runImageQueue() {
	for {
		a.imgQ.mu.Lock()
		if len(a.imgQ.pending) == 0 {
			a.imgQ.running = false
			a.imgQ.mu.Unlock()
			return
		}
		job := a.imgQ.pending[0]
		a.imgQ.pending = a.imgQ.pending[1:]
		a.imgQ.mu.Unlock()
		a.workImage(job)
	}
}

func (a *App) workImage(job imageWork) {
	a.setImageState(job.ID, imgWorking, "fetching")
	a.logEvent("", "info", "image "+job.ID+": fetching "+job.URL)

	out, err := a.runTool("mirror-image.sh", job.ID, job.URL, job.OSID)
	if err != nil {
		a.setImageState(job.ID, imgError, "fetch failed: "+lastLine(out))
		a.logEvent("", "warn", "image "+job.ID+": fetch failed — "+lastLine(out))
		return
	}

	if len(job.Packages) > 0 {
		a.setImageState(job.ID, imgWorking, "preparing: "+strings.Join(job.Packages, ", "))
		a.logEvent("", "info", "image "+job.ID+": preparing with "+strings.Join(job.Packages, ", "))
		out, err = a.runTool("prepare-image.sh", append([]string{job.ID}, job.Packages...)...)
		if err != nil {
			// The image is on disk and usable; only the customisation failed.
			// Saying "error" would suggest there is nothing to install.
			a.setImageState(job.ID, imgReady, "fetched, but not prepared: "+lastLine(out))
			a.logEvent("", "warn", "image "+job.ID+": preparation failed — "+lastLine(out))
			return
		}
	}
	a.setImageState(job.ID, imgReady, "")
	a.logEvent("", "info", "image "+job.ID+" is ready")
}

// runTool runs one of the shell tools that ship with Sheath. Through sudo,
// because preparing an image means mounting it: the server itself runs
// unprivileged and should stay that way, so exactly these two scripts are
// permitted and nothing else.
func (a *App) runTool(name string, args ...string) (string, error) {
	path := a.toolsDir + "/" + name
	// --root ahead of the arguments: sudo resets the environment, so the
	// working directory has to travel as an argument or not at all.
	argv := append([]string{"-n", path, "--root=" + a.rootDir}, args...)
	cmd := exec.Command("sudo", argv...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Minute):
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("%s took longer than ninety minutes", name)
	}
	if err != nil {
		log.Printf("%s %v: %v", name, args, err)
	}
	return string(out), err
}

func (a *App) setImageState(id, state, note string) {
	_, _ = a.db.Exec(`UPDATE images SET state=?,note=?,updated=? WHERE id=?`,
		state, note, now(), id)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			if len(t) > 160 {
				t = t[:160] + "…"
			}
			return t
		}
	}
	return "no output"
}

// ── Recipes ──────────────────────────────────────────────────────────
//
// Three distributions are supported, and each needs something different done
// to it before a blade can be installed from it. That knowledge used to live
// in the shell history of whoever added the last image; here it is written
// down once, keyed on what the URL says.

type recipe struct {
	Name     string   // shown in the interface
	Match    []string // all of these must appear in the URL or the id
	OSID     string
	Kernel   string   // downstream = device tree overlays work, upstream = they do not
	MinDisk  int64    // bytes the written image needs
	Packages []string // installed into the image before any blade sees it
	Note     string   // why it is done this way
	IDHint   string   // catalogue name, where the file name carries no version
	Release  bool     // the Debian release name in the file name is the version
}

var recipes = []recipe{
	{
		Name: "Ubuntu 24.04 (arm64, Raspberry Pi)", Match: []string{"ubuntu", "24.04"},
		OSID: "ubuntu", Kernel: "downstream", MinDisk: 8 << 30,
		Note: "Brings cloud-init and SSH; the Raspberry Pi kernel applies device tree overlays, so the smart fan reports.",
	},
	{
		Name: "DietPi v10 (arm64)", Match: []string{"dietpi"},
		OSID: "dietpi", Kernel: "downstream", MinDisk: 4 << 30,
		IDHint: "dietpi-arm64", Release: true,
		Note: "Configures itself at first boot — apt in a chroot would run before that and confuse it, so nothing is installed here.",
	},
	{
		Name: "Debian 13 Trixie (arm64)", Match: []string{"debian", "13"},
		OSID: "debian", Kernel: "upstream", MinDisk: 8 << 30,
		Packages: []string{"openssh-server"},
		Note:     "Ships without SSH, so it is installed here. The upstream kernel ignores device tree overlays: no fan telemetry.",
	},
	{
		Name: "Debian (arm64)", Match: []string{"debian"},
		OSID: "debian", Kernel: "upstream", MinDisk: 8 << 30,
		Packages: []string{"openssh-server"},
		Note:     "Ships without SSH, so it is installed here.",
	},
}

// matchRecipe picks the recipe for a URL. The order in the list decides:
// the more specific entry stands before the general one.
func matchRecipe(id, url string) (recipe, bool) {
	hay := strings.ToLower(id + " " + url)
	for _, r := range recipes {
		hit := true
		for _, m := range r.Match {
			if !strings.Contains(hay, m) {
				hit = false
				break
			}
		}
		if hit {
			return r, true
		}
	}
	return recipe{}, false
}

// suggestID turns a URL into a catalogue id, so nobody has to invent one:
// ubuntu-24.04-preinstalled-server-arm64+raspi.img.xz → ubuntu-24.04-arm64.
func suggestID(url string) string {
	base := url
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ToLower(base)
	for _, suf := range []string{".xz", ".gz", ".zst", ".tar", ".img", ".raw"} {
		base = strings.TrimSuffix(base, suf)
	}
	base = strings.NewReplacer("+", "-", "_", "-", " ", "-").Replace(base)
	if r, ok := matchRecipe("", url); ok {
		// DietPi names its files after the board and the Debian release it is
		// built on: a number picked out of one would say "RPi5" and mean
		// nothing, while the release name says which DietPi this is.
		if r.Release {
			if c := releaseIn(base); c != "" {
				return r.OSID + "-" + c + "-arm64"
			}
		}
		if r.IDHint != "" {
			return r.IDHint
		}
		if v := versionIn(base); v != "" {
			return r.OSID + "-" + v + "-arm64"
		}
		return r.OSID + "-arm64"
	}
	return base
}

// releaseIn finds a Debian release name in a file name.
func releaseIn(s string) string {
	for _, c := range []string{"trixie", "bookworm", "bullseye", "forky"} {
		if strings.Contains(s, c) {
			return c
		}
	}
	return ""
}

// versionIn finds the first dotted or bare version number in a file name.
func versionIn(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		j := i
		for j < len(s) && (s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
			j++
		}
		v := strings.Trim(s[i:j], ".")
		// A date stamp is not a version — Debian's cloud images carry one.
		if len(v) >= 8 && !strings.Contains(v, ".") {
			i = j
			continue
		}
		if v != "" {
			return v
		}
		i = j
	}
	return ""
}

// ── Adding and removing ──────────────────────────────────────────────

type imageFetchReq struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Packages string `json:"packages"` // comma or space separated; empty = what the recipe says
	NoPrep   bool   `json:"no_prepare"`
}

func (a *App) hImagesFetch(w http.ResponseWriter, r *http.Request) {
	var req imageFetchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, "JSON invalid")
		return
	}
	id, rec, err := a.startImageFetch(req)
	if err != nil {
		fail(w, 400, "%v", err)
		return
	}
	writeJSON(w, 202, map[string]any{"id": id, "recipe": rec.Name})
}

// startImageFetch enters the image in the catalogue and hands the work to the
// queue. The page and the API both come through here, so both add an image
// the same way.
func (a *App) startImageFetch(req imageFetchReq) (string, recipe, error) {
	req.URL = strings.TrimSpace(req.URL)
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		return "", recipe{}, me("err.imgurl")
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = suggestID(req.URL)
	}
	if strings.ContainsAny(id, "/\\ \t\"'") {
		return "", recipe{}, me("err.imgid")
	}
	if a.imageBusy(id) {
		return "", recipe{}, me("err.imgbusy", id)
	}

	rec, known := matchRecipe(id, req.URL)
	pkgs := rec.Packages
	if s := strings.TrimSpace(req.Packages); s != "" {
		pkgs = strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	}
	if req.NoPrep {
		pkgs = nil
	}
	notes := rec.Note
	if !known {
		notes = "unknown source — attributes were not derived"
	}

	// The entry exists before the download does, so the interface can show
	// what is happening instead of nothing at all.
	if _, err := a.db.Exec(`INSERT INTO images(id,url,sha256,seed,os_id,notes,local,bytes,created,
		  kernel,min_disk,verified,state,note,updated)
		VALUES(?,?,'','generic',?,?,'',0,?,?,?,0,?,'queued',?)
		ON CONFLICT(id) DO UPDATE SET url=excluded.url, state=excluded.state,
		  note=excluded.note, updated=excluded.updated,
		  os_id=CASE WHEN excluded.os_id<>'' THEN excluded.os_id ELSE images.os_id END,
		  kernel=CASE WHEN excluded.kernel<>'' THEN excluded.kernel ELSE images.kernel END,
		  min_disk=CASE WHEN excluded.min_disk<>0 THEN excluded.min_disk ELSE images.min_disk END,
		  notes=CASE WHEN excluded.notes<>'' THEN excluded.notes ELSE images.notes END`,
		id, req.URL, rec.OSID, notes, now(), rec.Kernel, rec.MinDisk, imgQueued, now()); err != nil {
		return "", recipe{}, err
	}
	a.enqueueImage(imageWork{ID: id, URL: req.URL, OSID: rec.OSID, Packages: pkgs})
	return id, rec, nil
}

// imageBusy says whether an image is already in the queue or being worked on.
func (a *App) imageBusy(id string) bool {
	a.imgQ.mu.Lock()
	defer a.imgQ.mu.Unlock()
	for _, j := range a.imgQ.pending {
		if j.ID == id {
			return true
		}
	}
	var state string
	_ = a.db.QueryRow(`SELECT state FROM images WHERE id=?`, id).Scan(&state)
	return state == imgWorking
}

func (a *App) hImageDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.removeImage(r.PathValue("id")); err != nil {
		fail(w, 409, "%v", err)
		return
	}
	w.WriteHeader(204)
}

func (a *App) removeImage(id string) error {
	if a.imageBusy(id) {
		return me("err.imgbusy", id)
	}
	// A blade that is installed from it keeps its image; removing the entry
	// under it would leave a reinstall with nowhere to go.
	var used int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM blades WHERE image=?`, id).Scan(&used)
	if used > 0 {
		return me("err.imgused", used)
	}
	var local string
	_ = a.db.QueryRow(`SELECT local FROM images WHERE id=?`, id).Scan(&local)
	if _, err := a.db.Exec(`DELETE FROM images WHERE id=?`, id); err != nil {
		return err
	}
	if local != "" && !strings.ContainsAny(local, "/\\") {
		_ = os.Remove(filepath.Join(a.imagesDir, local))
		_ = os.Remove(filepath.Join(a.imagesDir, local+".verified"))
	}
	a.logEvent("", "info", "image "+id+" removed")
	return nil
}
