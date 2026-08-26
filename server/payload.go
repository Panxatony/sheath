package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The netboot payload, and knowing which one you are looking at.
//
// The payload is built by hand and copied here, and the sites fetch it from
// here. Until this existed, nothing said which installer a site was actually
// serving: an installer fix was deployed by remembering to deploy it, and two
// copies of boot.img in two TFTP roots looked identical whether they were or
// not.
//
// So the centre states what it has — the checksum of the file, and a short
// version derived from it — and every site reports what it holds. Where the
// two differ, the interface says so.

type payloadInfo struct {
	SHA256  string    `json:"sha256"`  // of boot.img, kept for older sites
	Version string    `json:"version"` // of the whole set, first eight, for people
	Bytes   int64     `json:"bytes"`
	Built   time.Time `json:"built"`

	// Every file a site has to serve, not just boot.img. The CM4 bootloader
	// asks for start4.elf, fixup4.dat, the device tree and config.txt before
	// it ever looks at boot.img — a site with only boot.img answers "file not
	// found" four times and the blade falls through to its NVMe. That was the
	// state of the second site until a blade in it tried to boot.
	Files []payloadFile `json:"files"`
}

type payloadFile struct {
	Name   string `json:"name"` // relative to the TFTP root, slash separated
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// payloadCache keeps the checksum, because hashing 26 MB on every desired
// state request would be work done thirty times a minute for an answer that
// changes when somebody rebuilds the payload.
type payloadCache struct {
	mu      sync.Mutex
	info    payloadInfo
	modTime time.Time
	size    int64
}

func (a *App) payload() payloadInfo {
	newest, total, err := payloadStamp(a.tftpDir)
	if err != nil {
		return payloadInfo{}
	}
	a.pay.mu.Lock()
	defer a.pay.mu.Unlock()
	if a.pay.info.Version != "" && newest.Equal(a.pay.modTime) && total == a.pay.size {
		return a.pay.info
	}

	files, err := readPayloadDir(a.tftpDir)
	if err != nil || len(files) == 0 {
		return payloadInfo{}
	}
	info := payloadInfo{Files: files, Built: newest.UTC()}
	// The version covers the set. A site missing one firmware file is a site
	// serving a different payload, and saying otherwise is how the second
	// site sat there for an evening looking correct and booting nothing.
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s %s\n", f.Name, f.SHA256)
		if f.Name == "boot.img" {
			info.SHA256, info.Bytes = f.SHA256, f.Bytes
		}
	}
	info.Version = shortVersion(hex.EncodeToString(h.Sum(nil)))
	a.pay.info = info
	a.pay.modTime, a.pay.size = newest, total
	return a.pay.info
}

// payloadStamp is the cheap question asked on every request: has anything in
// there been touched, and does it still add up to the same number of bytes.
func payloadStamp(dir string) (time.Time, int64, error) {
	var newest time.Time
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || skipPayloadFile(d.Name()) {
			return nil
		}
		st, serr := d.Info()
		if serr != nil {
			return nil
		}
		if st.ModTime().After(newest) {
			newest = st.ModTime()
		}
		total += st.Size()
		return nil
	})
	return newest, total, err
}

// readPayloadDir lists what a site has to end up with, sorted so the version
// does not depend on the order a directory happens to be read in.
func readPayloadDir(dir string) ([]payloadFile, error) {
	var out []payloadFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || skipPayloadFile(d.Name()) {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		sum, serr := sha256File(p)
		if serr != nil {
			return nil
		}
		st, _ := d.Info()
		out = append(out, payloadFile{
			Name: filepath.ToSlash(rel), SHA256: sum, Bytes: st.Size(),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

// skipPayloadFile leaves out what is bookkeeping rather than payload: the
// checksum stamps this server and the sites write beside the files.
func skipPayloadFile(name string) bool {
	return strings.HasSuffix(name, ".verified") ||
		strings.HasSuffix(name, ".upstream") ||
		strings.HasSuffix(name, ".tmp") ||
		strings.HasPrefix(name, ".")
}

// shortVersion is what a person compares at a glance. Eight hex characters
// are not a cryptographic claim — the full checksum next to it is — but they
// are what fits beside a site's name.
func shortVersion(sum string) string {
	if len(sum) < 8 {
		return sum
	}
	return sum[:8]
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// payloadState says how a site's payload compares with this one, in a word
// the interface can colour.
func payloadState(centre, site string) (key, led string) {
	switch {
	case site == "":
		return "pay.unknown", ""
	case centre == "":
		return "pay.nocentre", "warn"
	case site == centre:
		return "pay.same", "ok"
	}
	return "pay.differs", "warn"
}

func (a *App) hPayload(w http.ResponseWriter, r *http.Request) {
	p := a.payload()
	if p.SHA256 == "" {
		fail(w, 404, "no payload at %s", filepath.Join(a.tftpDir, "boot.img"))
		return
	}
	writeJSON(w, 200, p)
}

// payloadFromStatus reads what a site said about its payload, tolerating a
// site that is older than this field.
func payloadFromStatus(m map[string]any) string {
	v, _ := m["payload"].(string)
	return strings.TrimSpace(v)
}
