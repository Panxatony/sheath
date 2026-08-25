package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Views ────────────────────────────────────────────────────────────

type slotView struct {
	Slot     int
	Empty    bool
	Serial   string
	Hostname string
	IP       string
	MAC      string
	Image    string
	State    string
	LED      string
	Ago      string
	Health   string // short summary of what stands out
	HLED     string
	Install  string // key: inst.idle | inst.pending | inst.done | inst.error
	InstLED  string

	// Columns of the slot view
	Distro string
	Role   string
	Status string // translated text
	SLED   string // colour of the status chip
	Soc    string
	Fan    string

	// Stage 1: what the compute-blade-agent knows about this piece of
	// hardware. Empty strings mean "not reported", which is different from
	// zero and is rendered as a dash.
	Airflow string
	FanPct  string
	FanUnit string
	Module  string
	BladeSt string
	Stealth string
	Buttons string
	// What the blade's LEDs are doing right now, so the overlay can offer the
	// other direction rather than a button that repeats the current state.
	Identifying bool
	Stealthy    bool

	SparkSoc template.HTML
	SparkFan template.HTML
	SparkNum int
}

type rackView struct {
	Rack     Rack
	SiteName string
	SiteNet  string
	From     string
	To       string
	Slots    []slotView
	Cells    []slotCell // compact occupancy strip for the overview
	Therm    thermView  // stage 2: what the enclosure as a whole is doing
	Used     int
	Free     int
	Percent  int
}

// slotCell is one slot at a glance: number, colour, and the essentials in
// the title for the pointer. Four states are enough — a coloured square
// cannot carry more than that anyway.
type slotCell struct {
	Slot  int
	Class string // free | ok | busy | bad
	Title string
}

func ledFor(state string) string {
	switch state {
	case "online":
		return "ok"
	case "critical", "error":
		return "crit"
	case "provisioning", "new":
		return "id"
	case "enrolled":
		return "warn"
	}
	return "off"
}

// ago phrases the distance to a point in time. The wording follows the
// language so "never" does not sit in the middle of a German page.
func ago(l Lang, ts string) string {
	if ts == "" {
		if l == LangDE {
			return "nie"
		}
		return "never"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		if l == LangDE {
			return "gerade eben"
		}
		return "just now"
	case d < time.Hour:
		return d.Round(time.Minute).String()
	case d < 48*time.Hour:
		return d.Round(time.Hour).String()
	default:
		if l == LangDE {
			return t.Format("02.01.2006")
		}
		return t.Format("2006-01-02")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func olderThan(ts string, d time.Duration) bool {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return time.Since(t) > d
}

func render(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// resolveLang determines the language and pins an explicit choice into the
// cookie so it survives navigation.
func (a *App) resolveLang(w http.ResponseWriter, r *http.Request) Lang {
	l := a.langOf(r)
	if q := Lang(r.URL.Query().Get("lang")); q.Valid() {
		setLangCookie(w, q)
	}
	return l
}

func redirectMsg(w http.ResponseWriter, r *http.Request, to, kind, msg string) {
	http.Redirect(w, r, to+"?"+kind+"="+template.URLQueryEscaper(msg), http.StatusSeeOther)
}

func flash(r *http.Request) (string, string) {
	return r.URL.Query().Get("msg"), r.URL.Query().Get("err")
}

// hLang switches the language and returns where you were.
func (a *App) hLang(w http.ResponseWriter, r *http.Request) {
	l := Lang(r.PathValue("code"))
	if !l.Valid() {
		l = LangEN
	}
	setLangCookie(w, l)
	next := r.URL.Query().Get("next")
	if next == "" || next[0] != '/' {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) buildRackView(rk Rack, blades []Blade, l Lang) rackView {
	rv := rackView{
		Rack:     rk,
		SiteName: a.siteName(rk.SiteID),
		SiteNet:  a.siteNetBase(rk.SiteID),
		From:     a.siteNetBase(rk.SiteID) + "." + itoa(rk.IPOffset+1),
		To:       a.siteNetBase(rk.SiteID) + "." + itoa(rk.IPOffset+rk.Size),
	}
	idx := int64(a.rackIndex(rk))
	inSlot := map[int]Blade{}
	for _, b := range blades {
		if b.RackID != nil && *b.RackID == rk.ID && b.Slot != nil {
			inSlot[*b.Slot] = b
		}
	}
	for s := 1; s <= rk.Size; s++ {
		b, ok := inSlot[s]
		if !ok {
			rv.Slots = append(rv.Slots, slotView{
				Slot: s, Empty: true, LED: "off",
				IP:  a.bladeIP(rk, s),
				MAC: bladeMAC(idx, s),
			})
			continue
		}
		rv.Used++
		lvl, reasons := a.evalHealth(&b)
		h := healthMap(&b)
		statusKey, statusLED, statusArg := a.rowStatus(&b, lvl)
		sv := slotView{
			Slot: s, Serial: b.Serial, Hostname: b.Hostname, IP: b.IP, MAC: b.MAC,
			Image: b.Image, State: b.State, Ago: ago(l, b.LastSeen),
			Install: T(l, "inst."+installOr(b.InstallState)),
			InstLED: instLED(b.InstallState),
			Health:  joinErr(l, reasons),
			HLED:    lvl.chip(),
			Distro:  distroText(&b),
			Role:    roleText(l, &b),
			Soc:     tempValue(l, h, "soc_temp_c"),
			Fan:     fanText(l, h),
			SLED:    statusLED,

			Airflow:     hwText(h, "airflow_temp_c", "°C"),
			FanPct:      hwText(h, "fan_percent", "%"),
			FanUnit:     hwString(h, "fan_unit"),
			Module:      hwString(h, "module"),
			BladeSt:     hwString(h, "blade_state"),
			Stealth:     onOff(l, h["stealth"]),
			Identifying: hwString(h, "blade_state") == "identify",
			Stealthy:    truthy(h["stealth"]),
			Buttons:     hwText(h, "button_events", ""),
		}
		// The curve of the last two days, drawn from the stored samples.
		if hist, err := a.bladeSamples(b.Serial, a.globalPolicy().sampleKeep()); err == nil && len(hist) > 1 {
			var socs, rpms []float64
			for _, sm := range hist {
				if sm.Soc >= 0 {
					socs = append(socs, sm.Soc)
				}
				if sm.RPM >= 0 {
					rpms = append(rpms, sm.RPM)
				}
			}
			sv.SparkSoc = sparkline(socs, "soc")
			sv.SparkFan = sparkline(rpms, "fan")
			sv.SparkNum = len(hist)
		}
		if statusArg != "" {
			sv.Status = T(l, statusKey, statusArg)
		} else {
			sv.Status = T(l, statusKey)
		}
		// The LED follows health, not the administrative state: a blade can
		// be "online" and still be running too hot.
		sv.LED = lvl.chip()
		if lvl == hUnknown {
			sv.LED = ledFor(b.State)
		}
		// Identify wins over all of it. Someone asked this blade to make
		// itself findable, and the screen should agree with the light they
		// are walking towards — a healthy green says nothing about which of
		// twenty blades is blinking.
		if sv.Identifying {
			sv.LED = "ident"
		}
		rv.Slots = append(rv.Slots, sv)
	}
	for _, sv := range rv.Slots {
		rv.Cells = append(rv.Cells, cellFor(l, sv))
	}
	rv.Free = rk.Size - rv.Used
	if rk.Size > 0 {
		rv.Percent = rv.Used * 100 / rk.Size
	}
	byBlade := map[string]*Blade{}
	for i := range blades {
		byBlade[blades[i].Serial] = &blades[i]
	}
	rv.Therm = buildTherm(l, rv.Slots, byBlade)
	return rv
}

// ── Overview ─────────────────────────────────────────────────────────

type nbView struct {
	SiteName string
	NetbootSession
	Label     string
	LED       string
	LeaseOnly bool
}

func (a *App) hUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	l := a.resolveLang(w, r)
	racks, err := a.listRacks()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	blades, _ := a.listBlades()

	var views []rackView
	for _, rk := range racks {
		views = append(views, a.buildRackView(rk, blades, l))
	}
	var unassigned []slotView
	for _, b := range blades {
		if b.RackID == nil || b.Slot == nil {
			unassigned = append(unassigned, slotView{
				Serial: b.Serial, Hostname: b.Hostname, MAC: b.MAC,
				State: b.State, LED: ledFor(b.State), Ago: ago(l, b.LastSeen),
			})
		}
	}

	sessions, _ := a.listNetboot(l)
	images, _ := a.listImages()
	var nb []nbView
	active := false
	for _, sn := range sessions {
		// "On the network right now" should mean right now. Something that
		// finished half an hour ago belongs in the event list, not in a panel
		// that claims attention.
		if (sn.Stage == stageDone || sn.Stage == stageError) && olderThan(sn.LastSeen, 10*time.Minute) {
			continue
		}
		v := nbView{NetbootSession: sn, Label: T(l, "stage."+sn.Stage), LED: stageLED(sn.Stage)}
		if sn.SiteID != 0 {
			v.SiteName = a.siteName(sn.SiteID)
		}
		// A device that only took an address, and whose request did not come
		// from the RPi bootloader, never wanted to netboot. That is a
		// different thing from a failed netboot and must be named
		// differently, or you look for the fault in the wrong place.
		if sn.Stage == stageDHCP && sn.Files == 0 && sn.Client != "netboot" {
			v.LeaseOnly = true
			v.Label = T(l, "stage.leaseonly")
			v.LED = "off"
		}
		nb = append(nb, v)
		if !v.LeaseOnly && sn.Stage != stageDone && sn.Stage != stageError {
			active = true
		}
	}

	msg, errMsg := flash(r)
	poolFrom, poolTo := 210, 240
	if st, err := a.localSite(); err == nil {
		poolFrom, poolTo = st.PoolFrom, st.PoolTo
	}
	var nextOff int
	var offErr error
	if st, err := a.localSite(); err == nil {
		nextOff, offErr = a.nextRackOffset(st.ID)
	} else {
		offErr = err
	}

	render(w, overviewTmpl, map[string]any{
		"L":          l,
		"Path":       "/",
		"Netboot":    nb,
		"Images":     images,
		"Refresh":    active,
		"Racks":      views,
		"Sites":      a.groupBySite(views, blades, l),
		"SiteCounts": a.rackCounts(),
		"Unassigned": unassigned,
		"Blades":     len(blades),
		"NetBase":    a.netBase(),
		"PoolFrom":   poolFrom,
		"PoolTo":     poolTo,
		"Warnings":   a.checkNet(l),
		"Msg":        msg,
		"Err":        errMsg,
		"NextFrom":   a.netBase() + "." + itoa(nextOff+1),
		"NoSpace":    offErr != nil,
		"Open":       a.adminToken == "",
	})
}

// siteGroup is one site with the BladeRunners standing in it. The overview is
// grouped rather than flat because an address only means something inside its
// network: two blades called .103 in two sites are two different machines.
type siteGroup struct {
	Site   Site
	Local  bool
	Net    string
	Pool   string
	State  string // translated: online, stale, offline, or no site process
	SLED   string // colour of the chip
	Seen   string // how long ago it last spoke
	Stock  string // what it holds of the images its blades need
	SkLED  string
	Racks  []rackView
	Blades int
	Used   int
	Free   int
}

// siteChoices is the list a form offers. Kept small on purpose: a select
// needs a name and an id, not a whole site.
type siteChoice struct {
	ID   int64
	Name string
	Net  string
}

func (a *App) siteChoices() []siteChoice {
	sites, err := a.listSites()
	if err != nil {
		return nil
	}
	out := make([]siteChoice, 0, len(sites))
	for _, st := range sites {
		out = append(out, siteChoice{ID: st.ID, Name: st.Name, Net: st.NetBase + ".0/24"})
	}
	return out
}

// siteHealth judges a site by when it last spoke. A site that has never been
// given a token has no process of its own — that is a state, not a fault, and
// saying "offline" about it would be wrong.
func siteHealth(l Lang, st Site) (key, led, seen string) {
	if st.Token == "" {
		return "site.noagent", "off", ""
	}
	if st.LastSeen == "" {
		return "site.never", "warn", ""
	}
	t, err := time.Parse(time.RFC3339, st.LastSeen)
	if err != nil {
		return "site.never", "warn", ""
	}
	seen = ago(l, st.LastSeen)
	switch d := time.Since(t); {
	case d < 3*time.Minute:
		return "site.online", "ok", seen
	case d < 15*time.Minute:
		return "site.stale", "warn", seen
	default:
		return "site.offline", "crit", seen
	}
}

// stockText condenses a site's image stock into one line. Ready is the quiet
// case and says only a number; anything else names itself, because "one image
// is still being fetched" is the sentence that explains why an installation
// has not started.
func stockText(l Lang, in []SiteImageState) (string, string) {
	if len(in) == 0 {
		return "—", "off"
	}
	ready, fetching, bad := 0, 0, 0
	for _, im := range in {
		switch im.State {
		case "ready":
			ready++
		case "fetching":
			fetching++
		default:
			bad++
		}
	}
	switch {
	case bad > 0:
		return T(l, "stock.bad", ready, len(in), bad), "crit"
	case fetching > 0:
		return T(l, "stock.fetching", ready, len(in), fetching), "warn"
	default:
		return T(l, "stock.ready", ready), "ok"
	}
}

func (a *App) groupBySite(views []rackView, blades []Blade, l Lang) []siteGroup {
	stock := a.siteImages()
	sites, err := a.listSites()
	if err != nil {
		return nil
	}
	local := int64(0)
	if st, lerr := a.localSite(); lerr == nil {
		local = st.ID
	}
	byID := map[int64]*siteGroup{}
	out := make([]siteGroup, 0, len(sites))
	for _, st := range sites {
		key, led, seen := siteHealth(l, st)
		sk, skLED := stockText(l, stock[st.ID])
		out = append(out, siteGroup{
			Site:  st,
			Local: st.ID == local,
			Net:   st.NetBase + ".0/24",
			Pool:  fmt.Sprintf(".%d–.%d", st.PoolFrom, st.PoolTo),
			State: T(l, key),
			SLED:  led,
			Seen:  seen,
			Stock: sk,
			SkLED: skLED,
		})
	}
	for i := range out {
		byID[out[i].Site.ID] = &out[i]
	}
	for _, rv := range views {
		g, ok := byID[rv.Rack.SiteID]
		if !ok {
			continue
		}
		g.Racks = append(g.Racks, rv)
		g.Used += rv.Used
		g.Free += rv.Free
	}
	for _, b := range blades {
		if g, ok := byID[b.SiteID]; ok && b.RackID != nil {
			g.Blades++
		}
	}
	return out
}

// ── Sites (UI) ───────────────────────────────────────────────────────

func (a *App) hUISiteCreate(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, "/", "err", T(l, "err.form"))
		return
	}
	st := Site{
		Name:       strings.TrimSpace(r.FormValue("name")),
		Location:   strings.TrimSpace(r.FormValue("location")),
		NetBase:    strings.TrimSpace(r.FormValue("net_base")),
		PoolFrom:   210,
		PoolTo:     240,
		OffsetBase: 100,
		OffsetStep: 20,
	}
	if _, err := a.createSite(st); err != nil {
		redirectMsg(w, r, "/", "err", errText(l, err))
		return
	}
	a.logEvent("", "info", "site "+st.Name+" created ("+st.NetBase+".0/24)")
	redirectMsg(w, r, "/", "msg", T(l, "msg.sitecreated", st.Name, st.NetBase))
}

func (a *App) hUISiteUpdate(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, "/", "err", T(l, "err.form"))
		return
	}
	old, err := a.getSite(id)
	if err != nil {
		redirectMsg(w, r, "/", "err", errText(l, err))
		return
	}
	st := *old
	st.Name = strings.TrimSpace(r.FormValue("name"))
	st.Location = strings.TrimSpace(r.FormValue("location"))
	st.NetBase = strings.TrimSpace(r.FormValue("net_base"))
	if v, err := strconv.Atoi(r.FormValue("pool_from")); err == nil {
		st.PoolFrom = v
	}
	if v, err := strconv.Atoi(r.FormValue("pool_to")); err == nil {
		st.PoolTo = v
	}
	if err := a.updateSite(id, st); err != nil {
		redirectMsg(w, r, "/", "err", errText(l, err))
		return
	}
	// Addresses are derived from the site, so a moved network moves every
	// blade standing in it — the reservations must be rewritten at once.
	note := T(l, "msg.sitesaved", st.Name)
	if old.NetBase != st.NetBase {
		if res, serr := a.syncDHCP(); serr != nil {
			note += " — " + errText(l, serr)
		} else {
			note += " — " + T(l, "msg.dhcprewritten", len(res.Written))
		}
	}
	a.logEvent("", "info", "site changed: "+st.Name)
	redirectMsg(w, r, "/", "msg", note)
}

func (a *App) hUISiteDelete(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.deleteSite(id); err != nil {
		redirectMsg(w, r, "/", "err", errText(l, err))
		return
	}
	a.logEvent("", "warn", "site removed")
	redirectMsg(w, r, "/", "msg", T(l, "msg.siteremoved"))
}

// ── BladeRunner detail ───────────────────────────────────────────────

func (a *App) hRackPage(w http.ResponseWriter, r *http.Request) {
	l := a.resolveLang(w, r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rk, err := a.getRack(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	blades, _ := a.listBlades()
	rv := a.buildRackView(*rk, blades, l)

	var free []slotView
	for _, b := range blades {
		if b.RackID == nil || b.Slot == nil {
			free = append(free, slotView{Serial: b.Serial, State: b.State, MAC: b.MAC})
		}
	}
	images, _ := a.listImages()
	msg, errMsg := flash(r)

	events, lerr := a.rackEvents(id, 60)
	if lerr != nil {
		log.Printf("activity log for BladeRunner %d: %v", id, lerr)
	}

	render(w, rackTmpl, map[string]any{
		"L":         l,
		"Path":      "/bladerunners/" + strconv.FormatInt(id, 10),
		"R":         rv,
		"Free":      free,
		"Images":    images,
		"Msg":       msg,
		"Err":       errMsg,
		"Sites":     a.siteChoices(),
		"Log":       buildLog(events, l),
		"CanDelete": rv.Used == 0,
		"Open":      a.adminToken == "",
	})
}

// ── Actions: always POST, then redirect ──────────────────────────────

func (a *App) hUIRackCreate(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, "/", "err", T(l, "err.form"))
		return
	}
	name := r.FormValue("name")
	size, _ := strconv.Atoi(r.FormValue("size"))
	loc := r.FormValue("location")

	if name == "" {
		redirectMsg(w, r, "/", "err", T(l, "err.nameneeded"))
		return
	}
	if !validSize(size) {
		redirectMsg(w, r, "/", "err", T(l, "err.badsize", size))
		return
	}
	// A site may be chosen; without one the local site is meant.
	siteID, _ := strconv.ParseInt(r.FormValue("site"), 10, 64)
	if siteID == 0 {
		st, err := a.localSite()
		if err != nil {
			redirectMsg(w, r, "/", "err", "no site present")
			return
		}
		siteID = st.ID
	}
	net := a.siteNetBase(siteID)
	off, err := a.nextRackOffset(siteID)
	if err != nil {
		redirectMsg(w, r, "/", "err", errText(l, err))
		return
	}
	res, err := a.db.Exec(
		`INSERT INTO racks(site_id,name,size,ip_offset,location,created) VALUES(?,?,?,?,?,?)`,
		siteID, name, size, off, loc, now())
	if err != nil {
		redirectMsg(w, r, "/", "err", T(l, "err.rackexists"))
		return
	}
	id, _ := res.LastInsertId()
	a.logEvent("", "info", "Rack \""+name+"\" created")
	redirectMsg(w, r, "/bladerunners/"+strconv.FormatInt(id, 10), "msg",
		T(l, "msg.rackcreated", name,
			net+"."+itoa(off+1), net+"."+itoa(off+size)))
}

func (a *App) hUIRackUpdate(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	to := "/bladerunners/" + strconv.FormatInt(id, 10)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, to, "err", T(l, "err.form"))
		return
	}
	size, _ := strconv.Atoi(r.FormValue("size"))
	if err := a.updateRack(id, r.FormValue("name"), r.FormValue("location"), size); err != nil {
		redirectMsg(w, r, to, "err", errText(l, err))
		return
	}
	// Moving is part of the same form but a different kind of change: every
	// address in this BladeRunner is derived from the site, so all of them
	// move with it. Say so afterwards rather than let it happen quietly.
	moved := ""
	if sid, err := strconv.ParseInt(r.FormValue("site"), 10, 64); err == nil && sid > 0 {
		rk, _ := a.getRack(id)
		if rk != nil && rk.SiteID != sid {
			if err := a.moveRack(id, sid); err != nil {
				redirectMsg(w, r, to, "err", errText(l, err))
				return
			}
			nk, _ := a.getRack(id)
			if nk != nil {
				net := a.siteNetBase(sid)
				moved = T(l, "site.moved", nk.Name, a.siteName(sid),
					net+"."+itoa(nk.IPOffset+1), net+"."+itoa(nk.IPOffset+nk.Size))
			}
		}
	}
	if _, err := a.syncDHCP(); err != nil {
		redirectMsg(w, r, to, "err", T(l, "err.dhcpsync", errText(l, err)))
		return
	}
	note := T(l, "msg.saved")
	if moved != "" {
		note = moved
	}
	redirectMsg(w, r, to, "msg", note)
}

func (a *App) hUIRackDelete(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rk, err := a.getRack(id)
	if err != nil {
		redirectMsg(w, r, "/", "err", T(l, "err.rackgone"))
		return
	}
	if n := a.countBladesInRack(id); n > 0 {
		redirectMsg(w, r, "/bladerunners/"+strconv.FormatInt(id, 10), "err", T(l, "err.stillinrack", n))
		return
	}
	if _, err := a.db.Exec(`DELETE FROM racks WHERE id=?`, id); err != nil {
		redirectMsg(w, r, "/", "err", T(l, "err.deletefail", err.Error()))
		return
	}
	_, _ = a.syncDHCP()
	a.logEvent("", "warn", "Rack \""+rk.Name+"\" deleted")
	redirectMsg(w, r, "/", "msg", T(l, "msg.deleted", rk.Name))
}

func (a *App) hUIAssign(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	slot, _ := strconv.Atoi(r.PathValue("slot"))
	to := "/bladerunners/" + strconv.FormatInt(id, 10)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, to, "err", T(l, "err.form"))
		return
	}
	serial := r.FormValue("serial")
	if serial == "" {
		redirectMsg(w, r, to, "err", T(l, "err.noblade"))
		return
	}
	if err := a.placeBlade(serial, &id, &slot); err != nil {
		redirectMsg(w, r, to, "err", errText(l, err))
		return
	}
	sync, err := a.syncDHCP()
	if err != nil {
		redirectMsg(w, r, to, "err", T(l, "err.dhcpinsert", errText(l, err)))
		return
	}
	b, _ := a.getBlade(serial)
	note := T(l, "msg.slotset", slot, b.Hostname, b.IP)
	if sync.Warning != "" {
		note += " (" + sync.Warning + ")"
	}
	a.logEvent(serial, "info", "placed in slot "+itoa(slot))
	redirectMsg(w, r, to, "msg", note)
}

// backTo works out where an action should return the reader to. A blade that
// sits in a BladeRunner returns to that page; a blade without a place would
// otherwise drop the reader onto the overview even though they were standing
// on a BladeRunner page. So the referring page is used as the next-best
// answer, and only a request from elsewhere ends up on the overview.
// logView is one line of the BladeRunner activity log.
type logView struct {
	When  string
	Ago   string
	Slot  string
	Name  string
	Level string
	LED   string
	Msg   string
}

// buildLog turns the stored events into something readable: the slot instead
// of the serial number, the local time instead of the stored UTC string, and
// a colour for the severity.
func buildLog(rows []EventRow, l Lang) []logView {
	out := make([]logView, 0, len(rows))
	for _, e := range rows {
		lv := logView{Name: e.Hostname, Msg: e.Msg, Level: e.Level, LED: "ok", When: e.TS}
		switch e.Level {
		case "warn":
			lv.LED = "warn"
		case "error":
			lv.LED = "crit"
		}
		if e.Slot != nil {
			lv.Slot = fmt.Sprintf("%02d", *e.Slot)
		}
		if t, err := time.Parse(time.RFC3339, e.TS); err == nil {
			lv.When = t.Local().Format("2006-01-02 15:04:05")
			lv.Ago = ago(l, e.TS)
		}
		out = append(out, lv)
	}
	return out
}

// thermView is the enclosure seen as one thing. A BladeRunner shares its air,
// so the spread across the slots says more than any single reading: two
// blades at 45 °C and one at 78 °C is a different machine room from three at
// 56 °C, and the average hides exactly that.
type thermView struct {
	Have    bool
	Hot     string // hottest slot, as "07"
	HotTemp string
	SocLow  string
	SocHigh string
	RPMLow  string
	RPMHigh string
	Smart   int
	Fans    int
}

func buildTherm(l Lang, slots []slotView, blades map[string]*Blade) thermView {
	var tv thermView
	var socs, rpms []float64
	for _, sv := range slots {
		if sv.Empty {
			continue
		}
		b := blades[sv.Serial]
		if b == nil {
			continue
		}
		h := healthMap(b)
		if v, ok := num(h["soc_temp_c"]); ok {
			socs = append(socs, v)
			if !tv.Have || v > parseTemp(tv.HotTemp) {
				tv.Hot = fmt.Sprintf("%02d", sv.Slot)
				tv.HotTemp = fmt.Sprintf("%.0f", v)
			}
			tv.Have = true
		}
		if v, ok := num(h["fan_rpm"]); ok && v > 0 {
			rpms = append(rpms, v)
		}
		if u, _ := h["fan_unit"].(string); u != "" {
			tv.Fans++
			if u == "smart" {
				tv.Smart++
			}
		}
	}
	if len(socs) > 0 {
		lo, hi := minMax(socs)
		tv.SocLow, tv.SocHigh = fmt.Sprintf("%.0f", lo), fmt.Sprintf("%.0f", hi)
	}
	if len(rpms) > 0 {
		lo, hi := minMax(rpms)
		tv.RPMLow, tv.RPMHigh = fmt.Sprintf("%.0f", lo), fmt.Sprintf("%.0f", hi)
	}
	return tv
}

func minMax(v []float64) (float64, float64) {
	lo, hi := v[0], v[0]
	for _, x := range v[1:] {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	return lo, hi
}

func parseTemp(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(s, "%f", &f)
	return f
}

// sparkline draws a series as a bare SVG polyline. No library, no script: a
// path is a string, and the shape of the last two days is all this has to
// carry. A flat line at the bottom would be a lie about a missing series, so
// too few points draw nothing at all.
func sparkline(values []float64, class string) template.HTML {
	const w, h = 132.0, 26.0
	if len(values) < 2 {
		return ""
	}
	lo, hi := minMax(values)
	span := hi - lo
	if span < 0.5 {
		// A perfectly flat series still deserves a line, drawn in the middle
		// rather than jammed against an edge.
		span = 1
		lo = lo - 0.5
	}
	var b strings.Builder
	step := (w - 2) / float64(len(values)-1)
	for i, v := range values {
		x := 1 + float64(i)*step
		y := h - 1 - (v-lo)/span*(h-2)
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="spark %s" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" `+
			`role="img" aria-hidden="true"><polyline points="%s"/></svg>`,
		class, w, h, b.String()))
}

// truthy reads a reported flag the way both the agent and the metrics write
// it: as a bool from JSON, or as a 0/1 gauge.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	}
	return false
}

// onOff renders a reported flag. Anything not reported stays a dash rather
// than becoming a confident "off".
func onOff(l Lang, v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return T(l, "hw.on")
		}
		return T(l, "hw.off")
	case float64:
		if t != 0 {
			return T(l, "hw.on")
		}
		return T(l, "hw.off")
	}
	return "—"
}

// hwText renders one reported number, or a dash where nothing was reported.
func hwText(h map[string]any, key, unit string) string {
	v, ok := num(h[key])
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.0f %s", v, unit)
}

func hwString(h map[string]any, key string) string {
	if v, _ := h[key].(string); v != "" {
		return v
	}
	return "—"
}

func backTo(r *http.Request, fallback string) string {
	if n := r.FormValue("next"); strings.HasPrefix(n, "/") && !strings.HasPrefix(n, "//") {
		return n
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && (u.Host == "" || u.Host == r.Host) {
			if strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, "//") {
				return u.Path
			}
		}
	}
	return fallback
}

func bladePage(b *Blade, r *http.Request) string {
	if b != nil && b.RackID != nil {
		return "/bladerunners/" + strconv.FormatInt(*b.RackID, 10)
	}
	return backTo(r, "/")
}

func (a *App) hUIUnassign(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	serial := r.PathValue("serial")
	b, err := a.getBlade(serial)
	if err != nil {
		redirectMsg(w, r, backTo(r, "/"), "err", T(l, "err.bladegone"))
		return
	}
	to := bladePage(b, r)
	if err := a.placeBlade(serial, nil, nil); err != nil {
		redirectMsg(w, r, to, "err", errText(l, err))
		return
	}
	_, _ = a.syncDHCP()
	a.logEvent(serial, "warn", "removed from slot")
	redirectMsg(w, r, to, "msg", T(l, "msg.removed"))
}

func (a *App) hUIBladeImage(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	serial := r.PathValue("serial")
	b, err := a.getBlade(serial)
	if err != nil {
		redirectMsg(w, r, backTo(r, "/"), "err", T(l, "err.bladegone"))
		return
	}
	to := bladePage(b, r)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, to, "err", T(l, "err.form"))
		return
	}
	img := r.FormValue("image")
	if _, err := a.db.Exec(`UPDATE blades SET image=? WHERE serial=?`, img, serial); err != nil {
		redirectMsg(w, r, to, "err", err.Error())
		return
	}
	if img == "" {
		redirectMsg(w, r, to, "msg", T(l, "msg.imagecleared"))
		return
	}
	redirectMsg(w, r, to, "msg", T(l, "msg.imageset", img))
}

func (a *App) hUIBladeAction(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	serial, kind := r.PathValue("serial"), r.PathValue("kind")
	b, err := a.getBlade(serial)
	if err != nil {
		redirectMsg(w, r, backTo(r, "/"), "err", T(l, "err.bladegone"))
		return
	}
	to := bladePage(b, r)
	switch kind {
	case "identify", "identify_off", "stealth_on", "stealth_off", "reboot", "reimage":
	default:
		redirectMsg(w, r, to, "err", T(l, "err.unknownact"))
		return
	}
	if kind == "reimage" {
		if b.Image == "" {
			redirectMsg(w, r, to, "err", T(l, "err.needimage"))
			return
		}
		if err := a.requestInstall(serial); err != nil {
			redirectMsg(w, r, to, "err", errText(l, err))
			return
		}
		a.logEvent(serial, "info", "install requested")
		redirectMsg(w, r, to, "msg", T(l, "msg.installrequested", b.Image))
		return
	}
	if _, err := a.db.Exec(`INSERT INTO commands(serial,kind,created) VALUES(?,?,?)`,
		serial, kind, now()); err != nil {
		redirectMsg(w, r, to, "err", err.Error())
		return
	}
	a.logEvent(serial, "info", "command queued: "+kind)
	redirectMsg(w, r, to, "msg", T(l, "msg.queued", kind))
}

func (a *App) hUINetbootImage(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	mac := r.PathValue("mac")
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, "/", "err", T(l, "err.form"))
		return
	}
	img := r.FormValue("image")
	if img == "" {
		redirectMsg(w, r, "/", "err", T(l, "err.noimage"))
		return
	}
	if err := a.chooseImage(mac, img); err != nil {
		redirectMsg(w, r, "/", "err", errText(l, err))
		return
	}
	redirectMsg(w, r, "/", "msg", T(l, "msg.imagechosen", img, mac))
}

// cellFor turns a slot row into a cell. The mapping follows what you want to
// see on the rack: green is running, grey is empty, amber is doing something,
// red needs attention.
func cellFor(l Lang, sv slotView) slotCell {
	c := slotCell{Slot: sv.Slot}
	if sv.Identifying && !sv.Empty {
		c.Class = "ident"
		c.Title = fmt.Sprintf("%s %02d — %s · %s",
			T(l, "th.slot"), sv.Slot, sv.Hostname, T(l, "st.identify"))
		return c
	}
	if sv.Empty {
		c.Class = "free"
		c.Title = fmt.Sprintf("%s %02d — %s (%s)", T(l, "th.slot"), sv.Slot, T(l, "st.free"), sv.IP)
		return c
	}
	switch sv.SLED {
	case "crit":
		c.Class = "bad"
	case "id": // being provisioned or written
		c.Class = "busy"
	case "warn":
		c.Class = "busy"
	case "off":
		c.Class = "bad"
	default:
		c.Class = "ok"
	}
	c.Title = fmt.Sprintf("%s %02d — %s · %s · %s",
		T(l, "th.slot"), sv.Slot, sv.Hostname, sv.Status, sv.Soc)
	return c
}

// rowStatus decides what the status column says. The order is one of
// urgency: whatever is happening or broken displaces the quiet states.
func (a *App) rowStatus(b *Blade, lvl healthLevel) (key, led, arg string) {
	// Is an installation running? The netboot session knows.
	if pct, ok := a.writingPercent(b.Serial); ok {
		return "st.writing", "id", pct
	}
	switch {
	case b.State == "provisioning":
		return "st.provisioning", "id", ""
	case b.State == "offline":
		return "st.offline", "off", ""
	case lvl == hCrit:
		return "st.critical", "crit", ""
	case b.State == "new" || b.State == "enrolled":
		return "st.enrolled", "warn", ""
	case lvl == hWarn:
		return "st.warn", "warn", ""
	}
	// Identify is a state someone switched on and will want to switch off
	// again; it belongs in the status column, not only in the lamp.
	if h := healthMap(b); h != nil {
		if st, _ := h["blade_state"].(string); st == "identify" {
			return "st.identify", "ident", ""
		}
	}
	// Something was configured that only the firmware reads — the blade holds
	// the configuration but is not yet running it.
	if factsBool(b, "reboot_required") {
		return "st.rebootreq", "warn", ""
	}
	// Does the applied state match the desired one?
	if _, want := a.mergedConfig(b); want != b.ConfigApp {
		return "st.drift", "warn", ""
	}
	return "st.insync", "ok", ""
}

// writingPercent reads progress from the running netboot session.
func (a *App) writingPercent(serial string) (string, bool) {
	var stage, note string
	err := a.db.QueryRow(`SELECT stage,note FROM netboot WHERE serial=? ORDER BY last_seen DESC LIMIT 1`,
		serial).Scan(&stage, &note)
	if err != nil || stage != stageWriting {
		return "", false
	}
	// note looks like "provisioning: writing 45%"
	if i := strings.LastIndex(note, " "); i >= 0 {
		return strings.TrimSuffix(strings.TrimSpace(note[i:]), "%"), true
	}
	return "", true
}

func healthMap(b *Blade) map[string]any {
	var h map[string]any
	if len(b.Health) > 0 {
		_ = json.Unmarshal(b.Health, &h)
	}
	return h
}

// factsBool reads a flag out of the facts the agent last reported.
func factsBool(b *Blade, key string) bool {
	var f map[string]any
	if len(b.Facts) > 0 {
		_ = json.Unmarshal(b.Facts, &f)
	}
	v, _ := f[key].(bool)
	return v
}

// distroText shows what is actually running — and, until something has
// checked in, what is meant to run. The arrow makes the difference visible.
func distroText(b *Blade) string {
	var f map[string]any
	if len(b.Facts) > 0 {
		_ = json.Unmarshal(b.Facts, &f)
	}
	if name, _ := f["os_name"].(string); name != "" {
		return shortenOS(name)
	}
	if b.Image != "" {
		return b.Image + " →"
	}
	return "—"
}

// shortenOS trims "Ubuntu 24.04.3 LTS" to "ubuntu 24.04" — in a slot row
// what counts is recognition at a glance, not precision.
func shortenOS(name string) string {
	f := strings.Fields(strings.ToLower(name))
	if len(f) == 0 {
		return name
	}
	if len(f) == 1 {
		return f[0]
	}
	ver := f[1]
	if parts := strings.Split(ver, "."); len(parts) > 2 {
		ver = parts[0] + "." + parts[1]
	}
	return f[0] + " " + ver
}

func roleText(l Lang, b *Blade) string {
	if len(b.Groups) == 0 {
		return T(l, "role.none")
	}
	return strings.Join(b.Groups, ", ")
}

// tempValue formats a temperature in the notation of the language — with a
// comma in German. A dot in an otherwise German table reads like a
// translation slip.
func tempValue(l Lang, h map[string]any, key string) string {
	v, ok := num(h[key])
	if !ok {
		return "—"
	}
	return decimal(l, v, 1) + " °C"
}

func decimal(l Lang, v float64, digits int) string {
	s := strconv.FormatFloat(v, 'f', digits, 64)
	if l == LangDE {
		s = strings.Replace(s, ".", ",", 1)
	}
	return s
}

// fanText shows RPM when there is a tacho — otherwise the target in percent.
// The standard fan unit cannot report RPM, and an invented number would be
// worse than a percentage.
func fanText(l Lang, h map[string]any) string {
	if v, ok := num(h["fan_rpm"]); ok && v > 0 {
		return groupThousands(l, int64(v))
	}
	if v, ok := num(h["fan_percent"]); ok {
		return decimal(l, v, 0) + " %"
	}
	return "—"
}

// groupThousands uses a narrow space as the thousands separator —
// "3 200" reads at a glance, "3200" does not.
func groupThousands(l Lang, n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, 0xE2, 0x80, 0xAF) // narrow no-break space
		}
		out = append(out, c)
	}
	return string(out)
}

// tempText combines both temperatures compactly — more does not fit a slot
// row, and more is not needed when skimming.
func tempText(b Blade) string {
	var h map[string]any
	if len(b.Health) > 0 {
		_ = json.Unmarshal(b.Health, &h)
	}
	soc, okS := num(h["soc_temp_c"])
	nvme, okN := num(h["nvme_temp_c"])
	switch {
	case okS && okN:
		return fmt.Sprintf("%.0f / %.0f °C", soc, nvme)
	case okS:
		return fmt.Sprintf("%.0f °C", soc)
	}
	return "—"
}

func installOr(s string) string {
	if s == "" {
		return installIdle
	}
	return s
}

func instLED(s string) string {
	switch s {
	case installPending:
		return "id"
	case installDone:
		return "ok"
	case installError:
		return "crit"
	}
	return "off"
}

func stageLED(st string) string {
	switch st {
	case stageDone:
		return "ok"
	case stageError:
		return "crit"
	case stageWriting, stageInstaller, stageRamdisk:
		return "id"
	}
	return "warn"
}

// ── Templates ────────────────────────────────────────────────────────

// markSVG is the mark: a crenellated tower — the chess rook, and at the same
// time a rack seen head-on. The three cut-outs are the slots. A single path
// with evenodd so the slots are real holes and the mark sits on any
// background; currentColor takes the text colour, and with it dark mode.
const markSVG = `<svg class="mark" viewBox="0 0 24 24" aria-hidden="true" focusable="false">` +
	`<path fill="currentColor" fill-rule="evenodd" d="M3.2 2 H6.6 V4.4 H9.2 V2 H12.8 V4.4 H15.4 V2 ` +
	`H18.8 V7.6 H16.8 V18.6 H19 V21.8 H3 V18.6 H5.2 V7.6 H3.2 Z ` +
	`M7 9.6 H15 V11.1 H7 Z M7 12.4 H15 V13.9 H7 Z M7 15.2 H15 V16.7 H7 Z"/></svg>`

var tmplFuncs = template.FuncMap{
	"mark": func() template.HTML { return template.HTML(markSVG) },
	"t":    T,
	// th returns translations that deliberately carry markup (a <code> around
	// a path, say). Only for text from the catalogue — never for input.
	"th": func(l Lang, key string, args ...any) template.HTML {
		return template.HTML(T(l, key, args...))
	},
	"otherLang": otherLang,
	"langName":  langName,
}

const baseCSS = `
:root{--ground:#ECEEF1;--surface:#FAFBFC;--surface-2:#E2E6EB;--ink:#15181E;--ink-2:#4B525D;
--ink-3:#7A828F;--rule:#D3D8DF;--rule-s:#B9C1CB;--accent:#A9520F;--accent-ink:#8C4308;
--accent-soft:#F2E2D3;--ok:#2C6647;--warn:#8B6210;--crit:#A4322A;--ident:#1D4ED8}
@media(prefers-color-scheme:dark){:root{--ground:#13161B;--surface:#1A1E25;--surface-2:#232830;
--ink:#E7EAEF;--ink-2:#A3ABB8;--ink-3:#727B89;--rule:#2C323C;--rule-s:#3E4653;--accent:#E4884A;
--accent-ink:#F0A56F;--accent-soft:#38271A;--ok:#61B587;--warn:#D5A343;--crit:#E06B5C;
--ident:#5B9BFF}}
*{box-sizing:border-box}
body{margin:0;background:var(--ground);color:var(--ink);
font:17px/1.62 system-ui,-apple-system,"Segoe UI",sans-serif;padding-bottom:4rem}
.wrap{max-width:1360px;margin:0 auto;padding:0 1.6rem}
a{color:var(--accent-ink)}
header.top{border-bottom:1px solid var(--rule);margin-bottom:1.8rem;padding:1.6rem 0 1.1rem}
.topbar{display:flex;flex-wrap:wrap;gap:1rem;align-items:baseline;justify-content:space-between}
.topright{display:flex;gap:.6rem;align-items:center}
h1{margin:0;font-size:2rem;letter-spacing:-.025em}
.brand{display:flex;align-items:center;gap:.6rem}
.brand em{font-style:normal;color:var(--accent)}
.mark{width:1.5rem;height:1.5rem;flex:0 0 auto;color:var(--accent)}
.crumb .mark{width:.85rem;height:.85rem;vertical-align:-.1em;color:var(--ink-3)}
.sub{color:var(--ink-2);margin:.4rem 0 0;font-size:1.02rem}
.meta{display:flex;flex-wrap:wrap;gap:1.4rem;margin-top:1rem;
font:500 .8rem/1 ui-monospace,monospace;letter-spacing:.09em;text-transform:uppercase;color:var(--ink-3)}
.meta b{color:var(--accent-ink)}
h2{font-size:1.24rem;margin:0;letter-spacing:-.012em}
.crumb{font:.82rem/1 ui-monospace,monospace;letter-spacing:.08em;text-transform:uppercase;
color:var(--ink-3);margin:0 0 .5rem}
.crumb a{color:var(--ink-3);text-decoration:none}
.crumb a:hover{color:var(--accent-ink)}
.note,.bad{padding:.85rem 1.1rem;border-radius:0 3px 3px 0;margin:0 0 1.3rem;font-size:1rem}
.note{background:var(--accent-soft);border-left:2px solid var(--accent)}
.bad{background:var(--surface);border-left:2px solid var(--crit);color:var(--crit)}
/* No overflow:hidden — it clipped the action overlay at the card edge.
   The header carries the rounded corners instead. */
.card{border:1px solid var(--rule);border-radius:4px;background:var(--surface);margin:0 0 1.5rem}
.card>.card-head:first-child{border-radius:3px 3px 0 0}
.card>table:last-child tr:last-child td:first-child{border-radius:0 0 0 3px}
.card>table:last-child tr:last-child td:last-child{border-radius:0 0 3px 0}
.card-head{display:flex;flex-wrap:wrap;gap:.8rem;align-items:baseline;justify-content:space-between;
padding:.8rem 1.05rem;background:var(--surface-2);border-bottom:1px solid var(--rule-s)}
.tag{font:500 .78rem/1.45 ui-monospace,monospace;letter-spacing:.08em;text-transform:uppercase;color:var(--ink-3)}
/* .tag is a short all-caps label. For explanatory sentences and anything
   that needs real casing — paths, commands, hostnames — use .hint. */
.hint{font-size:.92rem;line-height:1.6;color:var(--ink-3)}
.hint code{font-size:.85em}
/* Safety net: a path must never be upper-cased by an ancestor —
   /SRV/... would simply be wrong. */
code,kbd,pre{text-transform:none}
.body{padding:1.05rem}
form.inline{display:inline}
label{display:block;font:600 .74rem/1 ui-monospace,monospace;letter-spacing:.1em;
text-transform:uppercase;color:var(--ink-3);margin:0 0 .35rem}
input[type=text],input[type=password],select{font:inherit;font-size:1rem;padding:.55rem .7rem;
border:1px solid var(--rule-s);border-radius:3px;background:var(--ground);color:var(--ink);width:100%}
input:focus,select:focus{outline:2px solid var(--accent);outline-offset:1px}
.row{display:flex;flex-wrap:wrap;gap:.9rem;align-items:flex-end}
.row>div{flex:1 1 11rem}
.row>div.narrow{flex:0 0 8rem}
button{font:600 .9rem/1 inherit;padding:.62rem 1rem;border-radius:3px;cursor:pointer;
border:1px solid var(--accent);background:var(--accent);color:var(--surface)}
button:hover{filter:brightness(1.08)}
button.ghost{background:transparent;color:var(--accent-ink);border-color:var(--rule-s)}
button.danger{background:transparent;color:var(--crit);border-color:var(--crit)}
button.mini{font-size:.85rem;padding:.46rem .8rem}
button:focus-visible,a:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
a.langlink{font:600 .74rem/1 ui-monospace,monospace;text-decoration:none;padding:.5rem .7rem;
border:1px solid var(--rule-s);border-radius:3px;color:var(--ink-2)}
a.langlink:hover{color:var(--accent-ink);border-color:var(--accent)}
table{border-collapse:collapse;width:100%;font-size:1rem}
th{text-align:left;font:600 .78rem/1 ui-monospace,monospace;letter-spacing:.1em;
text-transform:uppercase;color:var(--ink-3);padding:.7rem .9rem;border-bottom:1px solid var(--rule-s)}
td{padding:.6rem .9rem;border-bottom:1px solid var(--rule);vertical-align:middle}
tr:last-child td{border-bottom:0}
.mono{font:.92rem/1.5 ui-monospace,monospace;color:var(--ink-2)}
.host{font-weight:600}
.slotno{font:600 1rem/1 ui-monospace,monospace;color:var(--ink-3);width:3.2rem}
.led{display:inline-block;width:.58rem;height:.58rem;border-radius:50%;
background:var(--ink-3);box-shadow:0 0 0 1px var(--rule-s) inset}
.led.ok{background:var(--ok);box-shadow:0 0 6px -1px var(--ok)}
.led.warn{background:var(--warn);box-shadow:0 0 6px -1px var(--warn)}
.led.crit{background:var(--crit);box-shadow:0 0 8px -1px var(--crit)}
.led.id{background:var(--accent);box-shadow:0 0 8px -1px var(--accent);animation:bl 1.1s steps(1,end) infinite}
/* Identify: blue and breathing. Someone is standing in front of the rack
   looking for this blade — the screen should agree with the light. */
.led.ident{background:var(--ident);box-shadow:0 0 9px 0 var(--ident);
  animation:pulse 1.4s ease-in-out infinite}
.led.off{background:transparent}
@keyframes bl{0%,49%{opacity:1}50%,100%{opacity:.15}}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.35}}
@media(prefers-reduced-motion:reduce){.led.id,.led.ident,.cell.ident{animation:none}}
.chip{font:500 .78rem/1 ui-monospace,monospace;letter-spacing:.06em;text-transform:uppercase;
padding:.3rem .55rem;border:1px solid currentColor;border-radius:2px;white-space:nowrap;
display:inline-block}
.chip.ok{color:var(--ok)}.chip.warn{color:var(--warn)}.chip.id{color:var(--accent-ink)}
.chip.ident{color:#fff;background:var(--ident);border-color:var(--ident)}
.chip.off{color:var(--ink-3)}.chip.crit{color:#fff;background:var(--crit);border-color:var(--crit)}
.bar{height:.3rem;background:var(--surface-2);border-radius:2px;overflow:hidden;margin:.5rem 0 0}
.bar i{display:block;height:100%;background:var(--accent)}
.grid{display:grid;gap:.9rem;grid-template-columns:repeat(auto-fill,minmax(17rem,1fr))}
.rackcard{border:1px solid var(--rule);border-radius:4px;background:var(--surface);padding:1.05rem 1.15rem}

/* ── Occupancy at a glance ───────────────────────────────────────────
   One square per slot. Four colours; a square cannot carry more. The detail
   lives in the title and on the detail page. */
.cells{display:grid;grid-template-columns:repeat(auto-fill,minmax(2.1rem,1fr));
  gap:.28rem;margin:.75rem 0 .6rem;text-decoration:none}
.cell{display:flex;align-items:center;justify-content:center;
  aspect-ratio:1;border-radius:3px;font:600 .74rem/1 ui-monospace,monospace;
  border:1px solid;transition:transform .08s}
.cells:hover .cell{transform:none}
.cell:hover{transform:scale(1.08)}
.cell.free{background:var(--surface-2);border-color:var(--rule);color:var(--ink-3)}
.cell.ident{background:var(--ident);border-color:var(--ident);color:#fff;
  animation:pulse 1.4s ease-in-out infinite}
.cell.ok{background:var(--ok);border-color:var(--ok);color:var(--surface)}
.cell.busy{background:var(--warn);border-color:var(--warn);color:var(--surface)}
.cell.bad{background:var(--crit);border-color:var(--crit);color:#fff}
.cellnote{margin:0 0 .35rem}
.rackcard a.name{font-weight:600;font-size:1.02rem;text-decoration:none;color:var(--ink)}
.rackcard a.name:hover{color:var(--accent-ink)}
.empty{color:var(--ink-3)}
.acts{display:flex;gap:.35rem;flex-wrap:wrap}

/* ── Slot view ────────────────────────────────────────────────────── */
/* The map. Colours come from the tokens, so the diagram follows the theme
   like everything else; nothing here is a literal. */
.topo-wrap{overflow-x:auto}
svg.topo{display:block;width:100%;min-width:48rem;height:auto}
svg.topo .node rect{fill:var(--surface-2);stroke:var(--rule-s);stroke-width:1}
svg.topo .centre rect{fill:var(--accent-soft);stroke:var(--accent)}
svg.topo text{font:400 .82rem/1 ui-monospace,monospace;fill:var(--ink-2)}
svg.topo text.t1{font-weight:600;font-size:1rem;fill:var(--ink)}
svg.topo text.t3{font-size:.76rem;fill:var(--ink-3)}
svg.topo text.right{text-anchor:end}
svg.topo .link{fill:none;stroke:var(--rule-s);stroke-width:1.5}
svg.topo .link.warn{stroke:var(--warn);stroke-dasharray:6 4}
svg.topo .link.crit{stroke:var(--crit);stroke-dasharray:2 5}
svg.topo .link.off{stroke:var(--rule);stroke-dasharray:2 5}
svg.topo .dot{fill:var(--ok)}
svg.topo .dot.warn{fill:var(--warn)}
svg.topo .dot.crit{fill:var(--crit)}
svg.topo .dot.off{fill:var(--ink-3)}
/* The same four states the BladeRunner cards use, so a square means the same
   thing wherever it is seen. */
svg.topo .cell{fill:var(--ok)}
svg.topo .cell.busy{fill:var(--warn)}
svg.topo .cell.bad{fill:var(--crit)}
svg.topo .cell.free{fill:var(--surface);stroke:var(--rule-s);stroke-width:.8}
svg.topo .cell.ident{fill:var(--ident)}
.therm{display:flex;flex-wrap:wrap;gap:.4rem 1.4rem;margin:.5rem 0 0;
  font:.85rem/1.6 ui-monospace,monospace;color:var(--ink-2)}
.therm b{color:var(--ink);font-weight:600}
.hw{display:grid;grid-template-columns:1fr 1fr;gap:.15rem .9rem;margin:.5rem 0 .2rem;
  font:.8rem/1.7 ui-monospace,monospace}
.hw div{display:flex;justify-content:space-between;gap:.6rem}
.hw span{color:var(--ink-3)}
.hw b{color:var(--ink);font-weight:600}
.sparks{margin:.5rem 0 .2rem;font:.75rem/1.5 ui-monospace,monospace;color:var(--ink-3)}
.sparks div{display:flex;align-items:center;gap:.5rem;justify-content:space-between}
svg.spark{width:8.25rem;height:1.6rem;overflow:visible}
svg.spark polyline{fill:none;stroke-width:1.2;vector-effect:non-scaling-stroke}
svg.spark.soc polyline{stroke:var(--accent)}
svg.spark.fan polyline{stroke:var(--ok)}
table.log td{padding:.45rem .9rem;font-size:.92rem}
table.log td.mono{font:.82rem/1.5 ui-monospace,monospace;color:var(--ink-2)}
table.log td.nowrap{white-space:nowrap}
table.log .hint{font-size:.78rem}
table.log .led{margin-right:.5rem}
table.rack td{padding:.85rem 1rem}
table.rack tr.free td{background:repeating-linear-gradient(135deg,
  transparent 0 7px,var(--surface-2) 7px 8px)}
table.rack .host{font-weight:600;font-size:1.12rem;letter-spacing:-.01em}
table.rack .sub2{font-size:.84rem;line-height:1.45;margin-top:.28rem;color:var(--ink-3)}
table.rack .num{font-variant-numeric:tabular-nums;white-space:nowrap}
table.rack td.right{text-align:right;width:3.5rem;position:relative}
table.rack .led{width:.8rem;height:.8rem}

/* ── Actions as an overlay ───────────────────────────────────────────
   Deliberately without JavaScript: <details> does this, is keyboard-operable
   out of the box, and still works when scripts are blocked. */
.menu{position:relative;display:inline-block}
.menu[open]{z-index:60}
tr:has(.menu[open]){position:relative;z-index:60}
.menu>summary{list-style:none;cursor:pointer;user-select:none;
  font:700 1.25rem/1 ui-monospace,monospace;letter-spacing:.08em;
  color:var(--ink-3);padding:.35rem .6rem;border:1px solid var(--rule);border-radius:3px}
.menu>summary::-webkit-details-marker{display:none}
.menu>summary:hover{color:var(--accent-ink);border-color:var(--rule-s)}
.menu[open]>summary{color:var(--accent-ink);border-color:var(--accent);background:var(--accent-soft)}
.menu>summary:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.menu-panel{position:absolute;right:0;top:calc(100% + .4rem);z-index:60;
  width:23rem;max-width:min(23rem,calc(100vw - 3rem));text-align:left;background:var(--surface);
  border:1px solid var(--rule-s);border-radius:4px;padding:.8rem .9rem;
  box-shadow:0 2px 4px rgba(0,0,0,.06),0 12px 32px -10px rgba(0,0,0,.35)}
@media(prefers-color-scheme:dark){.menu-panel{box-shadow:0 2px 4px rgba(0,0,0,.5),0 14px 34px -10px rgba(0,0,0,.9)}}
.menu-head{font-weight:600;font-size:1.08rem;margin:0 0 .6rem;padding-bottom:.45rem;
  border-bottom:1px solid var(--rule)}
.menu-panel form{display:flex;gap:.4rem;align-items:flex-end;margin:0 0 .5rem}
.menu-panel form:last-of-type{margin-bottom:0}
.menu-panel select{flex:1}
.menu-panel label{margin-bottom:.3rem}
/* A form with more than one field cannot be a row: the fields shrink to
   nothing and the labels end up beside them. Those forms stack. */
.menu-panel form.stack{display:block}
.menu-panel form.stack label{display:block;margin:.55rem 0 .2rem;
  font:600 .74rem/1 ui-monospace,monospace;letter-spacing:.09em;
  text-transform:uppercase;color:var(--ink-3)}
.menu-panel form.stack label:first-child{margin-top:0}
.menu-panel form.stack input{width:100%;display:block}
.menu-panel form.stack .menu-row{gap:.4rem;margin:.2rem 0 0}
.menu-panel form.stack .menu-row input{flex:1;min-width:0}
.menu-panel form.stack button{margin-top:.7rem}
.menu-sep{height:1px;background:var(--rule);margin:.7rem 0}
.menu-row{display:flex;gap:.4rem;margin:0 0 .45rem}
.menu-row form{flex:1;margin:0}
.menu-row button{width:100%}
.menu-note{font:.82rem/1.55 ui-monospace,monospace;color:var(--ink-3);
  margin-top:.6rem;padding-top:.5rem;border-top:1px solid var(--rule);word-break:break-all}
code{font:.82em ui-monospace,monospace;background:var(--surface-2);padding:.1em .35em;border-radius:2px}
.tm{opacity:.75}
.explain{padding:.7rem 1.05rem;border-top:1px solid var(--rule)}
.explain>summary{cursor:pointer;font:500 .8rem/1.4 ui-monospace,monospace;
  letter-spacing:.06em;text-transform:uppercase;color:var(--ink-3);list-style:none}
.explain>summary::-webkit-details-marker{display:none}
.explain>summary::before{content:"›";display:inline-block;margin-right:.45rem;transition:transform .12s}
.explain[open]>summary::before{transform:rotate(90deg)}
.explain>summary:hover{color:var(--accent-ink)}
.explain .hint{margin-top:.6rem}

/* ── Sign-in page ────────────────────────────────────────────────────
   A single card in the top-left corner looks lost on a large screen. It
   belongs in the middle and may be considerably larger — it is the only task
   on this page. */
.signin{min-height:100vh;display:grid;place-items:center;padding:2rem 1.5rem}
.signin-box{width:100%;max-width:31rem}
.signin .brand{justify-content:flex-start;font-size:2.4rem}
.signin .mark{width:2.1rem;height:2.1rem}
.signin header.top{border:0;padding:0 0 1.6rem;margin:0}
.signin .sub{font-size:1.05rem;margin-top:.5rem}
.signin .card{margin:0}
.signin .body{padding:1.8rem}
.signin label{font-size:.75rem;margin-bottom:.5rem}
.signin input[type=password]{font-size:1.1rem;padding:.8rem .9rem;letter-spacing:.02em}
.signin button[type=submit]{font-size:.95rem;padding:.75rem 1.6rem;margin-top:.2rem}
.signin .hint{font-size:.9rem;line-height:1.6}
.signin pre{font-size:.88rem !important;padding:.85rem 1rem !important}
footer{margin-top:2.2rem;padding-top:.9rem;border-top:1px solid var(--rule);
font:.82rem/1.65 ui-monospace,monospace;color:var(--ink-3);
display:flex;justify-content:space-between;flex-wrap:wrap;gap:.8rem}
`

const headHTML = `<!doctype html><html lang="{{.L}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Rookery</title>{{if .Refresh}}
<script>
// Not <meta http-equiv="refresh">: that timer keeps its own navigation to
// this page pending, and if the reader clicks into a BladeRunner just as it
// fires, the reload overtakes the click and throws them back to the overview.
// A script timer belongs to the document, dies with it, and a click on
// anything that navigates cancels it first.
(function () {
  var t = setTimeout(function () {
    if (!document.hidden) { location.reload(); }
  }, 5000);
  var stop = function () { clearTimeout(t); };
  addEventListener("pagehide", stop);
  addEventListener("beforeunload", stop);
  addEventListener("click", function (e) {
    if (e.target.closest("a[href], button, input[type=submit]")) { stop(); }
  }, true);
  addEventListener("submit", stop, true);
})();
</script>{{end}}
<style>` + baseCSS + `</style></head><body>`

// topRight holds the language switch and sign-out — identical on every page.
const topRight = `<div class="topright">
  <a class="langlink" href="/lang/{{otherLang .L}}?next={{.Path | urlquery}}"
     hreflang="{{otherLang .L}}">{{langName (otherLang .L)}}</a>
  <form method="post" action="/logout"><button class="ghost">{{t .L "btn.signout"}}</button></form>
</div>`

var overviewTmpl = template.Must(template.New("ov").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top"><div class="topbar"><div>
  <h1 class="brand">{{mark}}<span>Rook<em>ery</em></span></h1>
  <p class="sub">{{t .L "sub.network" .NetBase .PoolFrom .PoolTo}}</p>
</div>` + topRight + `</div>
<div class="meta"><span>{{t .L "meta.racks"}} <b>{{len .Racks}}</b></span>
<span>{{t .L "meta.blades"}} <b>{{.Blades}}</b></span>
<span>{{t .L "site.title"}} <b>{{len .Sites}}</b></span>
<span><a href="/map">{{t .L "map.link"}} →</a></span></div>
</header>

{{if .Open}}<div class="bad">{{th .L "warn.open"}}</div>{{end}}
{{if .Msg}}<div class="note">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="bad">{{.Err}}</div>{{end}}
{{range .Warnings}}<div class="bad"><b>{{t $.L "warn.net"}}:</b> {{.}}</div>{{end}}

{{$l := .L}}
{{if .Racks}}
{{range .Sites}}
<div class="card"><div class="card-head">
  <h2><a href="/sites/{{.Site.ID}}">{{.Site.Name}}</a>{{if .Local}} <span class="chip ok">{{t $l "site.here"}}</span>{{end}}
    <span class="chip {{.SLED}}">{{.State}}{{if .Seen}} · {{.Seen}}{{end}}</span>
    <span class="chip {{.SkLED}}">{{.Stock}}</span></h2>
  <span class="tag">{{.Net}} · {{t $l "site.pool"}} {{.Pool}}{{if .Site.Location}} · {{.Site.Location}}{{end}}</span></div>
{{if .Racks}}
<div class="body"><div class="grid">
{{range .Racks}}
  <div class="rackcard">
    <a class="name" href="/bladerunners/{{.Rack.ID}}">{{.Rack.Name}}</a>
    <div class="tag" style="margin-top:.3rem">{{t $l "ov.slots" .Rack.Size}}{{if .Rack.Location}} · {{.Rack.Location}}{{end}}</div>
    <a class="cells" href="/bladerunners/{{.Rack.ID}}" aria-label="{{t $l "ov.occupancy" .Used .Free}}">
      {{range .Cells}}<span class="cell {{.Class}}" title="{{.Title}}">{{printf "%02d" .Slot}}</span>{{end}}
    </a>
    <div class="mono cellnote">{{.From}} – {{.To}}</div>
    <div class="tag">{{t $l "ov.occupancy" .Used .Free}}</div>
  </div>
{{end}}
</div></div>
{{else}}
<div class="body empty">{{t $l "site.norack"}}</div>
{{end}}
</div>
{{end}}
{{else}}
<div class="card"><div class="body empty">{{t .L "ov.norack"}}</div></div>
{{end}}

<div class="card">
  <div class="card-head"><h2>{{t .L "ov.newrack"}}</h2>
  {{if not .NoSpace}}<span class="tag">{{t .L "ov.nextblock" .NextFrom}}</span>{{end}}</div>
  <div class="body">
  {{if .NoSpace}}
    <p class="empty">{{t .L "ov.nospace"}}</p>
  {{else}}
    <form method="post" action="/bladerunners">
      <div class="row">
        <div><label for="n">{{t .L "form.name"}}</label>
          <input id="n" type="text" name="name" required maxlength="60" placeholder="{{t .L "form.example"}}"></div>
        <div class="narrow"><label for="s">{{t .L "form.slots"}}</label>
          <select id="s" name="size">
            <option value="2">2</option><option value="4">4</option>
            <option value="10" selected>10</option><option value="20">20</option>
          </select></div>
        <div><label for="l">{{t .L "form.location"}}</label>
          <input id="l" type="text" name="location" maxlength="60" placeholder="{{t .L "form.optional"}}"></div>
        <div><label for="site">{{t .L "site.one"}}</label>
          <select id="site" name="site">
            {{range .Sites}}<option value="{{.Site.ID}}"{{if .Local}} selected{{end}}>{{.Site.Name}} · {{.Net}}</option>{{end}}
          </select></div>
        <div class="narrow"><button type="submit">{{t .L "form.create"}}</button></div>
      </div>
    </form>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "ov.blockhint"}}</p>
  {{end}}
  </div>
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "site.title"}}</h2>
    <span class="tag">{{t .L "site.count" (len .Sites)}}</span></div>
  <div class="body" style="padding:0">
    <table>
      <thead><tr><th>{{t .L "site.name"}}</th><th>{{t .L "site.net"}}</th>
        <th>{{t .L "site.pool"}}</th><th>{{t .L "meta.racks"}}</th>
        <th>{{t .L "stock.title"}}</th>
        <th>{{t .L "th.status"}}</th><th></th></tr></thead>
      <tbody>{{$counts := .SiteCounts}}{{range .Sites}}
        <tr>
          <td><a class="name" href="/sites/{{.Site.ID}}">{{.Site.Name}}</a>{{if .Local}} <span class="chip ok">{{t $l "site.here"}}</span>{{end}}
            {{if .Site.Location}}<div class="mono sub2">{{.Site.Location}}</div>{{end}}</td>
          <td class="mono">{{.Net}}</td>
          <td class="mono">{{.Pool}}</td>
          <td class="mono num">{{index $counts .Site.ID}}</td>
          <td><span class="chip {{.SkLED}}">{{.Stock}}</span></td>
          <td><span class="chip {{.SLED}}">{{.State}}</span>
            {{if .Seen}}<div class="mono sub2">{{.Seen}}</div>{{end}}</td>
          <td class="right">
            <details class="menu"><summary title="{{t $l "menu.open"}}">···</summary>
              <div class="menu-panel">
                <div class="menu-head">{{.Site.Name}}</div>
                <form class="stack" method="post" action="/sites/{{.Site.ID}}">
                  <label>{{t $l "site.name"}}</label>
                  <input type="text" name="name" value="{{.Site.Name}}" required maxlength="60">
                  <label>{{t $l "form.location"}}</label>
                  <input type="text" name="location" value="{{.Site.Location}}" maxlength="60">
                  <label>{{t $l "site.net"}}</label>
                  <input type="text" name="net_base" value="{{.Site.NetBase}}" required
                         pattern="[0-9]+\.[0-9]+\.[0-9]+" placeholder="10.0.0">
                  <label>{{t $l "site.poolrange"}}</label>
                  <div class="menu-row">
                    <input type="number" name="pool_from" value="{{.Site.PoolFrom}}" min="1" max="254"
                           aria-label="{{t $l "site.poolfrom"}}">
                    <input type="number" name="pool_to" value="{{.Site.PoolTo}}" min="1" max="254"
                           aria-label="{{t $l "site.poolto"}}">
                  </div>
                  <button class="mini" type="submit">{{t $l "rk.set"}}</button>
                </form>
                {{if not .Local}}
                <div class="menu-sep"></div>
                <form method="post" action="/sites/{{.Site.ID}}/delete">
                  <button class="mini danger" type="submit">{{t $l "act.remove"}}</button></form>
                {{end}}
                <div class="menu-note">{{t $l "site.movehint"}}</div>
              </div>
            </details>
          </td>
        </tr>
      {{end}}</tbody>
    </table>
  </div>
  <div class="body">
    <form method="post" action="/sites">
      <div class="row">
        <div><label for="sn">{{t .L "site.name"}}</label>
          <input id="sn" type="text" name="name" required maxlength="60" placeholder="{{t .L "site.example"}}"></div>
        <div class="narrow"><label for="snet">{{t .L "site.net"}}</label>
          <input id="snet" type="text" name="net_base" required pattern="[0-9]+\.[0-9]+\.[0-9]+"
                 placeholder="10.1.0"></div>
        <div><label for="sl">{{t .L "form.location"}}</label>
          <input id="sl" type="text" name="location" maxlength="60" placeholder="{{t .L "form.optional"}}"></div>
        <div class="narrow"><button type="submit">{{t .L "form.create"}}</button></div>
      </div>
    </form>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "site.hint"}}</p>
  </div>
</div>

{{if .Netboot}}
<div class="card" style="border-color:var(--accent)">
  <div class="card-head" style="background:var(--accent-soft)">
    <h2>{{t .L "nb.title"}}</h2>
    <span class="tag">{{t .L "nb.count" (len .Netboot)}}{{if .Refresh}}{{t .L "nb.refresh"}}{{end}}</span></div>
  <table>
    <thead><tr><th></th><th>{{t .L "th.device"}}</th>
      {{if gt (len .Sites) 1}}<th>{{t .L "site.one"}}</th>{{end}}
      <th>{{t .L "th.progress"}}</th><th>{{t .L "th.image"}}</th><th>{{t .L "th.last"}}</th></tr></thead>
    <tbody>
    {{$top := .}}
    {{range .Netboot}}
      <tr>
        <td><span class="led {{.LED}}"></span></td>
        <td>
          {{if .Known}}<span class="host">{{.Hostname}}</span>
          {{else}}<span class="host">{{t $top.L "nb.unknown"}}</span>{{end}}
          <div class="mono sub2">{{if .IP}}{{.IP}}{{else}}{{.MAC}}{{end}}</div>
        </td>
        {{if gt (len $top.Sites) 1}}
        <td class="mono">{{if .SiteName}}{{.SiteName}}{{else}}—{{end}}</td>
        {{end}}
        <td><span class="chip {{.LED}}">{{.Label}}</span>
          {{if .LeaseOnly}}<div class="mono sub2">{{t $top.L "nb.bootorder"}}</div>{{end}}</td>
        <td>
          {{if .LeaseOnly}}<span class="empty">—</span>
          {{else if .Image}}<span class="mono">{{.Image}}</span>
          {{else if $top.Images}}
            <form method="post" action="/netboot/{{.MAC}}/image" class="row" style="gap:.3rem">
              <div style="flex:1 1 8rem"><select name="image" aria-label="{{t $top.L "th.image"}}">
                {{range $top.Images}}<option value="{{.ID}}">{{.ID}}</option>{{end}}
              </select></div>
              <div style="flex:0 0 auto"><button class="mini" type="submit">{{t $top.L "nb.write"}}</button></div>
            </form>
          {{else}}<span class="hint">{{t $top.L "nb.nocatalog"}}</span>{{end}}
        </td>
        <td class="mono">{{.Ago}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  <details class="explain"><summary>{{t .L "nb.how"}}</summary>
    <div class="hint">{{th .L "nb.hint"}}</div></details>
</div>
{{end}}

{{if .Unassigned}}
<div class="card">
  <div class="card-head"><h2>{{t .L "ov.unassigned"}}</h2>
  <span class="tag">{{t .L "ov.unassignedtag" (len .Unassigned)}}</span></div>
  <table>
    <thead><tr><th></th><th>{{t .L "th.serial"}}</th><th>{{t .L "th.mac"}}</th>
      <th>{{t .L "th.status"}}</th><th>{{t .L "th.lastseen"}}</th></tr></thead>
    <tbody>{{range .Unassigned}}
      <tr><td><span class="led {{.LED}}"></span></td>
      <td class="mono">{{.Serial}}</td><td class="mono">{{if .MAC}}{{.MAC}}{{else}}—{{end}}</td>
      <td><span class="chip {{.LED}}">{{.State}}</span></td>
      <td class="mono">{{.Ago}}</td></tr>
    {{end}}</tbody>
  </table>
  <div class="body hint">{{t .L "ov.unassignedhint"}}</div>
</div>
{{end}}

<footer><span>{{t .L "foot.api"}}<br><span class="tm">{{t .L "foot.tm"}}</span></span>
<span>{{.NetBase}}.0/24</span></footer>
</div></body></html>`))

var rackTmpl = template.Must(template.New("rack").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top"><div class="topbar"><div>
  <p class="crumb"><a href="/">{{mark}} {{t .L "nav.overview"}}</a> / {{t .L "nav.rack"}}</p>
  <h1>{{.R.Rack.Name}}</h1>
  <p class="sub">{{t .L "ov.slots" .R.Rack.Size}}{{if .R.Rack.Location}} · {{.R.Rack.Location}}{{end}}
   · {{t .L "rk.block" .R.From .R.To}}{{if .R.SiteName}} · {{t .L "site.one"}} {{.R.SiteName}}{{end}}</p>
</div>` + topRight + `</div>
<div class="meta"><span>{{t .L "meta.used"}} <b>{{.R.Used}}</b></span>
<span>{{t .L "meta.free"}} <b>{{.R.Free}}</b></span></div>
{{if .R.Therm.Have}}
<div class="therm">
  <span>{{t .L "hw.hottest"}} <b>{{.R.Therm.Hot}}</b> · {{.R.Therm.HotTemp}} °C</span>
  <span>{{t .L "hw.socspan"}} <b>{{.R.Therm.SocLow}}–{{.R.Therm.SocHigh}} °C</b></span>
  {{if .R.Therm.RPMHigh}}<span>{{t .L "hw.rpmspan"}} <b>{{.R.Therm.RPMLow}}–{{.R.Therm.RPMHigh}}</b></span>{{end}}
  {{if .R.Therm.Fans}}<span>{{t .L "hw.smartfans" .R.Therm.Smart .R.Therm.Fans}}</span>{{end}}
</div>
{{end}}
</header>

{{if .Open}}<div class="bad">{{th .L "warn.open"}}</div>{{end}}
{{if .Msg}}<div class="note">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="bad">{{.Err}}</div>{{end}}

<div class="card">
  <div class="card-head"><h2>{{t .L "rk.slots"}}</h2>
  {{if not .Free}}<span class="tag">{{t .L "rk.nofree"}}</span>{{end}}</div>
  <table class="rack">
    <thead><tr>
      <th>{{t .L "th.slot"}}</th><th></th><th>{{t .L "th.hostname"}}</th>
      <th>{{t .L "th.distro"}}</th><th>{{t .L "th.role"}}</th>
      <th>{{t .L "th.status"}}</th><th>{{t .L "th.soc"}}</th><th>{{t .L "th.fan"}}</th>
      <th></th></tr></thead>
    <tbody>
    {{$top := .}}
    {{range .R.Slots}}
      <tr {{if .Empty}}class="free"{{end}}>
        <td class="slotno">{{printf "%02d" .Slot}}</td>
        <td><span class="led {{.LED}}"></span></td>
        {{if .Empty}}
          <td class="empty">—</td>
          <td class="empty mono">{{.IP}}</td>
          <td class="empty">—</td>
          <td><span class="chip off">{{t $top.L "st.free"}}</span></td>
          <td class="empty">—</td><td class="empty">—</td>
          <td class="right">
            {{if $top.Free}}
            <details class="menu"><summary title="{{t $top.L "menu.open"}}">···</summary>
              <div class="menu-panel">
                <div class="menu-head">{{t $top.L "rk.insert"}} — {{t $top.L "th.slot"}} {{printf "%02d" .Slot}}</div>
                <form method="post" action="/bladerunners/{{$top.R.Rack.ID}}/slots/{{.Slot}}/assign">
                  <select name="serial" aria-label="{{t $top.L "th.serial"}}">
                    {{range $top.Free}}<option value="{{.Serial}}">{{.Serial}}{{if .MAC}} · {{.MAC}}{{end}}</option>{{end}}
                  </select>
                  <button class="mini" type="submit">{{t $top.L "rk.insert"}}</button>
                </form>
                <div class="menu-note">{{t $top.L "menu.planned"}} {{.IP}} · {{.MAC}}</div>
              </div>
            </details>
            {{end}}
          </td>
        {{else}}
          <td class="host">{{.Hostname}}
            <div class="mono sub2">{{.Serial}}{{if .IP}} · {{.IP}}{{end}}</div></td>
          <td class="mono">{{.Distro}}</td>
          <td class="mono">{{.Role}}</td>
          <td><span class="chip {{.SLED}}">{{.Status}}</span>
            {{if .Health}}<div class="mono sub2">{{.Health}}</div>{{end}}</td>
          <td class="mono num">{{.Soc}}</td>
          <td class="mono num">{{.Fan}}</td>
          <td class="right">
            <details class="menu"><summary title="{{t $top.L "menu.open"}}">···</summary>
              <div class="menu-panel">
                <div class="menu-head">{{.Hostname}}</div>

                <form method="post" action="/blades/{{.Serial}}/image">
                  <label for="img{{.Slot}}">{{t $top.L "th.image"}}</label>
                  {{$img := .Image}}
                  <select id="img{{.Slot}}" name="image">
                    <option value="">{{t $top.L "rk.none"}}</option>
                    {{range $top.Images}}<option value="{{.ID}}"{{if eq .ID $img}} selected{{end}}>{{.ID}}{{if .Kernel}} · {{.Kernel}}{{end}}</option>{{end}}
                  </select>
                  <button class="mini ghost" type="submit">{{t $top.L "rk.set"}}</button>
                </form>

                <div class="menu-sep"></div>
                <div class="menu-row">
                  {{if .Identifying}}
                  <form method="post" action="/blades/{{.Serial}}/actions/identify_off">
                    <button class="mini" type="submit" title="{{t $top.L "act.identifyofftip"}}">{{t $top.L "act.identifyoff"}}</button></form>
                  {{else}}
                  <form method="post" action="/blades/{{.Serial}}/actions/identify">
                    <button class="mini ghost" type="submit" title="{{t $top.L "act.identifytip"}}">{{t $top.L "act.identify"}}</button></form>
                  {{end}}
                  <form method="post" action="/blades/{{.Serial}}/actions/reboot">
                    <button class="mini ghost" type="submit">{{t $top.L "act.reboot"}}</button></form>
                </div>
                <div class="menu-row">
                  {{if .Stealthy}}
                  <form method="post" action="/blades/{{.Serial}}/actions/stealth_off">
                    <button class="mini ghost" type="submit">{{t $top.L "act.stealthoff"}}</button></form>
                  {{else}}
                  <form method="post" action="/blades/{{.Serial}}/actions/stealth_on">
                    <button class="mini ghost" type="submit" title="{{t $top.L "act.stealthtip"}}">{{t $top.L "act.stealthon"}}</button></form>
                  {{end}}
                </div>
                <div class="menu-row">
                  <form method="post" action="/blades/{{.Serial}}/actions/reimage">
                    <button class="mini" type="submit" title="{{t $top.L "act.installtip"}}">{{t $top.L "act.install"}}</button></form>
                  <form method="post" action="/blades/{{.Serial}}/unassign">
                    <button class="mini danger" type="submit">{{t $top.L "act.remove"}}</button></form>
                </div>
                <div class="menu-sep"></div>
                <div class="hw">
                  <div><span>{{t $top.L "hw.soc"}}</span><b>{{.Soc}}</b></div>
                  <div><span>{{t $top.L "hw.airflow"}}</span><b>{{.Airflow}}</b></div>
                  <div><span>{{t $top.L "hw.fan"}}</span><b>{{.Fan}}</b></div>
                  <div><span>{{t $top.L "hw.fantarget"}}</span><b>{{.FanPct}}</b></div>
                  <div><span>{{t $top.L "hw.fanunit"}}</span><b>{{.FanUnit}}</b></div>
                  <div><span>{{t $top.L "hw.module"}}</span><b>{{.Module}}</b></div>
                  <div><span>{{t $top.L "hw.state"}}</span><b>{{.BladeSt}}</b></div>
                  <div><span>{{t $top.L "hw.stealth"}}</span><b>{{.Stealth}}</b></div>
                </div>
                {{if .SparkNum}}
                <div class="sparks">
                  <div><span>{{t $top.L "hw.soctrend"}}</span>{{.SparkSoc}}</div>
                  <div><span>{{t $top.L "hw.fantrend"}}</span>{{.SparkFan}}</div>
                  <div class="menu-note">{{t $top.L "hw.window" .SparkNum}}</div>
                </div>
                {{end}}
                <div class="menu-note">{{t $top.L "th.install"}}: {{.Install}} · {{t $top.L "th.mac"}} {{.MAC}}</div>
              </div>
            </details>
          </td>
        {{end}}
      </tr>
    {{end}}
    </tbody>
  </table>
  <div class="body hint">{{t .L "rk.installhint"}}</div>
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "rk.edit"}}</h2></div>
  <div class="body">
    <form method="post" action="/bladerunners/{{.R.Rack.ID}}">
      <div class="row">
        <div><label for="n">{{t .L "form.name"}}</label>
          <input id="n" type="text" name="name" value="{{.R.Rack.Name}}" required maxlength="60"></div>
        <div class="narrow"><label for="s">{{t .L "form.slots"}}</label>
          <select id="s" name="size">
            <option value="2"{{if eq .R.Rack.Size 2}} selected{{end}}>2</option>
            <option value="4"{{if eq .R.Rack.Size 4}} selected{{end}}>4</option>
            <option value="10"{{if eq .R.Rack.Size 10}} selected{{end}}>10</option>
            <option value="20"{{if eq .R.Rack.Size 20}} selected{{end}}>20</option>
          </select></div>
        <div><label for="l">{{t .L "form.location"}}</label>
          <input id="l" type="text" name="location" value="{{.R.Rack.Location}}" maxlength="60"></div>
        <div><label for="site">{{t .L "site.assign"}}</label>
          {{$sid := .R.Rack.SiteID}}
          <select id="site" name="site">
            {{range .Sites}}<option value="{{.ID}}"{{if eq .ID $sid}} selected{{end}}>{{.Name}} · {{.Net}}</option>{{end}}
          </select></div>
        <div class="narrow"><button type="submit">{{t .L "form.save"}}</button></div>
      </div>
    </form>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "rk.edithint"}}</p>
    <p class="hint" style="margin:.4rem 0 0">{{t .L "rk.movehint"}}</p>
  </div>
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "log.title"}}</h2>
    {{if .Log}}<span class="tag">{{t .L "log.count" (len .Log)}}</span>{{end}}</div>
  {{if .Log}}
  <div class="body" style="padding:0;max-height:24rem;overflow-y:auto">
    <table class="log">
      <thead><tr><th>{{t .L "log.when"}}</th><th>{{t .L "th.slot"}}</th>
        <th>{{t .L "log.blade"}}</th><th>{{t .L "log.what"}}</th></tr></thead>
      <tbody>{{range .Log}}
        <tr>
          <td class="mono nowrap">{{.When}}{{if .Ago}}<span class="hint"> · {{.Ago}}</span>{{end}}</td>
          <td class="mono">{{.Slot}}</td>
          <td class="mono">{{.Name}}</td>
          <td><span class="led {{.LED}}"></span>{{.Msg}}</td>
        </tr>
      {{end}}</tbody>
    </table>
  </div>
  {{else}}
  <div class="body empty">{{t .L "log.empty"}}</div>
  {{end}}
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "rk.delete"}}</h2></div>
  <div class="body">
    {{if .CanDelete}}
      <form method="post" action="/bladerunners/{{.R.Rack.ID}}/delete"
            onsubmit="return confirm('{{t .L "rk.confirm"}}')">
        <button class="danger" type="submit">{{t .L "rk.delete"}}</button>
      </form>
      <p class="hint" style="margin:.7rem 0 0">{{t .L "rk.deletehint"}}</p>
    {{else}}
      <p class="empty">{{t .L "rk.hasblades" .R.Used}}</p>
    {{end}}
  </div>
</div>

<footer><span><a href="/">← {{t .L "nav.overview"}}</a><br><span class="tm">{{t .L "foot.tm"}}</span></span>
<span>{{t .L "nav.rack"}} {{.R.Rack.ID}}</span></footer>
</div></body></html>`))

var loginTmpl = template.Must(template.New("login").Funcs(tmplFuncs).Parse(headHTML + `
<div class="signin"><div class="signin-box">
<header class="top"><div class="topbar"><div>
  <h1 class="brand">{{mark}}<span>Rook<em>ery</em></span></h1>
  <p class="sub">{{t .L "login.lead"}}</p>
</div><a class="langlink" href="/lang/{{otherLang .L}}?next={{.Path | urlquery}}"
   hreflang="{{otherLang .L}}">{{langName (otherLang .L)}}</a></div></header>
{{if .Error}}<div class="bad">{{.Error}}</div>{{end}}
<div class="card"><div class="body">
  <form method="post" action="/login">
    <input type="hidden" name="next" value="{{.Next}}">
    <label for="tk">{{t .L "login.token"}}</label>
    <input id="tk" type="password" name="token" autofocus autocomplete="current-password" required>
    <div style="margin-top:1.1rem"><button type="submit">{{t .L "login.submit"}}</button></div>
  </form>
  <p class="hint" style="margin:1.4rem 0 0">{{th .L "login.hint"}}</p>
  <pre style="margin:.6rem 0 0;padding:.6rem .7rem;background:var(--surface-2);border-radius:3px;
    font:.78rem/1.5 ui-monospace,monospace;overflow-x:auto;color:var(--ink-2)"><code>sudo cat /srv/rookery/data/admin-token</code></pre>
</div></div>
</div></div></body></html>`))

// ── The map ──────────────────────────────────────────────────────────
//
// One page that answers "what does this installation consist of" without
// scrolling: the central server, the sites hanging off it, and in each site
// the blades as one square apiece. The line between centre and site carries
// the state of that link, because with several sites the interesting failure
// is no longer a blade but a stretch of network.

type topoSite struct {
	Site   Site
	State  string
	SLED   string
	Seen   string
	Net    string
	Stock  string
	SkLED  string
	Racks  int
	Blades int
	Cells  []slotCell
	Local  bool
}

const (
	topoWidth  = 980.0
	topoRow    = 132.0
	topoBoxW   = 600.0
	topoBoxH   = 108.0
	topoSiteX  = 340.0
	topoBusX   = 300.0
	topoTopPad = 28.0
)

func (a *App) hTopology(w http.ResponseWriter, r *http.Request) {
	l := a.resolveLang(w, r)
	sites, err := a.listSites()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	racks, _ := a.listRacks()
	blades, _ := a.listBlades()

	local := int64(0)
	if st, lerr := a.localSite(); lerr == nil {
		local = st.ID
	}

	stock := a.siteImages()
	byID := map[int64]*topoSite{}
	views := make([]topoSite, 0, len(sites))
	for _, st := range sites {
		key, led, seen := siteHealth(l, st)
		sk, skLED := stockText(l, stock[st.ID])
		views = append(views, topoSite{
			Site: st, State: T(l, key), SLED: led, Seen: seen,
			Net: st.NetBase + ".0/24", Stock: sk, SkLED: skLED,
			Local: st.ID == local,
		})
	}
	for i := range views {
		byID[views[i].Site.ID] = &views[i]
	}
	for _, rk := range racks {
		g, ok := byID[rk.SiteID]
		if !ok {
			continue
		}
		g.Racks++
		rv := a.buildRackView(rk, blades, l)
		for _, c := range rv.Cells {
			if c.Class != "off" {
				g.Blades++
			}
			g.Cells = append(g.Cells, c)
		}
	}

	msg, errMsg := flash(r)
	render(w, topoTmpl, map[string]any{
		"L":      l,
		"Path":   "/map",
		"Sites":  views,
		"SVG":    topoSVG(l, views, a.baseURL),
		"Blades": len(blades),
		"Msg":    msg,
		"Err":    errMsg,
		"Open":   a.adminToken == "",
	})
}

// topoSVG draws the picture. Server-side and without a library: the shape is
// a handful of boxes and lines, and a diagram that needs a megabyte of
// JavaScript to show four rectangles is a diagram nobody should ship.
func topoSVG(l Lang, sites []topoSite, baseURL string) template.HTML {
	n := len(sites)
	height := topoTopPad*2 + float64(n)*topoRow
	if height < 200 {
		height = 200
	}
	centreY := height / 2
	var b strings.Builder

	fmt.Fprintf(&b, `<svg class="topo" viewBox="0 0 %.0f %.0f" role="img" `+
		`aria-label="%s">`, topoWidth, height, T(l, "map.alt"))

	// The centre.
	fmt.Fprintf(&b, `<g class="node centre"><rect x="20" y="%.1f" width="250" height="96" rx="4"/>`,
		centreY-48)
	fmt.Fprintf(&b, `<text class="t1" x="40" y="%.1f">Rookery</text>`, centreY-20)
	fmt.Fprintf(&b, `<text class="t2" x="40" y="%.1f">%s</text>`, centreY+2, esc(T(l, "map.centre")))
	fmt.Fprintf(&b, `<text class="t3" x="40" y="%.1f">%s</text></g>`, centreY+26, esc(shortURL(baseURL)))

	for i, s := range sites {
		y := topoTopPad + float64(i)*topoRow
		mid := y + topoBoxH/2

		// The link. Its state is the state of the site: a dashed line is a
		// site that has not spoken recently, and that is worth seeing before
		// reading any number.
		fmt.Fprintf(&b, `<path class="link %s" d="M270 %.1f H%.0f V%.1f H%.0f"/>`,
			s.SLED, centreY, topoBusX, mid, topoSiteX)
		fmt.Fprintf(&b, `<circle class="dot %s" cx="%.0f" cy="%.1f" r="4"/>`,
			s.SLED, topoSiteX, mid)

		fmt.Fprintf(&b, `<g class="node site"><rect x="%.0f" y="%.1f" width="%.0f" height="%.0f" rx="4"/>`,
			topoSiteX, y, topoBoxW, topoBoxH)

		// First line: who this is, and — right-aligned, with the dot beside
		// the text rather than inside it — how it is doing.
		label := s.Site.Name
		if s.Local {
			label += " · " + T(l, "site.here")
		}
		fmt.Fprintf(&b, `<text class="t1" x="%.0f" y="%.1f">%s</text>`,
			topoSiteX+18, y+27, esc(label))
		state := s.State
		if s.Seen != "" {
			state += " · " + s.Seen
		}
		fmt.Fprintf(&b, `<circle class="dot %s" cx="%.0f" cy="%.1f" r="5"/>`,
			s.SLED, topoSiteX+topoBoxW-18, y+22)
		fmt.Fprintf(&b, `<text class="t3 right" x="%.0f" y="%.1f">%s</text>`,
			topoSiteX+topoBoxW-32, y+26, esc(state))

		// Second line the network and what stands in it, third what it holds.
		// Two lines rather than one, because one ran out of box.
		fmt.Fprintf(&b, `<text class="t3" x="%.0f" y="%.1f">%s · %s</text>`,
			topoSiteX+18, y+49, esc(s.Net), esc(T(l, "map.counts", s.Racks, s.Blades)))
		fmt.Fprintf(&b, `<text class="t3" x="%.0f" y="%.1f">%s</text>`,
			topoSiteX+18, y+68, esc(s.Stock))

		// One square per slot, in the colour it has on its BladeRunner page.
		x := topoSiteX + 18
		cy := y + 80
		for j, c := range s.Cells {
			if j >= 48 {
				fmt.Fprintf(&b, `<text class="t3" x="%.1f" y="%.1f">…</text>`, x+4, cy+11)
				break
			}
			fmt.Fprintf(&b, `<rect class="cell %s" x="%.1f" y="%.1f" width="9" height="13" rx="1.5">`+
				`<title>%s</title></rect>`, c.Class, x, cy, esc(c.Title))
			x += 11
		}
		if len(s.Cells) == 0 {
			fmt.Fprintf(&b, `<text class="t3" x="%.0f" y="%.1f">%s</text>`,
				topoSiteX+18, cy+11, esc(T(l, "site.norack")))
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func esc(s string) string { return template.HTMLEscapeString(s) }

func shortURL(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return strings.TrimSuffix(u, "/")
}

var topoTmpl = template.Must(template.New("map").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top"><div class="topbar"><div>
  <p class="crumb"><a href="/">{{mark}} {{t .L "nav.overview"}}</a> / {{t .L "nav.map"}}</p>
  <h1>{{t .L "map.title"}}</h1>
  <p class="sub">{{t .L "map.lead"}}</p>
</div>` + topRight + `</div>
<div class="meta"><span>{{t .L "site.title"}} <b>{{len .Sites}}</b></span>
<span>{{t .L "meta.blades"}} <b>{{.Blades}}</b></span></div>
</header>

{{if .Open}}<div class="bad">{{th .L "warn.open"}}</div>{{end}}
{{if .Msg}}<div class="note">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="bad">{{.Err}}</div>{{end}}

<div class="card">
  <div class="card-head"><h2>{{t .L "map.title"}}</h2>
    <span class="tag">{{t .L "map.legend"}}</span></div>
  <div class="body topo-wrap">{{.SVG}}</div>
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "site.title"}}</h2></div>
  <div class="body" style="padding:0">
    <table>
      <thead><tr><th>{{t .L "site.name"}}</th><th>{{t .L "site.net"}}</th>
        <th>{{t .L "meta.racks"}}</th><th>{{t .L "meta.blades"}}</th>
        <th>{{t .L "stock.title"}}</th><th>{{t .L "th.status"}}</th></tr></thead>
      <tbody>{{range .Sites}}
        <tr>
          <td><a class="name" href="/sites/{{.Site.ID}}">{{.Site.Name}}</a>{{if .Local}} <span class="chip ok">{{t $.L "site.here"}}</span>{{end}}
            {{if .Site.Location}}<div class="mono sub2">{{.Site.Location}}</div>{{end}}</td>
          <td class="mono">{{.Net}}</td>
          <td class="mono num">{{.Racks}}</td>
          <td class="mono num">{{.Blades}}</td>
          <td><span class="chip {{.SkLED}}">{{.Stock}}</span></td>
          <td><span class="chip {{.SLED}}">{{.State}}</span>
            {{if .Seen}}<div class="mono sub2">{{.Seen}}</div>{{end}}</td>
        </tr>
      {{end}}</tbody>
    </table>
  </div>
</div>

<footer><span><a href="/">← {{t .L "nav.overview"}}</a><br><span class="tm">{{t .L "foot.tm"}}</span></span>
<span>{{t .L "foot.api"}}</span></footer>
</div></body></html>`))

// hUISitePolicy saves the numbers a site judges by. An empty field means
// "inherit", which is why they are parsed as optional rather than defaulted
// to zero — a critical temperature of nought would be a strange thing to mean.
func (a *App) hUISitePolicy(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	to := "/sites/" + strconv.FormatInt(id, 10)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, to, "err", T(l, "err.form"))
		return
	}
	numOrZero := func(name string) float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue(name)), 64)
		if err != nil {
			return 0
		}
		return v
	}
	p := Policy{
		SocWarn:    numOrZero("soc_warn"),
		SocCrit:    numOrZero("soc_crit"),
		NVMeWarn:   numOrZero("nvme_warn"),
		DiskWarn:   numOrZero("disk_warn"),
		DiskCrit:   numOrZero("disk_crit"),
		OfflineMin: int(numOrZero("offline_min")),
	}
	if p.SocCrit != 0 && p.SocWarn != 0 && p.SocCrit <= p.SocWarn {
		redirectMsg(w, r, to, "err", T(l, "err.policyorder"))
		return
	}
	if p.DiskCrit != 0 && p.DiskWarn != 0 && p.DiskCrit <= p.DiskWarn {
		redirectMsg(w, r, to, "err", T(l, "err.policyorder"))
		return
	}
	if err := a.setSitePolicy(id, p); err != nil {
		redirectMsg(w, r, to, "err", err.Error())
		return
	}
	a.logEvent("", "info", "policy of site "+a.siteName(id)+" changed")
	redirectMsg(w, r, to, "msg", T(l, "msg.policysaved"))
}

// imageNote says what an image will and will not be able to do once written.
// The point is the fan unit: it hangs off UART5, which has to be enabled by
// the firmware, and an image running the upstream kernel never gets that far
// — the firmware applies no device-tree directive there at all. Saying so in
// the catalogue costs one line; finding it out costs an evening.
func imageNote(l Lang, im Image) (string, string) {
	switch im.Kernel {
	case "upstream":
		return T(l, "img.upstream"), "warn"
	case "downstream":
		return T(l, "img.downstream"), "ok"
	}
	return T(l, "img.unknownkernel"), "off"
}

// ── Site detail ──────────────────────────────────────────────────────
//
// One site, in full: what stands in it, what it holds, and how it is doing.
// The image stock is the reason this page exists — "two images ready" is
// enough on an overview and useless when an installation is waiting and
// someone needs to know which image, how big, and who is waiting for it.

type stockRow struct {
	ImageID    string
	Kernel     string
	KernelNote string
	State      string // translated
	SLED       string
	Here       string // size on the site
	Catalog    string // size in the catalogue
	Complete   bool
	Wanted     int // blades at this site assigned to this image
	Waiting    int // of those, still waiting for their installation
	Note       string
	Seen       string
}

func (a *App) hSitePage(w http.ResponseWriter, r *http.Request) {
	l := a.resolveLang(w, r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	st, err := a.getSite(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	blades, _ := a.listBlades()
	racks, _ := a.listRacks()
	images, _ := a.listImages()
	stock := a.siteImages()[id]

	// Which images the blades here are assigned to, and how many want each.
	// Assigned is not the same as waiting: a blade that has been running its
	// image for a week is assigned to it and waiting for nothing.
	wanted := map[string]int{}
	waiting := map[string]int{}
	used := 0
	for _, b := range blades {
		if b.SiteID != id {
			continue
		}
		if b.RackID != nil && b.Slot != nil {
			used++
		}
		if b.Image == "" {
			continue
		}
		wanted[b.Image]++
		if b.InstallState != installDone {
			waiting[b.Image]++
		}
	}

	inCatalog := map[string]Image{}
	for _, im := range images {
		inCatalog[im.ID] = im
	}
	held := map[string]SiteImageState{}
	for _, s := range stock {
		held[s.ImageID] = s
	}

	// Every image that either stands here or is wanted here. An assigned
	// image the site has not fetched is the interesting row, and a list of
	// what the site holds would be missing exactly that one.
	seen := map[string]bool{}
	var rows []stockRow
	add := func(imgID string) {
		if imgID == "" || seen[imgID] {
			return
		}
		seen[imgID] = true
		row := stockRow{ImageID: imgID, Wanted: wanted[imgID], Waiting: waiting[imgID]}
		cat, okCat := inCatalog[imgID]
		if okCat {
			row.Kernel = cat.Kernel
			row.KernelNote, _ = imageNote(l, cat)
		}
		if okCat && cat.Bytes > 0 {
			row.Catalog = human(cat.Bytes)
		}
		h, okHeld := held[imgID]
		switch {
		case !okHeld:
			row.State, row.SLED = T(l, "stock.absent"), "off"
		case h.State == "ready":
			row.State, row.SLED = T(l, "stock.state.ready"), "ok"
			row.Here = human(h.Bytes)
			row.Complete = !okCat || cat.Bytes == 0 || h.Bytes == cat.Bytes
		case h.State == "fetching":
			row.State, row.SLED = T(l, "stock.state.fetching"), "warn"
			row.Here = human(h.Bytes)
		default:
			row.State, row.SLED = T(l, "stock.state.error"), "crit"
			row.Note = h.Note
		}
		if okHeld {
			row.Seen = ago(l, h.TS)
		}
		rows = append(rows, row)
	}
	for _, s := range stock {
		add(s.ImageID)
	}
	for _, im := range images {
		if wanted[im.ID] > 0 {
			add(im.ID)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ImageID < rows[j].ImageID })

	var rvs []rackView
	for _, rk := range racks {
		if rk.SiteID == id {
			rvs = append(rvs, a.buildRackView(rk, blades, l))
		}
	}

	key, led, seenAt := siteHealth(l, *st)
	msg, errMsg := flash(r)
	render(w, siteTmpl, map[string]any{
		"L":        l,
		"Path":     "/sites/" + strconv.FormatInt(id, 10),
		"S":        *st,
		"Net":      st.NetBase + ".0/24",
		"Pool":     fmt.Sprintf(".%d–.%d", st.PoolFrom, st.PoolTo),
		"State":    T(l, key),
		"SLED":     led,
		"Seen":     seenAt,
		"HasToken": st.Token != "",
		"Pol":      a.siteOwnPolicy(id),
		"Eff":      a.sitePolicy(id),
		"Glob":     a.globalPolicy(),
		"Stock":    rows,
		"Racks":    rvs,
		"Used":     used,
		"Msg":      msg,
		"Err":      errMsg,
		"Open":     a.adminToken == "",
	})
}

// human renders a byte count the way someone reads it out loud.
func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/float64(1<<20))
	case n > 0:
		return fmt.Sprintf("%d B", n)
	}
	return "—"
}

var siteTmpl = template.Must(template.New("site").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top"><div class="topbar"><div>
  <p class="crumb"><a href="/">{{mark}} {{t .L "nav.overview"}}</a> / <a href="/map">{{t .L "nav.map"}}</a> / {{t .L "site.one"}}</p>
  <h1>{{.S.Name}} <span class="chip {{.SLED}}">{{.State}}{{if .Seen}} · {{.Seen}}{{end}}</span></h1>
  <p class="sub">{{.Net}} · {{t .L "site.pool"}} {{.Pool}}{{if .S.Location}} · {{.S.Location}}{{end}}</p>
</div>` + topRight + `</div>
<div class="meta"><span>{{t .L "meta.racks"}} <b>{{len .Racks}}</b></span>
<span>{{t .L "meta.used"}} <b>{{.Used}}</b></span>
<span>{{t .L "stock.title"}} <b>{{len .Stock}}</b></span></div>
</header>

{{if .Open}}<div class="bad">{{th .L "warn.open"}}</div>{{end}}
{{if .Msg}}<div class="note">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="bad">{{.Err}}</div>{{end}}
{{if not .HasToken}}<div class="bad">{{t .L "site.notoken"}}</div>{{end}}

<div class="card">
  <div class="card-head"><h2>{{t .L "stock.detail"}}</h2>
    <span class="tag">{{t .L "stock.hint"}}</span></div>
  {{if .Stock}}
  <table>
    <thead><tr><th>{{t .L "th.image"}}</th><th>{{t .L "th.status"}}</th>
      <th>{{t .L "stock.here"}}</th><th>{{t .L "stock.catalog"}}</th>
      <th>{{t .L "stock.assigned"}}</th><th>{{t .L "th.last"}}</th></tr></thead>
    <tbody>{{$l := .L}}{{range .Stock}}
      <tr>
        <td class="mono">{{.ImageID}}
          {{if .Kernel}}<div class="mono sub2">{{.KernelNote}}</div>{{end}}
          {{if .Note}}<div class="mono sub2">{{.Note}}</div>{{end}}</td>
        <td><span class="chip {{.SLED}}">{{.State}}</span></td>
        <td class="mono num">{{if .Here}}{{.Here}}{{else}}—{{end}}
          {{if and .Here (not .Complete)}}<div class="mono sub2">{{t $l "stock.partial"}}</div>{{end}}</td>
        <td class="mono num">{{if .Catalog}}{{.Catalog}}{{else}}—{{end}}</td>
        <td class="mono num">{{if .Wanted}}{{.Wanted}}{{else}}—{{end}}
          {{if .Waiting}}<div class="mono sub2">{{t $l "stock.waiting" .Waiting}}</div>{{end}}</td>
        <td class="mono">{{if .Seen}}{{.Seen}}{{else}}—{{end}}</td>
      </tr>
    {{end}}</tbody>
  </table>
  {{else}}
  <div class="body empty">{{t .L "stock.none"}}</div>
  {{end}}
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "ov.racks"}}</h2></div>
  {{if .Racks}}
  <div class="body"><div class="grid">
  {{$l := .L}}
  {{range .Racks}}
    <div class="rackcard">
      <a class="name" href="/bladerunners/{{.Rack.ID}}">{{.Rack.Name}}</a>
      <div class="tag" style="margin-top:.3rem">{{t $l "ov.slots" .Rack.Size}}{{if .Rack.Location}} · {{.Rack.Location}}{{end}}</div>
      <a class="cells" href="/bladerunners/{{.Rack.ID}}" aria-label="{{t $l "ov.occupancy" .Used .Free}}">
        {{range .Cells}}<span class="cell {{.Class}}" title="{{.Title}}">{{printf "%02d" .Slot}}</span>{{end}}
      </a>
      <div class="mono cellnote">{{.From}} – {{.To}}</div>
      <div class="tag">{{t $l "ov.occupancy" .Used .Free}}</div>
    </div>
  {{end}}
  </div></div>
  {{else}}
  <div class="body empty">{{t .L "site.norack"}}</div>
  {{end}}
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "pol.title"}}</h2>
    <span class="tag">{{t .L "pol.hint"}}</span></div>
  <div class="body">
    <form method="post" action="/sites/{{.S.ID}}/policy">
      <div class="row">
        <div class="narrow"><label for="sw">{{t .L "pol.socwarn"}}</label>
          <input id="sw" type="number" step="1" name="soc_warn" value="{{if .Pol.SocWarn}}{{.Pol.SocWarn}}{{end}}"
                 placeholder="{{.Glob.SocWarn}}"></div>
        <div class="narrow"><label for="sc">{{t .L "pol.soccrit"}}</label>
          <input id="sc" type="number" step="1" name="soc_crit" value="{{if .Pol.SocCrit}}{{.Pol.SocCrit}}{{end}}"
                 placeholder="{{.Glob.SocCrit}}"></div>
        <div class="narrow"><label for="nw">{{t .L "pol.nvmewarn"}}</label>
          <input id="nw" type="number" step="1" name="nvme_warn" value="{{if .Pol.NVMeWarn}}{{.Pol.NVMeWarn}}{{end}}"
                 placeholder="{{.Glob.NVMeWarn}}"></div>
        <div class="narrow"><label for="dw">{{t .L "pol.diskwarn"}}</label>
          <input id="dw" type="number" step="1" name="disk_warn" value="{{if .Pol.DiskWarn}}{{.Pol.DiskWarn}}{{end}}"
                 placeholder="{{.Glob.DiskWarn}}"></div>
        <div class="narrow"><label for="dc">{{t .L "pol.diskcrit"}}</label>
          <input id="dc" type="number" step="1" name="disk_crit" value="{{if .Pol.DiskCrit}}{{.Pol.DiskCrit}}{{end}}"
                 placeholder="{{.Glob.DiskCrit}}"></div>
        <div class="narrow"><label for="om">{{t .L "pol.offline"}}</label>
          <input id="om" type="number" step="1" name="offline_min" value="{{if .Pol.OfflineMin}}{{.Pol.OfflineMin}}{{end}}"
                 placeholder="{{.Glob.OfflineMin}}"></div>
        <div class="narrow"><button type="submit">{{t .L "form.save"}}</button></div>
      </div>
    </form>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "pol.inherit"}}
      {{t .L "pol.current" .Eff.SocWarn .Eff.SocCrit .Eff.DiskWarn .Eff.DiskCrit .Eff.OfflineMin}}</p>
  </div>
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "site.edit"}}</h2></div>
  <div class="body">
    <form method="post" action="/sites/{{.S.ID}}">
      <div class="row">
        <div><label for="n">{{t .L "site.name"}}</label>
          <input id="n" type="text" name="name" value="{{.S.Name}}" required maxlength="60"></div>
        <div class="narrow"><label for="net">{{t .L "site.net"}}</label>
          <input id="net" type="text" name="net_base" value="{{.S.NetBase}}" required
                 pattern="[0-9]+\.[0-9]+\.[0-9]+"></div>
        <div><label for="loc">{{t .L "form.location"}}</label>
          <input id="loc" type="text" name="location" value="{{.S.Location}}" maxlength="60"></div>
        <div class="narrow"><label for="pf">{{t .L "site.poolfrom"}}</label>
          <input id="pf" type="number" name="pool_from" value="{{.S.PoolFrom}}" min="1" max="254"></div>
        <div class="narrow"><label for="pt">{{t .L "site.poolto"}}</label>
          <input id="pt" type="number" name="pool_to" value="{{.S.PoolTo}}" min="1" max="254"></div>
        <div class="narrow"><button type="submit">{{t .L "form.save"}}</button></div>
      </div>
    </form>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "site.movehint"}}</p>
  </div>
</div>

<footer><span><a href="/">← {{t .L "nav.overview"}}</a><br><span class="tm">{{t .L "foot.tm"}}</span></span>
<span>{{t .L "site.one"}} {{.S.ID}}</span></footer>
</div></body></html>`))
