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

// ensureBoot keeps the netboot payload in the TFTP root current — the whole
// root, not just boot.img.
//
// The CM4 bootloader asks for start4.elf, fixup4.dat, the device tree and
// config.txt before it looks at boot.img at all. A site holding only boot.img
// answers "file not found" four times and the blade gives up and boots from
// its NVMe, which looks exactly like a blade that was never asked to netboot.
// That is what the second site did on its first evening.
//
// What is compared is the version of the set as the centre states it, kept in
// a stamp beside the files. Not the files themselves: two of them are
// rewritten after they arrive, so their checksums here are deliberately not
// the checksums there.
func (s *site) ensureBoot(d *desired) error {
	if d.Boot.BootImg == "" && len(d.Boot.Files) == 0 {
		return nil
	}
	if err := os.MkdirAll(s.cfg.TFTPDir, 0o755); err != nil {
		return err
	}
	stamp := filepath.Join(s.cfg.TFTPDir, ".payload")
	want := d.Boot.Version
	if want == "" {
		want = d.Boot.SHA256 // a centre older than the file list
	}
	if want != "" && s.payloadHeld() == want {
		return nil
	}
	if s.dry {
		return nil
	}

	files := d.Boot.Files
	if len(files) == 0 {
		// An older centre states one file and no list. Serving only that is
		// still what it asked for.
		files = append(files, struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
			Bytes  int64  `json:"bytes"`
		}{Name: "boot.img", SHA256: d.Boot.SHA256})
	}

	base := strings.TrimRight(d.Boot.ServerURL, "/") + "/boot/"
	fetched := 0
	for _, f := range files {
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			log.Printf("boot payload: refusing %q", f.Name)
			continue
		}
		dest := filepath.Join(s.cfg.TFTPDir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if ok, err := s.haveImage(dest, f.SHA256, f.Bytes); err == nil && ok {
			continue
		}
		if err := s.fetchImage(base+f.Name, dest, f.SHA256); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		fetched++
	}

	if err := s.aimPayloadHere(filepath.Join(s.cfg.TFTPDir, "boot.img")); err != nil {
		log.Printf("boot payload: %v", err)
		s.note("", "warn", "netboot payload not aimed at this site: "+err.Error())
	}
	if err := os.WriteFile(stamp, []byte(want+"\n"), 0o644); err != nil {
		log.Printf("boot payload: stamp not written: %v", err)
	}
	if fetched > 0 {
		log.Printf("boot payload: %d file(s) fetched, now at %s", fetched, short(want))
		s.note("", "info", fmt.Sprintf("netboot payload updated to %s (%d file(s))",
			short(want), fetched))
	}
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
	// Two of them name a server: the one inside boot.img, which the mini OS
	// reads, and the one in the TFTP root, which the firmware reads when it
	// boots the files directly. Aiming one and not the other is how a blade
	// ends up talking to the wrong machine depending on how it started.
	plain := filepath.Join(filepath.Dir(path), "cmdline.txt")
	if b, err := os.ReadFile(plain); err == nil {
		out := replaceServer(string(b), s.cfg.RelayURL)
		if strings.TrimSpace(out) != strings.TrimSpace(string(b)) {
			if err := os.WriteFile(plain, []byte(out+"\n"), 0o644); err != nil {
				log.Printf("boot payload: cmdline.txt not rewritten: %v", err)
			}
		}
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

// payloadHeld is the version of the payload set this site serves, as the
// centre stated it, or empty when there is none to speak of.
func (s *site) payloadHeld() string {
	for _, name := range []string{".payload", "boot.img.upstream"} {
		if b, err := os.ReadFile(filepath.Join(s.cfg.TFTPDir, name)); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return ""
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
