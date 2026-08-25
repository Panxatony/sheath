package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
-- Once rookery-site is split out, there will be none left.
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
    name      TEXT    NOT NULL UNIQUE,
    size      INTEGER NOT NULL CHECK (size IN (2, 4, 10, 20)),
    ip_offset INTEGER NOT NULL UNIQUE,
    location  TEXT    NOT NULL DEFAULT '',
    created   TEXT    NOT NULL
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
`

// install_state separates "this blade has an image assigned" from "this
// blade should be written now". Without that split every netboot would
// trigger a full reinstall — endlessly, on a blade whose BOOT_ORDER puts the
// network ahead of the NVMe.
var migrations = []string{
	`ALTER TABLE blades ADD COLUMN install_state TEXT NOT NULL DEFAULT 'idle'`,
	`ALTER TABLE blades ADD COLUMN installed_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE racks ADD COLUMN site_id INTEGER NOT NULL DEFAULT 1`,
}

const (
	installIdle    = "idle"    // nothing to do
	installPending = "pending" // write on the next netboot
	installDone    = "done"    // written
	installError   = "error"
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
	LastSeen   string `json:"last_seen"`
	Created    string `json:"created"`
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
	if err := widenSizes(db); err != nil {
		return nil, fmt.Errorf("Migration Groessen: %w", err)
	}
	return db, nil
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
			name      TEXT    NOT NULL UNIQUE,
			size      INTEGER NOT NULL CHECK (size IN (2, 4, 10, 20)),
			ip_offset INTEGER NOT NULL UNIQUE,
			location  TEXT    NOT NULL DEFAULT '',
			created   TEXT    NOT NULL)`,
		`INSERT INTO racks_new SELECT id,name,size,ip_offset,location,created FROM racks`,
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
type EventRow struct {
	TS       string
	Serial   string
	Level    string
	Msg      string
	Hostname string
	Slot     *int
}

// rackEvents returns the most recent activities of the blades in one
// BladeRunner, newest first. Events without a serial (creating the
// BladeRunner itself) are deliberately left out: this is the log of the
// blades, not of the enclosure.
func (a *App) rackEvents(rackID int64, limit int) ([]EventRow, error) {
	rows, err := a.db.Query(`
		SELECT e.ts, e.serial, e.level, e.msg, b.hostname, b.slot
		  FROM events e
		  JOIN blades b ON b.serial = e.serial
		 WHERE b.rack_id = ?
		 ORDER BY e.id DESC
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

const (
	sampleEvery = 5 * time.Minute
	sampleKeep  = 48 * time.Hour
)

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
	if last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil &&
			time.Since(t) < sampleEvery {
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
		serial, time.Now().UTC().Add(-sampleKeep).Format(time.RFC3339))
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
func (a *App) listImages() ([]Image, error) {
	rows, err := a.db.Query(`SELECT id,url,sha256,seed,os_id,notes,local,bytes,created
		FROM images ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Image{}
	for rows.Next() {
		var i Image
		if err := rows.Scan(&i.ID, &i.URL, &i.SHA256, &i.Seed, &i.OSID,
			&i.Notes, &i.Local, &i.Bytes, &i.Created); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ── Sites ────────────────────────────────────────────────────────────

const siteCols = `id,name,location,net_base,gateway,dns,domain,
	pool_from,pool_to,offset_base,offset_step,local,last_seen,created`

func scanSite(sc interface{ Scan(...any) error }) (*Site, error) {
	var st Site
	var local int
	err := sc.Scan(&st.ID, &st.Name, &st.Location, &st.NetBase, &st.Gateway, &st.DNS,
		&st.Domain, &st.PoolFrom, &st.PoolTo, &st.OffsetBase, &st.OffsetStep,
		&local, &st.LastSeen, &st.Created)
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
// Until rookery-site is split out there is exactly one.
func (a *App) localSite() (*Site, error) {
	st, err := scanSite(a.db.QueryRow(`SELECT ` + siteCols + ` FROM sites WHERE local=1 ORDER BY id LIMIT 1`))
	if err == nil {
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
	netBase := a.netBase()
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
		decorate(&out[i], byID, netBase, idx)
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
	decorate(b, byID, a.netBase(), idx)
	return b, nil
}

// decorate fills in the derived fields. Deliberately free of database access
// so it stays safe to call while a cursor is open.
func decorate(b *Blade, racks map[int64]Rack, netBase string, idx map[int64]int) {
	if b.RackID == nil || b.Slot == nil {
		return
	}
	r, ok := racks[*b.RackID]
	if !ok {
		return
	}
	b.RackName = r.Name
	b.RackIdx = idx[r.ID]
	if *b.Slot >= 1 && *b.Slot <= r.Size {
		b.IP = fmt.Sprintf("%s.%d", netBase, r.IPOffset+*b.Slot)
	}
}
