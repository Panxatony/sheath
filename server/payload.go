package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	SHA256  string    `json:"sha256"`
	Version string    `json:"version"` // first eight of the checksum, for people
	Bytes   int64     `json:"bytes"`
	Built   time.Time `json:"built"`
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
	path := filepath.Join(a.tftpDir, "boot.img")
	st, err := os.Stat(path)
	if err != nil {
		return payloadInfo{}
	}
	a.pay.mu.Lock()
	defer a.pay.mu.Unlock()
	if a.pay.info.SHA256 != "" && st.ModTime().Equal(a.pay.modTime) && st.Size() == a.pay.size {
		return a.pay.info
	}
	sum, err := sha256File(path)
	if err != nil {
		return payloadInfo{}
	}
	a.pay.info = payloadInfo{
		SHA256: sum, Version: shortVersion(sum), Bytes: st.Size(), Built: st.ModTime().UTC(),
	}
	a.pay.modTime, a.pay.size = st.ModTime(), st.Size()
	return a.pay.info
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
