package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

// Rewriting one file inside the netboot payload.
//
// The payload is a bare FAT16 image — no partition table, because that is what
// the CM4 bootloader expects — and one file in it, cmdline.txt, tells the mini
// OS which server to talk to. The centre builds the payload and therefore
// writes its own address in there, which is wrong for every site but the one
// standing next to it: a blade at a remote site would be handed an address
// across a link that may be down, when the thing that could answer is in the
// same room.
//
// So the site rewrites that one line for itself. Not with mtools — a core
// function should not depend on a package somebody remembered to install — and
// not by rebuilding the image, which would need the kernel and the firmware
// blobs. The file is small, it already exists, and FAT16 keeps its bytes in
// one place: the directory entry says which cluster, the cluster holds the
// content, and the entry holds the length. Replacing content that is no longer
// than the space already allocated touches exactly those two places.

type fat16 struct {
	data        []byte
	bytesPerSec int
	secPerClus  int
	rootOff     int // byte offset of the root directory
	rootEntries int
	dataOff     int // byte offset of cluster 2
}

func openFAT16(data []byte) (*fat16, error) {
	if len(data) < 512 {
		return nil, fmt.Errorf("not an image: %d bytes", len(data))
	}
	f := &fat16{data: data}
	f.bytesPerSec = int(binary.LittleEndian.Uint16(data[11:13]))
	f.secPerClus = int(data[13])
	reserved := int(binary.LittleEndian.Uint16(data[14:16]))
	numFATs := int(data[16])
	f.rootEntries = int(binary.LittleEndian.Uint16(data[17:19]))
	secPerFAT := int(binary.LittleEndian.Uint16(data[22:24]))

	if f.bytesPerSec == 0 || f.secPerClus == 0 || numFATs == 0 || secPerFAT == 0 {
		return nil, fmt.Errorf("not a FAT16 image")
	}
	f.rootOff = (reserved + numFATs*secPerFAT) * f.bytesPerSec
	rootSectors := (f.rootEntries*32 + f.bytesPerSec - 1) / f.bytesPerSec
	f.dataOff = f.rootOff + rootSectors*f.bytesPerSec
	if f.dataOff >= len(data) {
		return nil, fmt.Errorf("image ends before its data does")
	}
	return f, nil
}

// find returns the offset of the directory entry for an 8.3 name.
func (f *fat16) find(name string) (int, error) {
	want := fat83(name)
	for i := 0; i < f.rootEntries; i++ {
		off := f.rootOff + i*32
		if off+32 > len(f.data) {
			break
		}
		e := f.data[off : off+32]
		switch e[0] {
		case 0x00:
			return 0, fmt.Errorf("%s is not in the image", name)
		case 0xE5:
			continue
		}
		if e[11]&0x0F == 0x0F || e[11]&0x08 != 0 { // long name part, or the volume label
			continue
		}
		if bytes.Equal(e[0:11], want) {
			return off, nil
		}
	}
	return 0, fmt.Errorf("%s is not in the image", name)
}

// replace writes new content into an existing file. It refuses to grow the
// file beyond what it already occupies: following and extending a cluster
// chain is a different job, and cmdline.txt is a single line.
func (f *fat16) replace(name string, content []byte) error {
	off, err := f.find(name)
	if err != nil {
		return err
	}
	e := f.data[off : off+32]
	cluster := int(binary.LittleEndian.Uint16(e[26:28]))
	if cluster < 2 {
		return fmt.Errorf("%s has no cluster of its own", name)
	}
	clusterSize := f.secPerClus * f.bytesPerSec
	if len(content) > clusterSize {
		return fmt.Errorf("%s would need %d bytes, one cluster holds %d",
			name, len(content), clusterSize)
	}
	start := f.dataOff + (cluster-2)*clusterSize
	if start+clusterSize > len(f.data) {
		return fmt.Errorf("%s points past the end of the image", name)
	}
	// The whole cluster is cleared first: leaving the tail of the old line
	// behind would be invisible in a listing and read back by anything that
	// trusts the length less than the content.
	for i := start; i < start+clusterSize; i++ {
		f.data[i] = 0
	}
	copy(f.data[start:], content)
	binary.LittleEndian.PutUint32(e[28:32], uint32(len(content)))
	return nil
}

// fat83 turns "cmdline.txt" into the eleven bytes FAT stores.
func fat83(name string) []byte {
	out := []byte("           ")
	base, ext, _ := strings.Cut(strings.ToUpper(name), ".")
	copy(out[0:8], pad(base, 8))
	copy(out[8:11], pad(ext, 3))
	return out
}

func pad(s string, n int) []byte {
	b := []byte(s)
	if len(b) > n {
		b = b[:n]
	}
	for len(b) < n {
		b = append(b, ' ')
	}
	return b
}

// patchCmdline replaces cmdline.txt inside a payload with the given line.
func patchCmdline(path, line string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f, err := openFAT16(data)
	if err != nil {
		return err
	}
	if err := f.replace("cmdline.txt", []byte(strings.TrimRight(line, "\n")+"\n")); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, f.data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readFATFile is here so the result can be checked without mtools.
func readFATFile(path, name string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	f, err := openFAT16(data)
	if err != nil {
		return "", err
	}
	off, err := f.find(name)
	if err != nil {
		return "", err
	}
	e := f.data[off : off+32]
	cluster := int(binary.LittleEndian.Uint16(e[26:28]))
	size := int(binary.LittleEndian.Uint32(e[28:32]))
	clusterSize := f.secPerClus * f.bytesPerSec
	start := f.dataOff + (cluster-2)*clusterSize
	if size > clusterSize || start+size > len(f.data) {
		return "", fmt.Errorf("%s spans more than one cluster", name)
	}
	return string(f.data[start : start+size]), nil
}
