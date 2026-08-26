package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- A site is a network segment, not a building: two VLANs in the same room
-- are two sites. The boundary is the broadcast domain, because that is the
-- only place DHCP reaches.
--
-- "local" marks the site whose network presence this process serves itself.
-- Once sheath-site is split out, there will be none left.
CREATE TABLE IF NOT EXISTS sites (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    location   TEXT    NOT NULL DEFAULT '',
    net_base   TEXT    NOT NULL,              -- e.g. 10.0.0
    gateway    TEXT    NOT NULL DEFAULT '',
    dns        TEXT    NOT NULL DEFAULT '',
    domain     TEXT    NOT NULL DEFAULT '',
    pool_from  INTEGER NOT NULL DEFAULT 210,
    pool_to    INTEGER NOT NULL DEFAULT 240,
    offset_base INTEGER NOT NULL DEFAULT 100,
    offset_step INTEGER NOT NULL DEFAULT 20,
    local      INTEGER NOT NULL DEFAULT 0,
    token      TEXT    NOT NULL DEFAULT '',
    last_seen  TEXT    NOT NULL DEFAULT '',
    created    TEXT    NOT NULL
);

-- A BladeRunner has 2, 4, 10 or 20 slots. ip_offset fixes the address block:
-- blade IP = <net_base>.(ip_offset + slot)
CREATE TABLE IF NOT EXISTS racks (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id   INTEGER NOT NULL DEFAULT 1,
    name      TEXT    NOT NULL UNIQUE,
    size      INTEGER NOT NULL CHECK (size IN (2, 4, 10, 20)),
    ip_offset INTEGER NOT NULL,
    location  TEXT    NOT NULL DEFAULT '',
    created   TEXT    NOT NULL,
    -- Per site, not globally: two sites are two networks, and .100 in one is
    -- a different address from .100 in the other.
    UNIQUE (site_id, ip_offset)
);

-- The serial number is the identity. rack_id/slot are the position; both may
-- be NULL as long as a blade has not been placed yet.
CREATE TABLE IF NOT EXISTS blades (
    serial          TEXT PRIMARY KEY,
    short_serial    TEXT NOT NULL DEFAULT '',   -- low 4 bytes = TFTP directory
    rack_id         INTEGER REFERENCES racks(id) ON DELETE SET NULL,
    slot            INTEGER,
    hostname        TEXT NOT NULL DEFAULT '',
    mac             TEXT NOT NULL DEFAULT '',
    image           TEXT NOT NULL DEFAULT '',
    variant         TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT 'new',
    groups_json     TEXT NOT NULL DEFAULT '[]',
    facts_json      TEXT NOT NULL DEFAULT '{}',
    health_json     TEXT NOT NULL DEFAULT '{}',
    token           TEXT NOT NULL DEFAULT '',
    config_applied  TEXT NOT NULL DEFAULT '',
    last_seen       TEXT NOT NULL DEFAULT '',
    created         TEXT NOT NULL,
    UNIQUE (rack_id, slot)
);

CREATE TABLE IF NOT EXISTS alerts (
    serial      TEXT PRIMARY KEY,
    level       TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    since       TEXT NOT NULL,
    notified    TEXT NOT NULL DEFAULT '',
    notified_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS images (
    id        TEXT PRIMARY KEY,
    url       TEXT NOT NULL,
    sha256    TEXT NOT NULL DEFAULT '',
    seed      TEXT NOT NULL DEFAULT 'generic',
    os_id     TEXT NOT NULL DEFAULT '',
    notes     TEXT NOT NULL DEFAULT '',
    local     TEXT NOT NULL DEFAULT '',   -- path under images/ when mirrored
    bytes     INTEGER NOT NULL DEFAULT 0,
    created   TEXT NOT NULL
);

-- Desired configuration in three layers: scope = 'global' | 'group:<name>' | 'blade:<serial>'
CREATE TABLE IF NOT EXISTS configs (
    scope    TEXT PRIMARY KEY,
    body     TEXT NOT NULL DEFAULT '{}',
    updated  TEXT NOT NULL
);

-- Pending commands per blade (reboot, identify, reimage)
CREATE TABLE IF NOT EXISTS commands (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    serial  TEXT NOT NULL,
    kind    TEXT NOT NULL,
    args    TEXT NOT NULL DEFAULT '{}',
    created TEXT NOT NULL,
    taken   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_cmd_serial ON commands(serial, taken);

CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts      TEXT NOT NULL,
    serial  TEXT NOT NULL DEFAULT '',
    level   TEXT NOT NULL DEFAULT 'info',
    msg     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts DESC);

-- A blade reports every minute; keeping every report would be a database of
-- weather, not of blades. One sample every five minutes for two days is
-- enough to see a fan ramping or a slot running hot, and costs about six
-- hundred rows per blade.
CREATE TABLE IF NOT EXISTS samples (
    serial  TEXT NOT NULL,
    ts      TEXT NOT NULL,
    soc     REAL,
    airflow REAL,
    rpm     REAL,
    PRIMARY KEY (serial, ts)
);
CREATE INDEX IF NOT EXISTS idx_samples_serial_ts ON samples(serial, ts);

-- What a site actually holds. The catalogue is central, the bytes are local,
-- and the difference between the two is worth showing: an image assigned to a
-- blade at a site that has not fetched it yet is an installation that will
-- wait, and saying so beats letting someone find out at the rack.
CREATE TABLE IF NOT EXISTS site_images (
    site_id  INTEGER NOT NULL,
    image_id TEXT    NOT NULL,
    state    TEXT    NOT NULL DEFAULT 'absent',  -- absent | fetching | ready | error
    bytes    INTEGER NOT NULL DEFAULT 0,
    note     TEXT    NOT NULL DEFAULT '',
    ts       TEXT    NOT NULL,
    PRIMARY KEY (site_id, image_id)
);
`

// install_state separates "this blade has an image assigned" from "this
// blade should be written now". Without that split every netboot would
// trigger a full reinstall — endlessly, on a blade whose BOOT_ORDER puts the
// network ahead of the NVMe.
var migrations = []string{
	`ALTER TABLE blades ADD COLUMN install_state TEXT NOT NULL DEFAULT 'idle'`,
	`ALTER TABLE blades ADD COLUMN installed_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE racks ADD COLUMN site_id INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE netboot ADD COLUMN site_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sites ADD COLUMN policy_json TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE images ADD COLUMN kernel TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE images ADD COLUMN min_disk INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE images ADD COLUMN verified INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE images ADD COLUMN state TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN payload TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN enroll_code TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN host_prefix TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN enroll_until TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sites ADD COLUMN site_version TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE images ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE images ADD COLUMN updated TEXT NOT NULL DEFAULT ''`,
}

const (
	installIdle    = "idle"    // nothing to do
	installPending = "pending" // write on the next netboot
	installDone    = "done"    // written
	installError   = "error"
	// installWipe: the next netboot erases this blade's disk instead of
	// writing to it. Deliberately a state and not a command — a command the
	// agent carries out would have to erase the disk it is running from.
	installWipe = "wipe"
)

type Site struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Location   string `json:"location"`
	NetBase    string `json:"net_base"`
	Gateway    string `json:"gateway"`
	DNS        string `json:"dns"`
	Domain     string `json:"domain"`
	PoolFrom   int    `json:"pool_from"`
	PoolTo     int    `json:"pool_to"`
	OffsetBase int    `json:"offset_base"`
	OffsetStep int    `json:"offset_step"`
	Local      bool   `json:"local"`
	// Token is the site's own credential. Never serialised outwards — the
	// desired state a site holds on disk must not contain the key to itself.
	Token    string `json:"-"`
	LastSeen string `json:"last_seen"`
	Created  string `json:"created"`

	// What the site last said about itself: the checksum of the netboot
	// payload it serves, and the version of the program serving it. A site
	// that has never reported says nothing, which is different from a site
	// that reported nothing.
	Payload     string `json:"payload"`
	SiteVersion string `json:"site_version"`

	// A pending enrollment code, and how long it is good for. Never
	// serialised outwards for the same reason the token is not.
	EnrollCode  string `json:"-"`
	EnrollUntil string `json:"-"`

	// What goes into the names of blades here: blade-<prefix>-r1s01. Empty
	// keeps the name a single-site installation has always had.
	HostPrefix string `json:"host_prefix"`
}

type Rack struct {
	ID       int64  `json:"id"`
	SiteID   int64  `json:"site_id"`
	Name     string `json:"name"`
	Size     int    `json:"size"`
	IPOffset int    `json:"ip_offset"`
	Location string `json:"location"`
	Created  string `json:"created"`

	// derived
	SiteName string `json:"site_name"`
}

type Blade struct {
	Serial       string          `json:"serial"`
	ShortSerial  string          `json:"short_serial"`
	RackID       *int64          `json:"rack_id"`
	Slot         *int            `json:"slot"`
	Hostname     string          `json:"hostname"`
	MAC          string          `json:"mac"`
	Image        string          `json:"image"`
	Variant      string          `json:"variant"`
	State        string          `json:"state"`
	Groups       []string        `json:"groups"`
	Facts        json.RawMessage `json:"facts"`
	Health       json.RawMessage `json:"health"`
	InstallState string          `json:"install_state"`
	InstalledAt  string          `json:"installed_at"`
	ConfigApp    string          `json:"config_applied"`
	LastSeen     string          `json:"last_seen"`
	Created      string          `json:"created"`

	// derived, not stored
	IP       string `json:"ip"`
	RackName string `json:"rack_name"`
	RackIdx  int    `json:"rack_index"`
	SiteID   int64  `json:"site_id"`
	SiteName string `json:"site_name"`
}

type Image struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Seed    string `json:"seed"`
	OSID    string `json:"os_id"`
	Notes   string `json:"notes"`
	Local   string `json:"local"`
	Bytes   int64  `json:"bytes"`
	Created string `json:"created"`

	// What this image can and cannot do. Learned the hard way: Debian's raspi
	// image runs the upstream kernel, and there the firmware applies no
	// device-tree directive at all — no dtoverlay, not even a dtparam. So the
	// smart fan unit stays invisible on it, and nothing about that is
	// discoverable from the file name. It belongs in the catalogue, where the
	// interface can say it before someone spends an evening at the rack.
	Kernel   string `json:"kernel"`   // downstream | upstream | ""
	MinDisk  int64  `json:"min_disk"` // bytes, 0 = unknown
	Verified bool   `json:"verified"` // has actually been booted on a blade

	// How far the preparation has come. An entry somebody typed a URL for is
	// not yet an image anyone can install, and calling it ready before the
	// bytes are here would be the kind of lie that costs an afternoon.
	State   string `json:"state"` // "" (older entries) | queued | working | ready | error
	Note    string `json:"note_state"`
	Updated string `json:"updated"`
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// More than one connection: otherwise a single nested query (a cursor
	// held open while a second query runs) deadlocks the server for good.
	// WAL plus busy_timeout carry the concurrency.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	for _, ddl := range []string{schema, netbootSchema} {
		if _, err := db.Exec(ddl); err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	// Late-added columns. ALTER TABLE fails on the second start because the
	// column already exists — expected, not an error.
	for _, ddl := range migrations {
		_, _ = db.Exec(ddl)
	}
	if err := scopeOffsets(db); err != nil {
		return nil, err
	}
	if err := widenSizes(db); err != nil {
		return nil, fmt.Errorf("size migration: %w", err)
	}
	return db, nil
}

// scopeOffsets moves the uniqueness of an address block from "globally" to
// "per site". With one site the two are the same thing, which is why the
// original constraint went unnoticed; with the second site it rejects a
// perfectly ordinary block. SQLite cannot alter a constraint, so the table is
// rebuilt — and only while the old one is genuinely still in place.
func scopeOffsets(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='racks'`).Scan(&ddl); err != nil {
		return nil // table just created — then it is already right
	}
	if !strings.Contains(ddl, "ip_offset INTEGER NOT NULL UNIQUE") {
		return nil
	}
	hasSite := strings.Contains(ddl, "site_id")
	sel := `SELECT id,site_id,name,size,ip_offset,location,created FROM racks`
	if !hasSite {
		sel = `SELECT id,1,name,size,ip_offset,location,created FROM racks`
	}
	stmts := []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE racks_new (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			site_id   INTEGER NOT NULL DEFAULT 1,
			name      TEXT    NOT NULL UNIQUE,
			size      INTEGER NOT NULL CHECK (size IN (2, 4, 10, 20)),
			ip_offset INTEGER NOT NULL,
			location  TEXT    NOT NULL DEFAULT '',
			created   TEXT    NOT NULL,
			UNIQUE (site_id, ip_offset))`,
		`INSERT INTO racks_new(id,site_id,name,size,ip_offset,location,created) ` + sel,
		`DROP TABLE racks`,
		`ALTER TABLE racks_new RENAME TO racks`,
		`PRAGMA foreign_keys=ON`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", strings.Split(q, "\n")[0], err)
		}
	}
	return nil
}

// widenSizes catches up the permitted sizes. SQLite cannot alter a CHECK
// constraint — the table has to be rebuilt. The rebuild only runs while the
// old constraint is genuinely still in place.
func widenSizes(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='racks'`).Scan(&ddl); err != nil {
		return nil // table just created — then it is already right
	}
	if !strings.Contains(ddl, "(4, 10, 20)") {
		return nil
	}
	stmts := []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE racks_new (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			site_id   INTEGER NOT NULL DEFAULT 1,
			name      TEXT    NOT NULL UNIQUE,
			size      INTEGER NOT NULL CHECK (size IN (2, 4, 10, 20)),
			ip_offset INTEGER NOT NULL,
			location  TEXT    NOT NULL DEFAULT '',
			created   TEXT    NOT NULL,
			UNIQUE (site_id, ip_offset))`,
		`INSERT INTO racks_new(id,site_id,name,size,ip_offset,location,created)
			SELECT id,site_id,name,size,ip_offset,location,created FROM racks`,
		`DROP TABLE racks`,
		`ALTER TABLE racks_new RENAME TO racks`,
		`PRAGMA foreign_keys=ON`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", strings.Split(q, "\n")[0], err)
		}
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (a *App) setting(key, def string) string {
	var v string
	err := a.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err != nil {
		return def
	}
	return v
}

func (a *App) setSetting(key, val string) error {
	_, err := a.db.Exec(
		`INSERT INTO settings(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, val)
	return err
}

func (a *App) logEvent(serial, level, msg string) {
	_, _ = a.db.Exec(`INSERT INTO events(ts,serial,level,msg) VALUES(?,?,?,?)`,
		now(), serial, level, msg)
}

// EventRow is one line of the activity log, already joined with the blade it
// belongs to — the log is read per BladeRunner, and a bare serial number says
// nothing to someone standing in front of the rack.
// EventRow carries json tags because it is served: /api/v1/events handed out
// TS, Serial, Level, Msg while every other endpoint speaks snake_case, and a
// client cannot be expected to guess which of the two a field will be in.
type EventRow struct {
	TS       string `json:"ts"`
	Serial   string `json:"serial"`
	Level    string `json:"level"`
	Msg      string `json:"msg"`
	Hostname string `json:"hostname,omitempty"`
	Slot     *int   `json:"slot,omitempty"`
}

// rackEvents returns the most recent activities of the blades in one
// BladeRunner, newest first. By time and not by row number: a site buffers
// events while the centre is unreachable and delivers them afterwards, so a
// row written last is not necessarily a thing that happened last. Events without a serial (creating the
// BladeRunner itself) are deliberately left out: this is the log of the
// blades, not of the enclosure.
func (a *App) rackEvents(rackID int64, limit int) ([]EventRow, error) {
	rows, err := a.db.Query(`
		SELECT e.ts, e.serial, e.level, e.msg, b.hostname, b.slot
		  FROM events e
		  JOIN blades b ON b.serial = e.serial
		 WHERE b.rack_id = ?
		 ORDER BY e.ts DESC, e.id DESC
		 LIMIT ?`, rackID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.TS, &e.Serial, &e.Level, &e.Msg, &e.Hostname, &e.Slot); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── Samples ──────────────────────────────────────────────────────────

// Sample is one measurement of the three values that move.
type Sample struct {
	TS      time.Time
	Soc     float64
	Airflow float64
	RPM     float64
}

// recordSample keeps one measurement per blade per interval. It is called on
// every status report and is silent about the ones it drops — the point is
// the shape of the curve, not the density of the points.
func (a *App) recordSample(serial string, health map[string]any) {
	soc, hasSoc := num(health["soc_temp_c"])
	if !hasSoc {
		soc, hasSoc = num(health["blade_soc_temp_c"])
	}
	air, hasAir := num(health["airflow_temp_c"])
	rpm, hasRPM := num(health["fan_rpm"])
	if !hasSoc && !hasAir && !hasRPM {
		return
	}

	var last string
	_ = a.db.QueryRow(`SELECT ts FROM samples WHERE serial=? ORDER BY ts DESC LIMIT 1`,
		serial).Scan(&last)
	pol := a.globalPolicy()
	if last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil &&
			time.Since(t) < pol.sampleEvery() {
			return
		}
	}

	nullable := func(v float64, ok bool) any {
		if !ok {
			return nil
		}
		return v
	}
	_, _ = a.db.Exec(`INSERT OR REPLACE INTO samples(serial,ts,soc,airflow,rpm)
		VALUES(?,?,?,?,?)`, serial, now(),
		nullable(soc, hasSoc), nullable(air, hasAir), nullable(rpm, hasRPM))

	// Pruned here rather than on a timer: the only thing that grows this
	// table is a blade reporting, so that is also the only moment it needs
	// cutting back.
	_, _ = a.db.Exec(`DELETE FROM samples WHERE serial=? AND ts < ?`,
		serial, time.Now().UTC().Add(-pol.sampleKeep()).Format(time.RFC3339))
}

// bladeSamples returns the stored measurements of one blade, oldest first.
func (a *App) bladeSamples(serial string, window time.Duration) ([]Sample, error) {
	from := time.Now().UTC().Add(-window).Format(time.RFC3339)
	rows, err := a.db.Query(`SELECT ts,
		COALESCE(soc,-1), COALESCE(airflow,-1), COALESCE(rpm,-1)
		FROM samples WHERE serial=? AND ts>=? ORDER BY ts`, serial, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var ts string
		var sm Sample
		if err := rows.Scan(&ts, &sm.Soc, &sm.Airflow, &sm.RPM); err != nil {
			return nil, err
		}
		sm.TS, _ = time.Parse(time.RFC3339, ts)
		out = append(out, sm)
	}
	return out, rows.Err()
}

// createSite adds a site. A site is a broadcast domain with a name: two
// racks in one network belong to one site, two networks are two sites, and
// the decision which is which is the operator's, not the code's.
func (a *App) createSite(st Site) (int64, error) {
	if st.Name == "" {
		return 0, me("err.sitename")
	}
	if !validNetBase(st.NetBase) {
		return 0, me("err.sitenet")
	}
	if st.PoolFrom <= 0 || st.PoolTo <= st.PoolFrom || st.PoolTo > 254 {
		return 0, me("err.sitepool")
	}
	res, err := a.db.Exec(`INSERT INTO sites
		(name,location,net_base,gateway,dns,domain,pool_from,pool_to,
		 offset_base,offset_step,local,token,created)
		VALUES(?,?,?,?,?,?,?,?,?,?,0,'',?)`,
		st.Name, st.Location, st.NetBase, st.Gateway, st.DNS, st.Domain,
		st.PoolFrom, st.PoolTo, st.OffsetBase, st.OffsetStep, now())
	if err != nil {
		return 0, err
	}
	a.invalidateNetCache()
	return res.LastInsertId()
}

// updateSite changes what a site is, not where its blades sit. The network
// may be moved: the addresses are derived, so they follow by themselves —
// but every reservation has to be rewritten afterwards, which is why the
// caller syncs DHCP.
func (a *App) updateSite(id int64, st Site) error {
	if st.Name == "" {
		return me("err.sitename")
	}
	if !validNetBase(st.NetBase) {
		return me("err.sitenet")
	}
	if st.PoolFrom <= 0 || st.PoolTo <= st.PoolFrom || st.PoolTo > 254 {
		return me("err.sitepool")
	}
	_, err := a.db.Exec(`UPDATE sites SET name=?,location=?,net_base=?,gateway=?,
		dns=?,domain=?,pool_from=?,pool_to=? WHERE id=?`,
		st.Name, st.Location, st.NetBase, st.Gateway, st.DNS, st.Domain,
		st.PoolFrom, st.PoolTo, id)
	if err == nil {
		a.invalidateNetCache()
	}
	return err
}

// deleteSite refuses while BladeRunners still stand in it, and refuses to
// remove the last one — without a site the addressing has no ground.
func (a *App) deleteSite(id int64) error {
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM racks WHERE site_id=?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return me("err.sitehasracks", n)
	}
	var total int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM sites`).Scan(&total); err != nil {
		return err
	}
	if total <= 1 {
		return me("err.sitelast")
	}
	_, err := a.db.Exec(`DELETE FROM sites WHERE id=?`, id)
	if err == nil {
		a.invalidateNetCache()
	}
	return err
}

// validNetBase accepts the first three octets of a /24 and nothing else.
func validNetBase(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 || (len(p) > 1 && p[0] == '0') {
			return false
		}
	}
	return true
}

// rackCounts tells the interface how full each site is, in one query rather
// than one per site.
func (a *App) rackCounts() map[int64]int {
	out := map[int64]int{}
	rows, err := a.db.Query(`SELECT site_id, COUNT(*) FROM racks GROUP BY site_id`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err == nil {
			out[id] = n
		}
	}
	return out
}

// SiteImageState is one line of a site's stock report.
type SiteImageState struct {
	ImageID string `json:"image_id"`
	State   string `json:"state"`
	Bytes   int64  `json:"bytes"`
	Note    string `json:"note"`
	TS      string `json:"ts"`
}

// recordSiteImages replaces what a site says it holds. Replaced rather than
// merged: the site is the only authority on its own disk, and an image it no
// longer mentions is one it no longer has.
func (a *App) recordSiteImages(siteID int64, in []SiteImageState) {
	if _, err := a.db.Exec(`DELETE FROM site_images WHERE site_id=?`, siteID); err != nil {
		return
	}
	for _, im := range in {
		if im.ImageID == "" {
			continue
		}
		_, _ = a.db.Exec(`INSERT OR REPLACE INTO site_images(site_id,image_id,state,bytes,note,ts)
			VALUES(?,?,?,?,?,?)`, siteID, im.ImageID, im.State, im.Bytes, im.Note, now())
	}
}

// siteImages reads the stock of every site in one query — the overview shows
// them all, and one query per site would be one query too many.
func (a *App) siteImages() map[int64][]SiteImageState {
	out := map[int64][]SiteImageState{}
	rows, err := a.db.Query(`SELECT site_id,image_id,state,bytes,note,ts
		FROM site_images ORDER BY site_id, image_id`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var im SiteImageState
		if err := rows.Scan(&id, &im.ImageID, &im.State, &im.Bytes, &im.Note, &im.TS); err == nil {
			out[id] = append(out[id], im)
		}
	}
	return out
}

// ── BladeRunners ─────────────────────────────────────────────────────

const rackCols = `id,site_id,name,size,ip_offset,location,created`

func (a *App) listRacks() ([]Rack, error) {
	rows, err := a.db.Query(`SELECT ` + rackCols + ` FROM racks ORDER BY site_id, ip_offset`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Rack{}
	for rows.Next() {
		var r Rack
		if err := rows.Scan(&r.ID, &r.SiteID, &r.Name, &r.Size, &r.IPOffset, &r.Location, &r.Created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (a *App) getRack(id int64) (*Rack, error) {
	var r Rack
	err := a.db.QueryRow(`SELECT `+rackCols+` FROM racks WHERE id=?`, id).
		Scan(&r.ID, &r.SiteID, &r.Name, &r.Size, &r.IPOffset, &r.Location, &r.Created)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// nextRackOffset hands out the next free address block WITHIN a site. Every
// BladeRunner reserves the same block size regardless of how many nodes it
// has — that keeps addresses stable when a larger unit later takes its place.
//
// Blocks are unique per site only. Two sites may use the same network as long
// as they stay separate.
func (a *App) nextRackOffset(siteID int64) (int, error) {
	st, err := a.getSite(siteID)
	if err != nil {
		return 0, err
	}
	racks, err := a.listRacks()
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, r := range racks {
		if r.SiteID == siteID {
			used[r.IPOffset] = true
		}
	}
	for off := st.OffsetBase; off+st.OffsetStep <= 250; off += st.OffsetStep {
		if !used[off] {
			return off, nil
		}
	}
	return 0, me("err.nooffset")
}

// validSize knows the four BladeRunner sizes.
func validSize(n int) bool {
	return n == 2 || n == 4 || n == 10 || n == 20
}

// updateRack changes name, location and size. Shrinking is allowed only
// while no blade sits in a slot that would disappear — otherwise the blade
// would hold a position outside its enclosure.
// moveRack moves a BladeRunner to another site. The addresses of its blades
// are derived from the site, so they all change — which is why the caller
// rewrites the reservations, and why this is a deliberate action and not a
// side effect of editing a name.
//
// The block offset is taken fresh at the destination: the old one may well be
// occupied there, and keeping a number that means something different in
// another network would be worse than renumbering.
func (a *App) moveRack(id, siteID int64) error {
	rk, err := a.getRack(id)
	if err != nil {
		return me("err.racknotfound", id)
	}
	if rk.SiteID == siteID {
		return nil
	}
	if _, err := a.getSite(siteID); err != nil {
		return me("err.sitegone")
	}
	off, err := a.nextRackOffset(siteID)
	if err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE racks SET site_id=?, ip_offset=? WHERE id=?`,
		siteID, off, id); err != nil {
		return me("err.updatefail", err.Error())
	}
	a.invalidateNetCache()
	return nil
}
func (a *App) updateRack(id int64, name, location string, size int) error {
	rk, err := a.getRack(id)
	if err != nil {
		return me("err.racknotfound", id)
	}
	if size == 0 {
		size = rk.Size
	}
	if !validSize(size) {
		return me("err.badsize", size)
	}
	if size < rk.Size {
		max, err := a.maxOccupiedSlot(id)
		if err != nil {
			return err
		}
		if max > size {
			return me("err.shrink", size, max)
		}
	}
	if name == "" {
		name = rk.Name
	}
	_, err = a.db.Exec(`UPDATE racks SET name=?,location=?,size=? WHERE id=?`,
		name, location, size, id)
	if err != nil {
		return me("err.updatefail", err.Error())
	}
	return nil
}

func (a *App) maxOccupiedSlot(rackID int64) (int, error) {
	var max *int
	err := a.db.QueryRow(`SELECT MAX(slot) FROM blades WHERE rack_id=?`, rackID).Scan(&max)
	if err != nil {
		return 0, err
	}
	if max == nil {
		return 0, nil
	}
	return *max, nil
}

func (a *App) countBladesInRack(rackID int64) int {
	var n int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM blades WHERE rack_id=?`, rackID).Scan(&n)
	return n
}

// requestWipe arms the erase. The slot is NOT cleared here: a blade whose
// disk is half erased must not vanish from the interface, because then nobody
// knows what state it is in. The slot is freed when the installer says the
// disk is empty.
func (a *App) requestWipe(serial string) error {
	res, err := a.db.Exec(
		`UPDATE blades SET install_state=? WHERE serial=?`, installWipe, serial)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return me("err.bladegone")
	}
	return nil
}

// finishWipe takes the blade out of its slot once the disk is empty. It keeps
// the record: the serial number is the same piece of hardware, and its
// history is worth more than a tidy table.
func (a *App) finishWipe(serial string) error {
	_, err := a.db.Exec(`UPDATE blades SET rack_id=NULL, slot=NULL, image='',
		install_state=?, state='new', hostname='', config_applied='',
		facts_json='{}', health_json='{}' WHERE serial=?`, installIdle, serial)
	return err
}

// requestInstall records that this blade should be written on its next
// netboot. That is a deliberate act — merely assigning an image triggers
// nothing.
func (a *App) requestInstall(serial string) error {
	res, err := a.db.Exec(
		`UPDATE blades SET install_state=? WHERE serial=? AND image<>''`,
		installPending, serial)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return me("err.needimage")
	}
	// The reservation carries the netboot switch — it has to follow.
	_, _ = a.syncDHCP()
	return nil
}

// listImages returns the catalogue. The interface needs it for the per-blade
// image picker.
// imageExists says whether the catalogue already knows this id — the
// difference between creating an entry and touching one.
func (a *App) imageExists(id string) bool {
	var n int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM images WHERE id=?`, id).Scan(&n)
	return n > 0
}

func (a *App) listImages() ([]Image, error) {
	rows, err := a.db.Query(`SELECT id,url,sha256,seed,os_id,notes,local,bytes,created,
		kernel,min_disk,verified,state,note,updated FROM images ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Image{}
	for rows.Next() {
		var i Image
		var verified int
		if err := rows.Scan(&i.ID, &i.URL, &i.SHA256, &i.Seed, &i.OSID,
			&i.Notes, &i.Local, &i.Bytes, &i.Created,
			&i.Kernel, &i.MinDisk, &verified, &i.State, &i.Note, &i.Updated); err != nil {
			return nil, err
		}
		i.Verified = verified != 0
		out = append(out, i)
	}
	return out, rows.Err()
}

// ── Sites ────────────────────────────────────────────────────────────

const siteCols = `id,name,location,net_base,gateway,dns,domain,
	pool_from,pool_to,offset_base,offset_step,local,token,last_seen,created,
	payload,site_version,enroll_code,enroll_until,host_prefix`

func scanSite(sc interface{ Scan(...any) error }) (*Site, error) {
	var st Site
	var local int
	err := sc.Scan(&st.ID, &st.Name, &st.Location, &st.NetBase, &st.Gateway, &st.DNS,
		&st.Domain, &st.PoolFrom, &st.PoolTo, &st.OffsetBase, &st.OffsetStep,
		&local, &st.Token, &st.LastSeen, &st.Created, &st.Payload, &st.SiteVersion,
		&st.EnrollCode, &st.EnrollUntil, &st.HostPrefix)
	if err != nil {
		return nil, err
	}
	st.Local = local != 0
	return &st, nil
}

func (a *App) listSites() ([]Site, error) {
	rows, err := a.db.Query(`SELECT ` + siteCols + ` FROM sites ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Site{}
	for rows.Next() {
		st, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

func (a *App) getSite(id int64) (*Site, error) {
	return scanSite(a.db.QueryRow(`SELECT `+siteCols+` FROM sites WHERE id=?`, id))
}

// localSite is the site whose network presence this process serves itself.
// Until sheath-site is split out there is exactly one.
// localSite is the site this server physically stands in, and nothing else.
// It used to fall back to the first site, which meant that after the centre
// moved to a machine in another network the interface still labelled a site
// it had left as "here" — the fallback answered a question nobody had asked.
func (a *App) localSite() (*Site, error) {
	return scanSite(a.db.QueryRow(`SELECT ` + siteCols + ` FROM sites WHERE local=1 ORDER BY id LIMIT 1`))
}

// defaultSite is the different question: which site does something belong to
// when nobody said. The site here, if there is one, and otherwise the first —
// a BladeRunner created without a choice has to land somewhere.
func (a *App) defaultSite() (*Site, error) {
	if st, err := a.localSite(); err == nil {
		return st, nil
	}
	return scanSite(a.db.QueryRow(`SELECT ` + siteCols + ` FROM sites ORDER BY id LIMIT 1`))
}

// ensureDefaultSite creates a site from the previous global settings on the
// first start after the change. Without it, existing BladeRunners would have
// no site and therefore no network.
func (a *App) ensureDefaultSite(netBase string) error {
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM sites`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		if netBase != "" {
			_, _ = a.db.Exec(`UPDATE sites SET net_base=? WHERE local=1`, netBase)
		}
		return nil
	}
	base := netBase
	if base == "" {
		base = a.setting("net_base", "10.0.0")
	}
	_, err := a.db.Exec(`INSERT INTO sites
		(id,name,location,net_base,gateway,dns,domain,pool_from,pool_to,
		 offset_base,offset_step,local,created)
		VALUES(1,?,?,?,?,?,?,?,?,?,?,1,?)`,
		"Site 1", "", base, base+".1", base+".10", "blades.lan",
		a.settingInt("pool_from", 210), a.settingInt("pool_to", 240),
		a.settingInt("rack_offset_base", 100), a.settingInt("rack_offset_step", 20),
		now())
	return err
}

func (a *App) settingInt(key string, def int) int {
	var v int
	s := a.setting(key, "")
	if s == "" {
		return def
	}
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return def
	}
	return v
}

// ── Blades ───────────────────────────────────────────────────────────

const bladeCols = `serial,short_serial,rack_id,slot,hostname,mac,image,variant,state,
	groups_json,facts_json,health_json,install_state,installed_at,config_applied,last_seen,created`

func (a *App) scanBlade(sc interface{ Scan(...any) error }) (*Blade, error) {
	var b Blade
	var groups, facts, health string
	err := sc.Scan(&b.Serial, &b.ShortSerial, &b.RackID, &b.Slot, &b.Hostname, &b.MAC,
		&b.Image, &b.Variant, &b.State, &groups, &facts, &health,
		&b.InstallState, &b.InstalledAt, &b.ConfigApp, &b.LastSeen, &b.Created)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(groups), &b.Groups)
	if b.Groups == nil {
		b.Groups = []string{}
	}
	b.Facts = json.RawMessage(facts)
	b.Health = json.RawMessage(health)
	return &b, nil
}

func (a *App) listBlades() ([]Blade, error) {
	// Order matters: anything that queries the database itself must happen
	// BEFORE the cursor opens. Otherwise this query holds one connection
	// while waiting for the next.
	racks, err := a.listRacks()
	if err != nil {
		return nil, err
	}
	byID := map[int64]Rack{}
	for _, r := range racks {
		byID[r.ID] = r
	}
	nets := a.siteNets()
	idx := map[int64]int{}
	for _, r := range racks {
		idx[r.ID] = a.rackIndex(r)
	}

	rows, err := a.db.Query(`SELECT ` + bladeCols + ` FROM blades ORDER BY rack_id, slot, serial`)
	if err != nil {
		return nil, err
	}
	out := []Blade{}
	for rows.Next() {
		b, err := a.scanBlade(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range out {
		decorate(&out[i], byID, nets, idx)
	}
	return out, nil
}

func (a *App) getBlade(serial string) (*Blade, error) {
	row := a.db.QueryRow(`SELECT `+bladeCols+` FROM blades WHERE serial=?`, serial)
	b, err := a.scanBlade(row)
	if err != nil {
		return nil, err
	}
	racks, _ := a.listRacks()
	byID := map[int64]Rack{}
	for _, r := range racks {
		byID[r.ID] = r
	}
	idx := map[int64]int{}
	for _, r := range racks {
		idx[r.ID] = a.rackIndex(r)
	}
	decorate(b, byID, a.siteNets(), idx)
	return b, nil
}

// siteNet is what a blade's address needs from its site, and nothing more.
type siteNet struct {
	Name    string
	NetBase string
}

// siteNets reads every site once. Deliberately called before a cursor is
// opened: decorate must stay free of database access, and with a single
// connection a nested query would deadlock.
func (a *App) siteNets() map[int64]siteNet {
	out := map[int64]siteNet{}
	sites, err := a.listSites()
	if err != nil {
		return out
	}
	for _, st := range sites {
		out[st.ID] = siteNet{Name: st.Name, NetBase: st.NetBase}
	}
	return out
}

// decorate fills in the derived fields. Deliberately free of database access
// so it stays safe to call while a cursor is open.
func decorate(b *Blade, racks map[int64]Rack, nets map[int64]siteNet, idx map[int64]int) {
	if b.RackID == nil || b.Slot == nil {
		return
	}
	r, ok := racks[*b.RackID]
	if !ok {
		return
	}
	b.RackName = r.Name
	b.RackIdx = idx[r.ID]
	b.SiteID = r.SiteID
	sn := nets[r.SiteID]
	b.SiteName = sn.Name
	if *b.Slot >= 1 && *b.Slot <= r.Size && sn.NetBase != "" {
		b.IP = fmt.Sprintf("%s.%d", sn.NetBase, r.IPOffset+*b.Slot)
	}
}

// ── Alerts ───────────────────────────────────────────────────────────
//
// One row per blade that is currently not well. The row is what keeps a
// notification from being sent twice, and what makes "it recovered" a thing
// that can be said at all.

func (a *App) openAlerts() (map[string]alert, error) {
	rows, err := a.db.Query(`SELECT serial,level,reason,since,notified FROM alerts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]alert{}
	for rows.Next() {
		var al alert
		var since string
		if err := rows.Scan(&al.Serial, &al.Level, &al.Reason, &since, &al.Notified); err != nil {
			continue
		}
		al.Since, _ = time.Parse(time.RFC3339, since)
		out[al.Serial] = al
	}
	return out, rows.Err()
}

func (a *App) raiseAlert(al alert) error {
	notifiedAt := ""
	if al.Notified != "" {
		notifiedAt = now()
	}
	_, err := a.db.Exec(`INSERT INTO alerts(serial,level,reason,since,notified,notified_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(serial) DO UPDATE SET level=excluded.level, reason=excluded.reason,
		  since=excluded.since, notified=excluded.notified,
		  notified_at=CASE WHEN excluded.notified<>'' THEN excluded.notified_at ELSE alerts.notified_at END`,
		al.Serial, al.Level, al.Reason, al.Since.UTC().Format(time.RFC3339), al.Notified, notifiedAt)
	return err
}

func (a *App) clearAlert(serial string) error {
	_, err := a.db.Exec(`DELETE FROM alerts WHERE serial=?`, serial)
	return err
}

// recordSiteSelf keeps what a site says about itself: which netboot payload it
// serves and which version of sheath-site is serving it. Written on every
// heartbeat, because a payload can change between two of them.
func (a *App) recordSiteSelf(id int64, payload, siteVersion string) {
	_, _ = a.db.Exec(`UPDATE sites SET payload=?, site_version=? WHERE id=?`,
		payload, siteVersion, id)
}

// mergeFacts folds a few facts into what is already known about a blade,
// leaving the rest alone. The mini OS knows the hardware and nothing about
// the operating system; the agent knows both. Whichever spoke last should not
// erase what the other one said.
func (a *App) mergeFacts(serial string, add map[string]any) {
	var raw string
	if err := a.db.QueryRow(`SELECT facts_json FROM blades WHERE serial=?`, serial).Scan(&raw); err != nil {
		return
	}
	facts := map[string]any{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &facts)
	}
	for k, v := range add {
		facts[k] = v
	}
	out, err := json.Marshal(facts)
	if err != nil {
		return
	}
	_, _ = a.db.Exec(`UPDATE blades SET facts_json=? WHERE serial=?`, string(out), serial)
}
