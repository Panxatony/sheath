package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// What an image says about itself on its first sector.
//
// Measured rather than deduced. blade-kb-r1s04 wrote Debian 13 to its eMMC
// without a single error — checksum verified, partition grown, agent
// installed, "done 100%" — and then never came up again: no ping, no DHCP, 45
// minutes of nothing. The same blade, the same eMMC, the same bootloader and
// the same boot order booted DietPi five minutes later. The only difference
// between those two images is in the first kilobyte: Debian carries a GPT
// with a hybrid MBR, DietPi a plain MBR.
//
// The Raspberry Pi bootloader reads a GPT from an NVMe — it says so itself,
// it reports which partition it booted — and does not from the card
// interface. So an image with a GPT on it is an image that will spend an hour
// writing itself onto an eMMC and then boot from nowhere. That is worth
// knowing before the hour rather than after it.

// partTableOf classifies the first kilobyte of a disk image. An empty answer
// means "could not tell", which must never be treated as "no GPT" — an image
// nobody could read is not an image anyone should be refused over.
func partTableOf(head []byte) string {
	if len(head) < 520 {
		return ""
	}
	if head[510] != 0x55 || head[511] != 0xAA {
		return ""
	}
	if string(head[512:520]) == "EFI PART" {
		return "gpt"
	}
	// A plain MBR: at least one entry that is a real partition rather than
	// the 0xEE placeholder a GPT leaves behind.
	for i := 0; i < 4; i++ {
		if t := head[446+16*i+4]; t != 0x00 && t != 0xEE {
			return "mbr"
		}
	}
	return ""
}

// readImageHead reads the first kilobyte of an image, through whatever it is
// compressed with. Only a kilobyte: the decompressor is started, the head is
// taken and the process is stopped again, so a 6 GB image costs milliseconds.
func readImageHead(path string) ([]byte, error) {
	var argv []string
	switch strings.ToLower(filepath.Ext(path)) {
	case ".xz":
		argv = []string{"xz", "-dc"}
	case ".zst", ".zstd":
		argv = []string{"zstd", "-dc"}
	case ".gz":
		argv = []string{"gzip", "-dc"}
	}
	if argv == nil {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		head := make([]byte, 1024)
		n, err := io.ReadFull(f, head)
		if n == 1024 {
			return head, nil
		}
		return nil, err
	}
	tool, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, append(argv[1:], path)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	head := make([]byte, 1024)
	n, rerr := io.ReadFull(out, head)
	// The decompressor is killed on purpose, so its own complaint about the
	// broken pipe is not news.
	cancel()
	_ = cmd.Wait()
	if n < 1024 {
		return nil, rerr
	}
	return head, nil
}

// imageTable answers for one catalogue entry, reading the file once and
// remembering the answer. Entries that were fetched before any of this
// existed are read the first time somebody asks.
func (a *App) imageTable(id string) string {
	if id == "" {
		return ""
	}
	var stored, local string
	if err := a.db.QueryRow(`SELECT part_table,local FROM images WHERE id=?`, id).
		Scan(&stored, &local); err != nil {
		return ""
	}
	if stored != "" || local == "" {
		return stored
	}
	// The catalogue keeps the file name, not the path: everything else that
	// touches an image serves it out of the images directory.
	if !filepath.IsAbs(local) {
		local = filepath.Join(a.imagesDir, local)
	}
	head, err := readImageHead(local)
	if err != nil {
		return ""
	}
	t := partTableOf(head)
	if t == "" {
		return ""
	}
	_, _ = a.db.Exec(`UPDATE images SET part_table=? WHERE id=?`, t, id)
	return t
}
