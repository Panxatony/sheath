package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Whether anyone can get in.
//
// A blade can be perfectly healthy, report every thirty seconds, and still be
// a blade nobody can open a shell on — which is how the three at Terrassenweg
// were found: they answered every ping and refused port 22. The agent is
// already inside, so it can say what is actually the case, and the three
// answers together tell the cases apart. sshd missing is an image without it.
// sshd present but not running is a unit that is off or that failed, and a
// count of nought host keys usually says which. Running but not on 22 is
// somebody's deliberate choice, and worth showing rather than hiding.
//
// Reading is all this does. Turning the service on belongs to the installer,
// which is inside the filesystem before anything runs.

func sshAccess() map[string]any {
	out := map[string]any{
		"ssh_present":   sshdInstalled(),
		"ssh_running":   sshdRunning(),
		"ssh_listening": listeningOn(22),
	}
	if n := hostKeys(); n >= 0 {
		out["ssh_hostkeys"] = n
	}
	return out
}

// OpenSSH is not the only answer to "is there an SSH server". DietPi ships
// dropbear, which serves port 22 perfectly well and is not called sshd — and
// a blade running it was reported as having no SSH server at all while it was
// answering connections.
var sshServers = []string{"sshd", "dropbear"}

func sshdInstalled() bool {
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		for _, name := range sshServers {
			if _, err := os.Stat(dir + "/" + name); err == nil {
				return true
			}
		}
	}
	return false
}

// sshdRunning looks for the process rather than asking systemd: the answer is
// the same on a machine with no systemd, and it costs no program.
func sshdRunning() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		comm := strings.TrimSpace(readFileStr("/proc/" + e.Name() + "/comm"))
		for _, name := range sshServers {
			if comm == name {
				return true
			}
		}
	}
	return false
}

// listeningOn reads the kernel's own socket table. State 0A is LISTEN, and the
// local port is the hexadecimal half after the colon — both halves of both
// families, because a socket bound only to IPv6 still lets people in.
func listeningOn(port int) bool {
	want := strings.ToUpper(strings.TrimPrefix(hex4(port), "0x"))
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		for _, line := range strings.Split(readFileStr(f), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" {
				continue
			}
			_, p, ok := strings.Cut(fields[1], ":")
			if ok && strings.ToUpper(p) == want {
				return true
			}
		}
	}
	return false
}

func hex4(n int) string {
	const digits = "0123456789ABCDEF"
	return string([]byte{
		digits[(n>>12)&0xf], digits[(n>>8)&0xf], digits[(n>>4)&0xf], digits[n&0xf]})
}

// hostKeys counts what OpenSSH needs before it will start at all. On a fresh
// image there are none: the distribution makes them on the first boot, and
// where that step did not happen the service fails and the blade is shut.
// -1 means there is no /etc/ssh to look in — which is also what a dropbear
// system looks like, since it keeps its keys elsewhere.
func hostKeys() int {
	entries, err := os.ReadDir("/etc/ssh")
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "ssh_host_") && strings.HasSuffix(name, "_key") {
			if st, err := os.Stat(filepath.Join("/etc/ssh", name)); err == nil && st.Size() > 0 {
				n++
			}
		}
	}
	return n
}
