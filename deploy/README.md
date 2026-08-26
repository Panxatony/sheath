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

## What the playbook will not do for you

- **Build the netboot payload.** `tools/build-bootimg.sh` runs on an arm64
  machine and writes into the centre's TFTP root; the sites fetch it from
  there, checksums and all, and aim it at themselves.
- **Point `--base-url` at something the sites can reach.** The default is this
  machine's own address, which is right until it is not.
- **Decide the address plan.** `--net-base` is set once, on the very first
  start, and the interface owns it afterwards.
