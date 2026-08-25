package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// requireAdmin guards the management endpoints. With no admin token set the
// server stays open — deliberate during first setup, and startup says so.
func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.adminToken == "" {
			next(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(a.adminToken)) != 1 {
			fail(w, http.StatusUnauthorized, "admin token missing or wrong")
			return
		}
		next(w, r)
	}
}

// requireBlade checks the blade's own token. A blade may only read and
// report its own state.
func (a *App) requireBlade(r *http.Request, serial string) error {
	var tok string
	err := a.db.QueryRow(`SELECT token FROM blades WHERE serial=?`, serial).Scan(&tok)
	if err != nil {
		return fmt.Errorf("unknown blade")
	}
	if tok == "" || subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(tok)) != 1 {
		return fmt.Errorf("blade token missing or wrong")
	}
	return nil
}

// ── BladeRunners ─────────────────────────────────────────────────────

func (a *App) hSitesList(w http.ResponseWriter, r *http.Request) {
	sites, err := a.listSites()
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, sites)
}

func (a *App) hSiteCreate(w http.ResponseWriter, r *http.Request) {
	var in Site
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "invalid JSON: %v", err)
		return
	}
	if in.OffsetBase == 0 {
		in.OffsetBase = 100
	}
	if in.OffsetStep == 0 {
		in.OffsetStep = 20
	}
	id, err := a.createSite(in)
	if err != nil {
		fail(w, 409, "%v", err)
		return
	}
	a.logEvent("", "info", fmt.Sprintf("site %q created (%s.0/24)", in.Name, in.NetBase))
	st, _ := a.getSite(id)
	writeJSON(w, 201, st)
}

func (a *App) hSiteUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in Site
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "invalid JSON: %v", err)
		return
	}
	if err := a.updateSite(id, in); err != nil {
		fail(w, 409, "%v", err)
		return
	}
	// Addresses are derived from the site, so moving its network moves every
	// blade in it. The reservations have to follow in the same breath.
	_, _ = a.syncDHCP()
	a.logEvent("", "info", "site changed: "+in.Name)
	st, _ := a.getSite(id)
	writeJSON(w, 200, st)
}

func (a *App) hSiteDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.deleteSite(id); err != nil {
		fail(w, 409, "%v", err)
		return
	}
	a.logEvent("", "warn", "site removed")
	writeJSON(w, 200, map[string]string{"deleted": "1"})
}

// ── The site interface ───────────────────────────────────────────────
//
// A site is the network presence: it hands out addresses, gates netboot,
// serves images and watches the wire. It holds no decisions — which image a
// blade gets is decided centrally — but it must be able to act on the last
// decision it heard, including while the line to the centre is down.
//
// Three endpoints carry that: the desired state it should realise, the
// observations it made, and a heartbeat saying it is still there.

// SiteDesired is everything a site needs to serve its blades without asking
// again. Deliberately self-contained: a site that has this document can run
// through a WAN outage.
type SiteDesired struct {
	Site     Site           `json:"site"`
	Blades   []SiteBlade    `json:"blades"`
	Images   []SiteImage    `json:"images"`
	Boot     SiteBoot       `json:"boot"`
	Version  string         `json:"version"`
	Produced string         `json:"produced"`
	Extra    map[string]any `json:"extra,omitempty"`
}

// SiteBlade is one reservation plus the one bit that decides what happens at
// the next power-on: whether netboot is armed.
type SiteBlade struct {
	Serial   string `json:"serial"`
	MAC      string `json:"mac"`
	Hostname string `json:"hostname"`
	IP       string `json:"ip"`
	Rack     string `json:"rack"`
	Slot     int    `json:"slot"`
	Netboot  bool   `json:"netboot"`
	Image    string `json:"image,omitempty"`
}

// SiteImage is an image the site should hold, because a blade of its own is
// assigned to it. The bytes cross the site link once, not once per blade.
type SiteImage struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Local  string `json:"local,omitempty"`
}

// SiteBoot is the netboot payload the site should offer over TFTP.
type SiteBoot struct {
	BootImg    string `json:"boot_img"`
	SHA256     string `json:"sha256"`
	CmdlineURL string `json:"cmdline_url"`
	ServerURL  string `json:"server_url"`
}

func (a *App) siteDesired(id int64) (*SiteDesired, error) {
	st, err := a.getSite(id)
	if err != nil {
		return nil, err
	}
	blades, err := a.listBlades()
	if err != nil {
		return nil, err
	}
	out := &SiteDesired{Site: *st, Produced: now()}
	// The token is the site's own credential; it must not travel back to it
	// inside a document that may be cached on disk.
	out.Site.Token = ""

	wantImg := map[string]bool{}
	for _, b := range blades {
		if b.SiteID != id || b.RackID == nil || b.Slot == nil || b.IP == "" {
			continue
		}
		mac := b.MAC
		if mac == "" {
			mac = bladeMAC(int64(b.RackIdx), *b.Slot)
		}
		host := b.Hostname
		if host == "" {
			host = bladeHostname(int64(b.RackIdx), *b.Slot)
		}
		out.Blades = append(out.Blades, SiteBlade{
			Serial: b.Serial, MAC: mac, Hostname: host, IP: b.IP,
			Rack: b.RackName, Slot: *b.Slot,
			Netboot: b.InstallState == installPending,
			Image:   b.Image,
		})
		if b.Image != "" {
			wantImg[b.Image] = true
		}
	}
	sort.Slice(out.Blades, func(i, j int) bool { return out.Blades[i].IP < out.Blades[j].IP })

	images, _ := a.listImages()
	for _, im := range images {
		if !wantImg[im.ID] {
			continue
		}
		out.Images = append(out.Images, SiteImage{
			ID: im.ID, URL: im.URL, SHA256: im.SHA256, Bytes: im.Bytes, Local: im.Local,
		})
	}

	out.Boot = SiteBoot{
		BootImg:    a.baseURL + "/boot/boot.img",
		CmdlineURL: a.baseURL + "/boot/cmdline.txt",
		ServerURL:  a.baseURL,
	}

	// The version is the content, hashed — but only the part that is content.
	// last_seen changes on every request the site makes, so hashing the site
	// row whole would hand out a new version every thirty seconds and make
	// the conditional request pointless.
	raw, _ := json.Marshal(struct {
		Net, GW, DNS, Domain string
		From, To             int
		B                    []SiteBlade
		I                    []SiteImage
		T                    SiteBoot
	}{st.NetBase, st.Gateway, st.DNS, st.Domain, st.PoolFrom, st.PoolTo,
		out.Blades, out.Images, out.Boot})
	sum := sha256.Sum256(raw)
	out.Version = "sha256:" + hex.EncodeToString(sum[:])[:16]
	return out, nil
}

// requireSite authenticates a site by its own token. A site may act for
// itself and for nothing else — the id in the path and the token have to
// agree.
func (a *App) requireSite(r *http.Request, id int64) error {
	st, err := a.getSite(id)
	if err != nil {
		return fmt.Errorf("unknown site")
	}
	if st.Token == "" {
		return fmt.Errorf("site %d has no token yet", id)
	}
	given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(given), []byte(st.Token)) != 1 {
		return fmt.Errorf("site token missing or wrong")
	}
	return nil
}

func (a *App) hSiteDesired(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.requireSite(r, id); err != nil {
		fail(w, 401, "%v", err)
		return
	}
	d, err := a.siteDesired(id)
	if err != nil {
		fail(w, 404, "%v", err)
		return
	}
	// Unchanged is the common case; say so in four bytes rather than in a
	// document that repeats what the site already has.
	if match := r.Header.Get("If-None-Match"); match != "" && match == d.Version {
		a.touchSite(id)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	a.touchSite(id)
	w.Header().Set("ETag", d.Version)
	writeJSON(w, 200, d)
}

// hSiteEvents takes what the site saw. Batched on purpose: a site reports
// what happened while it was alone, and that may be a hundred lines at once.
func (a *App) hSiteEvents(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.requireSite(r, id); err != nil {
		fail(w, 401, "%v", err)
		return
	}
	var in struct {
		Events []struct {
			TS     string `json:"ts"`
			Serial string `json:"serial"`
			Level  string `json:"level"`
			Msg    string `json:"msg"`
			Stage  string `json:"stage"`
			MAC    string `json:"mac"`
			IP     string `json:"ip"`
		} `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "invalid JSON: %v", err)
		return
	}
	for _, e := range in.Events {
		// An observation about a MAC on the wire is a netboot session, not a
		// log line — the site watched the boot, the centre keeps the state.
		if e.Stage != "" && e.MAC != "" {
			a.touchNetboot(e.MAC, e.IP, e.Stage, e.Msg)
			continue
		}
		lvl := e.Level
		if lvl == "" {
			lvl = "info"
		}
		a.logEvent(e.Serial, lvl, "site: "+e.Msg)
	}
	a.touchSite(id)
	writeJSON(w, 200, map[string]int{"accepted": len(in.Events)})
}

// hSiteStatus is the heartbeat. It also carries the site's own clock, because
// commands expire after fifteen minutes and two clocks that disagree expire
// them wrongly.
func (a *App) hSiteStatus(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.requireSite(r, id); err != nil {
		fail(w, 401, "%v", err)
		return
	}
	var in struct {
		Version   string `json:"version"`
		Applied   string `json:"applied"`
		Clock     string `json:"clock"`
		Blades    int    `json:"blades"`
		Images    int    `json:"images"`
		Note      string `json:"note"`
		DnsmasqOK bool   `json:"dnsmasq_ok"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	a.touchSite(id)
	skew := ""
	if in.Clock != "" {
		if t, err := time.Parse(time.RFC3339, in.Clock); err == nil {
			if d := time.Since(t); d > time.Minute || d < -time.Minute {
				skew = fmt.Sprintf(" (clock off by %s)", d.Round(time.Second))
			}
		}
	}
	if in.Note != "" || skew != "" {
		a.logEvent("", "info", fmt.Sprintf("site %d: %s%s", id, in.Note, skew))
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// hSiteToken issues or rotates a site's credential. Shown once — it is stored
// as it is, and a site that lost it gets a new one rather than the old one
// back.
func (a *App) hSiteToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	tok := newToken()
	if _, err := a.db.Exec(`UPDATE sites SET token=? WHERE id=?`, tok, id); err != nil {
		fail(w, 500, "%v", err)
		return
	}
	a.logEvent("", "warn", fmt.Sprintf("site %d: token issued", id))
	writeJSON(w, 200, map[string]string{"site_id": strconv.FormatInt(id, 10), "token": tok})
}

func (a *App) touchSite(id int64) {
	_, _ = a.db.Exec(`UPDATE sites SET last_seen=? WHERE id=?`, now(), id)
}

func (a *App) hRacksList(w http.ResponseWriter, r *http.Request) {
	racks, err := a.listRacks()
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, racks)
}

func (a *App) hRacksCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		Size     int    `json:"size"`
		Location string `json:"location"`
		IPOffset int    `json:"ip_offset"`
		SiteID   int64  `json:"site_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "invalid JSON: %v", err)
		return
	}
	if in.Name == "" {
		fail(w, 400, "name is missing")
		return
	}
	if !validSize(in.Size) {
		fail(w, 400, "size must be 2, 4, 10 or 20 (was %d)", in.Size)
		return
	}
	siteID := in.SiteID
	if siteID == 0 {
		st, err := a.localSite()
		if err != nil {
			fail(w, 500, "no site configured: %v", err)
			return
		}
		siteID = st.ID
	}
	off := in.IPOffset
	if off == 0 {
		var err error
		if off, err = a.nextRackOffset(siteID); err != nil {
			fail(w, 409, "%v", err)
			return
		}
	}
	res, err := a.db.Exec(
		`INSERT INTO racks(site_id,name,size,ip_offset,location,created) VALUES(?,?,?,?,?,?)`,
		siteID, in.Name, in.Size, off, in.Location, now())
	if err != nil {
		fail(w, 409, "BladeRunner could not be created: %v", err)
		return
	}
	id, _ := res.LastInsertId()
	a.logEvent("", "info", fmt.Sprintf("BladeRunner %q created (%d slots, block .%d-.%d)",
		in.Name, in.Size, off+1, off+in.Size))
	rk, _ := a.getRack(id)
	writeJSON(w, 201, rk)
}

func (a *App) hRackUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Name     string `json:"name"`
		Location string `json:"location"`
		Size     int    `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "invalid JSON: %v", err)
		return
	}
	if err := a.updateRack(id, in.Name, in.Location, in.Size); err != nil {
		fail(w, 400, "%v", err)
		return
	}
	_, _ = a.syncDHCP()
	rk, _ := a.getRack(id)
	a.logEvent("", "info", "BladeRunner changed: "+rk.Name)
	writeJSON(w, 200, rk)
}

func (a *App) hRackDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var n int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM blades WHERE rack_id=?`, id).Scan(&n)
	if n > 0 {
		fail(w, 409, "BladeRunner still holds %d blade(s) — clear it first", n)
		return
	}
	if _, err := a.db.Exec(`DELETE FROM racks WHERE id=?`, id); err != nil {
		fail(w, 500, "%v", err)
		return
	}
	_, _ = a.syncDHCP()
	writeJSON(w, 200, map[string]any{"deleted": id})
}

// ── Blades ───────────────────────────────────────────────────────────

func (a *App) hBladesList(w http.ResponseWriter, r *http.Request) {
	blades, err := a.listBlades()
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, blades)
}

func (a *App) hBladeGet(w http.ResponseWriter, r *http.Request) {
	b, err := a.getBlade(r.PathValue("serial"))
	if err != nil {
		fail(w, 404, "blade not found")
		return
	}
	writeJSON(w, 200, b)
}

// hBladeUpdate sets position, name, image and groups. The address follows
// from those; MAC and hostname are derived when not given.
func (a *App) hBladeUpdate(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	cur, err := a.getBlade(serial)
	if err != nil {
		fail(w, 404, "blade not found")
		return
	}
	var in struct {
		RackID   *int64    `json:"rack_id"`
		Slot     *int      `json:"slot"`
		Hostname *string   `json:"hostname"`
		Image    *string   `json:"image"`
		MAC      *string   `json:"mac"`
		Groups   *[]string `json:"groups"`
		State    *string   `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "invalid JSON: %v", err)
		return
	}

	rackID, slot := cur.RackID, cur.Slot
	if in.RackID != nil {
		if *in.RackID == 0 {
			rackID = nil
		} else {
			rackID = in.RackID
		}
	}
	if in.Slot != nil {
		if *in.Slot == 0 {
			slot = nil
		} else {
			slot = in.Slot
		}
	}
	if rackID != nil && slot != nil {
		rk, err := a.getRack(*rackID)
		if err != nil {
			fail(w, 400, "BladeRunner %d does not exist", *rackID)
			return
		}
		if err := validSlot(rk.Size, *slot); err != nil {
			fail(w, 400, "%v", err)
			return
		}
		var occupant string
		err = a.db.QueryRow(`SELECT serial FROM blades WHERE rack_id=? AND slot=? AND serial<>?`,
			*rackID, *slot, serial).Scan(&occupant)
		if err == nil && occupant != "" {
			fail(w, 409, "slot %d in BladeRunner %q is already taken by %s", *slot, rk.Name, occupant)
			return
		}
	}

	var idx int64
	if rackID != nil {
		if rk, err := a.getRack(*rackID); err == nil {
			idx = int64(a.rackIndex(*rk))
		}
	}
	host := cur.Hostname
	if in.Hostname != nil {
		host = *in.Hostname
	}
	if host == "" && rackID != nil && slot != nil {
		host = bladeHostname(idx, *slot)
	}
	mac := cur.MAC
	if in.MAC != nil {
		mac = *in.MAC
	}
	if mac == "" && rackID != nil && slot != nil {
		mac = bladeMAC(idx, *slot)
	}
	img := cur.Image
	if in.Image != nil {
		img = *in.Image
	}
	groups := cur.Groups
	if in.Groups != nil {
		groups = *in.Groups
	}
	state := cur.State
	if in.State != nil {
		state = *in.State
	}
	if state == "new" && rackID != nil && slot != nil {
		state = "enrolled"
	}
	gj, _ := json.Marshal(groups)

	_, err = a.db.Exec(`UPDATE blades SET rack_id=?,slot=?,hostname=?,mac=?,image=?,
		groups_json=?,state=? WHERE serial=?`,
		rackID, slot, host, mac, img, string(gj), state, serial)
	if err != nil {
		fail(w, 409, "Aktualisierung fehlgeschlagen: %v", err)
		return
	}
	sync, _ := a.syncDHCP()
	b, _ := a.getBlade(serial)
	a.logEvent(serial, "info", "blade updated")
	writeJSON(w, 200, map[string]any{"blade": b, "dhcp": sync})
}

func (a *App) hBladeDelete(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if _, err := a.db.Exec(`DELETE FROM blades WHERE serial=?`, serial); err != nil {
		fail(w, 500, "%v", err)
		return
	}
	sync, _ := a.syncDHCP()
	a.logEvent(serial, "warn", "blade removed from the inventory")
	writeJSON(w, 200, map[string]any{"deleted": serial, "dhcp": sync})
}

func (a *App) hBladeAction(w http.ResponseWriter, r *http.Request) {
	serial, kind := r.PathValue("serial"), r.PathValue("kind")
	switch kind {
	case "identify", "reboot", "reimage":
	default:
		fail(w, 400, "unknown action %q (identify|reboot|reimage)", kind)
		return
	}
	if _, err := a.getBlade(serial); err != nil {
		fail(w, 404, "blade not found")
		return
	}
	if kind == "reimage" {
		if err := a.requestInstall(serial); err != nil {
			fail(w, 409, "%v", err)
			return
		}
	}
	// Replace pending commands of the same kind instead of stacking them:
	// queueing "reboot" three times must not reboot three times.
	_, _ = a.db.Exec(`UPDATE commands SET taken=? WHERE serial=? AND kind=? AND taken=''`,
		now()+" (ersetzt)", serial, kind)
	if _, err := a.db.Exec(`INSERT INTO commands(serial,kind,created) VALUES(?,?,?)`,
		serial, kind, now()); err != nil {
		fail(w, 500, "%v", err)
		return
	}
	a.logEvent(serial, "info", "command queued: "+kind)
	writeJSON(w, 202, map[string]string{"queued": kind, "serial": serial})
}

// ── Agent endpoints ──────────────────────────────────────────────────

func (a *App) hEnroll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Serial  string `json:"serial"`
		MAC     string `json:"mac"`
		Variant string `json:"variant"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Serial == "" {
		fail(w, 400, "serial is missing")
		return
	}
	short := in.Serial
	if len(short) > 8 {
		short = short[len(short)-8:]
	}

	var tok string
	err := a.db.QueryRow(`SELECT token FROM blades WHERE serial=?`, in.Serial).Scan(&tok)
	if err != nil {
		tok = newToken()
		_, err = a.db.Exec(`INSERT INTO blades(serial,short_serial,mac,variant,state,token,created)
			VALUES(?,?,?,?,'new',?,?)`, in.Serial, short, in.MAC, in.Variant, tok, now())
		if err != nil {
			fail(w, 500, "Enrollment fehlgeschlagen: %v", err)
			return
		}
		a.logEvent(in.Serial, "info", "new blade enrolled (MAC "+in.MAC+")")
	} else {
		// Known blade: update the MAC, keep the token.
		if in.MAC != "" {
			_, _ = a.db.Exec(`UPDATE blades SET mac=?, short_serial=? WHERE serial=?`,
				in.MAC, short, in.Serial)
		}
	}
	b, _ := a.getBlade(in.Serial)
	writeJSON(w, 200, map[string]any{"token": tok, "blade": b})
}

func (a *App) hBladeConfig(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if err := a.requireBlade(r, serial); err != nil {
		fail(w, 401, "%v", err)
		return
	}
	b, err := a.getBlade(serial)
	if err != nil {
		fail(w, 404, "blade not found")
		return
	}
	cfg, hash := a.mergedConfig(b)
	w.Header().Set("ETag", `"`+hash+`"`)
	if strings.Contains(r.Header.Get("If-None-Match"), hash) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, 200, map[string]any{"version": hash, "config": cfg})
}

func (a *App) hBladeStatus(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if err := a.requireBlade(r, serial); err != nil {
		fail(w, 401, "%v", err)
		return
	}
	var in struct {
		Facts         json.RawMessage `json:"facts"`
		Health        json.RawMessage `json:"health"`
		ConfigApplied string          `json:"config_applied"`
		Changes       []string        `json:"changes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "invalid JSON: %v", err)
		return
	}
	facts, health := "{}", "{}"
	if len(in.Facts) > 0 {
		facts = string(in.Facts)
	}
	if len(in.Health) > 0 {
		health = string(in.Health)
	}
	// If an agent checks in, the blade is online — whatever state it was in
	// before. Only "provisioning" stands: there the installer reports, not the
	// agent, and a heartbeat must not be read as the install having finished.
	_, err := a.db.Exec(`UPDATE blades SET facts_json=?,health_json=?,config_applied=?,
		last_seen=?,state=CASE WHEN state='provisioning' THEN state ELSE 'online' END
		WHERE serial=?`, facts, health, in.ConfigApplied, now(), serial)
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	// The measurements that move are kept as a thin history, so a slot can be
	// looked at over time rather than only right now.
	var hm map[string]any
	if err := json.Unmarshal([]byte(health), &hm); err == nil {
		a.recordSample(serial, hm)
	}

	// What the agent changed goes into the log. Without it, the only record of
	// a blade being reconfigured — or of the attempt failing — sits in the
	// journal of that blade, which is exactly the place you cannot reach when
	// the change that failed was the one that opens the door.
	for _, c := range in.Changes {
		lvl := "info"
		if strings.HasPrefix(c, "FAILED") {
			lvl = "warn"
		}
		a.logEvent(serial, lvl, "agent: "+c)
	}

	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// commandTTL bounds how long a command stays valid.
//
// Without that bound commands pile up until an agent finally appears — and
// then all fire at once. That is exactly what happened on the agent's first
// start: seven-hour-old test entries, four of them "reimage", triggered an
// immediate reboot. A command nobody expects any more must not run.
const commandTTL = 15 * time.Minute

func (a *App) hBladeCommands(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if err := a.requireBlade(r, serial); err != nil {
		fail(w, 401, "%v", err)
		return
	}

	// Clear expired commands before handing anything out.
	cutoff := time.Now().UTC().Add(-commandTTL).Format(time.RFC3339)
	if res, err := a.db.Exec(`UPDATE commands SET taken=? WHERE serial=? AND taken='' AND created < ?`,
		now()+" (abgelaufen)", serial, cutoff); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			a.logEvent(serial, "warn",
				fmt.Sprintf("%d command(s) expired — older than %s", n, commandTTL))
		}
	}

	rows, err := a.db.Query(`SELECT id,kind,args,created FROM commands
		WHERE serial=? AND taken='' ORDER BY id`, serial)
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	type cmd struct {
		ID      int64           `json:"id"`
		Kind    string          `json:"kind"`
		Args    json.RawMessage `json:"args"`
		Created string          `json:"created"`
	}
	out := []cmd{}
	var ids []int64
	for rows.Next() {
		var c cmd
		var args string
		if err := rows.Scan(&c.ID, &c.Kind, &args, &c.Created); err != nil {
			continue
		}
		c.Args = json.RawMessage(args)
		out = append(out, c)
		ids = append(ids, c.ID)
	}
	for _, id := range ids {
		_, _ = a.db.Exec(`UPDATE commands SET taken=? WHERE id=?`, now(), id)
	}
	writeJSON(w, 200, out)
}

// ── Provisioning (called by the mini-OS) ─────────────────────────────

// hProvision is called by the mini-OS. No image chosen yet is not an error:
// the installer waits, and the interface shows the blade as waiting for a
// choice. That is exactly where the choice is made.
func (a *App) hProvision(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	var in struct {
		MAC string `json:"mac"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	b, err := a.getBlade(serial)
	if err != nil {
		// An unknown blade may enrol itself while provisioning.
		short := serial
		if len(short) > 8 {
			short = short[len(short)-8:]
		}
		tok := newToken()
		if _, e := a.db.Exec(`INSERT INTO blades(serial,short_serial,state,token,created)
			VALUES(?,?,'new',?,?)`, serial, short, tok, now()); e != nil {
			fail(w, 500, "%v", e)
			return
		}
		a.logEvent(serial, "info", "unknown blade seen during netboot")
		b, _ = a.getBlade(serial)
	}

	// Link the session to the serial number and adopt an image choice already
	// made there.
	if in.MAC != "" {
		img := a.linkNetboot(serial, in.MAC)
		if b.MAC == "" {
			_, _ = a.db.Exec(`UPDATE blades SET mac=? WHERE serial=?`, in.MAC, serial)
		}
		if img != "" && b.Image == "" {
			_, _ = a.db.Exec(`UPDATE blades SET image=? WHERE serial=?`, img, serial)
		}
		b, _ = a.getBlade(serial)
	}

	if b.Image == "" {
		// 200, not 409: waiting is a regular state of the process, not an
		// error. The installer asks again in its own time.
		writeJSON(w, 200, map[string]any{
			"status":      "waiting",
			"serial":      serial,
			"retry_after": 5,
			"message":     "no image chosen yet — pick one in the interface",
		})
		return
	}

	// An assigned image does NOT mean "write it now". Otherwise a blade whose
	// BOOT_ORDER puts the network ahead of the NVMe would reinstall itself on
	// every start — and an accidental netboot would overwrite a running
	// system.
	if b.InstallState != installPending {
		writeJSON(w, 200, map[string]any{
			"status":      "idle",
			"serial":      serial,
			"image":       b.Image,
			"retry_after": 30,
			"message": "No install requested. This blade should boot locally — " +
				"is BOOT_ORDER set to 0xf26 (NVMe before network)?",
		})
		return
	}
	var img Image
	err = a.db.QueryRow(`SELECT id,url,sha256,seed,os_id,local,bytes FROM images WHERE id=?`, b.Image).
		Scan(&img.ID, &img.URL, &img.SHA256, &img.Seed, &img.OSID, &img.Local, &img.Bytes)
	if err != nil {
		fail(w, 404, "image %q is not in the catalogue", b.Image)
		return
	}
	url := img.URL
	if img.Local != "" {
		url = a.baseURL + "/images/" + img.Local
	}
	var tok string
	_ = a.db.QueryRow(`SELECT token FROM blades WHERE serial=?`, serial).Scan(&tok)

	// Without stored keys a freshly installed blade is a black box: Ubuntu
	// sets ssh_pwauth to false, so there is no way in. The keys come from the
	// merged configuration and are placed by the installer during seeding.
	cfg, _ := a.mergedConfig(b)
	keys := stringList(cfg["ssh_authorized_keys"])

	_, _ = a.db.Exec(`UPDATE blades SET state='provisioning' WHERE serial=?`, serial)
	// A hard reset rather than raise: a second run must be allowed back from
	// "done" to "writing", or the interface keeps showing "done" while it is
	// actually writing.
	a.resetStage(serial, stageWriting, "schreibt "+img.ID)
	a.logEvent(serial, "info", "provisioning requested: "+img.ID)

	writeJSON(w, 200, map[string]any{
		"status":     "go",
		"serial":     serial,
		"image":      img.ID,
		"url":        url,
		"sha256":     img.SHA256,
		"seed":       img.Seed,
		"target":     a.setting("install_target", "/dev/nvme0n1"),
		"server_url": a.baseURL,
		"token":      tok,
		"hostname":   b.Hostname,
		"ssh_keys":   keys,
	})
}

// stringList pulls a list of strings out of the configuration and accepts a
// single value too — both occur in practice.
func stringList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		if strings.TrimSpace(t) != "" {
			return []string{strings.TrimSpace(t)}
		}
	}
	return []string{}
}

func (a *App) hProvisionStatus(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	var in struct {
		Phase   string `json:"phase"`
		Percent int    `json:"percent"`
		Error   string `json:"error"`
		Note    string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	lvl := "info"
	msg := fmt.Sprintf("provisioning: %s %d%%", in.Phase, in.Percent)

	// A note is a sentence the installer wants in the log — whether the agent
	// was seeded, for instance. It never counts as progress, so it neither
	// touches the netboot session nor goes through the milestone filter.
	if in.Note != "" && in.Error == "" {
		lvl = "info"
		if strings.Contains(in.Note, "NOT ") || strings.Contains(in.Note, "not placed") {
			lvl = "warn"
		}
		a.logEvent(serial, lvl, "installer: "+in.Note)
		writeJSON(w, 200, map[string]string{"ok": "1"})
		return
	}
	if in.Error != "" {
		lvl, msg = "error", "provisioning failed: "+in.Error
		// On failure the netboot stays armed so a restart retries instead of
		// booting from a half-written disk.
		_, _ = a.db.Exec(`UPDATE blades SET state='error' WHERE serial=?`, serial)
		a.netbootStage(serial, stageError, in.Error)
	} else if in.Phase == "done" {
		// The intent is spent: the next netboot triggers nothing.
		_, _ = a.db.Exec(`UPDATE blades SET state='enrolled',install_state=?,installed_at=?
			WHERE serial=?`, installDone, now(), serial)
		// Netboot switch off again: the next start goes to the NVMe.
		_, _ = a.syncDHCP()
		a.netbootStage(serial, stageDone, "written")
	} else {
		a.netbootStage(serial, stageWriting, msg)
	}
	// The installer reports every five seconds. Live progress belongs in the
	// netboot session, which is overwritten each time; the log keeps the
	// milestones. Otherwise one installation buries an hour of real events
	// under two hundred lines of "writing 99%".
	if in.Error != "" || in.Phase == "done" || worthLogging(serial, in.Phase, in.Percent) {
		a.logEvent(serial, lvl, msg)
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// worthLogging keeps the first report of a phase and then every 25 %. It
// remembers what it last wrote per blade, which is why it holds a lock rather
// than reading it back out of the events table on every call.
func worthLogging(serial, phase string, percent int) bool {
	step := percent / 25
	key := serial + "|" + phase

	logMarks.mu.Lock()
	defer logMarks.mu.Unlock()
	// Equality, not "less than or equal": a second installation starts at 0 %
	// again, and that run must not be swallowed because the previous one had
	// already reached 100 %.
	if last, seen := logMarks.m[key]; seen && step == last {
		return false
	}
	logMarks.m[key] = step
	return true
}

var logMarks = struct {
	mu sync.Mutex
	m  map[string]int
}{m: map[string]int{}}

// ── Images ───────────────────────────────────────────────────────────

func (a *App) hImagesList(w http.ResponseWriter, r *http.Request) {
	imgs, err := a.listImages()
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, imgs)
}

func (a *App) hImagesCreate(w http.ResponseWriter, r *http.Request) {
	var i Image
	if err := json.NewDecoder(r.Body).Decode(&i); err != nil || i.ID == "" {
		fail(w, 400, "id missing or JSON invalid")
		return
	}
	if i.Seed == "" {
		i.Seed = "generic"
	}
	_, err := a.db.Exec(`INSERT INTO images(id,url,sha256,seed,os_id,notes,local,bytes,created)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET url=excluded.url,sha256=excluded.sha256,
		  seed=excluded.seed,os_id=excluded.os_id,notes=excluded.notes,
		  local=excluded.local,bytes=excluded.bytes`,
		i.ID, i.URL, i.SHA256, i.Seed, i.OSID, i.Notes, i.Local, i.Bytes, now())
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	writeJSON(w, 201, i)
}

// ── Configuration (global → group → blade) ───────────────────────────

func (a *App) configFor(scope string) map[string]any {
	var body string
	if err := a.db.QueryRow(`SELECT body FROM configs WHERE scope=?`, scope).Scan(&body); err != nil {
		return map[string]any{}
	}
	m := map[string]any{}
	_ = json.Unmarshal([]byte(body), &m)
	return m
}

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok2 := dst[k].(map[string]any); ok2 {
				mergeInto(dm, sm)
				continue
			}
			cp := map[string]any{}
			mergeInto(cp, sm)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

func (a *App) mergedConfig(b *Blade) (map[string]any, string) {
	out := map[string]any{}
	mergeInto(out, a.configFor("global"))
	groups := append([]string{}, b.Groups...)
	sort.Strings(groups)
	for _, g := range groups {
		mergeInto(out, a.configFor("group:"+g))
	}
	mergeInto(out, a.configFor("blade:"+b.Serial))

	// Values derived from the position always win.
	if b.Hostname != "" {
		out["hostname"] = b.Hostname
	}
	if b.IP != "" {
		out["expected_ip"] = b.IP
	}
	raw, _ := json.Marshal(out)
	sum := sha256.Sum256(raw)
	return out, "sha256:" + hex.EncodeToString(sum[:])[:16]
}

func (a *App) hConfigGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.configFor(r.PathValue("scope")))
}

func (a *App) hConfigPut(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, "invalid JSON: %v", err)
		return
	}
	raw, _ := json.Marshal(body)
	_, err := a.db.Exec(`INSERT INTO configs(scope,body,updated) VALUES(?,?,?)
		ON CONFLICT(scope) DO UPDATE SET body=excluded.body,updated=excluded.updated`,
		scope, string(raw), now())
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	a.logEvent("", "info", "configuration changed: "+scope)
	writeJSON(w, 200, body)
}

// ── Operations ───────────────────────────────────────────────────────

func (a *App) hDHCPSync(w http.ResponseWriter, r *http.Request) {
	res, err := a.syncDHCP()
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, res)
}

func (a *App) hNetbootList(w http.ResponseWriter, r *http.Request) {
	list, err := a.listNetboot(LangEN)
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, list)
}

func (a *App) hNetbootImage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "invalid JSON: %v", err)
		return
	}
	if err := a.chooseImage(r.PathValue("mac"), in.Image); err != nil {
		fail(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]string{"mac": r.PathValue("mac"), "image": in.Image})
}

func (a *App) hHealth(w http.ResponseWriter, r *http.Request) {
	blades, _ := a.listBlades()
	racks, _ := a.listRacks()
	counts := map[string]int{}
	for _, b := range blades {
		counts[b.State]++
	}
	writeJSON(w, 200, map[string]any{
		"racks":     len(racks),
		"blades":    len(blades),
		"by_state":  counts,
		"net_warns": a.checkNet(LangEN),
		"net_base":  a.netBase(), // the local site, kept for compatibility
		"sites":     a.siteNets(),
	})
}

func (a *App) hEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT ts,serial,level,msg FROM events ORDER BY id DESC LIMIT 200`)
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	type ev struct{ TS, Serial, Level, Msg string }
	out := []ev{}
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.TS, &e.Serial, &e.Level, &e.Msg); err == nil {
			out = append(out, e)
		}
	}
	writeJSON(w, 200, out)
}
