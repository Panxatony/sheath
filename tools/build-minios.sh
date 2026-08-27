#!/usr/bin/env bash
# Builds the mini OS from the official Raspberry Pi network-install image.
#
#   sudo tools/build-minios.sh                 # into /srv/sheath/build
#   sudo tools/build-minios.sh /srv/sheath/build
#   NET_INSTALL_SHA256=… sudo tools/build-minios.sh   # a new upstream build
#
# The mini OS was an heirloom: a kernel and a 43 MB ramdisk that had been
# assembled once and copied from machine to machine ever since, with nobody
# able to say which kernel it was or how the ramdisk had been made. This is
# what it actually is, written down.
#
# It is the Raspberry Pi network installer, with the imager taken out and the
# Sheath installer put in its place. That is a good base and not a lazy one:
# it already solves "boot a CM4 over the network and have an address" —  udev,
# the drivers, dhcpcd, an init that works — and all of that is somebody else's
# well-tested problem.
#
# Removed, because a headless blade in a rack has no use for them: the imager
# itself, Qt, the QML plugins, the graphics drivers, maps, icons and fonts.
# Together 57 MB, which is 57 MB not transferred over TFTP to every blade
# that netboots.
#
# The artefact is pinned by checksum. Upstream publishes it at one URL and
# replaces it in place, so a new build changes the payload every blade boots
# — that has to be a decision somebody makes, not something that happens.

set -euo pipefail

DEST=${1:-/srv/sheath/build}
URL=${NET_INSTALL_URL:-https://downloads.raspberrypi.org/net_install/boot.img}
WANT=${NET_INSTALL_SHA256:-9f26719cc254d701ccc1ae654649e31db3f033f13984dc48bc3c0d9dfc12fe77}
SRC=${INSTALLER_SRC:-$(cd "$(dirname "$0")/../installer" 2>/dev/null && pwd || echo /srv/sheath/src-installer)}

say() { printf '\n── %s\n' "$1"; }
need() { command -v "$1" >/dev/null || { echo "missing: $1" >&2; exit 1; }; }
for t in curl mcopy zstd cpio sha256sum; do need "$t"; done

WORK=$(mktemp -d "${TMPDIR:-/tmp}/minios.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$DEST"

say "Fetching the network installer"
IMG="$DEST/net_install.img"
if [ -f "$IMG" ] && [ "$(sha256sum "$IMG" | cut -d' ' -f1)" = "$WANT" ]; then
  echo "  already here and matching"
else
  curl -fL --retry 3 -o "$IMG.part" "$URL"
  GOT=$(sha256sum "$IMG.part" | cut -d' ' -f1)
  if [ "$GOT" != "$WANT" ]; then
    rm -f "$IMG.part"
    cat >&2 <<MSG
The image at $URL is not the one this recipe was written against.

  expected  $WANT
  got       $GOT

Upstream replaces that file in place. Look at what changed, try it on a blade,
and then set NET_INSTALL_SHA256 in this script — the payload every blade boots
is not a thing to update by accident.
MSG
    exit 1
  fi
  mv "$IMG.part" "$IMG"
fi
echo "  $(stat -c%s "$IMG") bytes, sha256 $WANT"

say "Taking the kernel and the firmware out of it"
for f in Image.gz start4.elf fixup4.dat bcm2711-rpi-cm4.dtb; do
  mcopy -n -i "$IMG" "::$f" "$DEST/$f"
  printf '  %-24s %s bytes\n' "$f" "$(stat -c%s "$DEST/$f")"
done
mcopy -n -s -i "$IMG" ::overlays "$DEST/" 2>/dev/null || true

say "Unpacking the ramdisk"
mcopy -n -i "$IMG" ::rootfs.cpio.zst "$WORK/rootfs.cpio.zst"
ROOTFS="$DEST/rootfs"
rm -rf "$ROOTFS"
mkdir -p "$ROOTFS"
( cd "$ROOTFS" && zstd -dc "$WORK/rootfs.cpio.zst" | cpio -idm --quiet )
echo "  $(find "$ROOTFS" -type f | wc -l) files, $(du -sh "$ROOTFS" | cut -f1)"

say "Removing what only a screen needs"
rm -f  "$ROOTFS/usr/bin/rpi-imager" "$ROOTFS/usr/bin/qmltime"
rm -rf "$ROOTFS/usr/qml" "$ROOTFS/usr/lib/dri" "$ROOTFS/usr/lib/qt5" \
       "$ROOTFS/usr/share/qmaps" "$ROOTFS/usr/share/icons" \
       "$ROOTFS/usr/share/fonts" "$ROOTFS/usr/share/applications" \
       "$ROOTFS/usr/share/metainfo" "$ROOTFS/usr/share/drirc.d"
rm -f  "$ROOTFS"/usr/lib/libQt5*.so.* "$ROOTFS"/usr/lib/libqgsttools*
echo "  $(find "$ROOTFS" -type f | wc -l) files left, $(du -sh "$ROOTFS" | cut -f1)"

say "Putting the installer in the imager's place"
( cd "$SRC" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags="-s -w" -o "$WORK/sheath-installer" . )
install -m 0755 "$WORK/sheath-installer" "$ROOTFS/usr/bin/sheath-installer"
install -m 0755 "$SRC/init" "$ROOTFS/init"
echo "  $(stat -c%s "$ROOTFS/usr/bin/sheath-installer") bytes, and an init that runs it"

printf '%s\n' "$WANT" > "$DEST/.net-install-sha256"
say "Done — now build the payload"
echo "  tools/build-bootimg.sh"
echo
echo "  And then boot a blade on it. A payload that has not booted is a"
echo "  payload nobody knows about."
