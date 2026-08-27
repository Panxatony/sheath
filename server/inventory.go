package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// The inventory.
//
// Every other page in here answers "how is it doing". This one answers "what
// is it" — the question asked when a blade has to be replaced, when a job
// needs eight gigabytes, or when somebody wants to know what is actually
// screwed into the rack without unscrewing it.
//
// It is a reading of what the blades reported, across every site: nothing is
// probed for this page, and a blade that has been away for a week still shows
// what it last said, marked as being from then.

type invRow struct {
	Serial   string
	Hostname string
	Site     string
	Rack     string
	Slot     string
	IP       string
	MAC      string

	Board   string // "Compute Module 4 Rev 1.1"
	RAM     string
	RAMMB   int
	SoC     string
	Maker   string
	Cores   string
	MHz     string
	Rev     string // the raw revision code, because it is the one true name
	EMMC    string
	NVMe    string
	Model   string // what the device tree calls it, when the code says nothing
	Radio   string
	Boot    string // bootloader, its build date, and how the blade came up
	VC      string // the VideoCore firmware, where the system can say
	BootVia string
	OS      string
	Kernel  string
	Agent   string
	Seen    string
	LED     string
	Unused  bool // in no BladeRunner, so it can be removed from here
	Missing bool // nothing hardware-wise has ever been reported
}

// invSummary is the line above the table: what the fleet adds up to.
type invSummary struct {
	Blades   int
	RAMTotal string
	NVMe     string
	Boards   []string // "4 × Compute Module 4 (8 GB)"
	Unknown  int
}

func (a *App) inventory(l Lang) ([]invRow, invSummary, error) {
	blades, err := a.listBlades()
	if err != nil {
		return nil, invSummary{}, err
	}
	rows := make([]invRow, 0, len(blades))
	sum := invSummary{}
	var ramMB, nvmeB int64
	kinds := map[string]int{}

	for i := range blades {
		b := &blades[i]
		var f map[string]any
		if len(b.Facts) > 0 {
			_ = json.Unmarshal(b.Facts, &f)
		}
		var h map[string]any
		if len(b.Health) > 0 {
			_ = json.Unmarshal(b.Health, &h)
		}

		r := invRow{
			Serial: b.Serial, Hostname: b.Hostname, Site: b.SiteName,
			Rack: b.RackName, IP: b.IP, MAC: b.MAC,
			OS: str(f, "os_name"), Kernel: str(f, "kernel"),
			Agent: str(f, "agent_version"), Model: str(f, "model"),
			Rev: str(f, "board_revision"), SoC: str(f, "soc"), Maker: str(f, "maker"),
		}
		if b.Slot != nil {
			r.Slot = fmt.Sprintf("%02d", *b.Slot)
		}
		r.Unused = b.RackID == nil
		if b.LastSeen != "" {
			r.Seen = ago(l, b.LastSeen)
		}
		lvl, _ := a.evalHealth(b)
		r.LED = lvl.chip()

		board := str(f, "board")
		if board != "" {
			r.Board = board
			if rev := str(f, "board_rev"); rev != "" {
				r.Board += " Rev " + rev
			}
		} else if r.Model != "" {
			// No revision code, but the device tree still names the board.
			r.Board = strings.TrimPrefix(r.Model, "Raspberry Pi ")
		}

		if mb, ok := num(f["ram_mb"]); ok && mb > 0 {
			r.RAMMB = int(mb)
			r.RAM = ramText(int64(mb) << 20)
			ramMB += int64(mb)
		} else if tb, ok := num(h["mem_total_b"]); ok && tb > 0 {
			// What the kernel can hand out, which is a little less than what
			// is soldered on. Said as "about", because it is.
			r.RAM = "~" + ramText(int64(tb))
			ramMB += int64(tb) / (1 << 20)
		}
		if n, ok := num(f["cpu_cores"]); ok && n > 0 {
			r.Cores = fmt.Sprintf("%.0f", n)
		}
		if n, ok := num(f["cpu_mhz"]); ok && n > 0 {
			r.MHz = fmt.Sprintf("%.0f MHz", n)
		}
		if n, ok := num(f["emmc_bytes"]); ok {
			if n > 0 {
				r.EMMC = T(l, "inv.emmc") + " " + human(int64(n))
			} else {
				// A Lite has no eMMC at all, and saying "eMMC: no eMMC" is
				// how a table starts sounding like a form.
				r.EMMC = T(l, "inv.lite")
			}
		}
		if n, ok := num(f["nvme_bytes"]); ok && n > 0 {
			r.NVMe = human(int64(n))
			nvmeB += int64(n)
			if m := str(f, "nvme_model"); m != "" {
				r.NVMe += " · " + m
			}
		}
		if w, ok := f["wireless"].(bool); ok && w {
			r.Radio = T(l, "inv.radio")
		}
		if bl := str(f, "bootloader_short"); bl != "" {
			r.Boot = bl
			if d := str(f, "bootloader_built"); d != "" {
				r.Boot += " · " + d
			}
		}
		r.VC = str(f, "vc_firmware")
		if m := str(f, "boot_mode"); m != "" {
			// The firmware has one boot mode for both card devices, because
			// on a Compute Module they are the same interface. What the blade
			// actually has decides which word is true.
			if m == "sd" {
				if n, ok := num(f["emmc_bytes"]); ok && n > 0 {
					m = "emmc"
				}
			}
			r.BootVia = T(l, "inv.via."+m)
		}
		if r.Board == "" && r.RAM == "" {
			r.Missing = true
			sum.Unknown++
		} else {
			key := r.Board
			if r.RAMMB > 0 {
				key += " (" + ramText(int64(r.RAMMB)<<20) + ")"
			}
			kinds[key]++
		}
		rows = append(rows, r)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Site != rows[j].Site {
			return rows[i].Site < rows[j].Site
		}
		if rows[i].Rack != rows[j].Rack {
			return rows[i].Rack < rows[j].Rack
		}
		return rows[i].Slot < rows[j].Slot
	})

	sum.Blades = len(rows)
	sum.RAMTotal = ramText(ramMB << 20)
	if nvmeB > 0 {
		sum.NVMe = human(nvmeB)
	}
	for k, n := range kinds {
		sum.Boards = append(sum.Boards, fmt.Sprintf("%d × %s", n, k))
	}
	sort.Strings(sum.Boards)
	return rows, sum, nil
}

// ramText says memory the way the box does: 8 GB, not 7.8 GiB. Anything that
// is not a whole number of gigabytes keeps one decimal, so an odd figure
// looks odd instead of looking rounded.
func ramText(bytes int64) string {
	gb := float64(bytes) / float64(1<<30)
	switch {
	case gb <= 0:
		return "—"
	case gb < 1:
		return fmt.Sprintf("%.0f MB", float64(bytes)/float64(1<<20))
	case gb == float64(int64(gb)):
		return fmt.Sprintf("%d GB", int64(gb))
	default:
		return fmt.Sprintf("%.1f GB", gb)
	}
}

func str(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// hInventoryAPI is the same table for something that is not a browser — a
// spreadsheet, a spare-parts order, a question about how much memory the
// fleet has.
func (a *App) hInventoryAPI(w http.ResponseWriter, r *http.Request) {
	rows, sum, err := a.inventory(a.langOf(r))
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"blades": rows, "summary": sum})
}

// freeSlot is one place a blade could be put, named the way somebody at the
// rack would say it: which building, which enclosure, which slot.
type freeSlot struct {
	Value string // "<rack id>:<slot>"
	Label string
}

// freeSlots lists every empty slot in the fleet. The inventory is where an
// unplaced blade is visible, so it is where putting it somewhere belongs —
// and a blade is put into a slot, not into a BladeRunner, so one list of
// slots beats two dependent menus.
func (a *App) freeSlots() []freeSlot {
	racks, err := a.listRacks()
	if err != nil {
		return nil
	}
	blades, err := a.listBlades()
	if err != nil {
		return nil
	}
	taken := map[string]bool{}
	for _, b := range blades {
		if b.RackID != nil && b.Slot != nil {
			taken[fmt.Sprintf("%d:%d", *b.RackID, *b.Slot)] = true
		}
	}
	var out []freeSlot
	for _, rk := range racks {
		site := a.siteName(rk.SiteID)
		for s := 1; s <= rk.Size; s++ {
			key := fmt.Sprintf("%d:%d", rk.ID, s)
			if taken[key] {
				continue
			}
			label := fmt.Sprintf("%s · %s · %02d", site, rk.Name, s)
			if site == "" {
				label = fmt.Sprintf("%s · %02d", rk.Name, s)
			}
			out = append(out, freeSlot{Value: key, Label: label})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}
