package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Views ────────────────────────────────────────────────────────────

type slotView struct {
	Armed    bool  // an installation or erase waits for the next netboot
	SiteID   int64 // for a blade with no slot: the site that last saw it
	SiteName string
	Devices  []installDevice // what this blade says it has to install onto
	Target   string          // the one it would be installed to
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
				MAC: bladeMAC(rk.SiteID, idx, s),
			})
			continue
		}
		rv.Used++
		lvl, reasons := a.evalHealth(&b)
		h := healthMap(&b)
		statusKey, statusLED, statusArg := a.rowStatus(&b, lvl)
		sv := slotView{
			Devices: a.installDevices(&b), Target: a.installTarget(&b),
			Slot: s, Serial: b.Serial, Hostname: b.Hostname, IP: b.IP, MAC: b.MAC,
			Image: b.Image, State: b.State, Ago: ago(l, b.LastSeen),
			Armed:   b.InstallState == installPending || b.InstallState == installWipe,
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
			sid, sname := a.siteLastSaw(b.Serial)
			unassigned = append(unassigned, slotView{
				Serial: b.Serial, Hostname: b.Hostname, MAC: b.MAC,
				State: b.State, LED: ledFor(b.State), Ago: ago(l, b.LastSeen),
				SiteID: sid, SiteName: sname,
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
	if st, err := a.defaultSite(); err == nil {
		poolFrom, poolTo = st.PoolFrom, st.PoolTo
	}
	var nextOff int
	var offErr error
	if st, err := a.defaultSite(); err == nil {
		nextOff, offErr = a.nextRackOffset(st.ID)
	} else {
		offErr = err
	}

	render(w, overviewTmpl, map[string]any{
		"L":          l,
		"Path":       "/",
		"Admin":      a.isAdmin(r),
		"LocalSite":  a.localSiteID(),
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
		"NameWarns":  a.checkNames(l),
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
	Pay    string // said only when it is not the payload the centre has
	PayLED string
	Racks  []rackView
	Blades int
	Used   int
	Free   int
}

// localSiteID is the site this server considers "here" — the menu links to
// it, and zero means there is none to link to.
func (a *App) localSiteID() int64 {
	if st, err := a.localSite(); err == nil {
		return st.ID
	}
	return 0
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
	// A site that answers on time and cannot write is the case this exists
	// for: everything else a site reports is reading, and reading goes on
	// working long after the disk has gone read-only. It has to beat "online"
	// or it is invisible.
	if st.Trouble != "" && time.Since(t) < 15*time.Minute {
		return "site.trouble", "crit", seen
	}
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
	centre := a.payload()
	for _, st := range sites {
		key, led, seen := siteHealth(l, st)
		sk, skLED := stockText(l, stock[st.ID])
		// Silence where it matches: a header that repeats what is expected
		// teaches people to stop reading it.
		payKey, payLED := payloadState(centre, st.Payload)
		payText := ""
		if payLED != "" && payLED != "ok" {
			payText = T(l, payKey)
		}
		out = append(out, siteGroup{
			Site:   st,
			Local:  st.ID == local,
			Net:    st.NetBase + ".0/24",
			Pool:   fmt.Sprintf(".%d–.%d", st.PoolFrom, st.PoolTo),
			State:  T(l, key),
			SLED:   led,
			Seen:   seen,
			Stock:  sk,
			SkLED:  skLED,
			Pay:    payText,
			PayLED: payLED,
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
	st.HostPrefix = strings.ToLower(strings.TrimSpace(r.FormValue("host_prefix")))
	st.Lease = strings.ToLower(strings.TrimSpace(r.FormValue("lease")))
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
	renamed := 0
	if st.HostPrefix != old.HostPrefix {
		renamed, _ = a.renameSiteBlades(id)
	}
	// Addresses are derived from the site, so a moved network moves every
	// blade standing in it — the reservations must be rewritten at once.
	note := T(l, "msg.sitesaved", st.Name)
	if renamed > 0 {
		note += " — " + fmt.Sprintf(T(l, "msg.renamed"), renamed)
	}
	// A rename has to reach the reservations as surely as a moved network
	// does: the name is in the file dnsmasq hands out.
	if old.NetBase != st.NetBase || renamed > 0 {
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
		"Admin":     a.isAdmin(r),
		"LocalSite": a.localSiteID(),
		"R":         rv,
		"Free":      free,
		"Images":    images,
		"Msg":       msg,
		"Err":       errMsg,
		"Sites":     a.siteChoices(),
		// Erasing destroys data, so it takes both: a site that allows it at
		// all, and an administrator asking.
		"CanWipe":   !a.sitePolicy(rk.SiteID).NoWipe && a.isAdmin(r),
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
		st, err := a.defaultSite()
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
	// Set only on a line that waited out an outage at its site: it says when
	// the centre finally heard about it. Every other line stays as short as
	// it was.
	Late string
	// Who did it, where a person did rather than the server itself.
	By string
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
		if r, err := time.Parse(time.RFC3339, e.Received); err == nil {
			lv.Late = fmt.Sprintf(T(l, "log.late"), r.Local().Format("15:04:05"))
		}
		lv.By = e.Actor
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
	// The choice is kept — it may well be made before the device is — but
	// saying nothing until somebody presses "Install now" is how an evening
	// gets spent on a blade that wrote for an hour and booted from nowhere.
	b, _ = a.getBlade(serial)
	if err := a.checkTarget(b); err != nil {
		redirectMsg(w, r, to, "err", fmt.Sprintf(T(l, "msg.imagesetbut"), img, errText(l, err)))
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
	if kind == "wipe" {
		if !a.mayAct(r, kind) {
			redirectMsg(w, r, to, "err", T(l, "err.notallowed"))
			return
		}
		a.hUIBladeWipe(w, r)
		return
	}
	switch kind {
	case "identify", "identify_off", "stealth_on", "stealth_off", "reboot", "shutdown", "reimage", "cancel", "probe", "reset":
	default:
		redirectMsg(w, r, to, "err", T(l, "err.unknownact"))
		return
	}
	// The role decides which of them, and the table it reads is the boundary
	// — not the buttons the page happens to draw.
	if !a.mayAct(r, kind) {
		redirectMsg(w, r, to, "err", T(l, "err.notallowed"))
		return
	}
	if kind == "reset" {
		if err := a.resetBlade(serial); err != nil {
			redirectMsg(w, r, to, "err", errText(l, err))
			return
		}
		redirectMsg(w, r, backTo(r, "/"), "msg", fmt.Sprintf(T(l, "msg.reset"), bladeName(b)))
		return
	}
	if kind == "shutdown" {
		if err := a.requestShutdown(serial); err != nil {
			redirectMsg(w, r, to, "err", errText(l, err))
			return
		}
		redirectMsg(w, r, to, "msg", fmt.Sprintf(T(l, "msg.halting"), bladeName(b)))
		return
	}
	if kind == "probe" {
		if err := a.requestProbe(serial); err != nil {
			redirectMsg(w, r, to, "err", errText(l, err))
			return
		}
		redirectMsg(w, r, to, "msg", fmt.Sprintf(T(l, "msg.probing"), bladeName(b)))
		return
	}
	if kind == "cancel" {
		if err := a.cancelInstall(serial); err != nil {
			redirectMsg(w, r, to, "err", errText(l, err))
			return
		}
		name := b.Hostname
		if name == "" {
			name = serial
		}
		redirectMsg(w, r, to, "msg", fmt.Sprintf(T(l, "msg.cancelled"), name))
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
		a.logActed(a.actor(r), serial, "info", "install requested")
		redirectMsg(w, r, to, "msg", T(l, "msg.installrequested", b.Image))
		return
	}
	if _, err := a.db.Exec(`INSERT INTO commands(serial,kind,created) VALUES(?,?,?)`,
		serial, kind, now()); err != nil {
		redirectMsg(w, r, to, "err", err.Error())
		return
	}
	a.logActed(a.actor(r), serial, "info", "command queued: "+kind)
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
	// Sitting in the mini OS. It has an address, it has asked the site what to
	// do, and it is waiting — which looks like "no agent yet" from the
	// outside and is a quite different thing: the blade is up, it is talking,
	// and it is one image away from being installed.
	if stage, fresh := a.stageOnTheWire(b.Serial); fresh {
		switch stage {
		case stageInstaller, stageRamdisk:
			return "st.installer", "id", ""
		}
	}
	// Reset and put aside. It is in no slot and expected nowhere; whatever it
	// is doing is not news.
	if b.Stored != "" {
		return "st.stored", "off", ""
	}
	// Switched off on purpose. "Offline" would be true and useless: it is the
	// same word the interface uses for a blade whose power supply died.
	if b.Halted != "" {
		return "st.halted", "off", ""
	}
	// Armed to come up in the mini OS once so its firmware can be read. It is
	// a state somebody switched on and will want to see the end of, and while
	// it stands the blade is about to restart twice.
	if probeArmed(b) {
		return "blade.probing", "id", ""
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

// markSVG is the mark: the scabbard itself, worn at an angle — throat to the
// upper right, tip to the lower left, the way a sheath hangs rather than the
// way a diagram stands. Three cut-outs across it read as slots, the way a
// BladeRunner does seen from the front. The blade is deliberately absent: the
// sheath is the thing that holds, and what it holds is somebody else's.
//
// An earlier version drew grip and crossguard too, which at 28 pixels beside
// the wordmark reads as a screw — and 28 pixels is the size this has to work
// at. One path with fill-rule="evenodd", so the slots are holes: no
// background of its own, and currentColor carries it into dark mode.
const markSVG = `<svg class="mark" viewBox="0 0 24 24" aria-hidden="true" focusable="false">` +
	`<path fill="currentColor" fill-rule="evenodd" transform="rotate(45 12 12)" d="` +
	`M6.2 2.6 H17.8 V6 H16.3 V17.3 L12 22.6 L7.7 17.3 V6 H6.2 Z ` +
	`M9.4 8 H14.6 V9.9 H9.4 Z ` +
	`M9.4 11.2 H14.6 V13.1 H9.4 Z ` +
	`M9.4 14.4 H14.6 V16.3 H9.4 Z"/></svg>`

var tmplFuncs = template.FuncMap{
	"hasPrefix": strings.HasPrefix,
	"mark":      func() template.HTML { return template.HTML(markSVG) },
	"t":         T,
	// th returns translations that deliberately carry markup (a <code> around
	// a path, say). Only for text from the catalogue — never for input.
	"th": func(l Lang, key string, args ...any) template.HTML {
		return template.HTML(T(l, key, args...))
	},
	// The release this server is running. It belongs in the footer of every
	// page: the first question about any reported behaviour is which version
	// reported it, and the network the centre sits on — which is what stood
	// there — is on the sites page and needed no repeating.
	"ver":       func() string { return version },
	"upper":     strings.ToUpper,
	"otherLang": otherLang,
	"langName":  langName,
	"hsize":     human,
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
/* A wide table is allowed to be wide: squeezing ten columns into the page
   turned "Compute Module 4 Rev 1.1" into four lines and the slot chooser into
   a sliver. It scrolls in its own box instead, and the page does not. */
.tbl-wrap{overflow-x:auto}
.inv{min-width:72rem}
.inv td{vertical-align:top}
/* The name of a module is one thing and reads as one line. Broken over four,
   "Compute Module 4 Rev 1.1" stops looking like a name at all. */
.inv .board{white-space:nowrap}
.inv td:first-child{min-width:13rem}
.inv td.right{white-space:nowrap}
th{text-align:left;font:600 .78rem/1 ui-monospace,monospace;letter-spacing:.1em;
text-transform:uppercase;color:var(--ink-3);padding:.7rem .9rem;border-bottom:1px solid var(--rule-s)}
td{padding:.6rem .9rem;border-bottom:1px solid var(--rule);vertical-align:middle}
tr:last-child td{border-bottom:0}
.mono{font:.92rem/1.5 ui-monospace,monospace;color:var(--ink-2)}
/* An enrollment code is read off the screen and typed somewhere else, so it
   is set large enough to read at arm's length and spaced so the groups do not
   run together. */
.code{font:1.5rem/1.3 ui-monospace,monospace;letter-spacing:.12em;color:var(--ink);
  background:var(--surface-2);border:1px solid var(--rule);border-radius:8px;
  padding:.7rem 1rem;display:inline-block;margin:0 0 1rem;user-select:all}
.cmd{font:.9rem/1.5 ui-monospace,monospace;color:var(--ink-2);background:var(--surface-2);
  border:1px solid var(--rule);border-radius:8px;padding:.7rem 1rem;margin:.4rem 0 0;
  overflow-x:auto;white-space:pre;user-select:all}
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
/* The boxes carry a class rather than being matched as "rect": that selector
   was more specific than the one for the slot squares inside them, so every
   occupied slot was painted in the box colour and the map showed a rack of
   empty slots while three blades were running. */
svg.topo .box{fill:var(--surface-2);stroke:var(--rule-s);stroke-width:1}
svg.topo .centre .box{fill:var(--accent-soft);stroke:var(--accent)}
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
/* The menu bar. Quiet by default — it is orientation, not decoration — with
   the page you are on marked by weight and a rule rather than a colour, so it
   still reads in both themes. */
a.brand{display:flex;align-items:center;gap:.5rem;text-decoration:none;color:inherit}
a.brand:hover{color:var(--accent-ink)}
/* The settings form: fields in a grid that keeps its columns narrow instead
   of one input stretching across the page, and switches in a single column so
   the eye runs down a list rather than hunting across a row. */
.setgrid{display:grid;gap:.9rem 1.2rem;
  grid-template-columns:repeat(auto-fit,minmax(9rem,1fr));align-items:end}
.setgrid .wide{grid-column:span 2;min-width:0}
.setgrid input,.setgrid select{width:100%}
.checks{display:grid;gap:.55rem;margin:1.1rem 0 0;max-width:44rem}
label.check{display:flex;align-items:center;gap:.5rem;font:400 .95rem/1.4 inherit;
  text-transform:none;letter-spacing:0;color:var(--ink)}
label.check input{width:auto;margin:0}
.pagehead{margin:1.1rem 0 0}
.pagehead h1{margin:0}
.pagehead .sub{margin:.35rem 0 0}
.nav{display:flex;flex-wrap:wrap;gap:1.4rem;margin:.75rem 0 0;
  font:600 .8rem/1 ui-monospace,monospace;letter-spacing:.09em;text-transform:uppercase}
.nav a{color:var(--ink-3);text-decoration:none;padding-bottom:.35rem;
  border-bottom:2px solid transparent}
.nav a:hover{color:var(--accent-ink)}
.nav a.here{color:var(--ink);border-bottom-color:var(--accent)}
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
<title>Sheath</title>{{if .Refresh}}
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
    if (e.target.closest("a[href], button, input[type=submit], summary, label")) { stop(); }
  }, true);
  addEventListener("submit", stop, true);
  // Typing is work in progress. Without this the page reloads five seconds
  // into filling in a form and takes what was typed with it — which is what
  // it did to somebody adding a BladeRunner.
  addEventListener("focusin", function (e) {
    if (e.target.closest("input, select, textarea")) { stop(); }
  }, true);
  addEventListener("input", stop, true);
  addEventListener("keydown", stop, true);
})();
</script>{{end}}
<style>` + baseCSS + `</style></head><body>`

// topRight holds the language switch and sign-out — identical on every page.
// navBar is the one row that says where you can go and where you are. It
// replaced a subtitle repeating the site's network, which the site card on
// the same page already carried — a header should orient, not restate.
const navBar = `<nav class="nav" aria-label="{{t .L "nav.label"}}">
  <a href="/"{{if eq .Path "/"}} class="here" aria-current="page"{{end}}>{{t .L "nav.overview"}}</a>
  <a href="/map"{{if eq .Path "/map"}} class="here" aria-current="page"{{end}}>{{t .L "nav.map"}}</a>
  <a href="/inventory"{{if eq .Path "/inventory"}} class="here" aria-current="page"{{end}}>{{t .L "inv.title"}}</a>
  {{if .Admin}}<a href="/users"{{if eq .Path "/users"}} class="here" aria-current="page"{{end}}>{{t .L "nav.users"}}</a>{{end}}
  <a href="/images"{{if eq .Path "/images"}} class="here" aria-current="page"{{end}}>{{t .L "img.title"}}</a>
  <a href="/settings"{{if eq .Path "/settings"}} class="here" aria-current="page"{{end}}>{{t .L "set.title"}}</a>
  {{if .LocalSite}}<a href="/sites/{{.LocalSite}}"{{if hasPrefix .Path "/sites/"}} class="here" aria-current="page"{{end}}>{{t .L "site.title"}}</a>{{end}}
</nav>`

const topRight = `<div class="topright">
  <a class="langlink" href="/lang/{{otherLang .L}}?next={{.Path | urlquery}}"
     hreflang="{{otherLang .L}}">{{langName (otherLang .L)}}</a>
  <form method="post" action="/logout"><button class="ghost">{{t .L "btn.signout"}}</button></form>
</div>`

// brandBar is the head every page wears: the mark, the name, the controls,
// the menu. It used to differ per page — the overview carried the mark and
// the others a breadcrumb — which made the whole page jump on every
// navigation. A header that moves is a header nobody can aim at.
const brandBar = `<div class="topbar">
  <a class="brand" href="/">{{mark}}<span>Sheath</span></a>` + topRight + `</div>` + navBar

var overviewTmpl = template.Must(template.New("ov").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top">` + brandBar + `
<div class="pagehead"><h1>{{t .L "nav.overview"}}</h1></div>
<div class="meta"><span>{{t .L "meta.racks"}} <b>{{len .Racks}}</b></span>
<span>{{t .L "meta.blades"}} <b>{{.Blades}}</b></span>
<span>{{t .L "site.title"}} <b>{{len .Sites}}</b></span></div>
</header>

{{if .Open}}<div class="bad">{{th .L "warn.open"}}</div>{{end}}
{{if .Msg}}<div class="note">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="bad">{{.Err}}</div>{{end}}
{{range .Warnings}}<div class="bad"><b>{{t $.L "warn.net"}}:</b> {{.}}</div>{{end}}
{{range .NameWarns}}<div class="bad"><b>{{t $.L "warn.names"}}:</b> {{.}}</div>{{end}}

{{$l := .L}}
{{if .Racks}}
{{range .Sites}}
<div class="card"><div class="card-head">
  <h2><a href="/sites/{{.Site.ID}}">{{.Site.Name}}</a>{{if .Local}} <span class="chip ok">{{t $l "site.here"}}</span>{{end}}
    <span class="chip {{.SLED}}">{{.State}}{{if .Seen}} · {{.Seen}}{{end}}</span>
    <span class="chip {{.SkLED}}">{{.Stock}}</span>
    {{if .Pay}}<span class="chip {{.PayLED}}">{{.Pay}}</span>{{end}}</h2>
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
            {{if .Seen}}<div class="mono sub2">{{.Seen}}</div>{{end}}
            {{if .Pay}}<div><span class="chip {{.PayLED}}">{{.Pay}}</span></div>{{end}}</td>
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
      <th>{{t .L "site.one"}}</th>
      <th>{{t .L "th.status"}}</th><th>{{t .L "th.lastseen"}}</th></tr></thead>
    <tbody>{{range .Unassigned}}
      <tr><td><span class="led {{.LED}}"></span></td>
      <td class="mono">{{.Serial}}{{if .Hostname}}<div class="sub2">{{.Hostname}}</div>{{end}}</td>
      <td class="mono">{{if .MAC}}{{.MAC}}{{else}}—{{end}}</td>
      <td>{{if .SiteName}}<a href="/sites/{{.SiteID}}">{{.SiteName}}</a>
          <div class="mono sub2">{{t $.L "ov.sitesaw"}}</div>{{else}}<span class="hint">—</span>{{end}}</td>
      <td><span class="chip {{.LED}}">{{.State}}</span></td>
      <td class="mono">{{.Ago}}</td></tr>
    {{end}}</tbody>
  </table>
  <div class="body hint">{{t .L "ov.unassignedhint"}}</div>
</div>
{{end}}

<footer><span>{{t .L "foot.api"}}<br><span class="tm">{{t .L "foot.tm"}}</span></span>
<span class="mono">{{ver}}</span></footer>
</div></body></html>`))

var rackTmpl = template.Must(template.New("rack").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top">` + brandBar + `
<div class="pagehead">
  <h1>{{.R.Rack.Name}}</h1>
  <p class="sub">{{t .L "ov.slots" .R.Rack.Size}}{{if .R.Rack.Location}} · {{.R.Rack.Location}}{{end}}
   · {{t .L "rk.block" .R.From .R.To}}{{if .R.SiteName}} · {{t .L "site.one"}} {{.R.SiteName}}{{end}}</p>
</div>
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
                    {{range $top.Images}}<option value="{{.ID}}"{{if eq .ID $img}} selected{{end}}>{{.ID}}{{if .Kernel}} · {{.Kernel}}{{end}}{{if .PartTable}} · {{upper .PartTable}}{{end}}</option>{{end}}
                  </select>
                  <button class="mini ghost" type="submit">{{t $top.L "rk.set"}}</button>
                </form>

                {{if .Devices}}
                <form method="post" action="/blades/{{.Serial}}/target">
                  <label for="tgt{{.Slot}}">{{t $top.L "tgt.title"}}</label>
                  {{$cur := .Target}}
                  <select id="tgt{{.Slot}}" name="target">
                    {{range .Devices}}<option value="{{.Path}}"{{if eq .Path $cur}} selected{{end}}>{{.Label}} · {{hsize .Bytes}}</option>{{end}}
                  </select>
                  <button class="mini ghost" type="submit">{{t $top.L "tgt.set"}}</button>
                </form>
                {{end}}

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
                  <form method="post" action="/blades/{{.Serial}}/actions/shutdown"
                        onsubmit="return confirm('{{printf (t $top.L "act.haltask") (or .Hostname .Serial)}}')">
                    <button class="mini danger" type="submit" title="{{t $top.L "act.halttip"}}">{{t $top.L "act.halt"}}</button></form>
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
                  {{if .Armed}}
                  <form method="post" action="/blades/{{.Serial}}/actions/cancel">
                    <button class="mini" type="submit" title="{{t $top.L "act.canceltip"}}">{{t $top.L "act.cancel"}}</button></form>
                  {{else}}
                  <form method="post" action="/blades/{{.Serial}}/actions/reimage">
                    <button class="mini" type="submit" title="{{t $top.L "act.installtip"}}">{{t $top.L "act.install"}}</button></form>
                  {{end}}
                  {{if $top.Admin}}
                  <form method="post" action="/blades/{{.Serial}}/unassign">
                    <button class="mini danger" type="submit">{{t $top.L "act.remove"}}</button></form>
                  {{end}}
                </div>
                <div class="menu-row">
                  <form method="post" action="/blades/{{.Serial}}/actions/reset"
                        onsubmit="return confirm('{{printf (t $top.L "act.resetask") (or .Hostname .Serial)}}')">
                    <button class="mini danger" type="submit" title="{{t $top.L "act.resettip"}}">{{t $top.L "act.reset"}}</button></form>
                </div>
                <div class="menu-row">
                  <form method="post" action="/blades/{{.Serial}}/actions/probe"
                        onsubmit="return confirm('{{printf (t $top.L "act.probeask") (or .Hostname .Serial)}}')">
                    <button class="mini ghost" type="submit" title="{{t $top.L "act.probetip"}}">{{t $top.L "act.probe"}}</button></form>
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
                {{if $top.CanWipe}}
                <div class="menu-sep"></div>
                <form class="stack" method="post" action="/blades/{{.Serial}}/actions/wipe">
                  <label>{{t $top.L "act.wipe"}}</label>
                  <input type="text" name="confirm" placeholder="{{.Hostname}}"
                         autocomplete="off" spellcheck="false">
                  <button class="mini danger" type="submit">{{t $top.L "act.wipego"}}</button>
                </form>
                <div class="menu-note">{{t $top.L "act.wipehint"}}</div>
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
          <td><span class="led {{.LED}}"></span>{{.Msg}}{{if .By}}<span class="hint"> · {{.By}}</span>{{end}}{{if .Late}}<span class="hint"> · {{.Late}}</span>{{end}}</td>
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
<span>{{t .L "nav.rack"}} {{.R.Rack.ID}}</span>
<span class="mono">{{ver}}</span></footer>
</div></body></html>`))

var loginTmpl = template.Must(template.New("login").Funcs(tmplFuncs).Parse(headHTML + `
<div class="signin"><div class="signin-box">
<header class="top"><div class="topbar"><div>
  <h1 class="brand">{{mark}}<span>Sheath</span></h1>
  <p class="sub">{{t .L "login.lead"}}</p>
</div><a class="langlink" href="/lang/{{otherLang .L}}?next={{.Path | urlquery}}"
   hreflang="{{otherLang .L}}">{{langName (otherLang .L)}}</a></div></header>
{{if .Error}}<div class="bad">{{.Error}}</div>{{end}}
<div class="card"><div class="body">
  <form method="post" action="/login">
    <input type="hidden" name="next" value="{{.Next}}">
    {{if .HaveUsers}}
    <label for="us">{{t .L "login.user"}}</label>
    <input id="us" name="user" autofocus autocomplete="username" spellcheck="false">
    <label for="tk" style="margin-top:.8rem">{{t .L "usr.password"}}</label>
    <input id="tk" type="password" name="token" autocomplete="current-password" required>
    <p class="hint" style="margin:.6rem 0 0">{{t .L "login.astoken"}}</p>
    {{else}}
    <label for="tk">{{t .L "login.token"}}</label>
    <input id="tk" type="password" name="token" autofocus autocomplete="current-password" required>
    {{end}}
    <div style="margin-top:1.1rem"><button type="submit">{{t .L "login.submit"}}</button></div>
  </form>
  <p class="hint" style="margin:1.4rem 0 0">{{th .L "login.hint"}}</p>
  <pre style="margin:.6rem 0 0;padding:.6rem .7rem;background:var(--surface-2);border-radius:3px;
    font:.78rem/1.5 ui-monospace,monospace;overflow-x:auto;color:var(--ink-2)"><code>sudo cat /srv/sheath/data/admin-token</code></pre>
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
			// "free" is what an empty slot is called; counting those as
			// blades made a four-slot BladeRunner with one blade in it
			// report four.
			if c.Class != "free" {
				g.Blades++
			}
			g.Cells = append(g.Cells, c)
		}
	}

	msg, errMsg := flash(r)
	render(w, topoTmpl, map[string]any{
		"L":         l,
		"Path":      "/map",
		"Admin":     a.isAdmin(r),
		"LocalSite": a.localSiteID(),
		"Sites":     views,
		"SVG":       topoSVG(l, views, a.baseURL),
		"Blades":    len(blades),
		"Msg":       msg,
		"Err":       errMsg,
		"Open":      a.adminToken == "",
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
	fmt.Fprintf(&b, `<g class="node centre"><rect class="box" x="20" y="%.1f" width="250" height="96" rx="4"/>`,
		centreY-48)
	fmt.Fprintf(&b, `<text class="t1" x="40" y="%.1f">Sheath</text>`, centreY-20)
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

		fmt.Fprintf(&b, `<g class="node site"><rect class="box" x="%.0f" y="%.1f" width="%.0f" height="%.0f" rx="4"/>`,
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
<header class="top">` + brandBar + `
<div class="pagehead">
  <h1>{{t .L "map.title"}}</h1>
  <p class="sub">{{t .L "map.lead"}}</p>
</div>
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
<span>{{t .L "foot.api"}}</span>
<span class="mono">{{ver}}</span></footer>
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

// hUIBladeWipe arms the erase. Two guards, because this is the one action
// that destroys something a reinstall cannot bring back: the site may forbid
// it outright, and whoever asks has to type the blade's name — a slip of the
// mouse in a list of twenty rows should not empty a disk.
func (a *App) hUIBladeWipe(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	serial := r.PathValue("serial")
	b, err := a.getBlade(serial)
	if err != nil {
		redirectMsg(w, r, backTo(r, "/"), "err", T(l, "err.bladegone"))
		return
	}
	to := bladePage(b, r)
	if a.sitePolicy(b.SiteID).NoWipe {
		redirectMsg(w, r, to, "err", T(l, "err.wipeoff"))
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, to, "err", T(l, "err.form"))
		return
	}
	if strings.TrimSpace(r.FormValue("confirm")) != b.Hostname {
		redirectMsg(w, r, to, "err", T(l, "err.wipeconfirm", b.Hostname))
		return
	}
	if err := a.requestWipe(serial); err != nil {
		redirectMsg(w, r, to, "err", errText(l, err))
		return
	}
	// Netboot has to be armed for this, and the blade has to get there: the
	// agent reboots it, so nobody has to walk to the rack.
	if _, err := a.syncDHCP(); err != nil {
		redirectMsg(w, r, to, "err", T(l, "err.dhcpsync", errText(l, err)))
		return
	}
	if err := a.queueCommand(serial, "reboot"); err != nil {
		a.logEvent(serial, "warn", "erase armed, but no reboot could be queued: "+err.Error())
	}
	a.logEvent(serial, "warn", "erase requested for "+b.Hostname)
	redirectMsg(w, r, to, "msg", T(l, "msg.wipearmed", b.Hostname))
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
	code, left := a.enrollState(*st)
	centre := a.payload()
	payKey, payLED := payloadState(centre, st.Payload)
	msg, errMsg := flash(r)
	render(w, siteTmpl, map[string]any{
		"Example":   bladeHostname(st.HostPrefix, 1, 1),
		"Code":      code,
		"CodeLeft":  fmt.Sprintf(T(l, "enr.valid"), humanDur(l, left)),
		"EnrollCmd": enrollCommand(a.baseURL, code),
		"Pay":       T(l, payKey),
		"PayLED":    payLED,
		"Trouble":   st.Trouble,
		"PayHere":   shortOr(st.Payload),
		"PayCentre": shortOr(centre.Version),
		"SiteVer":   st.SiteVersion,
		"L":         l,
		"Path":      "/sites/" + strconv.FormatInt(id, 10),
		"Admin":     a.isAdmin(r),
		"LocalSite": a.localSiteID(),
		"S":         *st,
		"Net":       st.NetBase + ".0/24",
		"Pool":      fmt.Sprintf(".%d–.%d", st.PoolFrom, st.PoolTo),
		"State":     T(l, key),
		"SLED":      led,
		"Seen":      seenAt,
		"HasToken":  st.Token != "",
		"Pol":       a.siteOwnPolicy(id),
		"Eff":       a.sitePolicy(id),
		"Glob":      a.globalPolicy(),
		"Stock":     rows,
		"Racks":     rvs,
		"Used":      used,
		"Msg":       msg,
		"Err":       errMsg,
		"Open":      a.adminToken == "",
	})
}

// human renders a byte count the way someone reads it out loud — and in the
// units the thing itself is sold in. Disks, cards and downloads are counted in
// powers of ten by everyone who makes them: a 500 GB SSD holds 500·10⁹ bytes,
// and calling that "465.8 GB" is how an inventory ends up disagreeing with the
// sticker on the drive. Memory is the other way round and has ramText.
func human(n int64) string {
	const k = 1000
	switch {
	case n >= k*k*k*k:
		return fmt.Sprintf("%.1f TB", float64(n)/(k*k*k*k))
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.0f MB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.0f KB", float64(n)/k)
	case n > 0:
		return fmt.Sprintf("%d B", n)
	}
	return "—"
}

var siteTmpl = template.Must(template.New("site").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top">` + brandBar + `
<div class="pagehead">
  <h1>{{.S.Name}} <span class="chip {{.SLED}}">{{.State}}{{if .Seen}} · {{.Seen}}{{end}}</span></h1>
  <p class="sub">{{.Net}} · {{t .L "site.pool"}} {{.Pool}}{{if .S.Location}} · {{.S.Location}}{{end}}</p>
</div>
<div class="meta"><span>{{t .L "meta.racks"}} <b>{{len .Racks}}</b></span>
<span>{{t .L "meta.used"}} <b>{{.Used}}</b></span>
<span>{{t .L "stock.title"}} <b>{{len .Stock}}</b></span></div>
</header>

{{if .Open}}<div class="bad">{{th .L "warn.open"}}</div>{{end}}
{{if .Msg}}<div class="note">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="bad">{{.Err}}</div>{{end}}
{{if not .HasToken}}<div class="bad">{{t .L "site.notoken"}}</div>{{end}}

{{if .Admin}}
<div class="card">
  <div class="card-head"><h2>{{t .L "enr.title"}}</h2>
    <span class="tag">{{if .Code}}{{.CodeLeft}}{{else}}{{t .L "enr.gone"}}{{end}}</span></div>
  <div class="body">
    <p class="hint" style="margin:0 0 1rem">{{t .L "enr.lead"}}</p>
    {{if .Code}}
      <p class="code">{{.Code}}</p>
      <p class="hint">{{t .L "enr.run"}}</p>
      <pre class="cmd">{{.EnrollCmd}}</pre>
    {{else}}
      {{if .HasToken}}<p class="hint">{{t .L "enr.has"}}</p>{{end}}
      <form method="post" action="/sites/{{.S.ID}}/enroll"><button>{{t .L "enr.make"}}</button></form>
    {{end}}
  </div>
</div>
{{end}}

{{if .Trouble}}
<div class="card">
  <div class="card-head"><h2>{{t .L "site.trouble"}}</h2>
    <span class="chip crit">{{t .L "site.trouble"}}</span></div>
  <div class="body"><p class="mono" style="margin:0">{{.Trouble}}</p>
    <p class="hint" style="margin:.4rem 0 0">{{t .L "site.troublehint"}}</p></div>
</div>
{{end}}

<div class="card">
  <div class="card-head"><h2>{{t .L "pay.title"}}</h2>
    <span class="chip {{.PayLED}}">{{.Pay}}</span></div>
  <div class="body">
    <div class="meta">
      <span>{{t .L "pay.here"}} <b class="mono">{{.PayHere}}</b></span>
      <span>{{t .L "pay.centre"}} <b class="mono">{{.PayCentre}}</b></span>
      {{if .SiteVer}}<span>sheath-site <b class="mono">{{.SiteVer}}</b></span>{{end}}
    </div>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "pay.hint"}}</p>
  </div>
</div>

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
        <div class="narrow"><label for="ls">{{t .L "site.lease"}}</label>
          <input id="ls" type="text" name="lease" value="{{.S.Lease}}"
                 pattern="([0-9]+[smhd]|infinite)" placeholder="1h"></div>
        <div class="narrow"><label for="hp">{{t .L "site.prefix"}}</label>
          <input id="hp" type="text" name="host_prefix" value="{{.S.HostPrefix}}"
                 maxlength="8" pattern="[a-z0-9]*" placeholder="{{t .L "site.prefixph"}}"></div>
        <div class="narrow"><button type="submit">{{t .L "form.save"}}</button></div>
      </div>
    </form>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "site.movehint"}}</p>
    <p class="hint" style="margin:.4rem 0 0">{{t .L "site.prefixhint" .Example}}</p>
    <p class="hint" style="margin:.4rem 0 0">{{t .L "site.leasehint"}}</p>
  </div>
</div>

<footer><span><a href="/">← {{t .L "nav.overview"}}</a><br><span class="tm">{{t .L "foot.tm"}}</span></span>
<span>{{t .L "site.one"}} {{.S.ID}}</span>
<span class="mono">{{ver}}</span></footer>
</div></body></html>`))

// ── Settings ─────────────────────────────────────────────────────────
//
// The two sections a person actually turns knobs in: what the agent does on a
// blade, and how an installation is carried out. Everything else in the
// desired state — keys, files, units, binaries — is fleet plumbing that
// belongs in the API, and this page leaves it alone rather than rewriting it
// from a form that never saw it.

type settingsView struct {
	Interval    string
	Jitter      string
	RebootOnCfg bool
	Window      string
	Allow       string

	Target     string
	After      string
	RebootWait string
	NoGrow     bool
	NeedSum    bool
	NoRootKeys bool
	NoCloud    bool
	NoAgent    bool
}

func (a *App) hSettings(w http.ResponseWriter, r *http.Request) {
	l := a.resolveLang(w, r)
	cfg := a.configFor("global")
	ag, _ := cfg["agent"].(map[string]any)
	in, _ := cfg["install"].(map[string]any)
	msg, errMsg := flash(r)

	numStr := func(m map[string]any, k string) string {
		if v, ok := num(m[k]); ok && v != 0 {
			return fmt.Sprintf("%.0f", v)
		}
		return ""
	}
	str := func(m map[string]any, k string) string {
		v, _ := m[k].(string)
		return v
	}
	flag := func(m map[string]any, k string) bool {
		v, _ := m[k].(bool)
		return v
	}

	sv := settingsView{
		Interval: numStr(ag, "interval"), Jitter: numStr(ag, "jitter"),
		RebootOnCfg: flag(ag, "reboot_on_boot_config"), Window: str(ag, "maintenance"),
		Allow:      strings.Join(stringList(ag["allow"]), ", "),
		Target:     str(in, "install_target"),
		After:      str(in, "after"),
		RebootWait: numStr(in, "reboot_delay"),
		NoGrow:     flag(in, "no_grow"), NeedSum: flag(in, "require_checksum"),
		NoRootKeys: flag(in, "no_root_keys"), NoCloud: flag(in, "no_cloud_init"),
		NoAgent: flag(in, "no_agent"),
	}
	mc := a.mailConf()
	openAl, _ := a.openAlerts()
	alerts := make([]alertView, 0, len(openAl))
	for _, al := range openAl {
		hn := al.Serial
		if b, err := a.getBlade(al.Serial); err == nil && b.Hostname != "" {
			hn = b.Hostname
		}
		led := "warn"
		if al.Level == "crit" {
			led = "bad"
		}
		alerts = append(alerts, alertView{
			Name: hn, Level: al.Level, LED: led, Reason: al.Reason,
			Since: al.Since.Local().Format("2006-01-02 15:04"),
			Sent:  al.Notified != "",
		})
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].Name < alerts[j].Name })

	bk := a.backupInfo()
	last := T(l, "bk.none")
	if !bk.Last.IsZero() {
		last = bk.Last.Local().Format("2006-01-02 15:04") + " · " + human(bk.Size)
	}
	render(w, settingsTmpl, map[string]any{
		"L": l, "Path": "/settings", "LocalSite": a.localSiteID(), "Admin": a.isAdmin(r),
		"S": sv, "Msg": msg, "Err": errMsg, "Open": a.adminToken == "",
		"BK": bk, "BKLast": last,
		"NF": mc, "HasPass": mc.Pass != "", "Alerts": alerts,
	})
}

// hSettingsSave writes only the two sections this page owns, merged into what
// is already there. The form has never seen the keys, files or binaries, so
// it must not be able to remove them — a lesson from a single API call that
// emptied the global configuration.
func (a *App) hSettingsSave(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, "/settings", "err", T(l, "err.form"))
		return
	}
	numOr := func(name string) any {
		v := strings.TrimSpace(r.FormValue(name))
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil
		}
		return n
	}
	onOff := func(name string) any {
		if r.FormValue(name) != "" {
			return true
		}
		return nil // absent means "not set", not "false"
	}
	set := func(m map[string]any, k string, v any) {
		if v == nil {
			delete(m, k)
			return
		}
		m[k] = v
	}

	cfg := a.configFor("global")
	ag, _ := cfg["agent"].(map[string]any)
	if ag == nil {
		ag = map[string]any{}
	}
	in, _ := cfg["install"].(map[string]any)
	if in == nil {
		in = map[string]any{}
	}

	set(ag, "interval", numOr("interval"))
	set(ag, "jitter", numOr("jitter"))
	set(ag, "reboot_on_boot_config", onOff("reboot_on_boot_config"))
	if win := strings.TrimSpace(r.FormValue("maintenance")); win != "" {
		if !validWindow(win) {
			redirectMsg(w, r, "/settings", "err", T(l, "err.window"))
			return
		}
		ag["maintenance"] = win
	} else {
		delete(ag, "maintenance")
	}
	if v := strings.TrimSpace(r.FormValue("allow")); v != "" {
		var list []any
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				list = append(list, p)
			}
		}
		ag["allow"] = list
	} else {
		delete(ag, "allow")
	}

	set(in, "install_target", strOrNil(r.FormValue("install_target")))
	set(in, "after", strOrNil(r.FormValue("after")))
	set(in, "reboot_delay", numOr("reboot_delay"))
	for _, k := range []string{"no_grow", "require_checksum", "no_root_keys",
		"no_cloud_init", "no_agent"} {
		set(in, k, onOff(k))
	}

	cfg["agent"] = ag
	cfg["install"] = in
	if len(ag) == 0 {
		delete(cfg, "agent")
	}
	if len(in) == 0 {
		delete(cfg, "install")
	}
	raw, _ := json.Marshal(cfg)
	if _, err := a.db.Exec(`INSERT INTO configs(scope,body,updated) VALUES(?,?,?)
		ON CONFLICT(scope) DO UPDATE SET body=excluded.body,updated=excluded.updated`,
		"global", string(raw), now()); err != nil {
		redirectMsg(w, r, "/settings", "err", err.Error())
		return
	}
	a.logEvent("", "info", "configuration changed: global (settings page)")
	redirectMsg(w, r, "/settings", "msg", T(l, "msg.saved"))
}

// validWindow accepts "HH:MM-HH:MM" and nothing else. The agent parses it
// again on the blade; this is here so a typo is caught while somebody is
// still looking at the form.
func validWindow(s string) bool {
	a, b, found := strings.Cut(s, "-")
	if !found {
		return false
	}
	ok := func(part string) bool {
		h, m, found := strings.Cut(strings.TrimSpace(part), ":")
		if !found {
			return false
		}
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		return err1 == nil && err2 == nil && hh >= 0 && hh < 24 && mm >= 0 && mm < 60
	}
	return ok(a) && ok(b)
}

func strOrNil(s string) any {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return s
}

var usersTmpl = template.Must(template.New("users").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top">` + brandBar + `
<div class="pagehead">
  <h1>{{t .L "usr.title"}}</h1>
  <p class="sub">{{t .L "usr.lead"}}</p>
</div>
</header>

{{if .Msg}}<div class="note">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="bad">{{.Err}}</div>{{end}}
{{if .TokenUser}}<div class="note">{{t .L "usr.astoken"}}</div>{{end}}

<div class="card">
  <div class="card-head"><h2>{{t .L "usr.accounts"}}</h2></div>
  {{if .Rows}}
  <div class="tbl-wrap">
  <table class="tbl">
    <thead><tr><th>{{t .L "usr.name"}}</th><th>{{t .L "usr.role"}}</th>
      <th>{{t .L "usr.last"}}</th><th>{{t .L "usr.password"}}</th><th></th></tr></thead>
    <tbody>
    {{range .Rows}}
      <tr>
        <td><b>{{.Name}}</b>{{if .Me}} <span class="hint">{{t $.L "usr.you"}}</span>{{end}}
          {{if .Disabled}}<div><span class="chip warn">{{t $.L "usr.off"}}</span></div>{{end}}</td>
        <td>
          <form method="post" action="/users/{{.Name}}/role" class="inline">
            <select name="role" aria-label="{{t $.L "usr.role"}}">
              <option value="operator"{{if eq (printf "%s" .Role) "operator"}} selected{{end}}>{{t $.L "role.operator"}}</option>
              <option value="admin"{{if eq (printf "%s" .Role) "admin"}} selected{{end}}>{{t $.L "role.admin"}}</option>
            </select>
            <button class="mini ghost" type="submit">{{t $.L "usr.set"}}</button></form></td>
        <td class="mono">{{if .Last}}{{.Last}}{{else}}<span class="hint">{{t $.L "usr.never"}}</span>{{end}}</td>
        <td>
          <form method="post" action="/users/{{.Name}}/password" class="inline">
            <input type="password" name="password" autocomplete="new-password"
                   placeholder="{{t $.L "usr.newpw"}}" minlength="10">
            <button class="mini ghost" type="submit">{{t $.L "usr.set"}}</button></form></td>
        <td class="right">
          {{if .Disabled}}
          <form method="post" action="/users/{{.Name}}/disable" class="inline">
            <input type="hidden" name="off" value="0">
            <button class="mini ghost" type="submit">{{t $.L "usr.enable"}}</button></form>
          {{else}}
          <form method="post" action="/users/{{.Name}}/disable" class="inline">
            <button class="mini ghost" type="submit">{{t $.L "usr.disable"}}</button></form>
          {{end}}
          <form method="post" action="/users/{{.Name}}/delete" class="inline"
                onsubmit="return confirm('{{printf (t $.L "usr.delask") .Name}}')">
            <button class="mini danger" type="submit">{{t $.L "usr.delete"}}</button></form>
        </td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}<div class="body"><p class="hint">{{t .L "usr.none"}}</p></div>{{end}}
  <div class="body"><p class="hint" style="margin:0">{{t .L "usr.hint"}}</p></div>
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "usr.add"}}</h2></div>
  <div class="body">
    <form method="post" action="/users">
      <div class="setgrid">
        <label for="name">{{t .L "usr.name"}}</label>
        <input id="name" name="name" required minlength="2" maxlength="32" pattern="[A-Za-z0-9._-]+">
        <label for="password">{{t .L "usr.password"}}</label>
        <input id="password" type="password" name="password" required minlength="10"
               autocomplete="new-password">
        <label for="role">{{t .L "usr.role"}}</label>
        <select id="role" name="role">
          <option value="operator">{{t .L "role.operator"}}</option>
          <option value="admin">{{t .L "role.admin"}}</option>
        </select>
      </div>
      <div class="actions"><button type="submit">{{t .L "usr.create"}}</button></div>
    </form>
  </div>
</div>

<footer><span><a href="/settings">← {{t .L "set.title"}}</a><br><span class="tm">{{t .L "foot.tm"}}</span></span>
<span class="mono">{{ver}}</span></footer>
</div></body></html>`))

var settingsTmpl = template.Must(template.New("settings").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top">` + brandBar + `
<div class="pagehead">
  <h1>{{t .L "set.title"}}</h1>
  <p class="sub">{{t .L "set.lead"}}</p>
</div>
</header>

{{if .Open}}<div class="bad">{{th .L "warn.open"}}</div>{{end}}
{{if .Msg}}<div class="note">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="bad">{{.Err}}</div>{{end}}

<form method="post" action="/settings">
<div class="card">
  <div class="card-head"><h2>{{t .L "set.agent"}}</h2>
    <span class="tag">{{t .L "set.scope"}}</span></div>
  <div class="body">
    <div class="setgrid">
      <div><label for="iv">{{t .L "set.interval"}}</label>
        <input id="iv" type="number" name="interval" min="10" max="3600" value="{{.S.Interval}}" placeholder="60"></div>
      <div><label for="ji">{{t .L "set.jitter"}}</label>
        <input id="ji" type="number" name="jitter" min="0" max="600" value="{{.S.Jitter}}" placeholder="0"></div>
      <div><label for="mw">{{t .L "set.window"}}</label>
        <input id="mw" type="text" name="maintenance" value="{{.S.Window}}" placeholder="02:00-04:00"></div>
      <div class="wide"><label for="al">{{t .L "set.allow"}}</label>
        <input id="al" type="text" name="allow" value="{{.S.Allow}}" placeholder="{{t .L "set.allowhint"}}"></div>
    </div>
    <div class="checks">
      <label class="check"><input type="checkbox" name="reboot_on_boot_config" value="1"{{if .S.RebootOnCfg}} checked{{end}}>
        {{t .L "set.rebootcfg"}}</label>
    </div>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "set.reboothint"}}</p>
  </div>
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "set.install"}}</h2></div>
  <div class="body">
    <div class="setgrid">
      <div class="wide"><label for="tg">{{t .L "set.target"}}</label>
        <input id="tg" type="text" name="install_target" value="{{.S.Target}}" placeholder="/dev/nvme0n1"></div>
      <div><label for="af">{{t .L "set.after"}}</label>
        <select id="af" name="after">
          <option value=""{{if eq .S.After ""}} selected{{end}}>{{t .L "set.after.reboot"}}</option>
          <option value="halt"{{if eq .S.After "halt"}} selected{{end}}>{{t .L "set.after.halt"}}</option>
          <option value="shell"{{if eq .S.After "shell"}} selected{{end}}>{{t .L "set.after.shell"}}</option>
        </select></div>
      <div><label for="rd">{{t .L "set.rebootwait"}}</label>
        <input id="rd" type="number" name="reboot_delay" min="0" max="600" value="{{.S.RebootWait}}" placeholder="5"></div>
    </div>
    <div class="checks">
      <label class="check"><input type="checkbox" name="require_checksum" value="1"{{if .S.NeedSum}} checked{{end}}>
        {{t .L "set.needsum"}}</label>
      <label class="check"><input type="checkbox" name="no_grow" value="1"{{if .S.NoGrow}} checked{{end}}>
        {{t .L "set.nogrow"}}</label>
      <label class="check"><input type="checkbox" name="no_root_keys" value="1"{{if .S.NoRootKeys}} checked{{end}}>
        {{t .L "set.nokeys"}}</label>
      <label class="check"><input type="checkbox" name="no_cloud_init" value="1"{{if .S.NoCloud}} checked{{end}}>
        {{t .L "set.nocloud"}}</label>
      <label class="check"><input type="checkbox" name="no_agent" value="1"{{if .S.NoAgent}} checked{{end}}>
        {{t .L "set.noagent"}}</label>
    </div>
    <div style="margin-top:1.2rem"><button type="submit">{{t .L "form.save"}}</button></div>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "set.seedhint"}}</p>
  </div>
</div>
</form>

<div class="card">
  <div class="card-head"><h2>{{t .L "nf.title"}}</h2>
    <span class="tag">{{t .L "nf.open"}}: {{if .Alerts}}{{len .Alerts}}{{else}}{{t .L "nf.opennone"}}{{end}}</span></div>
  <div class="body">
    <p class="hint" style="margin:0 0 1rem">{{t .L "nf.lead"}}</p>
    {{if .Alerts}}
    <table class="tbl" style="margin-bottom:1.2rem">
      <tbody>
      {{range .Alerts}}
        <tr><td><b>{{.Name}}</b></td>
          <td><span class="chip {{.LED}}">{{.Level}}</span></td>
          <td>{{.Reason}}</td>
          <td class="right"><span class="hint">{{.Since}}{{if .Sent}} · sent{{end}}</span></td></tr>
      {{end}}
      </tbody>
    </table>
    {{end}}
    <form method="post" action="/settings/notify">
      <div class="setgrid">
        <div class="wide"><label for="nh">{{t .L "nf.host"}}</label>
          <input id="nh" type="text" name="host" value="{{.NF.Host}}" placeholder="mail.example.org"></div>
        <div><label for="np">{{t .L "nf.port"}}</label>
          <input id="np" type="number" name="port" min="1" max="65535" value="{{.NF.Port}}"></div>
        <div><label for="ns">{{t .L "nf.sec"}}</label>
          <select id="ns" name="tls">
            <option value="starttls"{{if eq .NF.TLS "starttls"}} selected{{end}}>{{t .L "nf.starttls"}}</option>
            <option value="tls"{{if eq .NF.TLS "tls"}} selected{{end}}>{{t .L "nf.tls"}}</option>
            <option value="none"{{if eq .NF.TLS "none"}} selected{{end}}>{{t .L "nf.none"}}</option>
          </select></div>
        <div><label for="nu">{{t .L "nf.user"}}</label>
          <input id="nu" type="text" name="user" value="{{.NF.User}}" autocomplete="off"></div>
        <div><label for="nw">{{t .L "nf.pass"}}</label>
          <input id="nw" type="password" name="pass" autocomplete="new-password"
                 placeholder="{{if .HasPass}}{{t .L "nf.passset"}} — {{end}}{{t .L "nf.passkeep"}}"></div>
        <div class="wide"><label for="nfr">{{t .L "nf.from"}}</label>
          <input id="nfr" type="email" name="from" value="{{.NF.From}}"></div>
        <div class="wide"><label for="nt">{{t .L "nf.to"}}</label>
          <input id="nt" type="text" name="to" value="{{.NF.To}}" placeholder="{{t .L "nf.tohint"}}"></div>
        <div><label for="nm">{{t .L "nf.min"}}</label>
          <select id="nm" name="min">
            <option value="warn"{{if eq .NF.Min "warn"}} selected{{end}}>{{t .L "nf.min.warn"}}</option>
            <option value="crit"{{if eq .NF.Min "crit"}} selected{{end}}>{{t .L "nf.min.crit"}}</option>
          </select></div>
        <div><label for="nhd">{{t .L "nf.hold"}}</label>
          <input id="nhd" type="number" name="hold" min="1" max="1440" value="{{.NF.HoldMin}}"></div>
      </div>
      <div class="checks">
        <label class="check"><input type="checkbox" name="enabled" value="1"{{if .NF.Enabled}} checked{{end}}>
          {{t .L "nf.on"}}</label>
      </div>
      <div style="margin-top:1.2rem;display:flex;gap:.6rem;align-items:center">
        <button type="submit">{{t .L "form.save"}}</button>
        <button type="submit" name="test" value="1" class="ghost">{{t .L "nf.test"}}</button>
      </div>
    </form>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "nf.secret"}}</p>
  </div>
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "bk.title"}}</h2>
    <span class="tag">{{t .L "bk.at"}} {{.BK.At}}</span></div>
  <div class="body">
    <p class="hint" style="margin:0 0 1rem">{{t .L "bk.lead"}}</p>
    {{if .BK.Dir}}
    <div class="meta">
      <span>{{t .L "bk.dir"}} <b>{{.BK.Dir}}</b></span>
      <span>{{t .L "bk.last"}} <b>{{.BKLast}}</b></span>
      <span>{{t .L "bk.keep"}} <b>{{.BK.Count}}/{{.BK.Keep}}</b></span>
    </div>
    <div style="margin-top:1.2rem">
      <form method="post" action="/settings/backup"><button type="submit">{{t .L "bk.now"}}</button></form>
    </div>
    <p class="hint" style="margin:.9rem 0 0">{{t .L "bk.tokens"}}</p>
    <p class="hint" style="margin:.4rem 0 0">{{t .L "bk.restore"}}</p>
    {{else}}<p class="hint">{{t .L "bk.off"}}</p>{{end}}
  </div>
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "usr.title"}}</h2></div>
  <div class="body">
    <p class="hint" style="margin:0 0 .8rem">{{t .L "usr.lead"}}</p>
    <a class="btn" href="/users">{{t .L "usr.title"}} →</a>
  </div>
</div>

<footer><span><a href="/">← {{t .L "nav.overview"}}</a><br><span class="tm">{{t .L "foot.tm"}}</span></span>
<span>{{t .L "foot.api"}}</span>
<span class="mono">{{ver}}</span></footer>
</div></body></html>`))

// ── Images ───────────────────────────────────────────────────────────
//
// Adding an image used to be two commands on the server, run in the right
// order by whoever remembered them. The page does the same two things and
// says out loud where it got to, so a fetch that fails is visible instead of
// silent.

type imageRow struct {
	Image
	Size     string
	State    string // shown to the reader
	LED      string // ok | warn | bad | ""
	Note     string
	Kern     string
	Table    string // what the first sector says: GPT or MBR
	TableBad bool   // a GPT, which no card will boot
	InUse    int
	Recipe   string
	Blocking bool // being worked on: cannot be removed
}

func (a *App) hImagesPage(w http.ResponseWriter, r *http.Request) {
	l := a.resolveLang(w, r)
	imgs, _ := a.listImages()
	msg, errMsg := flash(r)

	used := map[string]int{}
	if blades, err := a.listBlades(); err == nil {
		for _, b := range blades {
			if b.Image != "" {
				used[b.Image]++
			}
		}
	}

	rows := make([]imageRow, 0, len(imgs))
	working := false
	for _, im := range imgs {
		row := imageRow{Image: im, Size: human(im.Bytes), InUse: used[im.ID], Note: im.Note}
		switch im.State {
		case imgQueued:
			row.State, row.LED = T(l, "img.st.queued"), "warn"
			row.Blocking = true
			working = true
		case imgWorking:
			row.State, row.LED = T(l, "img.st.work"), "warn"
			row.Blocking = true
			working = true
		case imgError:
			row.State, row.LED = T(l, "img.st.error"), "bad"
		default:
			// Everything from before this page existed, and everything done.
			if im.Local != "" {
				row.State, row.LED = T(l, "img.st.local"), "ok"
			} else {
				row.State, row.LED = T(l, "img.st.remote"), ""
			}
		}
		switch im.Kernel {
		case "downstream":
			row.Kern = T(l, "img.k.down")
		case "upstream":
			row.Kern = T(l, "img.k.up")
		}
		switch a.imageTable(im.ID) {
		case "gpt":
			row.Table, row.TableBad = T(l, "img.gpt"), true
		case "mbr":
			row.Table = T(l, "img.mbr")
		}
		if rec, ok := matchRecipe(im.ID, im.URL); ok {
			row.Recipe = rec.Name
		}
		rows = append(rows, row)
	}

	// One line per distribution, not per rule: the general Debian entry
	// exists so an older release still lands somewhere, and naming it beside
	// the Trixie one would read as two different things to choose between.
	known := make([]string, 0, len(recipes))
	seen := map[string]bool{}
	for _, rec := range recipes {
		if !seen[rec.OSID] {
			seen[rec.OSID] = true
			known = append(known, rec.Name)
		}
	}

	render(w, imagesTmpl, map[string]any{
		"L": l, "Path": "/images", "LocalSite": a.localSiteID(), "Admin": a.isAdmin(r),
		"Rows": rows, "Known": known, "Refresh": working,
		"Msg": msg, "Err": errMsg, "Open": a.adminToken == "",
	})
}

func (a *App) hUIImageAdd(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, "/images", "err", T(l, "err.form"))
		return
	}
	id, _, err := a.startImageFetch(imageFetchReq{
		ID:       strings.TrimSpace(r.FormValue("id")),
		URL:      strings.TrimSpace(r.FormValue("url")),
		Packages: strings.TrimSpace(r.FormValue("packages")),
		NoPrep:   r.FormValue("no_prepare") != "",
	})
	if err != nil {
		redirectMsg(w, r, "/images", "err", errText(l, err))
		return
	}
	redirectMsg(w, r, "/images", "msg", fmt.Sprintf(T(l, "img.queued"), id))
}

func (a *App) hUIImageRemove(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	id := r.PathValue("id")
	if err := a.removeImage(id); err != nil {
		redirectMsg(w, r, "/images", "err", errText(l, err))
		return
	}
	redirectMsg(w, r, "/images", "msg", fmt.Sprintf(T(l, "img.removed"), id))
}

var imagesTmpl = template.Must(template.New("images").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top">` + brandBar + `
<div class="pagehead">
  <h1>{{t .L "img.title"}}</h1>
  <p class="sub">{{t .L "img.lead"}}</p>
</div>
<div class="meta"><span>{{t .L "img.title"}} <b>{{len .Rows}}</b></span></div>
</header>

{{if .Open}}<div class="bad">{{th .L "warn.open"}}</div>{{end}}
{{if .Msg}}<div class="note">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="bad">{{.Err}}</div>{{end}}

<div class="card">
  <div class="card-head"><h2>{{t .L "img.title"}}</h2></div>
  {{if .Rows}}
  <table class="tbl">
    <thead><tr><th>{{t .L "img.name"}}</th><th>{{t .L "img.state"}}</th>
      <th>{{t .L "img.size"}}</th><th>{{t .L "img.kernel"}}</th><th></th></tr></thead>
    <tbody>
    {{range .Rows}}
      <tr>
        <td><b>{{.ID}}</b>
          {{if .Recipe}}<div class="hint">{{.Recipe}}</div>{{end}}
          {{if .Notes}}<div class="hint">{{.Notes}}</div>{{end}}</td>
        <td><span class="chip {{.LED}}">{{.State}}</span>
          {{if .Verified}}<div class="hint">{{t $.L "img.verified"}}</div>{{end}}
          {{if .Note}}<div class="hint">{{.Note}}</div>{{end}}</td>
        <td class="num">{{.Size}}
          {{if .InUse}}<div class="hint">{{.InUse}} {{t $.L "img.blades"}}</div>{{end}}</td>
        <td>{{if .Kern}}<span class="hint">{{.Kern}}</span>{{end}}
          {{if .Table}}<div>{{if .TableBad}}<span class="chip warn" title="{{t $.L "img.gpttip"}}">{{.Table}}</span>{{else}}<span class="hint">{{.Table}}</span>{{end}}</div>{{end}}</td>
        <td class="right">
          {{if not .Blocking}}
          <form method="post" action="/images/{{.ID}}/remove"
                onsubmit="return confirm('{{printf (t $.L "img.rmask") .ID}}')">
            <button class="ghost danger">{{t $.L "img.remove"}}</button></form>
          {{end}}
        </td>
      </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}<div class="body"><p class="hint">{{t .L "img.none"}}</p></div>{{end}}
</div>

<div class="card">
  <div class="card-head"><h2>{{t .L "img.add"}}</h2>
    <span class="tag">{{t .L "img.known"}}: {{range $i, $k := .Known}}{{if $i}} · {{end}}{{$k}}{{end}}</span></div>
  <div class="body">
    <form method="post" action="/images/add">
      <div class="setgrid">
        <div class="wide"><label for="url">{{t .L "img.url"}}</label>
          <input id="url" type="url" name="url" required
                 placeholder="https://…/ubuntu-24.04-preinstalled-server-arm64+raspi.img.xz">
          <p class="hint">{{t .L "img.urlhint"}}</p></div>
        <div><label for="iid">{{t .L "img.id"}}</label>
          <input id="iid" type="text" name="id" placeholder="{{t .L "img.idhint"}}"></div>
        <div><label for="pk">{{t .L "img.pkgs"}}</label>
          <input id="pk" type="text" name="packages" placeholder="{{t .L "img.pkgshint"}}"></div>
      </div>
      <div class="checks">
        <label class="check"><input type="checkbox" name="no_prepare" value="1"> {{t .L "img.noprep"}}</label>
      </div>
      <div style="margin-top:1.2rem"><button type="submit">{{t .L "img.fetch"}}</button></div>
    </form>
  </div>
</div>

<footer><span><a href="/">← {{t .L "nav.overview"}}</a><br><span class="tm">{{t .L "foot.tm"}}</span></span>
<span>{{t .L "foot.api"}}</span>
<span class="mono">{{ver}}</span></footer>
</div></body></html>`))

// hUIBackupNow is the button beside the backup card. A backup nobody can
// trigger is a backup nobody has ever seen work.
func (a *App) hUIBackupNow(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	path, size, err := a.backupNow()
	if err != nil {
		redirectMsg(w, r, "/settings", "err", errText(l, err))
		return
	}
	dropped := a.pruneBackups(a.backupKeep)
	msg := fmt.Sprintf("backup written: %s (%s)", filepath.Base(path), human(size))
	if dropped > 0 {
		msg += fmt.Sprintf(", %d older one(s) removed", dropped)
	}
	a.logEvent("", "info", msg)
	redirectMsg(w, r, "/settings", "msg",
		fmt.Sprintf(T(l, "bk.done"), filepath.Base(path), human(size)))
}

// alertView is one line of "currently unwell" on the settings page.
type alertView struct {
	Name   string
	Level  string
	LED    string
	Reason string
	Since  string
	Sent   bool
}

// hUINotifySave writes the notification settings. The password is the one
// field that survives an empty box: a form that has to show a secret in order
// to keep it is a form that shows a secret.
func (a *App) hUINotifySave(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, "/settings", "err", T(l, "err.form"))
		return
	}
	set := func(key, val string) { _ = a.setSetting(key, val) }
	set("notify_host", strings.TrimSpace(r.FormValue("host")))
	set("notify_port", strings.TrimSpace(r.FormValue("port")))
	set("notify_tls", r.FormValue("tls"))
	set("notify_user", strings.TrimSpace(r.FormValue("user")))
	set("notify_from", strings.TrimSpace(r.FormValue("from")))
	set("notify_to", strings.TrimSpace(r.FormValue("to")))
	set("notify_min", r.FormValue("min"))
	set("notify_hold_min", strings.TrimSpace(r.FormValue("hold")))
	if r.FormValue("enabled") != "" {
		set("notify_enabled", "1")
	} else {
		set("notify_enabled", "")
	}
	if pw := r.FormValue("pass"); pw != "" {
		set("notify_pass", pw)
	}
	a.logEvent("", "info", "notification settings changed")

	if r.FormValue("test") == "" {
		redirectMsg(w, r, "/settings", "msg", T(l, "msg.saved"))
		return
	}
	conf := a.mailConf()
	subject := "Sheath: test"
	body := "This is the test from the settings page.\n\n" +
		"If it arrived, a blade that goes bad will reach you the same way.\n"
	if err := sendMail(conf, subject, body); err != nil {
		redirectMsg(w, r, "/settings", "err", fmt.Sprintf(T(l, "nf.failed"), err.Error()))
		return
	}
	a.logEvent("", "info", "test notification sent to "+conf.To)
	redirectMsg(w, r, "/settings", "msg", fmt.Sprintf(T(l, "nf.sent"), conf.To))
}

// shortOr renders a checksum the way a person compares two of them: the first
// eight characters, or a word saying there is nothing to compare.
func shortOr(sum string) string {
	if sum == "" {
		return "—"
	}
	if len(sum) > 8 {
		return sum[:8]
	}
	return sum
}

// ── Inventory ────────────────────────────────────────────────────────

func (a *App) hInventory(w http.ResponseWriter, r *http.Request) {
	l := a.resolveLang(w, r)
	rows, sum, err := a.inventory(l)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	unknown := ""
	if sum.Unknown > 0 {
		unknown = fmt.Sprintf(T(l, "inv.unknown"), sum.Unknown)
	}
	msg, errMsg := flash(r)
	render(w, inventoryTmpl, map[string]any{
		"L": l, "Path": "/inventory", "LocalSite": a.localSiteID(), "Admin": a.isAdmin(r),
		"Rows": rows, "Sum": sum, "Unknown": unknown,
		"Msg": msg, "Err": errMsg, "Open": a.adminToken == "",
	})
}

var inventoryTmpl = template.Must(template.New("inv").Funcs(tmplFuncs).Parse(headHTML + `
<div class="wrap">
<header class="top">` + brandBar + `
<div class="pagehead">
  <h1>{{t .L "inv.title"}}</h1>
  <p class="sub">{{t .L "inv.lead"}}</p>
</div>
<div class="meta">
  <span>{{t .L "meta.blades"}} <b>{{.Sum.Blades}}</b></span>
  <span>{{t .L "inv.ram"}} <b>{{.Sum.RAMTotal}}</b></span>
  {{if .Sum.NVMe}}<span>{{t .L "inv.nvme"}} <b>{{.Sum.NVMe}}</b></span>{{end}}
  {{if .Unknown}}<span>{{.Unknown}}</span>{{end}}
</div>
</header>

{{if .Open}}<div class="bad">{{th .L "warn.open"}}</div>{{end}}
{{if .Msg}}<div class="note">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="bad">{{.Err}}</div>{{end}}

{{if .Sum.Boards}}
<div class="card">
  <div class="card-head"><h2>{{t .L "inv.sum"}}</h2></div>
  <div class="body"><div class="meta">
    {{range .Sum.Boards}}<span>{{.}}</span>{{end}}
  </div></div>
</div>
{{end}}

<div class="card">
  <div class="card-head"><h2>{{t .L "inv.title"}}</h2>
    <span class="tag">{{t .L "inv.revhint"}}</span></div>
  {{if .Rows}}
  <div class="tbl-wrap">
  <table class="tbl inv">
    <thead><tr>
      <th>{{t .L "inv.blade"}}</th><th>{{t .L "inv.where"}}</th>
      <th>{{t .L "th.status"}}</th>
      <th>{{t .L "inv.board"}}</th><th>{{t .L "inv.storage"}}</th>
      <th>{{t .L "inv.firmware"}}</th><th>{{t .L "inv.running"}}</th><th></th>
    </tr></thead>
    <tbody>
    {{range .Rows}}
      <tr>
        <td><span class="dot {{.LED}}"></span> <b>{{if .Hostname}}{{.Hostname}}{{else}}{{.Serial}}{{end}}</b>
          <div class="mono sub2">{{.Serial}}</div>
          <div class="mono sub2">{{if .LiveIP}}{{.LiveIP}}{{if .Drifted}} <span class="hint">({{t $.L "inv.given"}} {{.IP}})</span>{{end}}{{else}}{{if .IP}}{{.IP}}{{else}}—{{end}}{{end}}</div></td>
        <td>{{if .Site}}{{.Site}}
            <div class="mono sub2">{{.Rack}}{{if .Slot}} · {{t $.L "th.slot"}} {{.Slot}}{{end}}</div>
          {{else}}
            {{if .SawSite}}<a href="/sites/{{.SiteID}}">{{.SawSite}}</a>{{else}}—{{end}}
            <div class="sub2 hint">{{t $.L "inv.unused"}}</div>
          {{end}}</td>
        <td><span class="chip {{.SLED}}">{{.Status}}</span>
          {{if .Seen}}<div class="mono sub2">{{.Seen}}</div>{{end}}</td>
        <td>{{if .Missing}}<span class="hint">{{t $.L "inv.none"}}</span>{{else}}<span class="board">{{.Board}}</span>
          <div class="mono sub2">{{.RAM}}{{if .SoC}} · {{.SoC}}{{end}}{{if .Cores}} · {{.Cores}} × {{.MHz}}{{end}}</div>
          <div class="mono sub2">{{if .Rev}}{{.Rev}}{{end}}{{if .Maker}} · {{.Maker}}{{end}}{{if .Radio}} · {{.Radio}}{{end}}</div>{{end}}</td>
        <td class="mono">{{if .NVMe}}{{.NVMe}}{{end}}
          {{if .EMMC}}<div class="sub2">{{.EMMC}}</div>{{end}}</td>
        <td>{{if .Boot}}<span class="mono">{{.Boot}}</span>{{end}}
          <div class="mono sub2">{{if .VC}}VC {{.VC}}{{end}}</div>
          {{if .BootVia}}<div class="hint">{{.BootVia}}</div>{{end}}
          {{if .Order}}<div class="mono sub2">{{t $.L "bo.title"}} {{.Order}}</div>
            <div class="hint">{{.OrderText}}</div>{{end}}
          {{if .OrderWarn}}<div><span class="chip crit">{{.OrderWarn}}</span></div>{{end}}</td>
        <td>{{.OS}}
          <div class="mono sub2">{{.Kernel}}</div>
          {{if .SSH}}<div>{{if .SSHBad}}<span class="chip warn">{{.SSH}}</span>{{else}}<span class="hint">{{.SSH}}</span>{{end}}</div>{{end}}</td>
        <td class="right">
          {{if and .Unused $.Admin}}
          <form method="post" action="/inventory/{{.Serial}}/forget"
                onsubmit="return confirm('{{printf (t $.L "inv.forgetask") (or .Hostname .Serial)}}')">
            <button class="ghost danger">{{t $.L "inv.forget"}}</button></form>
          {{end}}
        </td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}<div class="body"><p class="hint">{{t .L "inv.empty"}}</p></div>{{end}}
  <div class="body"><p class="hint" style="margin:0">{{t .L "inv.fwhint"}}</p>
    <p class="hint" style="margin:.4rem 0 0">{{t .L "bo.hint"}}</p>
    <p class="hint" style="margin:.4rem 0 0">{{t .L "ssh.hint"}}</p>
    <p class="hint" style="margin:.4rem 0 0">{{t .L "inv.storedhint"}}</p>
    <p class="hint" style="margin:.4rem 0 0">{{t .L "inv.forgethint"}}</p></div>
</div>

<footer><span><a href="/">← {{t .L "nav.overview"}}</a><br><span class="tm">{{t .L "foot.tm"}}</span></span>
<span>{{t .L "foot.api"}}</span>
<span class="mono">{{ver}}</span></footer>
</div></body></html>`))

// hUISiteEnroll makes a code and shows it once. Once, because a code that is
// still on a page an hour later is a code somebody left on a screen.
func (a *App) hUISiteEnroll(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	code, _, err := a.makeEnrollCode(id)
	if err != nil {
		redirectMsg(w, r, "/sites/"+strconv.FormatInt(id, 10), "err", errText(l, err))
		return
	}
	// Not in the URL: an address is written down in logs, in histories and in
	// whatever the browser syncs. The page reads the pending code from the
	// database instead, and stops showing it the moment it is spent.
	_ = code
	redirectMsg(w, r, "/sites/"+strconv.FormatInt(id, 10), "msg", T(l, "enr.made"))
}

// enrollCommand is the line to type on the site machine. Shown in full, with
// the code in it, because the alternative is somebody assembling it from
// three places in the documentation and getting one of them wrong.
func enrollCommand(base, code string) string {
	if code == "" {
		return ""
	}
	return "sheath-site --server " + base + " --enroll " + code
}

// humanDur says a duration the way somebody reads a deadline.
func humanDur(l Lang, d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%.0f h %d min", d.Hours(), int(d.Minutes())%60)
}

// hUIBladeTarget points one blade at one of its own devices. Written into
// that blade's own configuration scope, so it outlives the next global change
// and applies to nothing else.
func (a *App) hUIBladeTarget(w http.ResponseWriter, r *http.Request) {
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
	target := strings.TrimSpace(r.FormValue("target"))
	ok := false
	for _, d := range a.installDevices(b) {
		if d.Path == target {
			ok = true
		}
	}
	if !ok {
		redirectMsg(w, r, to, "err",
			errText(l, me("err.nodevice", bladeName(b), target, deviceList(a.installDevices(b)))))
		return
	}
	scope := "blade:" + serial
	cfg := a.configFor(scope)
	install, _ := cfg["install"].(map[string]any)
	if install == nil {
		install = map[string]any{}
	}
	install["install_target"] = target
	cfg["install"] = install
	if err := a.putConfig(scope, cfg); err != nil {
		redirectMsg(w, r, to, "err", errText(l, err))
		return
	}
	a.logEvent(serial, "info", "install target set to "+target)
	// Same as choosing the image, from the other end: the pair has to work,
	// and whichever half was chosen last is the moment to say so.
	b, _ = a.getBlade(serial)
	if err := a.checkTarget(b); err != nil {
		redirectMsg(w, r, to, "err", fmt.Sprintf(T(l, "msg.targetsetbut"),
			bladeName(b), target, errText(l, err)))
		return
	}
	redirectMsg(w, r, to, "msg", fmt.Sprintf(T(l, "tgt.saved"), b.Hostname, target))
}

// hUIBladeForget removes a blade nobody has in a slot. The inventory is where
// this belongs: it is the page that lists blades regardless of where they
// stand, so it is the page where one that stands nowhere is visible at all.
func (a *App) hUIBladeForget(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	serial := r.PathValue("serial")
	b, err := a.getBlade(serial)
	name := serial
	if err == nil && b.Hostname != "" {
		name = b.Hostname
	}
	if err := a.forgetBlade(serial); err != nil {
		redirectMsg(w, r, "/inventory", "err", errText(l, err))
		return
	}
	redirectMsg(w, r, "/inventory", "msg", fmt.Sprintf(T(l, "inv.forgot"), name))
}
