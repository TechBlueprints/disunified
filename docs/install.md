# Installing switch-to-unifi

Written for someone (or some agent) who has never seen this project. Every
step has a check. Total time for one switch: about 20 minutes plus one click
in the UniFi UI.

> **Experimental software, no warranty of any kind.** With `control` on,
> the bridge rewrites your switch's configuration (VLANs, port state,
> speeds, STP, LACP, reboots) to match the controller. Back up the
> switch's config, keep out-of-band access to it, and enable control for
> one unused port before all of them. Verified only on the hardware and
> versions listed in the README; see its "Status" section before step 1.

## 0. What you need

- A UniFi Network controller (tested: Network 10.6 on a UniFi OS gateway),
  reachable from where the bridge runs on TCP 8080 (inform) and 443 (API).
- A switch with a supported driver (`switch-to-unifi -list-drivers`) and an
  account on it. For control (not just monitoring) the account needs write
  access; for the Arista driver that is a `network-admin` user and eAPI
  enabled (`management api http-commands` → `no shutdown`); for the
  Proxmox driver, root SSH to each node with a key and `apt-get install
  lldpd` on each node (the driver configures it); `docs/proxmox.md` §5.
  Spanning tree on a node is optional (`mstpd`, `docs/proxmox.md` §4b);
  without it the driver claims no STP and ignores the controller's STP
  settings for that node.
- Optional, for naming the device and ports after the switch: a UniFi API
  key (UniFi OS → Settings → Control Plane → Integrations → Create API Key).

## 1. Get the binary

```sh
git clone https://github.com/TechBlueprints/switch-to-unifi && cd switch-to-unifi
go build ./cmd/switch-to-unifi           # Go 1.25+
```
or build the container: `podman build -t switch-to-unifi .`

## 2. Check the switch connection

```sh
export STU_SWITCH_USER=stu STU_SWITCH_PASS='...'
./switch-to-unifi -collect-once -switch-url https://192.0.2.3/command-api
```
**Check:** JSON with the switch's model, every port, and `suggested_model`.
If it fails, the error names the command or credential at fault; fix that
before going on. Pass `-driver <name>` for a non-Arista switch, e.g.
`-driver proxmox -switch-ssh root@proxmox-2`.

## 3. Write the config

Copy `deploy/config.example.yaml` to `config.yaml` and `deploy/env.example`
to `env`; fill in the controller address, the switch address, and the
environment variable names. Start with `control.ports: "off"` (read-only).

## 4. First run and adoption

```sh
set -a; . ./env; set +a
./switch-to-unifi -config config.yaml
```
**Check:** the log shows `switch: <vendor> <model> ... N ports`, then
`inform: HTTP 404 (pending, nothing queued)`. In the UniFi UI the switch
appears under **Pending Adoption**. Click **Adopt**. Within a minute the log
shows `authkey adopted` then `adoption handshake complete -> CONNECTED`, and
the device page shows live ports. The adopted key is now in
`state/<name>/device.json` — keep that file; one bridge per switch.

## 5. Turn on control

Set `control.ports: all` (and the other `control` flags you want) and
restart. **Check:** the log shows `reconciled ... 0 of N ports changed` (or
the changes it made to bring the switch in line with the controller). From
now on the UniFi UI is the switch's configuration: names, port state,
speed, VLANs, FEC, storm control, STP, aggregation, mirroring.

Things to know before you flip it: UniFi becomes the source of truth for
port config and VLAN membership on that switch; the switch's own VLAN list
is extended with the site's VLANs; with `igmp: true` snooping follows
UniFi's per-network setting (UniFi defaults it off). Read
`docs/feature-map.md` for what each feature maps to on your driver.

## 6. Run it for good

- **Podman/Docker compose:** `deploy/compose.yaml` (config and env beside it).
  On Podman use `restart: always` and enable `podman-restart.service`;
  `unless-stopped` containers do not come back after a host reboot.
  Reference install: a directory on a Podman host, built
  from a copy of the source tree (`build: ./src`), state in the named
  volume `switch-to-unifi-state`, running as uid 65532.
- **Quadlet/systemd:** `deploy/switch-to-unifi.container`.
- Mount a volume at `/var/lib/switch-to-unifi` (state and reply logs) and
  set `state_dir: /var/lib/switch-to-unifi` in the config.
- An SSH driver (Proxmox, or Arista over SSH) in the container needs a key
  and a known_hosts file: mount them read-only and name them in the
  switch's `options` (`ssh_key: /etc/switch-to-unifi/id_ed25519`,
  `known_hosts: /etc/switch-to-unifi/known_hosts`); the container has no
  home directory or agent. The key file must be readable by uid 65532.

**Check:** after a container restart the log says `resuming adopted state`
and the controller never showed the device as disconnected for more than
one inform interval.

## Troubleshooting

- Device never appears: the inform URL must contain an IP literal, and the
  controller must be reachable on 8080 from the bridge.
- Adopted then pending again: the state file was lost or two bridges share
  one MAC. Forget the device in the UI and adopt again.
- A UI setting has no effect: look for `UNSUPPORTED` / `NOTE` lines in the
  log; the feature map says what that driver cannot do.
- Every reply from the controller is in `inform-log/<name>/*.ndjson`; that
  is the first thing to read when something unexpected happens.
