#!/usr/bin/env bash
# Builds the netboot payload: installer → ramdisk → boot.img → TFTP root.
#
# Runs on the Sheath server itself (arm64, so the installer is built
# natively). The sources are expected in /srv/sheath/src-installer, the
# unpacked ramdisk in /srv/sheath/build/rootfs.
#
#   sudo -u sheath tools/build-bootimg.sh          # build and install
#   BUILD_ONLY=1 tools/build-bootimg.sh             # build, do not publish
#
# Strict on purpose: a failed build must never leave a stale boot.img in the
# TFTP root. That has happened once, and the blade then netbooted yesterday's
# installer while the logs claimed today's.

set -euo pipefail

ROOT=${SHEATH_ROOT:-/srv/sheath}
BUILD="$ROOT/build"
SRC=${INSTALLER_SRC:-$ROOT/src-installer}
ROOTFS="$BUILD/rootfs"
TFTP="$ROOT/tftp"

export GOCACHE=${GOCACHE:-$ROOT/gocache}
export GOMODCACHE=${GOMODCACHE:-$ROOT/gomod}
export GOPATH=${GOPATH:-$ROOT/gopath}
export TMPDIR=${TMPDIR:-$ROOT/tmp}

step() { printf '\n── %s\n' "$1"; }

step "Building the installer ($(cd "$SRC" && go version | cut -d' ' -f3))"
rm -f "$BUILD/sheath-installer"
( cd "$SRC" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags="-s -w" -o "$BUILD/sheath-installer" . )
ls -l "$BUILD/sheath-installer"

step "Placing it in the ramdisk"
install -m 0755 "$BUILD/sheath-installer" "$ROOTFS/usr/bin/sheath-installer"
if [ -f "$BUILD/init" ]; then
  install -m 0755 "$BUILD/init" "$ROOTFS/init"
fi

step "Packing the ramdisk"
rm -f "$BUILD/rootfs.cpio.zst.new"
( cd "$ROOTFS" && find . | cpio -o -H newc --quiet ) \
  | zstd -q -19 -T0 -o "$BUILD/rootfs.cpio.zst.new"
ls -l "$BUILD/rootfs.cpio.zst.new"

step "Building boot.img"
# A bare FAT16 without a partition table — that is what the CM4 bootloader
# expects from a ramdisk image, not a disk image with an MBR.
IMG="$BUILD/boot.img.new"
rm -f "$IMG"
truncate -s 27262976 "$IMG"
mkfs.vfat -F 16 -n BOOT "$IMG" >/dev/null
mcopy -i "$IMG" "$BUILD/config.txt"    ::config.txt
mcopy -i "$IMG" "$BUILD/cmdline.txt"   ::cmdline.txt
mcopy -i "$IMG" "$BUILD/Image.gz"      ::Image.gz
mcopy -i "$IMG" "$BUILD/start4.elf"    ::start4.elf
mcopy -i "$IMG" "$BUILD/fixup4.dat"    ::fixup4.dat
mcopy -i "$IMG" "$BUILD/bcm2711-rpi-cm4.dtb" ::bcm2711-rpi-cm4.dtb
mcopy -i "$IMG" "$BUILD/rootfs.cpio.zst.new" ::rootfs.cpio.zst
mmd    -i "$IMG" ::overlays 2>/dev/null || true
for f in "$BUILD"/overlays/*; do
  [ -e "$f" ] || continue
  mcopy -i "$IMG" "$f" "::overlays/$(basename "$f")"
done
mdir -i "$IMG" ::

if [ -n "${BUILD_ONLY:-}" ]; then
  step "BUILD_ONLY set — $IMG not published"
  exit 0
fi

step "Publishing"
mv -f "$BUILD/rootfs.cpio.zst.new" "$BUILD/rootfs.cpio.zst"
mv -f "$IMG" "$BUILD/boot.img"
install -m 0644 "$BUILD/boot.img" "$TFTP/boot.img"
ls -l "$TFTP/boot.img"

step "Done — the next blade that netboots gets this installer"
