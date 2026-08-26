package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Facts and health
// ----------------
// The facts say WHAT this blade is — the server hangs its distribution-
// dependent decisions on them. Health says HOW IT IS DOING. Both are gathered
// fresh on every pass; nothing is cached, because a stale reading is worse
// than none.

// ── Facts ────────────────────────────────────────────────────────────

func collectFacts() map[string]any {
	rel := parseKV("/etc/os-release")
	osID, osName, osVer := rel["ID"], rel["PRETTY_NAME"], rel["VERSION_ID"]
	// DietPi is Debian underneath and says so in /etc/os-release, which is
	// true and useless: what someone chose in the catalogue was DietPi, and
	// what runs on the blade should be called by that name. Its own version
	// lives in a file of its own.
	if id, name, ver, ok := dietPi(); ok {
		osID, osName, osVer = id, name, ver
	}
	f := map[string]any{
		"os_id":         osID,
		"os_version_id": osVer,
		"os_name":       osName,
		"os_base":       rel["PRETTY_NAME"],
		"os_family":     family(rel),
		"init":          initSystem(),
		"pkg_mgr":       packageManager(),
		"net_backend":   netBackend(),
		"boot_path":     bootPath(),
		"kernel":        uname(),
		"arch":          runtimeArch(),
		"model":         strings.TrimRight(readFileStr("/proc/device-tree/model"), "\x00"),
		"serial":        readSerial(),
		"agent_version": userAgent,
		// Set once the agent has changed something that only the firmware
		// reads — the blade is configured, but not yet running that
		// configuration.
		"reboot_required": rebootRequired(),
	}
	return f
}

// dietPi recognises DietPi and reads the version it keeps for itself.
// /etc/os-release stays what Debian put there; this is the layer above it.
func dietPi() (id, name, version string, ok bool) {
	v := parseKV("/boot/dietpi/.version")
	if len(v) == 0 {
		v = parseKV("/var/lib/dietpi/.version")
	}
	core := v["G_DIETPI_VERSION_CORE"]
	if core == "" {
		return "", "", "", false
	}
	version = core
	if sub := v["G_DIETPI_VERSION_SUB"]; sub != "" {
		version += "." + sub
	}
	if rc := v["G_DIETPI_VERSION_RC"]; rc != "" {
		version += "." + rc
	}
	return "dietpi", "DietPi " + version, version, true
}

// family groups the distributions that are operated the same way. DietPi
// reports its own ID but is Debian underneath — which is why ID_LIKE counts.
func family(rel map[string]string) string {
	id := rel["ID"]
	like := rel["ID_LIKE"]
	switch {
	case id == "debian" || id == "ubuntu" || id == "dietpi" ||
		strings.Contains(like, "debian") || strings.Contains(like, "ubuntu"):
		return "debian"
	case id == "fedora" || strings.Contains(like, "rhel") || strings.Contains(like, "fedora"):
		return "rhel"
	case id == "alpine":
		return "alpine"
	}
	return id
}

func initSystem() string {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return "systemd"
	}
	if _, err := os.Stat("/sbin/openrc"); err == nil {
		return "openrc"
	}
	return "unknown"
}

func packageManager() string {
	for _, p := range []string{"apt-get", "dnf", "pacman", "apk"} {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	return "unknown"
}

// netBackend reports who manages the network here. The value decides whether
// the agent may touch network files at all — on DietPi, for instance, that
// belongs to the distribution.
func netBackend() string {
	switch {
	case exists("/etc/netplan") && hasFiles("/etc/netplan"):
		return "netplan"
	case active("NetworkManager"):
		return "NetworkManager"
	case active("systemd-networkd"):
		return "networkd"
	case exists("/etc/network/interfaces"):
		return "ifupdown"
	}
	return "unknown"
}

func bootPath() string {
	for _, p := range []string{"/boot/firmware", "/boot"} {
		if exists(filepath.Join(p, "config.txt")) {
			return p
		}
	}
	return "/boot"
}

func uname() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	return int8ToStr(u.Release[:])
}

func runtimeArch() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	return int8ToStr(u.Machine[:])
}

func int8ToStr(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// ── Health ───────────────────────────────────────────────────────────

func collectHealth() map[string]any {
	h := map[string]any{
		"uptime_s": uptimeSeconds(),
		"load1":    load1(),
	}
	if t, ok := socTemp(); ok {
		h["soc_temp_c"] = round1(t)
	}
	if t, ok := nvmeTemp(); ok {
		h["nvme_temp_c"] = round1(t)
	}
	if total, free, ok := diskUsage("/"); ok {
		h["disk_total_b"] = total
		h["disk_free_b"] = free
		if total > 0 {
			h["disk_used_pct"] = int((total - free) * 100 / total)
		}
	}
	if total, avail, ok := memory(); ok {
		h["mem_total_b"] = total
		h["mem_avail_b"] = avail
	}
	if v, ok := throttled(); ok {
		h["throttled"] = v
		// The low bits mean "right now"; everything above means "has been so
		// since boot". Both are interesting, but only the first is an alarm.
		h["undervoltage_now"] = v&0x1 != 0
		h["throttled_now"] = v&0x4 != 0
	}
	// Only the compute-blade-agent knows fan speed and airflow temperature;
	// if it is not running the fields simply stay absent.
	for k, v := range bladeAgentMetrics() {
		h[k] = v
	}
	return h
}

func socTemp() (float64, bool) {
	b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0, false
	}
	milli, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, false
	}
	return milli / 1000, true
}

// nvmeTemp looks for the NVMe's hwmon device. The path is not stable, so it
// searches by name rather than by a fixed number.
func nvmeTemp() (float64, bool) {
	entries, err := os.ReadDir("/sys/class/hwmon")
	if err != nil {
		return 0, false
	}
	for _, e := range entries {
		base := filepath.Join("/sys/class/hwmon", e.Name())
		if !strings.HasPrefix(strings.TrimSpace(readFileStr(filepath.Join(base, "name"))), "nvme") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(base, "temp1_input"))
		if err != nil {
			continue
		}
		milli, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil {
			continue
		}
		return milli / 1000, true
	}
	return 0, false
}

func uptimeSeconds() int64 {
	f := strings.Fields(readFileStr("/proc/uptime"))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v)
}

func load1() float64 {
	f := strings.Fields(readFileStr("/proc/loadavg"))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func diskUsage(path string) (total, free int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	return int64(st.Blocks) * st.Bsize, int64(st.Bavail) * st.Bsize, true
}

func memory() (total, avail int64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var kb int64
		switch {
		case strings.HasPrefix(sc.Text(), "MemTotal:"):
			fmt.Sscanf(sc.Text(), "MemTotal: %d kB", &kb)
			total = kb * 1024
		case strings.HasPrefix(sc.Text(), "MemAvailable:"):
			fmt.Sscanf(sc.Text(), "MemAvailable: %d kB", &kb)
			avail = kb * 1024
		}
	}
	return total, avail, total > 0
}

// throttled reads the SoC's throttling register. It reveals undervoltage and
// thermal throttling — on a PoE-powered blade under load, exactly the two
// things you want to know.
func throttled() (uint64, bool) {
	out, err := exec.Command("vcgencmd", "get_throttled").Output()
	if err != nil {
		return 0, false
	}
	_, v, ok := strings.Cut(strings.TrimSpace(string(out)), "=")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// bladeAgentMetrics taps the compute-blade-agent if it is running. Only it
// knows fan speed, target and the critical mode.
//
// The names come from the running 0.11.2 build — guessing them was wrong
// before. And a trap hides in the values themselves: the standard fan unit
// has no tacho and reports "+Inf". That cannot be serialised as JSON; taken
// unchecked it breaks the entire status report — which is exactly what
// happened on the first joint run.
func bladeAgentMetrics() map[string]any {
	out := map[string]any{}
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get("http://127.0.0.1:9666/metrics")
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return out
	}

	// Plain numeric values without labels.
	plain := map[string]string{
		"computeblade_fan_speed":               "fan_rpm",
		"computeblade_fan_target_percent":      "fan_percent",
		"computeblade_airflow_temperature":     "airflow_temp_c",
		"computeblade_soc_temperature":         "blade_soc_temp_c",
		"computeblade_stealth_mode_enabled":    "stealth",
		"computeblade_edge_button_event_count": "button_events",
	}

	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		name, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		labels := ""
		if i := strings.IndexByte(name, '{'); i >= 0 {
			labels = name[i:]
			name = name[:i]
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			// "+Inf" here means: this value does not exist. Leaving it out is
			// more honest than inventing a zero — and it saves the
			// serialisation.
			continue
		}
		if key, want := plain[name]; want {
			out[key] = round1(v)
			continue
		}
		// States arrive as one line per state carrying 0 or 1.
		if name == "computeblade_state_state" && v == 1 {
			out["blade_state"] = labelValue(labels, "state")
		}
		// The fan unit's type is in the label, not the value. "smart" supplies
		// RPM and airflow temperature, "standard" does not.
		if name == "computeblade_fan_unit" {
			out["fan_unit"] = labelValue(labels, "type")
		}
		if name == "computeblade_compute_module_present" && v == 1 {
			out["module"] = labelValue(labels, "type")
		}
	}
	if len(out) > 0 {
		out["fan_source"] = "compute-blade-agent"
	}
	return out
}

// labelValue pulls one label value out of {a="1",b="2"}.
func labelValue(labels, key string) string {
	for _, part := range strings.Split(strings.Trim(labels, "{}"), ",") {
		k, v, ok := strings.Cut(part, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// ── Odds and ends ────────────────────────────────────────────────────

func readSerial() string {
	if s := strings.TrimRight(readFileStr("/proc/device-tree/serial-number"), "\x00\n "); s != "" {
		return s
	}
	for _, line := range strings.Split(readFileStr("/proc/cpuinfo"), "\n") {
		if strings.HasPrefix(line, "Serial") {
			if _, v, ok := strings.Cut(line, ":"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func parseKV(path string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(readFileStr(path), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		out[k] = strings.Trim(v, `"'`)
	}
	return out
}

func readFileStr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func hasFiles(dir string) bool {
	e, err := os.ReadDir(dir)
	return err == nil && len(e) > 0
}

func active(unit string) bool {
	out, err := exec.Command("systemctl", "is-active", unit).Output()
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
