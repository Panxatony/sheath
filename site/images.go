package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The image cache.
//
// The catalogue is central, the bytes are local: an image crosses the site
// link once per site, not once per blade. At a gigabyte over a narrow line
// that is the difference between eleven minutes and an afternoon.
func (s *site) ensureImages(d *desired) error {
	if err := os.MkdirAll(s.cfg.ImagesDir, 0o755); err != nil {
		return err
	}
	stock := make([]stockItem, 0, len(d.Images))
	defer func() { s.setStock(stock) }()

	for _, im := range d.Images {
		name := im.Local
		if name == "" {
			name = filepath.Base(im.URL)
		}
		path := filepath.Join(s.cfg.ImagesDir, name)

		if ok, err := s.haveImage(path, im.SHA256, im.Bytes); err == nil && ok {
			sz := int64(0)
			if st, serr := os.Stat(path); serr == nil {
				sz = st.Size()
			}
			stock = append(stock, stockItem{ImageID: im.ID, State: "ready", Bytes: sz})
			continue
		}
		// Prefer the copy the centre already holds: it is on the same server
		// that just handed out this list, and it is the one whose checksum
		// the centre knows.
		src := im.URL
		if im.Local != "" {
			src = strings.TrimRight(d.Boot.ServerURL, "/") + "/images/" + im.Local
		}
		log.Printf("image %s: fetching from %s", im.ID, src)
		s.note("", "info", "fetching image "+im.ID)
		if s.dry {
			stock = append(stock, stockItem{ImageID: im.ID, State: "absent"})
			continue
		}
		// Reported as fetching before the first byte moves, because a
		// gigabyte over a narrow line is a state someone may be waiting on.
		s.setStock(append(stock, stockItem{ImageID: im.ID, State: "fetching"}))
		if err := s.fetchImage(src, path, im.SHA256); err != nil {
			log.Printf("image %s: %v", im.ID, err)
			s.note("", "warn", "image "+im.ID+" not fetched: "+err.Error())
			stock = append(stock, stockItem{ImageID: im.ID, State: "error", Note: err.Error()})
			continue
		}
		sz := int64(0)
		if st, serr := os.Stat(path); serr == nil {
			sz = st.Size()
		}
		stock = append(stock, stockItem{ImageID: im.ID, State: "ready", Bytes: sz})
		s.note("", "info", "image "+im.ID+" ready")
	}
	return nil
}

// haveImage answers whether the file on disk is the wanted one. Checksums are
// verified once and remembered by size and time — hashing a gigabyte on every
// pass would keep a small server busy for nothing.
func (s *site) haveImage(path, sum string, size int64) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if size > 0 && st.Size() != size {
		return false, nil
	}
	stamp := path + ".verified"
	if v, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(v)) == sum {
		return true, nil
	}
	got, err := fileSHA256(path)
	if err != nil {
		return false, err
	}
	if sum != "" && !strings.EqualFold(got, sum) {
		return false, nil
	}
	_ = os.WriteFile(stamp, []byte(got), 0o644)
	return true, nil
}

func (s *site) fetchImage(url, path, sum string) error {
	tmp := path + ".part"
	// Resume what is already there: the site link is exactly the kind of line
	// that drops in the middle of a gigabyte.
	var from int64
	if st, err := os.Stat(tmp); err == nil {
		from = st.Size()
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	if from > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	resp, err := s.dl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		from = 0
		flags |= os.O_TRUNC
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if sum != "" {
		got, err := fileSHA256(tmp)
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, sum) {
			// A wrong image is worse than no image: it would be written to a
			// blade's disk. Start over rather than keep it.
			os.Remove(tmp)
			return fmt.Errorf("checksum mismatch: expected %s, got %s", sum, got)
		}
		_ = os.WriteFile(path+".verified", []byte(got), 0o644)
	}
	return os.Rename(tmp, path)
}

// ensureBoot keeps the netboot payload in the TFTP root current. Without it a
// site would serve yesterday's installer to a blade booting today.
//
// What is compared is not the file — the file is rewritten after it arrives,
// see below — but the checksum it had upstream, kept beside it. That is the
// only thing both ends can agree on, and it is what this site reports as the
// installer it serves.
func (s *site) ensureBoot(d *desired) error {
	if d.Boot.BootImg == "" {
		return nil
	}
	if err := os.MkdirAll(s.cfg.TFTPDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(s.cfg.TFTPDir, "boot.img")
	_, missing := os.Stat(path)
	if d.Boot.SHA256 != "" {
		if s.payloadHeld() == d.Boot.SHA256 && missing == nil {
			return nil
		}
	} else if missing == nil {
		// Without a checksum there is nothing to compare against, so an
		// existing payload is left alone rather than re-fetched every pass.
		return nil
	}
	if s.dry {
		return nil
	}
	log.Printf("boot payload: fetching %s", d.Boot.BootImg)
	if err := s.fetchImage(d.Boot.BootImg, path, d.Boot.SHA256); err != nil {
		return err
	}
	if err := os.WriteFile(path+".upstream", []byte(d.Boot.SHA256+"\n"), 0o644); err != nil {
		log.Printf("boot payload: stamp not written: %v", err)
	}
	if err := s.aimPayloadHere(path); err != nil {
		log.Printf("boot payload: %v", err)
		s.note("", "warn", "netboot payload not aimed at this site: "+err.Error())
	}
	s.note("", "info", "netboot payload updated to "+short(d.Boot.SHA256))
	return nil
}

// aimPayloadHere rewrites the one line in the payload that names a server.
// The centre builds the payload and writes its own address into it, which is
// right for exactly one site: everywhere else a blade would be told to fetch
// hundreds of megabytes across the link that may be the very thing that is
// down, when the answer is in the same room.
func (s *site) aimPayloadHere(path string) error {
	if s.cfg.RelayURL == "" {
		log.Printf("boot payload: no -relay-url, leaving the server address as built")
		return nil
	}
	line, err := readFATFile(path, "cmdline.txt")
	if err != nil {
		return fmt.Errorf("cmdline.txt: %w", err)
	}
	out := replaceServer(line, s.cfg.RelayURL)
	if strings.TrimSpace(out) == strings.TrimSpace(line) {
		return nil
	}
	if err := patchCmdline(path, out); err != nil {
		return err
	}
	log.Printf("boot payload: blades here are pointed at %s", s.cfg.RelayURL)
	return nil
}

// replaceServer swaps the value of sheath_server= and leaves everything else
// on the line alone — the console settings are not ours to decide.
func replaceServer(line, url string) string {
	fields := strings.Fields(strings.TrimSpace(line))
	found := false
	for i, f := range fields {
		if strings.HasPrefix(f, "sheath_server=") {
			fields[i] = "sheath_server=" + url
			found = true
		}
	}
	if !found {
		fields = append(fields, "sheath_server="+url)
	}
	return strings.Join(fields, " ")
}

// payloadHeld is the upstream checksum of the payload this site serves, or
// empty when there is none to speak of.
func (s *site) payloadHeld() string {
	b, err := os.ReadFile(filepath.Join(s.cfg.TFTPDir, "boot.img.upstream"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func short(sum string) string {
	if len(sum) > 8 {
		return sum[:8]
	}
	return sum
}

func fileSHA256(path string) (string, error) {
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
