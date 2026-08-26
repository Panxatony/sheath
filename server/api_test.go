package main

import (
	"encoding/json"
	"testing"
)

func TestKeepHardware(t *testing.T) {
	before := `{"board":"Compute Module 4","ram_mb":8192,"os_name":"Ubuntu 24.04.3 LTS","nvme_bytes":500107862016}`
	// What a blade on an upstream kernel reports: it knows its system and not
	// its module.
	now := `{"os_name":"Debian GNU/Linux 13 (trixie)","kernel":"6.12.43-arm64"}`

	var got map[string]any
	if err := json.Unmarshal([]byte(keepHardware(before, now)), &got); err != nil {
		t.Fatal(err)
	}
	if got["os_name"] != "Debian GNU/Linux 13 (trixie)" {
		t.Errorf("the new report should win where it speaks: %v", got["os_name"])
	}
	if got["ram_mb"] != float64(8192) || got["board"] != "Compute Module 4" {
		t.Errorf("the module's own description was lost: %v", got)
	}
	if _, stale := got["kernel"]; !stale {
		t.Error("the new report's own fields went missing")
	}

	// A report that does mention the hardware replaces it — a module swapped
	// into the same slot is a different module.
	fresh := keepHardware(before, `{"board":"Compute Module 5","ram_mb":16384}`)
	_ = json.Unmarshal([]byte(fresh), &got)
	if got["board"] != "Compute Module 5" || got["ram_mb"] != float64(16384) {
		t.Errorf("a report that knows better was overruled: %v", got)
	}
}
