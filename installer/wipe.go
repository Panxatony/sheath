package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Wiping a blade's disk.
//
// This happens here and not in the agent for one reason: the agent runs from
// the disk it would have to erase, and a root filesystem cannot be unmounted
// out from under itself. The mini OS lives entirely in RAM and has the NVMe
// in front of it, untouched — the same position from which it writes images.
//
// Two steps, and the second one is not optional. A discard tells the drive to
// forget the blocks, which on an NVMe takes seconds and is thorough; but a
// drive is allowed to ignore it, and nothing in the protocol promises the
// bytes are gone. Overwriting the head and the tail is what actually removes
// the partition table, the boot sector, the filesystem superblocks and the
// backup GPT — the things that decide what this disk claims to be.

const blkDiscard = 0x1277 // BLKDISCARD

// wipeEnds is how much is overwritten at each end of the device.
const wipeEnds = 64 << 20

func (c *client) wipeDisk(target string) error {
	f, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%s not writable: %w", target, err)
	}
	defer f.Close()

	raw, err := blockSize(target)
	if err != nil {
		return fmt.Errorf("size of %s unknown: %w", target, err)
	}
	size := int64(raw)
	logf("Target %s: %s", target, human(size))

	c.report("wiping", 10, "")
	if err := discardAll(f, size); err != nil {
		// Not fatal: plenty of devices refuse, and the overwrite below is the
		// part that does the real work anyway.
		logf("Discard not accepted (%v) — overwriting instead", err)
		c.note("discard not accepted: %v", err)
	} else {
		logf("Discarded the whole device")
	}

	c.report("wiping", 40, "")
	head := int64(wipeEnds)
	if head > size {
		head = size
	}
	if err := zeroAt(f, 0, head); err != nil {
		return fmt.Errorf("overwriting the start: %w", err)
	}
	logf("First %s overwritten", human(head))

	c.report("wiping", 75, "")
	if size > 2*wipeEnds {
		if err := zeroAt(f, size-wipeEnds, wipeEnds); err != nil {
			return fmt.Errorf("overwriting the end: %w", err)
		}
		logf("Last %s overwritten", human(int64(wipeEnds)))
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}
	c.report("wiping", 95, "")
	return nil
}

func discardAll(f *os.File, size int64) error {
	if size <= 0 {
		return errors.New("device size unknown")
	}
	rng := [2]uint64{0, uint64(size)}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkDiscard,
		uintptr(unsafe.Pointer(&rng[0])))
	if errno != 0 {
		return errno
	}
	return nil
}

// zeroAt writes zeroes over one stretch of the device, in chunks small enough
// for a machine with little memory.
func zeroAt(f *os.File, off, length int64) error {
	if _, err := f.Seek(off, 0); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	for written := int64(0); written < length; {
		n := int64(len(buf))
		if rest := length - written; rest < n {
			n = rest
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return err
		}
		written += n
	}
	return nil
}
