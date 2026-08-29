# Deploying Sheath

Two roles, because Sheath is two programs: `sheathd` keeps the inventory and
the decisions, and `sheath-site` is the network presence of one site. A machine
may be both, which is how most installations start.

```sh
cd deploy
cp inventory.example.ini inventory.ini   # and edit it
ansible-playbook site.yml --check --diff # look first
ansible-playbook site.yml
```

## What you have to decide

| Variable | Meaning |
|---|---|
| `sheath_version` | which release to install; `sheath_local_bin_dir` installs binaries you built yourself instead |
| `sheath_arch` | `arm64` unless you know otherwise — the centre must match the images it prepares |
| `sheath_server_url` | how a **site** reaches the centre |
| `sheath_site_relay_url` | how a **blade** reaches its site; written into the netboot payload |
| `sheath_site_dhcp` | off by default, see below |
| `sheath_site_enroll_code` | needed once per site, from the interface |

## How Ansible reaches the machines

The inventory names hosts, and by default your `~/.ssh/config` says how to
reach each name — user, port, key, jump host — which keeps one answer in one
place. Where a machine's address changes and the ssh config has not caught up,
put the address in the inventory (`ansible_host=`), and remember that the ssh
config block no longer matches then: the user and the key have to come with it
(`ansible_user=`, `ansible_ssh_private_key_file=`).

## Bringing a site up

A site needs a credential and asks for one rather than being handed one. On the
site's page in the interface, **Create an enrollment code**, then:

```sh
ansible-playbook site.yml -l site-b -e sheath_site_enroll_code=ABCD-EFGH-JKLM
```

The token and the site id are written on the site machine, in
`/etc/sheath-site/`, and the unit file contains neither. Running the playbook
again without a code is fine: it enrolls only where there is no token yet.

## DHCP is off unless you say so

`sheath_site_dhcp: true` makes the site machine the DHCP server for its
segment. A second DHCP server in a segment that already has one is not a
configuration detail, it is an outage, and this playbook cannot see from here
which of the two it would be. Turn it on when the segment belongs to Sheath.

Where it is on, the site also serves TFTP and the netboot switch: an unknown
blade may always netboot so it can enroll, a known one only when an
installation was asked for.

## Moving a site to another machine

A site is not its machine. Enroll the new one with a fresh code — the site
keeps its id, its blades and its history, and only the token changes — then
update `sheath_site_relay_url` and run the playbook again. Take the old machine
off the wire first if it serves DHCP: two DHCP servers in one segment is an
outage, not an overlap.

The netboot payload carries the site's address, and the site re-aims it when
that address changes. Blades already sitting in the mini OS are pointed at the
old one and stay there until they restart; blades that netboot afterwards find
the new address by themselves.

## What the playbook will not do for you

- **Give `/srv/sheath` its own room.** Images, the netboot payload and the
  binaries a blade installs all live there, and on a site that mirrors two
  images it is several gigabytes. The playbook creates the directory and
  nothing else: whether it sits on the OS card, on an NVMe or on a USB stick
  is a decision about the machine, and it belongs in that machine's `fstab`
  with `nofail`. A tired USB stick is a bad place for it — one lost its
  capacity mid-write, the filesystem went read-only, and the site went on
  answering every question while it could no longer cache a thing. It says so
  now, which is not the same as it not happening.

- **Build the netboot payload.** `tools/build-bootimg.sh` runs on an arm64
  machine and writes into the centre's TFTP root; the sites fetch it from
  there, checksums and all, and aim it at themselves.
- **Point `--base-url` at something the sites can reach.** The default is this
  machine's own address, which is right until it is not.
- **Decide the address plan.** `--net-base` is set once, on the very first
  start, and the interface owns it afterwards.
