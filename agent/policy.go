package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// agentPolicy is the "agent" section of a blade's desired state: how often it
// asks, what it is allowed to do, and whether it may restart itself after a
// change only the firmware reads.
//
//	agent:
//	  interval: 60                      # seconds between passes
//	  jitter: 15                        # random spread, so a rack does not
//	                                    # ask in lockstep after a power cut
//	  allow: [identify, identify_off, reboot]
//	  reboot_on_boot_config: true
//	  maintenance: "02:00-04:00"        # only restart inside this window
//
// Every field is optional; what is not said keeps the behaviour the agent had
// before any of this existed.
type agentPolicy struct {
	Interval    time.Duration
	Jitter      time.Duration
	Allow       []string // empty means: everything
	RebootOnCfg bool
	Window      string // "HH:MM-HH:MM", empty means any time
}

// num reads a number that arrived as JSON, where every number is a float.
func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	}
	return 0, false
}

func readAgentPolicy(cfg map[string]any) agentPolicy {
	var p agentPolicy
	sec, _ := cfg["agent"].(map[string]any)
	if sec == nil {
		return p
	}
	if v, ok := num(sec["interval"]); ok && v > 0 {
		p.Interval = time.Duration(v) * time.Second
	}
	if v, ok := num(sec["jitter"]); ok && v >= 0 {
		p.Jitter = time.Duration(v) * time.Second
	}
	if v, ok := sec["reboot_on_boot_config"].(bool); ok {
		p.RebootOnCfg = v
	}
	if v, ok := sec["maintenance"].(string); ok {
		p.Window = strings.TrimSpace(v)
	}
	p.Allow = stringList(sec["allow"])
	return p
}

// allows reports whether this blade may carry out a command. An empty list
// means every command — the restriction exists for the blade that must never
// be reimaged by accident, not as a default posture.
func (p agentPolicy) allows(kind string) bool {
	if len(p.Allow) == 0 {
		return true
	}
	for _, a := range p.Allow {
		if a == kind {
			return true
		}
	}
	return false
}

// inWindow says whether now is inside the maintenance window. A window that
// ends before it starts wraps around midnight, which is what a night window
// usually does.
func (p agentPolicy) inWindow(now time.Time) bool {
	if p.Window == "" {
		return true
	}
	from, to, ok := parseWindow(p.Window)
	if !ok {
		log.Printf("maintenance window %q not understood — ignoring it", p.Window)
		return true
	}
	mins := now.Hour()*60 + now.Minute()
	if from <= to {
		return mins >= from && mins < to
	}
	return mins >= from || mins < to
}

func parseWindow(s string) (from, to int, ok bool) {
	a, b, found := strings.Cut(s, "-")
	if !found {
		return 0, 0, false
	}
	pa, ok1 := parseHHMM(strings.TrimSpace(a))
	pb, ok2 := parseHHMM(strings.TrimSpace(b))
	return pa, pb, ok1 && ok2
}

func parseHHMM(s string) (int, bool) {
	h, m, found := strings.Cut(s, ":")
	if !found {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

func (p agentPolicy) describe() string {
	var parts []string
	if p.Interval > 0 {
		parts = append(parts, "every "+p.Interval.String())
	}
	if len(p.Allow) > 0 {
		parts = append(parts, "allowed: "+strings.Join(p.Allow, ","))
	}
	if p.RebootOnCfg {
		w := "any time"
		if p.Window != "" {
			w = p.Window
		}
		parts = append(parts, "restarts itself after boot config ("+w+")")
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("agent policy: %s", strings.Join(parts, " · "))
}
