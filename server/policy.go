package main

import (
	"encoding/json"
	"time"
)

// Policy is the set of numbers that decide when Rookery calls something a
// problem, and how long it remembers things.
//
// They used to stand in the code, which is the right place for a constant and
// the wrong place for a judgement. A blade in a ventilated rack and one in a
// warm office do not share a temperature at which someone should be woken;
// nor does a fleet of three share a heartbeat timeout with a fleet of two
// hundred. So: defaults that match what the code did before, a global
// setting, and a per-site override for the ones a site can reasonably differ
// on.
type Policy struct {
	SocWarn    float64 `json:"soc_warn_c"`
	SocCrit    float64 `json:"soc_crit_c"`
	NVMeWarn   float64 `json:"nvme_warn_c"`
	DiskWarn   float64 `json:"disk_warn_pct"`
	DiskCrit   float64 `json:"disk_crit_pct"`
	OfflineMin int     `json:"offline_after_min"`

	// NoWipe forbids erasing a blade's disk from the interface at this site.
	// Deliberately phrased as a prohibition: the default is that an operator
	// may erase a blade they can already reinstall, and a site that wants the
	// stronger rule says so.
	NoWipe bool `json:"no_wipe,omitempty"`

	// Global only: these are properties of the central server's bookkeeping,
	// not of a place.
	CommandTTLMin  int `json:"command_ttl_min,omitempty"`
	SampleEveryMin int `json:"sample_every_min,omitempty"`
	SampleKeepHrs  int `json:"sample_keep_hours,omitempty"`
}

// defaultPolicy is what the code did before any of this was configurable.
func defaultPolicy() Policy {
	return Policy{
		SocWarn: 70, SocCrit: 80,
		NVMeWarn: 70,
		DiskWarn: 85, DiskCrit: 95,
		OfflineMin:     5,
		CommandTTLMin:  15,
		SampleEveryMin: 5,
		SampleKeepHrs:  48,
	}
}

// fill replaces zero values with the fallback's. A field left empty in a
// stored policy means "unchanged", not "zero" — nobody means a critical
// temperature of 0 °C.
func (p Policy) fill(from Policy) Policy {
	if p.SocWarn == 0 {
		p.SocWarn = from.SocWarn
	}
	if p.SocCrit == 0 {
		p.SocCrit = from.SocCrit
	}
	if p.NVMeWarn == 0 {
		p.NVMeWarn = from.NVMeWarn
	}
	if p.DiskWarn == 0 {
		p.DiskWarn = from.DiskWarn
	}
	if p.DiskCrit == 0 {
		p.DiskCrit = from.DiskCrit
	}
	if p.OfflineMin == 0 {
		p.OfflineMin = from.OfflineMin
	}
	if !p.NoWipe {
		p.NoWipe = from.NoWipe
	}
	if p.CommandTTLMin == 0 {
		p.CommandTTLMin = from.CommandTTLMin
	}
	if p.SampleEveryMin == 0 {
		p.SampleEveryMin = from.SampleEveryMin
	}
	if p.SampleKeepHrs == 0 {
		p.SampleKeepHrs = from.SampleKeepHrs
	}
	return p
}

func (p Policy) commandTTL() time.Duration {
	return time.Duration(p.CommandTTLMin) * time.Minute
}
func (p Policy) sampleEvery() time.Duration {
	return time.Duration(p.SampleEveryMin) * time.Minute
}
func (p Policy) sampleKeep() time.Duration {
	return time.Duration(p.SampleKeepHrs) * time.Hour
}
func (p Policy) offlineAfter() time.Duration {
	return time.Duration(p.OfflineMin) * time.Minute
}

// globalPolicy reads the installation-wide policy, filled with the defaults.
func (a *App) globalPolicy() Policy {
	var p Policy
	if raw := a.setting("policy", ""); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return p.fill(defaultPolicy())
}

func (a *App) setGlobalPolicy(p Policy) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return a.setSetting("policy", string(raw))
}

// sitePolicy resolves what applies at one site: its own numbers where it set
// them, the global ones otherwise.
func (a *App) sitePolicy(siteID int64) Policy {
	g := a.globalPolicy()
	if siteID == 0 {
		return g
	}
	var raw string
	if err := a.db.QueryRow(`SELECT policy_json FROM sites WHERE id=?`, siteID).
		Scan(&raw); err != nil || raw == "" {
		return g
	}
	var p Policy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return g
	}
	return p.fill(g)
}

// siteOwnPolicy returns only what this site itself has set — the form has to
// show empty fields where the site inherits, not the inherited value pretending
// to be its own.
func (a *App) siteOwnPolicy(siteID int64) Policy {
	var raw string
	var p Policy
	if err := a.db.QueryRow(`SELECT policy_json FROM sites WHERE id=?`, siteID).
		Scan(&raw); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return p
}

func (a *App) setSitePolicy(siteID int64, p Policy) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`UPDATE sites SET policy_json=? WHERE id=?`, string(raw), siteID)
	return err
}
