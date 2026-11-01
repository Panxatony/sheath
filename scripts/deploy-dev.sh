#!/usr/bin/env bash
# Build the current working tree and put it on the machines, without cutting a
# release. For releases use the tag and the playbook — see AGENTS.md §4.
#
#   scripts/deploy-dev.sh centre          # sheathd on the centre, restart
#   scripts/deploy-dev.sh agent           # the agent binary + the checksum the
#                                         # blades compare against
#   scripts/deploy-dev.sh payload         # rebuild the mini OS and boot.img
#   scripts/deploy-dev.sh sites           # sheath-site on every site, via Ansible
#   scripts/deploy-dev.sh all
#
# Every step is idempotent and safe to repeat. Nothing here touches a blade.

set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# Which machine the centre is belongs to one installation, not to a public
# repository: $SHEATH_CENTRE, or a line in the gitignored local/centre.
CENTRE=${SHEATH_CENTRE:-$(cat "$REPO/local/centre" 2>/dev/null || true)}
[ -n "$CENTRE" ] || { echo "set SHEATH_CENTRE or write local/centre — see AGENTS.md §4" >&2; exit 2; }
OUT=${SHEATH_BUILD_DIR:-$(mktemp -d)}
VERSION=$(cd "$REPO" && git describe --tags --always)

say() { printf '\n── %s\n' "$1"; }

build() { # <module> <output name> [arch]
  local mod=$1 name=$2 arch=${3:-arm64}
  ( cd "$REPO/$mod" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
      go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o "$OUT/$name" . )
}

# ssh cannot take a local file as an argument, and scp through a jump host has
# been unreliable here; piping the bytes works everywhere.
push() { # <local file> <remote path> <mode>
  ssh "$CENTRE" "cat > /tmp/.push.$$" < "$1"
  ssh "$CENTRE" "sudo install -o sheath -g sheath -m $3 /tmp/.push.$$ '$2' && rm -f /tmp/.push.$$"
}

centre() {
  say "sheathd $VERSION → $CENTRE"
  build server sheathd-linux-arm64
  push "$OUT/sheathd-linux-arm64" /srv/sheath/sheathd 0755
  ssh "$CENTRE" 'sudo systemctl restart sheathd && sleep 3 && systemctl is-active sheathd'
  ssh "$CENTRE" 'journalctl -u sheathd --since -1min --no-pager | grep -o "Sheath v[^ ]*" | tail -1'
}

agent() {
  say "agent $VERSION → $CENTRE, and the checksum the blades compare against"
  build agent sheath-agent-linux-arm64
  push "$OUT/sheath-agent-linux-arm64" /srv/sheath/agent/sheath-agent-arm64 0755
  # Replacing the file is half the job: a blade installs a binary only when the
  # checksum in its configuration differs from what it has. Leave the two out
  # of step and the centre serves something nobody is told to fetch — and an
  # installer that does fetch it fails on the checksum.
  ssh "$CENTRE" 'set -e
    umask 077
    T=$(cat /srv/sheath/data/admin-token)
    NEW=$(sha256sum /srv/sheath/agent/sheath-agent-arm64 | cut -d" " -f1)
    curl -s -H "Authorization: Bearer $T" localhost:8080/api/v1/config/global > /tmp/.cfg.json
    python3 - "$NEW" <<PY
import json,sys
c=json.load(open("/tmp/.cfg.json"))
for b in c.get("binaries",[]):
    if b.get("path")=="/usr/local/bin/sheath-agent": b["sha256"]=sys.argv[1]
json.dump(c,open("/tmp/.cfg.new.json","w"))
PY
    curl -s -o /dev/null -X PUT -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
      --data-binary @/tmp/.cfg.new.json localhost:8080/api/v1/config/global
    rm -f /tmp/.cfg.json /tmp/.cfg.new.json
    echo "  agent sha256 now $(sha256sum /srv/sheath/agent/sheath-agent-arm64 | cut -c1-16)"'
}

payload() {
  say "mini OS and boot.img on $CENTRE"
  tar cz -C "$REPO" installer | ssh "$CENTRE" 'cat > /tmp/.installer.tgz'
  ssh "$CENTRE" 'set -e
    cd /tmp && rm -rf .srcnew && mkdir .srcnew && tar xzf .installer.tgz -C .srcnew
    sudo cp .srcnew/installer/*.go .srcnew/installer/go.mod .srcnew/installer/go.sum \
            .srcnew/installer/init /srv/sheath/src-installer/
    sudo GOCACHE=/srv/sheath/gocache GOMODCACHE=/srv/sheath/gomod /srv/sheath/tools/build-minios.sh >/dev/null
    sudo /srv/sheath/tools/build-bootimg.sh | tail -2
    rm -rf /tmp/.installer.tgz /tmp/.srcnew'
}

sites() {
  say "sheath-site $VERSION → every site, through the playbook"
  mkdir -p "$OUT/bin"
  build site sheath-site-linux-arm64 arm64
  mv "$OUT/sheath-site-linux-arm64" "$OUT/bin/"
  ( cd "$REPO/deploy" && ansible-playbook site.yml -l sheath_sites -e sheath_local_bin_dir="$OUT/bin" )
}

case "${1:-}" in
centre)  centre ;;
agent)   agent ;;
payload) payload ;;
sites)   sites ;;
all)     centre; agent; payload; sites ;;
*) sed -n '2,12p' "$0"; exit 2 ;;
esac
