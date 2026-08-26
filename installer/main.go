// rookery-installer runs inside the mini-OS loaded over the network.
//
// It replaces the Raspberry Pi Imager as the last step of the initramfs:
// instead of asking someone at the HDMI cable for device, operating system
// and target, it fetches both answers from the Rookery server and writes the
// assigned image to the NVMe.
//
// The initramfs ships busybox, udev and dhcpcd, but neither curl nor zstd, xz
// or resize2fs — and no kernel modules; everything needed is built in. So
// this program is deliberately self-contained: HTTP, decompression, writing
// and mounting all happen in here.
package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const (
	defaultTarget = "/dev/nvme0n1"
	chunkSize     = 1 << 20 // 1 MiB — frugal; the CM4 has little memory
)

// ── Server responses ─────────────────────────────────────────────────

type provisionResp struct {
	Status     string   `json:"status"` // "waiting" | "idle" | "go" | "wipe"
	Serial     string   `json:"serial"`
	Image      string   `json:"image"`
	URL        string   `json:"url"`
	SHA256     string   `json:"sha256"`
	Seed       string   `json:"seed"`
	Target     string   `json:"target"`
	ServerURL  string   `json:"server_url"`
	Token      string   `json:"token"`
	Hostname   string   `json:"hostname"`
	SSHKeys    []string `json:"ssh_keys"`
	RetryAfter int      `json:"retry_after"`
	Message    string   `json:"message"`

	// How this installation should be carried out. The installer used to
	// decide all of it by itself, which meant a change needed a new boot.img
	// on every site. It is the server's business: it knows which blade this
	// is, what it is for, and what the operator asked for.
	Opts installOpts `json:"options"`
}

// installOpts are the choices an installation makes. Zero values mean the
// long-standing default, so an older server talking to a newer installer gets
// exactly the behaviour it had before.
type installOpts struct {
	// GrowLast extends the last partition to the end of the disk. Off means
	// leave the image's own layout alone.
	NoGrow bool `json:"no_grow"`
	// After the write: "reboot" (default), "halt" — stop and stay put — or
	// "shell", which drops to the console for someone who wants to look
	// around before the first boot.
	After string `json:"after"`
	// RequireChecksum refuses to write an image the catalogue has no checksum
	// for, rather than writing it unverified.
	RequireChecksum bool `json:"require_checksum"`
	// The seeding steps, each of which can be turned off where it does not
	// fit the image.
	NoRootKeys  bool `json:"no_root_keys"`
	NoCloudInit bool `json:"no_cloud_init"`
	NoAgent     bool `json:"no_agent"`
	NoClockSync bool `json:"no_clock_sync"`
	// RebootDelay in seconds before the machine restarts, so a console can be
	// read. 0 means the default of five seconds.
	RebootDelay int `json:"reboot_delay"`
}

func main() {
	// The CM4 has little memory, and the whole root filesystem lives in RAM.
	// An explicit ceiling makes the collector work sooner instead of growing
	// until the kernel steps in.
	debug.SetMemoryLimit(320 << 20)
	debug.SetGCPercent(40)

	// A failure here must not end in a kernel panic: the blade may be sitting
	// unreachable in the rack. So every error is reported, shown, and then
	// parked in a wait loop so it can be inspected over the serial console.
	if err := run(); err != nil {
		logf("")
		logf("╔══════════════════════════════════════════════════════════")
		logf("║ FAILED: %v", err)
		logf("╚══════════════════════════════════════════════════════════")
		logf("")
		logf("This blade now stops here. Nothing was overwritten")
		logf("that had not already been overwritten.")
		for {
			time.Sleep(time.Hour)
		}
	}
}

func run() error {
	banner()

	serial, err := readSerial()
	if err != nil {
		return fmt.Errorf("serial number unreadable: %w", err)
	}
	mac := readMAC()
	server := serverFromCmdline()
	logf("Serial   %s", serial)
	logf("MAC      %s", mac)
	logf("Server   %s", server)
	logf("")

	if server == "" {
		return errors.New("no server address in /proc/cmdline (rookery_server=...)")
	}

	c := &client{base: strings.TrimRight(server, "/"), serial: serial}

	if err := c.waitForNetwork(); err != nil {
		return err
	}

	c.syncClock()

	job, err := c.waitForJob(mac)
	if err != nil {
		return err
	}

	target := job.Target
	if target == "" {
		target = defaultTarget
	}

	// A wipe is not an installation with an empty image: nothing is
	// downloaded, nothing is written, and the blade is meant to be pulled
	// out afterwards rather than started.
	if job.Status == "wipe" {
		return c.runWipe(job, target)
	}

	logf("")
	logf("Image    %s", job.Image)
	logf("Source   %s", job.URL)
	logf("Target   %s", target)
	logf("")

	if err := checkTarget(target); err != nil {
		return err
	}

	if err := c.writeImage(job, target); err != nil {
		c.report("error", 0, err.Error())
		return err
	}

	// The partition table is new; the kernel does not know it yet.
	rereadPartitions(target)

	// An image is built for the smallest disk it must fit; this one is 500 GB.
	// Growing the last partition here means the blade does not have to be
	// taught to do it later — Debian's fstab carries x-systemd.growfs and
	// Ubuntu runs cloud-init's growpart, so both grow the filesystem into the
	// space at first boot.
	if job.Opts.NoGrow {
		logf("Partition left as the image has it (asked for)")
	} else if grown, err := growLastPartition(target); err != nil {
		logf("Partition not grown: %v", err)
		c.note("partition not grown: %v", err)
	} else if grown > 0 {
		logf("Last partition grown by %s", human(grown))
		c.note("last partition grown by %s", human(grown))
	}

	if err := c.seed(job, target); err != nil {
		// Not a hard failure: the image is correctly on the disk. Without a
		// seed it still boots, it just does not report in by itself.
		logf("WARNING: seeding failed: %v", err)
		logf("The image is written, but the blade will not report to")
		logf("the server by itself.")
	}

	c.report("done", 100, "")
	logf("")
	switch job.Opts.After {
	case "halt":
		// Someone wants to touch the disk, or move the blade, before it ever
		// runs what was written.
		logf("Written. Staying put, as asked — the blade will not restart.")
		c.note("installation finished, blade halted as configured")
		for {
			time.Sleep(time.Hour)
		}
	case "shell":
		logf("Written. Dropping to the console, as asked.")
		c.note("installation finished, console as configured")
		return nil
	default:
		delay := job.Opts.RebootDelay
		if delay <= 0 {
			delay = 5
		}
		logf("Done. Rebooting in %d seconds ...", delay)
		time.Sleep(time.Duration(delay) * time.Second)
		reboot()
	}
	return nil
}

// runWipe erases the disk and leaves the blade standing, so it can be taken
// out of its slot and put somewhere else. It deliberately does not reboot:
// restarting into a freshly emptied disk means netbooting again, and a blade
// waiting quietly is easier to pull than one in a boot loop.
func (c *client) runWipe(job *provisionResp, target string) error {
	logf("")
	logf("╔══════════════════════════════════════════════════════════")
	logf("║ ERASING %s", target)
	logf("╚══════════════════════════════════════════════════════════")
	logf("")
	if err := checkTarget(target); err != nil {
		c.report("error", 0, err.Error())
		return err
	}
	if err := c.wipeDisk(target); err != nil {
		c.report("error", 0, err.Error())
		return err
	}
	rereadPartitions(target)

	c.report("wiped", 100, "")
	logf("")
	logf("The NVMe is empty. This blade can be pulled and put in")
	logf("another BladeRunner — Rookery has taken it out of its slot.")
	logf("")
	logf("Nothing else will happen here.")
	for {
		time.Sleep(time.Hour)
	}
}

// idleErr stands for "the server has nothing to do". That is not a defect
// but the normal case on every restart of a finished blade — it should boot
// locally instead of landing here.
type idleErr struct {
	msg   string
	image string
}

func (e *idleErr) Error() string { return "no installation requested" }

func explainIdle(e *idleErr) {
	logf("")
	logf("No installation requested.")
	if e.image != "" {
		logf("Assigned would be: %s", e.image)
	}
	logf("")
	if installedLooking(defaultTarget) {
		logf("A system is already present on %s.", defaultTarget)
		logf("This blade should boot from it, not over the network.")
		logf("")
		logf("  Set during bring-up via rpiboot:")
		logf("      BOOT_ORDER=0xf26     (NVMe first, network as fallback)")
		logf("")
		logf("  With network before NVMe the blade lands here on every start")
		logf("  — and without this guard it would reinstall itself every")
		logf("  single time.")
	} else {
		logf("No system is detectable on %s.", defaultTarget)
		logf("Choose \"Install now\" in the Rookery interface — this blade")
		logf("keeps asking and starts on its own, no restart needed.")
	}
	logf("")
	logf("Nothing is written until an install is requested.")
	logf("Asking again every 30 seconds ...")
}

// installedLooking roughly checks whether something is already on the
// target: if it has partitions, someone has been here. Deliberately only an
// indication, not a proof — writing happens only on explicit request anyway.
func installedLooking(dev string) bool {
	base := filepath.Base(dev)
	entries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if n != base && strings.HasPrefix(n, base) {
			return true
		}
	}
	return false
}

func banner() {
	logf("")
	logf("  ┌────────────────────────────────────┐")
	logf("  │  Rookery Installer             │")
	logf("  └────────────────────────────────────┘")
	logf("")
}

// ── Reading the environment ──────────────────────────────────────────

// readSerial returns the CM4 serial number. The device tree is the more
// reliable source; /proc/cpuinfo serves as a fallback.
func readSerial() (string, error) {
	if b, err := os.ReadFile("/proc/device-tree/serial-number"); err == nil {
		s := strings.TrimRight(string(b), "\x00\n ")
		if s != "" {
			return s, nil
		}
	}
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "Serial") {
			if i := strings.Index(line, ":"); i >= 0 {
				return strings.TrimSpace(line[i+1:]), nil
			}
		}
	}
	return "", errors.New("neither device tree nor cpuinfo names a serial number")
}

func readMAC() string {
	for _, iface := range []string{"eth0", "end0"} {
		if b, err := os.ReadFile("/sys/class/net/" + iface + "/address"); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// serverFromCmdline reads the server address from the kernel command line.
// It comes from the cmdline.txt we serve over TFTP ourselves — the only way
// to hand the mini-OS anything before it has a network.
//
// bm_server= is the spelling from before the rename. It is still read so a
// blade that catches an older boot.img does not stall in confusion.
func serverFromCmdline() string {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	for _, f := range strings.Fields(string(b)) {
		for _, key := range []string{"rookery_server=", "bm_server="} {
			if v, ok := strings.CutPrefix(f, key); ok {
				return v
			}
		}
	}
	return ""
}

// ── Talking to the server ────────────────────────────────────────────

type client struct {
	base   string
	serial string
	token  string
}

// http is for short requests to the server: status reports, polling. A tight
// overall timeout is right here.
func (c *client) http() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// download is for the image — and deliberately has NO overall timeout.
//
// In Go, http.Client.Timeout bounds the whole request including reading the
// body. At 30 seconds the download of a 1.2 GB image inevitably breaks off
// mid-stream (after 73 MB on the first attempt). Instead we bound the phases
// that can genuinely hang — connect, TLS handshake, response header — and
// watch the data flow ourselves: if nothing arrives for longer than
// stallTimeout, we abort.
func (c *client) download() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			TLSHandshakeTimeout:   20 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// Generous, because a flapping link needs several reconnects before data
// flows again.
const stallTimeout = 300 * time.Second

// syncEvery bounds how much unwritten data may pile up.
const syncEvery = 64 << 20

// copyToDisk writes in small chunks, syncs regularly and reports the real
// progress along with free memory. That memory figure is not decoration: on
// the first attempt the installer died at 64 %, and without it you cannot
// tell the causes apart.
func (c *client) copyToDisk(out *os.File, src io.Reader, total int64, counted *progressReader) (int64, error) {
	buf := make([]byte, chunkSize)
	var written, sinceSync int64
	last := time.Now()
	start := last
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := out.Write(buf[:n])
			written += int64(w)
			sinceSync += int64(w)
			if werr != nil {
				return written, werr
			}
			if sinceSync >= syncEvery {
				if err := out.Sync(); err != nil {
					return written, fmt.Errorf("sync failed: %w", err)
				}
				sinceSync = 0
			}
		}
		if time.Since(last) >= 5*time.Second {
			last = time.Now()
			pct := 0
			if total > 0 {
				// Share of source data consumed — the only reference we have:
				// nobody knows the uncompressed size in advance.
				pct = int(counted.bytesRead() * 100 / total)
			}
			mbs := float64(written) / time.Since(start).Seconds() / 1e6
			logf("  %s written  (%d %% of source)  %.0f MB/s  free: %s",
				human(written), pct, mbs, memAvailable())
			c.report("writing", pct, "")
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return written, rerr
		}
	}
	if err := out.Sync(); err != nil {
		return written, fmt.Errorf("sync failed: %w", err)
	}
	return written, nil
}

// memAvailable reads MemAvailable from /proc/meminfo.
func memAvailable() string {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "?"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			var kb int64
			fmt.Sscanf(line, "MemAvailable: %d kB", &kb)
			return human(kb * 1024)
		}
	}
	return "?"
}

// waitForNetwork waits until the server answers. dhcpcd is already running
// but needs a moment — and the switch possibly longer, if spanning tree is
// active.
func (c *client) waitForNetwork() error {
	logf("Waiting for the network ...")
	deadline := time.Now().Add(3 * time.Minute)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		resp, err := c.http().Get(c.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			logf("Network is up (IP %s)", localIP())
			return nil
		}
		if attempt%5 == 0 {
			logf("  ... still no connection to %s (%v)", c.base, err)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("server %s was unreachable for three minutes", c.base)
}

func localIP() string {
	conn, err := net.Dial("udp", "192.0.2.1:9")
	if err != nil {
		return "?"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// waitForJob keeps asking until an image has been chosen in the interface.
// That is the normal case, not an error — so it waits patiently instead of
// giving up.
// syncClock takes the time from the server. The mini OS boots without an RTC
// and without NTP, so it starts at 1970 — and a TLS certificate that is valid
// today is then "not yet valid", which kills every https image download with
// an error that looks like a certificate problem and is really a clock
// problem. The Date header of any HTTP response is enough to fix it; it costs
// one request and no dependency.
func (c *client) syncClock() {
	if time.Now().Year() >= 2024 {
		return
	}
	resp, err := c.http().Get(c.base + "/login")
	if err != nil {
		logf("Clock: server did not answer (%v) — staying at %s",
			err, time.Now().Format("2006-01-02"))
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	t, perr := http.ParseTime(resp.Header.Get("Date"))
	if perr != nil {
		logf("Clock: no usable Date header — staying at %s",
			time.Now().Format("2006-01-02"))
		return
	}
	tv := syscall.NsecToTimeval(t.UnixNano())
	if serr := syscall.Settimeofday(&tv); serr != nil {
		logf("Clock: could not be set (%v)", serr)
		return
	}
	logf("Clock set from the server: %s", t.UTC().Format(time.RFC3339))
}

func (c *client) waitForJob(mac string) (*provisionResp, error) {
	body, _ := json.Marshal(map[string]string{"mac": mac})
	announced, idleShown := false, false
	for {
		resp, err := c.http().Post(
			c.base+"/api/v1/provision/"+c.serial,
			"application/json", strings.NewReader(string(body)))
		if err != nil {
			logf("Server unreachable (%v) — retrying in 5 s", err)
			time.Sleep(5 * time.Second)
			continue
		}
		var job provisionResp
		dec := json.NewDecoder(resp.Body)
		derr := dec.Decode(&job)
		resp.Body.Close()
		if derr != nil {
			logf("Response unreadable (%v) — retrying in 5 s", derr)
			time.Sleep(5 * time.Second)
			continue
		}
		// "go" writes an image, "wipe" erases the disk — both are jobs to
		// carry out. Everything else is a state to keep asking about, and
		// treating "wipe" as one of those meant a blade polling forever while
		// the interface said the erase had been handed out.
		if job.Status == "go" || job.Status == "wipe" {
			c.token = job.Token
			return &job, nil
		}
		// "idle" means: an image exists, but nobody requested an install.
		// Nothing may be written here under any circumstances — otherwise a
		// blade that boots network-before-NVMe would flatten itself on every
		// start.
		//
		// It does keep asking, though. Stopping here used to mean that
		// requesting the install afterwards needed a power cycle, because
		// nobody was listening any more. Asking again is safe: only "go"
		// ever writes, and "go" needs someone to press the button.
		if job.Status == "idle" {
			if !idleShown {
				explainIdle(&idleErr{msg: job.Message, image: job.Image})
				idleShown = true
				announced = true
			}
			wait := job.RetryAfter
			if wait <= 0 {
				wait = 30
			}
			time.Sleep(time.Duration(wait) * time.Second)
			continue
		}
		if !announced {
			logf("")
			logf("This blade is enrolled and waiting.")
			logf("Choose an image in the Rookery interface.")
			if job.Message != "" {
				logf("  %s", job.Message)
			}
			logf("")
			announced = true
		}
		wait := job.RetryAfter
		if wait <= 0 {
			wait = 5
		}
		time.Sleep(time.Duration(wait) * time.Second)
	}
}

// note sends a line to the server that belongs in the log rather than on a
// console nobody is watching. Whether seeding worked decides whether the
// blade can be reached at all afterwards — that must not be visible only to
// someone standing in front of the rack.
func (c *client) note(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	body, _ := json.Marshal(map[string]any{"phase": "seed", "percent": 100, "note": msg})
	req, err := http.NewRequest("POST",
		c.base+"/api/v1/provision/"+c.serial+"/status", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := c.http().Do(req); err == nil {
		resp.Body.Close()
	}
}

func (c *client) report(phase string, percent int, errMsg string) {
	body, _ := json.Marshal(map[string]any{
		"phase": phase, "percent": percent, "error": errMsg,
	})
	req, err := http.NewRequest("POST",
		c.base+"/api/v1/provision/"+c.serial+"/status", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// ── Checking the target ──────────────────────────────────────────────

func checkTarget(dev string) error {
	st, err := os.Stat(dev)
	if err != nil {
		return fmt.Errorf("target device %s not found — is an NVMe in the slot? (%w)", dev, err)
	}
	if st.Mode()&os.ModeDevice == 0 {
		return fmt.Errorf("%s is not a block device", dev)
	}
	size, err := blockSize(dev)
	if err == nil && size > 0 {
		logf("Target %s: %.1f GB", dev, float64(size)/1e9)
	}
	return nil
}

func blockSize(dev string) (uint64, error) {
	f, err := os.Open(dev)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var sz uint64
	// BLKGETSIZE64
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(),
		uintptr(0x80081272), uintptr(unsafe.Pointer(&sz)))
	if errno != 0 {
		return 0, errno
	}
	return sz, nil
}

// growLastPartition extends the partition that sits furthest out on the disk
// to the end of the device, and reports how many bytes it gained.
//
// "Furthest out" rather than "highest number": Debian numbers its firmware
// partition 15 and puts it in front of root, so the numbering says nothing
// about the order on the disk.
//
// sfdisk does the work — the mini OS has it, and it also moves the backup GPT
// header to the new end of the disk, which hand-written sector arithmetic
// would forget.
func growLastPartition(target string) (int64, error) {
	parts, err := partitionsOf(target)
	if err != nil {
		return 0, err
	}
	base := filepath.Base(target)

	var lastName string
	var lastStart, lastSize int64 = -1, 0
	for _, p := range parts {
		n := filepath.Base(p)
		start, err1 := sysfsInt("/sys/class/block/" + n + "/start")
		size, err2 := sysfsInt("/sys/class/block/" + n + "/size")
		if err1 != nil || err2 != nil {
			continue
		}
		if start > lastStart {
			lastStart, lastSize, lastName = start, size, n
		}
	}
	if lastName == "" {
		return 0, errors.New("no partition with a readable start sector")
	}
	total, err := sysfsInt("/sys/class/block/" + base + "/size")
	if err != nil {
		return 0, err
	}
	// Sectors of 512 bytes, as /sys reports them regardless of the device's
	// own sector size.
	free := (total - (lastStart + lastSize)) * 512
	if free < 256<<20 {
		return 0, nil
	}

	num := strings.TrimPrefix(lastName, base)
	num = strings.TrimPrefix(num, "p")
	// Absolute path: the mini OS starts the installer as PID 1's child with a
	// bare environment, and sfdisk lives in /usr/sbin, which is not in the
	// PATH that comes with that. It cost a blade an ungrown partition to
	// notice, because the failure looked like "sfdisk is missing" and the
	// tool was there all along.
	sfdisk := "/usr/sbin/sfdisk"
	if _, err := os.Stat(sfdisk); err != nil {
		if found, lerr := exec.LookPath("sfdisk"); lerr == nil {
			sfdisk = found
		}
	}
	cmd := exec.Command(sfdisk, "--no-reread", "--force", "-N", num, target)
	cmd.Stdin = strings.NewReader(", +\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("%v: %s", err, strings.TrimSpace(tail(string(out), 200)))
	}
	rereadPartitions(target)
	return free, nil
}

// tail keeps the last n characters — sfdisk is chatty, and the interesting
// part of a failure is at the end.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func sysfsInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// rereadPartitions asks the kernel to re-read the partition table. Without
// it, it still knows the old one after writing.
func rereadPartitions(dev string) {
	if f, err := os.OpenFile(dev, os.O_RDONLY, 0); err == nil {
		syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(0x125F), 0) // BLKRRPART
		f.Close()
	}
	// partprobe from the initramfs as a second attempt
	_ = exec.Command("/sbin/partprobe", dev).Run()
	// udev needs a moment before /dev/nvme0n1p2 appears
	_ = exec.Command("/bin/udevadm", "settle", "--timeout=10").Run()
	time.Sleep(2 * time.Second)
}

// ── Writing ──────────────────────────────────────────────────────────

// writeImage fetches the image and writes it straight to the disk. Nothing
// is buffered: a 6 GB image does not fit into a CM4's memory, and there is
// nowhere to put a file.
func (c *client) writeImage(job *provisionResp, target string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src, total, err := c.openResuming(ctx, job.URL)
	if err != nil {
		return err
	}
	defer src.Close()

	// The checksum covers the compressed file, so the stream is hashed before
	// decompression.
	hasher := sha256.New()
	// Count only, do not report: download progress races far ahead of
	// writing, because decompression is the bottleneck on a CM4. What is
	// reported below is what actually lands on the disk.
	counted := &progressReader{r: io.TeeReader(src, hasher), total: total}

	// Watchdog: if the stream stalls the request is cancelled, so the blade
	// does not hang for hours on a dead connection.
	stalled := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stalled:
				return
			case <-t.C:
				if counted.idleFor() > stallTimeout {
					logf("No data for %.0f s — aborting.", counted.idleFor().Seconds())
					cancel()
					return
				}
			}
		}
	}()
	defer close(stalled)

	dec, err := decompressor(job.URL, counted)
	if err != nil {
		return err
	}

	// No O_SYNC: that syncs every single write and makes the run unbearably
	// slow. Instead we sync deliberately every syncEvery bytes — that keeps
	// dirty pages in check without waiting on each block.
	out, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("%s not writable: %w", target, err)
	}
	defer out.Close()

	logf("Writing ... (progress = data actually written)")
	written, err := c.copyToDisk(out, dec, total, counted)
	if err != nil {
		return fmt.Errorf("write aborted after %s: %w", human(written), err)
	}
	logf("Written: %s", human(written))

	if job.SHA256 != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, job.SHA256) {
			return fmt.Errorf("checksum mismatch:\n  expected %s\n  got      %s",
				job.SHA256, got)
		}
		logf("Checksum matches (%s...)", got[:16])
	} else if job.Opts.RequireChecksum {
		// Refusing is the point: an unverified image is one nobody can say
		// arrived intact, and it has just been written to a disk.
		return errors.New("no checksum in the catalogue, and this installation requires one")
	} else {
		logf("No checksum on file — content unverified.")
	}
	return nil
}

// decompressor picks by file extension. A wrongly guessed method would write
// garbage to the disk, so an unknown format is rejected rather than passed
// through raw.
func decompressor(url string, r io.Reader) (io.Reader, error) {
	name := strings.ToLower(url)
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	switch {
	case strings.HasSuffix(name, ".xz"):
		return xz.NewReader(r)
	case strings.HasSuffix(name, ".zst"), strings.HasSuffix(name, ".zstd"):
		d, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	case strings.HasSuffix(name, ".gz"):
		return gzip.NewReader(r)
	case strings.HasSuffix(name, ".img"), strings.HasSuffix(name, ".raw"):
		return r, nil
	}
	return nil, fmt.Errorf("unknown format: %s (expected .img, .xz, .zst or .gz)", name)
}

// ── A download that survives a dropped link ──────────────────────────
//
// On the hardware the Ethernet link flapped under load, once a second
// ("bcmgenet eth0: Link is Down / Up"). A single stream running for minutes
// does not survive that — and an aborted download leaves a half-written disk
// behind.
//
// resumingReader continues the transfer after a break via HTTP Range exactly
// where it stopped. The decompressor behind it notices nothing: to it the
// stream is continuous.

type resumingReader struct {
	ctx    context.Context
	c      *client
	url    string
	offset int64
	total  int64
	body   io.ReadCloser
	tries  int
}

const maxResumes = 60

func (c *client) openResuming(ctx context.Context, url string) (*resumingReader, int64, error) {
	r := &resumingReader{ctx: ctx, c: c, url: url}
	if err := r.connect(); err != nil {
		return nil, 0, err
	}
	return r, r.total, nil
}

func (r *resumingReader) connect() error {
	req, err := http.NewRequestWithContext(r.ctx, "GET", r.url, nil)
	if err != nil {
		return err
	}
	if r.offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.offset))
	}
	resp, err := r.c.download().Do(req)
	if err != nil {
		return err
	}
	switch {
	case r.offset == 0 && resp.StatusCode == http.StatusOK:
		r.total = resp.ContentLength
	case r.offset > 0 && resp.StatusCode == http.StatusPartialContent:
		// weiter geht es
	case r.offset > 0 && resp.StatusCode == http.StatusOK:
		// The server cannot do Range. Starting over would be worse than
		// stopping: the disk is already half written.
		resp.Body.Close()
		return fmt.Errorf("server does not support resuming (HTTP 200 on Range)")
	default:
		resp.Body.Close()
		return fmt.Errorf("unexpected HTTP %d", resp.StatusCode)
	}
	r.body = resp.Body
	return nil
}

func (r *resumingReader) Read(b []byte) (int, error) {
	// A reader may return (0, nil) by contract. Without a counter this loop
	// would spin hot instead of reading.
	empty := 0
	for {
		n, err := r.body.Read(b)
		if n > 0 {
			r.offset += int64(n)
			r.tries = 0
			return n, nil
		}
		if err == nil {
			empty++
			if empty > 100 {
				return 0, errors.New("source keeps returning nothing")
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		empty = 0
		if err == io.EOF {
			return 0, io.EOF
		}
		if r.ctx.Err() != nil {
			return 0, r.ctx.Err()
		}
		// Break: reconnect and continue reading from r.offset.
		r.tries++
		if r.tries > maxResumes {
			return 0, fmt.Errorf("no stable connection after %d attempts: %w", maxResumes, err)
		}
		r.body.Close()
		logf("  Connection dropped at %s (%v) — resuming, attempt %d",
			human(r.offset), err, r.tries)
		time.Sleep(time.Duration(min(r.tries, 10)) * time.Second)
		if cerr := r.connect(); cerr != nil {
			logf("  Reconnect failed: %v", cerr)
			r.body = io.NopCloser(strings.NewReader(""))
			continue
		}
	}
}

func (r *resumingReader) Close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}

// ── Seeding ──────────────────────────────────────────────────────────

// seed places the server details into the freshly written system so the
// agent later knows where to report. The trick of the whole architecture: we
// have the root filesystem writable in hand anyway, so neither cloud-init nor
// dietpi.txt nor sysconf.txt is required.
func (c *client) seed(job *provisionResp, target string) error {
	part, err := findRootPartition(target)
	if err != nil {
		return err
	}
	mnt := "/mnt/seed"
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount(part, mnt, "ext4", 0, ""); err != nil {
		return fmt.Errorf("%s not mountable: %w", part, err)
	}
	defer syscall.Unmount(mnt, 0)

	dir := filepath.Join(mnt, "etc", "rookery")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	server := job.ServerURL
	if server == "" {
		server = c.base
	}
	env := fmt.Sprintf(
		"# placed by rookery-installer during provisioning\n"+
			"ROOKERY_SERVER=%s\nROOKERY_SERIAL=%s\nROOKERY_TOKEN=%s\n"+
			"ROOKERY_IMAGE=%s\nROOKERY_INSTALLED=%s\n",
		server, c.serial, job.Token, job.Image, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "agent.env"), []byte(env), 0o600); err != nil {
		return err
	}
	logf("Seed placed: %s/etc/rookery/agent.env", part)

	// Place SSH keys for root directly. That is the one path that works on
	// every distribution — Ubuntu, Debian and DietPi all permit key-based
	// root login (PermitRootLogin prohibit-password). cloud-init, by
	// contrast, exists only on Ubuntu.
	if job.Opts.NoRootKeys {
		logf("Root SSH keys skipped (asked for)")
	} else if err := seedRootKeys(mnt, job.SSHKeys); err != nil {
		logf("WARNING: SSH keys for root not placed: %v", err)
		c.note("SSH keys for root not placed: %v", err)
	} else if len(job.SSHKeys) > 0 {
		c.note("SSH keys for root placed (%d)", len(job.SSHKeys))
	}

	// If an agent binary is available it is installed straight away. If not
	// that is no error — the system simply boots without one.
	if job.Opts.NoAgent {
		logf("Agent not installed (asked for)")
		c.note("agent skipped as configured")
	} else if err := c.installAgent(job, mnt); err != nil {
		logf("Agent not installed: %v", err)
		c.note("agent NOT installed: %v", err)
	} else {
		c.note("agent installed and enabled")
	}

	// Additionally, where it fits: cloud-init on the boot partition. That is
	// the path Ubuntu intends, and it sets the hostname cleanly alongside the
	// keys.
	if job.Opts.NoCloudInit {
		logf("cloud-init seed skipped (asked for)")
	} else if err := c.seedCloudInit(job, target); err != nil {
		logf("cloud-init seed skipped: %v", err)
		c.note("cloud-init seed skipped: %v", err)
	} else {
		c.note("cloud-init seed placed")
	}
	return nil
}

// seedRootKeys creates authorized_keys for root. Deliberately root rather
// than a user account: which accounts exist is decided only by the first boot
// of the given distribution — root always exists.
func seedRootKeys(mnt string, keys []string) error {
	if len(keys) == 0 {
		logf("No SSH keys on file — this blade will have no access.")
		return nil
	}
	dir := filepath.Join(mnt, "root", ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body := "# placed by rookery-installer\n" + strings.Join(keys, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "authorized_keys"), []byte(body), 0o600); err != nil {
		return err
	}
	logf("SSH keys for root placed (%d)", len(keys))
	return nil
}

// seedCloudInit writes user-data and meta-data onto the FAT boot partition.
// Ubuntu's images read NoCloud from there (fs_label: system-boot). On
// distributions without cloud-init simply nothing happens — which is why a
// failure here is not fatal.
func (c *client) seedCloudInit(job *provisionResp, target string) error {
	if len(job.SSHKeys) == 0 && job.Hostname == "" {
		return errors.New("nothing to set")
	}
	part, err := findBootPartition(target)
	if err != nil {
		return err
	}
	mnt := "/mnt/boot"
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount(part, mnt, "vfat", 0, ""); err != nil {
		return fmt.Errorf("%s not mountable as FAT: %w", part, err)
	}
	defer syscall.Unmount(mnt, 0)

	// Without "- default" you lose the distribution's preconfigured user —
	// on Ubuntu the "ubuntu" account would be gone.
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("# generated by rookery-installer during provisioning\n")
	if job.Hostname != "" {
		fmt.Fprintf(&b, "hostname: %s\nmanage_etc_hosts: true\n", job.Hostname)
	}
	b.WriteString("ssh_pwauth: false\nusers:\n  - default\n")
	if len(job.SSHKeys) > 0 {
		b.WriteString("ssh_authorized_keys:\n")
		for _, k := range job.SSHKeys {
			fmt.Fprintf(&b, "  - %s\n", k)
		}
	}
	if err := os.WriteFile(filepath.Join(mnt, "user-data"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	meta := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", c.serial, job.Hostname)
	if err := os.WriteFile(filepath.Join(mnt, "meta-data"), []byte(meta), 0o644); err != nil {
		return err
	}
	logf("cloud-init seed placed on %s (user-data, meta-data)", part)
	return nil
}

// findBootPartition returns the target's FAT partition. Not "the first one":
// Debian numbers its firmware partition 15 and puts the root on 1, so
// position says nothing. Mounting it is the only answer that is true on every
// image — a partition that mounts as FAT is the FAT partition.
func findBootPartition(target string) (string, error) {
	parts, err := partitionsOf(target)
	if err != nil {
		return "", err
	}
	probe := "/mnt/fatprobe"
	if err := os.MkdirAll(probe, 0o755); err != nil {
		return "", err
	}
	for _, p := range parts {
		if err := syscall.Mount(p, probe, "vfat", syscall.MS_RDONLY, ""); err != nil {
			continue
		}
		_ = syscall.Unmount(probe, 0)
		return p, nil
	}
	return "", fmt.Errorf("no FAT partition on %s", target)
}

// partitionsOf lists the target's partitions in device order.
func partitionsOf(target string) ([]string, error) {
	base := filepath.Base(target)
	entries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return nil, err
	}
	var parts []string
	for _, e := range entries {
		n := e.Name()
		if n != base && strings.HasPrefix(n, base) {
			parts = append(parts, "/dev/"+n)
		}
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("no partition on %s", target)
	}
	sort.Strings(parts)
	return parts, nil
}

// installAgent fetches the agent binary from the server and anchors it with
// systemd. Offline, "enabling" means nothing more than creating the symlink
// systemctl enable would create.
func (c *client) installAgent(job *provisionResp, mnt string) error {
	resp, err := c.http().Get(c.base + "/agent/rookery-agent-arm64")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("no agent published on the server (HTTP %d)", resp.StatusCode)
	}
	binDir := filepath.Join(mnt, "usr", "local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(binDir, "rookery-agent"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	f.Close()

	unit := "[Unit]\nDescription=Rookery agent\nAfter=network-online.target\n" +
		"Wants=network-online.target\n\n[Service]\nType=simple\n" +
		"EnvironmentFile=/etc/rookery/agent.env\n" +
		"ExecStart=/usr/local/bin/rookery-agent\nRestart=always\nRestartSec=10\n\n" +
		"[Install]\nWantedBy=multi-user.target\n"
	sysDir := filepath.Join(mnt, "etc", "systemd", "system")
	if err := os.MkdirAll(sysDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(sysDir, "rookery-agent.service"),
		[]byte(unit), 0o644); err != nil {
		return err
	}
	wants := filepath.Join(sysDir, "multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		return err
	}
	link := filepath.Join(wants, "rookery-agent.service")
	_ = os.Remove(link)
	if err := os.Symlink("/etc/systemd/system/rookery-agent.service", link); err != nil {
		return err
	}
	logf("Agent installed and enabled.")
	return nil
}

// findRootPartition looks for the target's largest partition. On the usual
// Pi images that is the root; the small FAT boot partition rules itself out.
func findRootPartition(target string) (string, error) {
	base := filepath.Base(target)
	entries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return "", err
	}
	best, bestSize := "", uint64(0)
	for _, e := range entries {
		n := e.Name()
		if n == base || !strings.HasPrefix(n, base) {
			continue
		}
		b, err := os.ReadFile("/sys/class/block/" + n + "/size")
		if err != nil {
			continue
		}
		var sectors uint64
		fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &sectors)
		if sectors > bestSize {
			best, bestSize = n, sectors
		}
	}
	if best == "" {
		return "", fmt.Errorf("no partition found on %s", target)
	}
	return "/dev/" + best, nil
}

// ── Odds and ends ────────────────────────────────────────────────────

type progressReader struct {
	mu       sync.Mutex
	r        io.Reader
	total    int64
	read     int64
	last     time.Time
	lastData time.Time
	start    time.Time
	onTick   func(read int64, pct int, rate float64)
}

func (p *progressReader) bytesRead() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.read
}

// idleFor reports how long no byte has arrived.
func (p *progressReader) idleFor() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastData.IsZero() {
		return 0
	}
	return time.Since(p.lastData)
}

func (p *progressReader) Read(b []byte) (int, error) {
	if p.start.IsZero() {
		p.start = time.Now()
		p.last = p.start
		p.mu.Lock()
		p.lastData = p.start
		p.mu.Unlock()
	}
	n, err := p.r.Read(b)
	p.mu.Lock()
	p.read += int64(n)
	if n > 0 {
		p.lastData = time.Now()
	}
	p.mu.Unlock()
	if time.Since(p.last) >= 3*time.Second {
		p.last = time.Now()
		pct := 0
		if p.total > 0 {
			pct = int(p.read * 100 / p.total)
		}
		secs := time.Since(p.start).Seconds()
		rate := 0.0
		if secs > 0 {
			rate = float64(p.read) / secs / 1e6
		}
		if p.onTick != nil {
			p.onTick(p.read, pct, rate)
		}
	}
	return n, err
}

func pctText(total int64, pct int) string {
	if total <= 0 {
		return ""
	}
	return fmt.Sprintf(" / %s (%d %%)", human(total), pct)
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	fmt.Println(line)
	// Also straight to the console, so the output reaches the HDMI port even
	// when stdout points elsewhere.
	if f, err := os.OpenFile("/dev/console", os.O_WRONLY, 0); err == nil {
		fmt.Fprintln(f, line)
		f.Close()
	}
}

func reboot() {
	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
}
