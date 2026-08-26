#!/usr/bin/env bash
# Prepares a mirrored image before any blade ever sees it: installs the
# packages that have to be there at first boot, and clears the identity the
# image was built with.
#
#   tools/prepare-image.sh <catalogue-id> [package ...]
#   tools/prepare-image.sh debian-13-arm64 openssh-server
#
# Why here and not on the blade: the Debian raspi image ships neither
# openssh-server nor cloud-init, so a blade installed from it has exactly one
# door — the Sheath agent. One door is one door too few when the question is
# why the agent did not start. Doing it once per image also beats doing it
# once per blade.
#
# The host must be the same architecture as the image (arm64 here), because
# the work happens in a chroot without emulation.

set -euo pipefail

# --root=DIR ahead of everything else, because sudo drops the environment and
# a tool that silently works in the wrong directory is worse than one that
# refuses to start.
ROOT=${SHEATH_ROOT:-/srv/sheath}
case "${1:-}" in
--root=*) ROOT=${1#--root=}; shift ;;
esac
DIR="$ROOT/images"
ID=${1:?usage: prepare-image.sh <catalogue-id> [package ...]}
shift
PKGS=("$@")
[ ${#PKGS[@]} -gt 0 ] || PKGS=(openssh-server)

say() { printf '[%s] %s\n' "$(date +%T)" "$1"; }

# Checked here as well as where they were typed: the server may invoke this
# through sudo, the id goes into a SQL query and the package names go to apt.
case "$ID" in
*[!A-Za-z0-9._-]* | "" | .*) echo "invalid image id: $ID" >&2; exit 2 ;;
esac
for p in "${PKGS[@]}"; do
  case "$p" in
  *[!A-Za-z0-9.+-]* | "") echo "invalid package name: $p" >&2; exit 2 ;;
  esac
done

FILE=$(sudo sqlite3 "$ROOT/data/sheath.db" "SELECT local FROM images WHERE id='$ID';")
[ -n "$FILE" ] || { echo "no local file for $ID in the catalogue" >&2; exit 1; }
SRC="$DIR/$FILE"

WORK="$ROOT/tmp/prepare.$$"
MNT="$WORK/mnt"
mkdir -p "$MNT"
LOOP=""
cleanup() {
  set +e
  mountpoint -q "$MNT/boot/firmware" && sudo umount "$MNT/boot/firmware"
  for d in dev/pts dev proc sys; do
    mountpoint -q "$MNT/$d" && sudo umount -l "$MNT/$d"
  done
  mountpoint -q "$MNT" && sudo umount "$MNT"
  [ -n "$LOOP" ] && sudo losetup -d "$LOOP"
  sudo rm -rf "$WORK"
}
trap cleanup EXIT

say "unpacking $FILE"
case "$SRC" in
*.xz) xz -dc "$SRC" > "$WORK/disk.img" ;;
*.gz) gzip -dc "$SRC" > "$WORK/disk.img" ;;
*.zst) zstd -dc "$SRC" -o "$WORK/disk.img" ;;
*) cp "$SRC" "$WORK/disk.img" ;;
esac

# Room for the packages. The image is sized for its own contents, and apt
# needs somewhere to put what it downloads.
say "growing the image by 1 GB"
truncate -s +1G "$WORK/disk.img"
LOOP=$(sudo losetup -Pf --show "$WORK/disk.img")

# The root is the largest partition — on Debian that is number 1, with the
# firmware on 15, so position tells you nothing.
ROOTPART=$(lsblk -bnro NAME,SIZE "$LOOP" | tail -n +2 | sort -k2 -n | tail -1 | cut -d' ' -f1)
ROOTDEV="/dev/$ROOTPART"
say "root partition $ROOTDEV"
sudo e2fsck -fp "$ROOTDEV" >/dev/null 2>&1 || true
# The partition itself has to grow before the filesystem in it can.
echo ", +" | sudo sfdisk -N "${ROOTPART##*p}" --no-reread "$LOOP" >/dev/null 2>&1 || true
sudo partprobe "$LOOP" 2>/dev/null || true
sudo resize2fs "$ROOTDEV" >/dev/null 2>&1 || true

sudo mount "$ROOTDEV" "$MNT"
for d in dev dev/pts proc sys; do sudo mount --bind "/$d" "$MNT/$d"; done
# Debian points /etc/resolv.conf at systemd-resolved, which is not running in
# a chroot. Replace the dangling link for the duration and restore it after.
sudo rm -f "$MNT/etc/resolv.conf"
sudo cp /etc/resolv.conf "$MNT/etc/resolv.conf"

say "installing: ${PKGS[*]}"
sudo chroot "$MNT" /bin/sh -c "
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends ${PKGS[*]}
  apt-get clean
  rm -rf /var/lib/apt/lists/*
"

# systemd derives the DHCP identity from the machine id. Every blade written
# from this image would otherwise carry the same one and they would fight over
# a lease. Empty means "generate one at first boot".
say "clearing the machine identity"
sudo sh -c ": > $MNT/etc/machine-id"
sudo rm -f "$MNT/var/lib/dbus/machine-id"
# Host keys belong to a host, not to an image — every blade written from this
# one would otherwise present the same identity. Removing them is not enough,
# though: sshd refuses to start without keys and Debian's sshd-keygen does not
# reliably step in, so a unit that regenerates them takes their place.
sudo rm -f "$MNT"/etc/ssh/ssh_host_*
sudo tee "$MNT/etc/systemd/system/sheath-sshkeys.service" >/dev/null <<'UNIT'
[Unit]
Description=Generate SSH host keys if they are missing
Before=ssh.service ssh.socket

[Service]
Type=oneshot
RemainAfterExit=yes
# ssh-keygen -A creates only what is not there, so this is idempotent.
ExecStart=/usr/bin/ssh-keygen -A

[Install]
WantedBy=multi-user.target
UNIT
sudo chroot "$MNT" systemctl enable sheath-sshkeys.service >/dev/null 2>&1 ||
  sudo ln -sf /etc/systemd/system/sheath-sshkeys.service \
    "$MNT/etc/systemd/system/multi-user.target.wants/sheath-sshkeys.service"
# The package's own enablement does not survive every chroot, so make sure.
sudo chroot "$MNT" systemctl enable ssh.service >/dev/null 2>&1 ||
  sudo ln -sf /lib/systemd/system/ssh.service \
    "$MNT/etc/systemd/system/multi-user.target.wants/ssh.service"

sudo rm -f "$MNT/etc/resolv.conf"
sudo ln -sf ../run/systemd/resolve/stub-resolv.conf "$MNT/etc/resolv.conf"
for d in dev/pts dev proc sys; do sudo umount -l "$MNT/$d"; done
sudo umount "$MNT"
sudo e2fsck -fp "$ROOTDEV" >/dev/null 2>&1 || true
sudo losetup -d "$LOOP"; LOOP=""

say "compressing"
xz -T0 -3 -c "$WORK/disk.img" > "$DIR/$FILE.part"
mv "$DIR/$FILE.part" "$DIR/$FILE"
SUM=$(sha256sum "$DIR/$FILE" | cut -d' ' -f1)
SZ=$(stat -c%s "$DIR/$FILE")
say "$ID: $((SZ / 1048576)) MB, sha256 $SUM"

T=$(sudo cat "$ROOT/data/admin-token")
URL=$(sudo sqlite3 "$ROOT/data/sheath.db" "SELECT url FROM images WHERE id='$ID';")
OS=$(sudo sqlite3 "$ROOT/data/sheath.db" "SELECT os_id FROM images WHERE id='$ID';")
curl -sS -X POST -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
  -d "{\"id\":\"$ID\",\"url\":\"$URL\",\"local\":\"$FILE\",\"sha256\":\"$SUM\",\"bytes\":$SZ,\"seed\":\"generic\",\"os_id\":\"$OS\",\"notes\":\"mirrored locally, prepared: ${PKGS[*]}\"}" \
  http://127.0.0.1:8080/api/v1/images >/dev/null
say "catalogue updated"
