package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Applying the desired state
// --------------------------
// Two rules run through everything here:
//
//  1. Idempotent. Every change is checked first and only made when needed.
//     The agent runs every minute — without this rule it would restart
//     services once a minute.
//
//  2. Restrained. Only what the configuration names explicitly is touched.
//     The agent never touches the network: the fixed address comes from the
//     DHCP reservation, and on DietPi that belongs to the distribution
//     anyway.

func applyConfig(cfg map[string]any) ([]string, error) {
	var changes []string
	var firstErr error
	unitsChanged := false

	note := func(f string, a ...any) { changes = append(changes, fmt.Sprintf(f, a...)) }
	fail := func(what string, err error) {
		note("FAILED at %s: %v", what, err)
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", what, err)
		}
	}

	if v, ok := cfg["hostname"].(string); ok && v != "" {
		if changed, err := setHostname(v); err != nil {
			fail("hostname", err)
		} else if changed {
			note("hostname set to %s", v)
		}
	}

	if v, ok := cfg["timezone"].(string); ok && v != "" {
		if changed, err := setTimezone(v); err != nil {
			fail("timezone", err)
		} else if changed {
			note("timezone set to %s", v)
		}
	}

	if keys := stringList(cfg["ssh_authorized_keys"]); len(keys) > 0 {
		for _, target := range keyTargets() {
			if changed, err := writeAuthorizedKeys(target, keys); err != nil {
				fail("SSH keys for "+target.name, err)
			} else if changed {
				note("SSH keys for %s updated (%d)", target.name, len(keys))
			}
		}
	}

	for _, f := range fileList(cfg["files"]) {
		if changed, err := writeManagedFile(f); err != nil {
			fail("file "+f.Path, err)
		} else if changed {
			note("file %s written", f.Path)
			if strings.HasSuffix(f.Path, ".service") || strings.HasSuffix(f.Path, ".timer") {
				unitsChanged = true
			}
		}
	}

	for _, bin := range binaryList(cfg["binaries"]) {
		if changed, err := installBinary(bin); err != nil {
			fail("binary "+bin.Path, err)
		} else if changed {
			note("binary %s installed", bin.Path)
			unitsChanged = true
		}
	}

	if lines := stringList(cfg["boot_config"]); len(lines) > 0 {
		if changed, err := applyBootConfig(lines); err != nil {
			fail("boot config", err)
		} else if changed {
			note("boot config updated (%s) — takes effect after a restart",
				strings.Join(lines, ", "))
			markRebootPending()
		}
	}

	if pkgs := packageList(cfg["packages"]); len(pkgs) > 0 {
		missing := missingPackages(pkgs)
		if len(missing) > 0 {
			if err := installPackages(missing); err != nil {
				fail("packages", err)
			} else {
				note("packages installed: %s", strings.Join(missing, ", "))
			}
		}
	}

	// A newly written unit file or a replaced binary is invisible to systemd
	// until it is told; and a unit whose binary changed has to be restarted,
	// otherwise the old one keeps running and the report claims otherwise.
	if unitsChanged {
		if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
			fail("daemon-reload", fmt.Errorf("%v: %s", err, lastLines(string(out), 2)))
		}
	}

	for _, u := range unitList(cfg["units"]) {
		if unitsChanged {
			u.Restart = true
		}
		if changed, err := applyUnit(u); err != nil {
			fail("unit "+u.Name, err)
		} else if changed {
			note("unit %s: %s", u.Name, u.state())
		}
	}

	sort.Strings(changes)
	return changes, firstErr
}

// ── Hostname ─────────────────────────────────────────────────────────

func setHostname(want string) (bool, error) {
	cur, _ := os.Hostname()
	if cur == want {
		return false, nil
	}
	// hostnamectl handles /etc/hostname, the running kernel and telling
	// systemd, all in one go.
	if _, err := exec.LookPath("hostnamectl"); err == nil {
		if out, err := exec.Command("hostnamectl", "set-hostname", want).CombinedOutput(); err != nil {
			return false, fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
	} else {
		if err := os.WriteFile("/etc/hostname", []byte(want+"\n"), 0o644); err != nil {
			return false, err
		}
		_ = exec.Command("hostname", want).Run()
	}
	ensureHostsEntry(want)
	return true, nil
}

// ensureHostsEntry keeps 127.0.1.1 current. Without it, sudo and some
// services run into DNS timeouts.
func ensureHostsEntry(host string) {
	const path = "/etc/hosts"
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(b), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "127.0.1.1") {
			lines[i] = "127.0.1.1\t" + host
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, "127.0.1.1\t"+host)
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

func setTimezone(want string) (bool, error) {
	cur, _ := os.Readlink("/etc/localtime")
	if strings.HasSuffix(cur, "/"+want) {
		return false, nil
	}
	if _, err := exec.LookPath("timedatectl"); err != nil {
		return false, errNoTool
	}
	if out, err := exec.Command("timedatectl", "set-timezone", want).CombinedOutput(); err != nil {
		return false, fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// ── SSH keys ─────────────────────────────────────────────────────────

type keyTarget struct {
	name string
	home string
	uid  int
	gid  int
}

// keyTargets returns root and the first regular user account. Which one that
// is depends on the distribution — "ubuntu" on Ubuntu, "dietpi" on DietPi.
// So it searches rather than guesses.
func keyTargets() []keyTarget {
	out := []keyTarget{{name: "root", home: "/root", uid: 0, gid: 0}}
	b := readFileStr("/etc/passwd")
	for _, line := range strings.Split(b, "\n") {
		f := strings.Split(line, ":")
		if len(f) < 7 {
			continue
		}
		uid, err := strconv.Atoi(f[2])
		if err != nil || uid != 1000 {
			continue
		}
		gid, _ := strconv.Atoi(f[3])
		if f[5] == "" || strings.HasSuffix(f[6], "nologin") || strings.HasSuffix(f[6], "false") {
			continue
		}
		out = append(out, keyTarget{name: f[0], home: f[5], uid: uid, gid: gid})
		break
	}
	if u, err := user.Current(); err == nil && u.Uid != "0" {
		_ = u // nur zur Vollstaendigkeit: der Agent laeuft als root
	}
	return out
}

func writeAuthorizedKeys(t keyTarget, keys []string) (bool, error) {
	dir := filepath.Join(t.home, ".ssh")
	path := filepath.Join(dir, "authorized_keys")
	want := "# managed by rookery-agent\n" + strings.Join(keys, "\n") + "\n"
	if old, err := os.ReadFile(path); err == nil && string(old) == want {
		return false, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		return false, err
	}
	_ = os.Chown(dir, t.uid, t.gid)
	_ = os.Chown(path, t.uid, t.gid)
	return true, nil
}

// ── Files ────────────────────────────────────────────────────────────

type managedFile struct {
	Path    string
	Content string
	Mode    os.FileMode
}

func fileList(v any) []managedFile {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []managedFile
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		p, _ := m["path"].(string)
		c, _ := m["content"].(string)
		if p == "" {
			continue
		}
		mode := os.FileMode(0o644)
		switch t := m["mode"].(type) {
		case string:
			if n, err := strconv.ParseUint(t, 8, 32); err == nil {
				mode = os.FileMode(n)
			}
		case float64:
			mode = os.FileMode(int(t))
		}
		out = append(out, managedFile{Path: p, Content: c, Mode: mode})
	}
	return out
}

func writeManagedFile(f managedFile) (bool, error) {
	if old, err := os.ReadFile(f.Path); err == nil && string(old) == f.Content {
		if st, err := os.Stat(f.Path); err == nil && st.Mode().Perm() == f.Mode.Perm() {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(f.Path, []byte(f.Content), f.Mode); err != nil {
		return false, err
	}
	return true, os.Chmod(f.Path, f.Mode)
}

// ── Binaries ─────────────────────────────────────────────────────────

// Some things a blade needs are single static binaries, not packages — the
// compute-blade-agent that drives fan and LEDs is one. They are fetched from
// the Rookery server rather than from the internet: the site has them
// anyway, and a blade in a rack without a route out must not be a blade that
// cannot be configured.
type binarySpec struct {
	Path   string
	URL    string
	SHA256 string
	Mode   os.FileMode
}

// A client of its own: the pull loop's 30-second budget is right for a small
// JSON exchange and wrong for a binary of tens of megabytes, and http.Client
// counts the body against that timeout.
var downloadClient = &http.Client{Timeout: 10 * time.Minute}

func binaryList(v any) []binarySpec {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []binarySpec
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		b := binarySpec{Mode: 0o755}
		b.Path, _ = m["path"].(string)
		b.URL, _ = m["url"].(string)
		b.SHA256, _ = m["sha256"].(string)
		if b.Path == "" || b.URL == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

// installBinary fetches only when the file on disk is not already the wanted
// one. Without a checksum it fetches once and then leaves the file alone —
// re-downloading a binary on every pull would be sixty megabytes an hour per
// blade for nothing.
func installBinary(b binarySpec) (bool, error) {
	have, err := fileSHA256(b.Path)
	switch {
	case err == nil && b.SHA256 == "":
		return false, nil
	case err == nil && strings.EqualFold(have, b.SHA256):
		return false, nil
	}

	resp, err := downloadClient.Get(b.URL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("HTTP %d for %s", resp.StatusCode, b.URL)
	}

	if err := os.MkdirAll(filepath.Dir(b.Path), 0o755); err != nil {
		return false, err
	}
	tmp := b.Path + ".rookery-new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, b.Mode)
	if err != nil {
		return false, err
	}
	sum := sha256.New()
	_, cerr := io.Copy(io.MultiWriter(f, sum), resp.Body)
	closeErr := f.Close()
	if cerr != nil || closeErr != nil {
		os.Remove(tmp)
		return false, errors.Join(cerr, closeErr)
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if b.SHA256 != "" && !strings.EqualFold(got, b.SHA256) {
		os.Remove(tmp)
		return false, fmt.Errorf("checksum mismatch: expected %s, got %s", b.SHA256, got)
	}
	// Renamed over the target rather than written in place: a running binary
	// cannot be overwritten ("text file busy"), but its directory entry can
	// be replaced under it.
	if err := os.Rename(tmp, b.Path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, os.Chmod(b.Path, b.Mode)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// ── Boot configuration ───────────────────────────────────────────────

// The firmware reads config.txt before Linux exists, so anything that has to
// be there at boot — the UART the smart fan unit hangs on, for instance —
// cannot be set from a running system. The agent therefore keeps a block of
// its own at the end of the file and leaves everything else untouched: the
// distribution owns that file, we only add to it.
const (
	bootConfigStart = "# >>> rookery managed >>>"
	bootConfigEnd   = "# <<< rookery managed <<<"
)

// The marker for "this needs a restart" lives in /run on purpose: that is a
// tmpfs, so the restart clears it by happening. A variable in the process
// would be lost when the agent restarts, and a file on disk would have to be
// removed by someone.
const rebootMarker = "/run/rookery-agent.reboot-required"

func markRebootPending() {
	_ = os.WriteFile(rebootMarker, []byte("boot config changed\n"), 0o644)
}

// rebootRequired is reported as a fact, so the interface can say so instead
// of quietly showing a blade that is configured but is not yet running that
// configuration.
func rebootRequired() bool {
	_, err := os.Stat(rebootMarker)
	return err == nil
}

// bootConfigPath finds config.txt. Bookworm and later put it in
// /boot/firmware; older images keep it in /boot.
func bootConfigPath() string {
	for _, p := range []string{"/boot/firmware/config.txt", "/boot/config.txt"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// raspiFirmwareCustom is where Debian wants boot settings. Its config.txt
// says "Do not modify this file" and means it: the raspi-firmware package
// regenerates it on every kernel update and would drop anything written
// there. Writing both places is the honest answer — the custom file so the
// setting survives, config.txt so it takes effect at the next boot rather
// than at the next kernel update.
const raspiFirmwareCustom = "/etc/default/raspi-firmware-custom"

func applyBootConfig(lines []string) (bool, error) {
	changed := false
	if _, err := os.Stat("/etc/default/raspi-firmware"); err == nil {
		c, err := applyBlockTo(raspiFirmwareCustom, lines, true)
		if err != nil {
			return false, err
		}
		changed = c
	}

	path := bootConfigPath()
	if path == "" {
		if changed {
			return true, nil
		}
		return false, errors.New("no config.txt found (neither /boot/firmware nor /boot)")
	}
	c, err := applyBlockTo(path, lines, false)
	return changed || c, err
}

// applyBlockTo keeps a marked block of lines at the end of a file. create
// says whether the file may be brought into existence — config.txt must
// already be there, the Debian custom file need not be.
func applyBlockTo(path string, lines []string, create bool) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !create || !os.IsNotExist(err) {
			return false, err
		}
		raw = nil
	}
	old := string(raw)

	want := bootConfigStart + "\n" + strings.Join(lines, "\n") + "\n" + bootConfigEnd

	// Lines that already stand somewhere else in the file are left alone —
	// a hand-made dtoverlay must not be duplicated, and duplicating it can
	// change what the firmware does.
	var missing []string
	body := stripBlock(old)
	for _, l := range lines {
		if !hasLine(body, l) {
			missing = append(missing, l)
		}
	}
	if len(missing) == 0 {
		// Everything present outside our block: drop the block if we still
		// carry one, otherwise there is nothing to do.
		if !strings.Contains(old, bootConfigStart) {
			return false, nil
		}
		want = ""
	} else {
		want = bootConfigStart + "\n" + strings.Join(missing, "\n") + "\n" + bootConfigEnd
	}

	updated := replaceBlock(old, want)
	if updated == old {
		return false, nil
	}
	// Written next to the original and renamed over it: config.txt sits on the
	// FAT boot partition, and a half-written one is an unbootable blade.
	tmp := path + ".rookery-new"
	if err := os.WriteFile(tmp, []byte(updated), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// hasLine reports whether a non-comment line of the file is exactly this
// setting, ignoring surrounding whitespace.
func hasLine(body, want string) bool {
	want = strings.TrimSpace(want)
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if l == want {
			return true
		}
	}
	return false
}

func stripBlock(s string) string {
	i := strings.Index(s, bootConfigStart)
	if i < 0 {
		return s
	}
	j := strings.Index(s[i:], bootConfigEnd)
	if j < 0 {
		return s[:i]
	}
	return s[:i] + s[i+j+len(bootConfigEnd):]
}

func replaceBlock(s, block string) string {
	rest := strings.TrimRight(stripBlock(s), "\n")
	if block == "" {
		return rest + "\n"
	}
	return rest + "\n\n" + block + "\n"
}

// ── Packages ─────────────────────────────────────────────────────────

// packageList accepts either plain names or objects with per_os — the latter
// for cases where the same software is named differently per distribution.
func packageList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	osID := parseKV("/etc/os-release")["ID"]
	var out []string
	for _, e := range arr {
		switch t := e.(type) {
		case string:
			if t != "" {
				out = append(out, t)
			}
		case map[string]any:
			name, _ := t["name"].(string)
			if per, ok := t["per_os"].(map[string]any); ok {
				if v, ok := per[osID].(string); ok && v != "" {
					name = v
				}
			}
			if name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

func missingPackages(pkgs []string) []string {
	var missing []string
	for _, p := range pkgs {
		out, err := exec.Command("dpkg-query", "-W", "-f=${Status}", p).Output()
		if err != nil || !strings.Contains(string(out), "install ok installed") {
			missing = append(missing, p)
		}
	}
	return missing
}

func installPackages(pkgs []string) error {
	if _, err := exec.LookPath("apt-get"); err != nil {
		return errNoTool
	}
	env := append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	up := exec.Command("apt-get", "update", "-qq")
	up.Env = env
	_ = up.Run() // a stale index is no reason to give up

	args := append([]string{"install", "-y", "-qq", "--no-install-recommends"}, pkgs...)
	c := exec.Command("apt-get", args...)
	c.Env = env
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, lastLines(string(out), 3))
	}
	return nil
}

// ── Units ────────────────────────────────────────────────────────────

type unitSpec struct {
	Name    string
	Enabled bool
	Restart bool
}

func (u unitSpec) state() string {
	if u.Enabled {
		return "enabled and started"
	}
	return "stopped and disabled"
}

func unitList(v any) []unitSpec {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []unitSpec
	for _, e := range arr {
		switch t := e.(type) {
		case string:
			out = append(out, unitSpec{Name: t, Enabled: true})
		case map[string]any:
			n, _ := t["name"].(string)
			if n == "" {
				continue
			}
			en := true
			if b, ok := t["enabled"].(bool); ok {
				en = b
			}
			r, _ := t["restart"].(bool)
			out = append(out, unitSpec{Name: n, Enabled: en, Restart: r})
		}
	}
	return out
}

func applyUnit(u unitSpec) (bool, error) {
	isEnabled := systemctlIs("is-enabled", u.Name) == "enabled"
	isActive := systemctlIs("is-active", u.Name) == "active"

	if !u.Enabled {
		if !isEnabled && !isActive {
			return false, nil
		}
		if out, err := exec.Command("systemctl", "disable", "--now", u.Name).CombinedOutput(); err != nil {
			return false, fmt.Errorf("%v: %s", err, lastLines(string(out), 2))
		}
		return true, nil
	}

	if isEnabled && isActive && !u.Restart {
		return false, nil
	}
	if !isEnabled || !isActive {
		if out, err := exec.Command("systemctl", "enable", "--now", u.Name).CombinedOutput(); err != nil {
			return false, fmt.Errorf("%v: %s", err, lastLines(string(out), 2))
		}
		return true, nil
	}
	if u.Restart {
		if out, err := exec.Command("systemctl", "restart", u.Name).CombinedOutput(); err != nil {
			return false, fmt.Errorf("%v: %s", err, lastLines(string(out), 2))
		}
		return true, nil
	}
	return false, nil
}

func systemctlIs(verb, unit string) string {
	out, _ := exec.Command("systemctl", verb, unit).Output()
	return strings.TrimSpace(string(out))
}

// ── Commands ─────────────────────────────────────────────────────────

// identify makes the edge LED blink. Only the compute-blade-agent can do
// that; if it is missing that is worth reporting but not this agent's
// failure.
func identify() error {
	if _, err := exec.LookPath("bladectl"); err != nil {
		return fmt.Errorf("bladectl not present — is compute-blade-agent installed?")
	}
	out, err := exec.Command("bladectl", "set", "identify").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, lastLines(string(out), 2))
	}
	return nil
}

// delayedReboot gives the running pass time to send its status report —
// otherwise the server sees the blade vanish for no reason.
func delayedReboot(after time.Duration) {
	time.Sleep(after)
	if err := exec.Command("systemctl", "reboot").Run(); err != nil {
		_ = exec.Command("reboot").Run()
	}
}

// ── Odds and ends ────────────────────────────────────────────────────

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
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
