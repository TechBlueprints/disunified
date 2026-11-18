# Installing disunified

Written for someone (or some agent) who has never seen this project. Every
step has a check. Total time for one device: about 20 minutes plus one click
in the UniFi UI.

> **Experimental software, no warranty of any kind.** With `control` on,
> the bridge rewrites your device's configuration (VLANs, port state,
> speeds, STP, LACP, reboots) to match the controller. Back up the
> device's config, keep out-of-band access to it, and enable control for
> one unused port before all of them. Verified only on the hardware and
> versions listed in the README; see its "Status" section before step 1.

## 0. What you need

- A UniFi Network controller (tested: Network 10.6 on a UniFi OS gateway),
  reachable from where the bridge runs on TCP 8080 (inform) and 443 (API).
- A device with a supported driver (`disunified -list-drivers`) and an
  account on it. For control (not just monitoring) the account needs write
  access; for the Arista driver that is a `network-admin` user that reaches
  enable mode without an enable password, and eAPI enabled
  (`management api http-commands` → `no shutdown`), or SSH with a key
  (then the FEC counters are not collected); for the
  Proxmox driver, root SSH to each node with a key, or the `.deb` on each
  node (§7b), and `apt-get install
  lldpd` on each node (the driver configures it); [`docs/drivers/proxmox.md`](drivers/proxmox.md) §5.
  Spanning tree on a node is optional (`mstpd`, [`docs/drivers/proxmox.md`](drivers/proxmox.md) §4b);
  without it the driver claims no STP and ignores the controller's STP
  settings for that node. For the APC PDU driver, an SNMPv1 read community,
  and — to switch outlets — a write community whose access type is `Write+`
  (plain `Write` makes the card answer every SET with silence); naming an
  outlet additionally needs the card's admin login, because the driver writes
  names as a `config.ini` upload over FTP ([`docs/drivers/apc-pdu.md`](drivers/apc-pdu.md) §2, §5).
  For the APC UPS driver, Modbus enabled on the unit's display (Configuration
  → Menu Type → Advanced, then Configuration → Modbus) and no credentials
  (`auth: none`); the port's MAC, netmask and gateway MAC go in `options`
  because Modbus carries no network identity ([`docs/drivers/apc-ups.md`](drivers/apc-ups.md) §2, §5).
  For the Podman driver, root SSH to the host with a key, `python3` on it,
  and -- if the bridge runs on that very host -- a leg on a Podman bridge
  network to reach it, because a macvlan container cannot talk to its own
  host ([`docs/drivers/podman.md`](drivers/podman.md) §4).
- Optional, for naming the device and ports after the device itself: a UniFi
  API key (UniFi OS → Settings → Control Plane → Integrations → Create API Key).

## 1. Get it

The release image is multi-arch (linux/amd64, linux/arm64) and needs nothing
else installed:

```sh
docker pull ghcr.io/techblueprints/disunified:latest   # or podman pull
```
`:latest` follows releases, `:v1.2.3` pins one, `:edge` follows `main`.
`docker run --rm ghcr.io/techblueprints/disunified:latest -build-version`
says which build you have.

Or build from source (Go 1.26+), which is also how you add a driver:

```sh
git clone https://github.com/TechBlueprints/disunified && cd disunified
go build ./cmd/disunified
```
The binaries from each release are attached to it on GitHub as well; they are
static, so `tar xzf` and run. So is a `.deb` for running the bridge on a
SONiC switch itself (§7; when to choose that over a container:
[`docs/drivers/sonic-running.md`](drivers/sonic-running.md)) or on a Proxmox
VE node itself (§7b).

Steps 2 and 4 below are written as `./disunified …`. With the image, that
is `docker run --rm …  ghcr.io/techblueprints/disunified:latest …`
(everything after the image name is the bridge's own flags); step 6 is the
container install proper.

## 2. Check the device connection

```sh
export DUI_DEVICE_USER=stu DUI_DEVICE_PASS='...'
./disunified -collect-once -device-url https://192.0.2.3/command-api
```
or, with the image:

```sh
docker run --rm -e DUI_DEVICE_USER -e DUI_DEVICE_PASS \
  ghcr.io/techblueprints/disunified:latest \
  -collect-once -device-url https://192.0.2.3/command-api
```
**Check:** JSON with the device's model, every port, and `suggested_model`.
If it fails, the error names the command or credential at fault; fix that
before going on. `-driver <name>` (or `DUI_DRIVER`) names the driver, e.g.
`-driver arista-eos -device-url https://…/command-api` or
`-driver proxmox -device-ssh root@proxmox-2`; a config file names it per device.
`-device-ssh-key` (or `DUI_DEVICE_SSH_KEY`) names the SSH key as a config
file's `options.ssh_key` does: a private key file, or a `.pub` that selects
that one ssh-agent identity (without it every agent key is tried in turn).
The pre-rename `-switch-url`/`-switch-ssh` flags and `STU_*` env names are
still read, so an existing deployment keeps working unchanged.

## 3. Write the config

Copy [`deploy/config.example.yaml`](../deploy/config.example.yaml) to `config.yaml` and [`deploy/env.example`](../deploy/env.example)
to `env`; fill in the controller address, the device address, and the
environment variable names. Devices go under `devices:` (the pre-rename
`switches:` key is still accepted). No `control` block is needed: UniFi
owns the device from adoption (§5). Set `control.ports: "off"` only if you
want to look around in the UI before the bridge writes any port.

`children:` on a device reports other devices below its ports, for a
device the controller should place there that no switch sees (a breakout
cage's lane switch, [`docs/breakout-lag.md`](breakout-lag.md) §6.4):

```yaml
    children:
      - {port: 54, mac: "02:00:00:00:00:a5", name: lane-switch}
```

The MAC goes into the port's MAC table (in the port's native VLAN) and the
switch's, and -- while the port has no LLDP neighbour of its own -- the
child is reported as the port's neighbour (chassis id = the MAC, system
name = `name`, default the MAC). A port that already has a neighbour keeps
it, with a log line. A child is never the device's uplink. Reported values
only: nothing is written to the device; the start log has one line per
child. `report: lldp` or `report: mac` on a child reports only that entry
(for telling which one the controller places by; unset = both).

A **lane switch** presents a split cage of another device as a five-port
switch of its own ([`docs/drivers/breakout.md`](drivers/breakout.md)): ports
1-4 are the cage's lanes, port 5 the breakout cable.

**It appears by itself.** When a cage of a `sonic` or `arista-eos` device
is split (a breakout cable on a port left on Auto, a forced lane speed, or
by hand), the bridge starts a lane switch for it within about 15 seconds,
named `<device>-cage<N>` (`core-1-cage3`), and it shows up in the
controller as a pending Flex XG. **Nothing changes on the switch until it
is adopted**: the cage stays one port on its device, as before. **You do
not have to adopt it**: once its device is adopted, the bridge asks the
controller to adopt the lane switch too, about 15 seconds after it appears
(by its exact MAC; this needs `controller.api_url` and the API key, and
without them it stays pending for you to adopt). Once adopted,
the lanes are the lane switch's ports (state, name, VLANs, lane speed, FEC,
aggregates), while the cage's port on the device keeps only its speed (the
split itself) and its admin state; the log says `lane switch core-1-cage3
(<MAC>) attached to port 3`. If the cage's port on the device held a forced
lane speed (25 GbE with a FEC, saved while the cage was one port), the
bridge sets that port to Auto in the controller at the adoption and says so:
the controller refuses that pair on the port once the lane switch has the
lanes, and with it every other port save on the device. The lane switch
shows a 169.254.x.y address; it has none of its own, and it must not share
its parent's. Forget it in the controller and the lanes go back to the
cage's port, and while the cage is split the bridge adopts it again
within a minute or two. To be rid of a lane switch, unplug the breakout
cable (or join the cage), then forget its disconnected record; or set
`lane_switches: "manual"` on the device, and you adopt each one yourself.
Join the cage (force its port to 100G or 40G on
the device) and the lane switch shows disconnected until the cage is split
again or you forget it. The key on the device's entry:

```yaml
    lane_switches: "off"    # default auto: a lane switch per split cage, adopted by the bridge;
                            # "manual": it stays pending until you adopt it; "off": none
```

Its state is kept under `state/<device>-cage<N>/` like any device's. It
needs a config file (the flag form runs one device and starts none), and
`-collect-once` starts none.

**It can also be declared**, which attaches it from the start whether or
not the controller has adopted it, lets you name it and pick its MAC, and
wins over the automatic one for that cage. It is an entry of the **same
file as its parent** (it runs in the parent's process and reads and writes
through the parent's driver; no `url`, `ssh` or credentials):

```yaml
  - name: core-1-cage2
    driver: breakout
    options:
      parent: core-1        # the entry whose cage this is (sonic or arista-eos)
      cage: "2"             # the parent's port index of the cage, 1..99
      # mac: "02:00:00:00:00:a5"   # default: derived from the parent's MAC
      # max_age: 3m                # how old the parent's snapshot may be
    # model: defaults to USFXG (the Flex XG)
```

At start the parent logs `lane switch core-1-cage2 attached to port 2 as
<MAC>`. While the cage is whole the lane switch does not start (its start
is retried with a log line); once the cage is split from the parent's port
settings it informs and is adopted like any device, placed under the cage.
From then on the cage's own settings on the parent are its speed (the
breakout) and its admin state only; the lanes take theirs from the lane
switch. One lane switch per cage, never on a port the parent's `children:`
names. `-collect-once -config` opens the entries one after another, so a
lane switch there reports that its parent is not open: it is a probe of
devices, and a lane switch is not one.

## 4. First run and adoption

```sh
set -a; . ./env; set +a
./disunified -config config.yaml
```
**Check:** the log shows `switch: <vendor> <model> ... N ports`, then
`inform: HTTP 404 (pending, nothing queued)`. In the UniFi UI the device
appears under **Pending Adoption**. Click **Adopt**. Within a minute the log
shows `authkey adopted` then `adoption handshake complete -> CONNECTED`, and
the device page shows live ports. The adopted key is now in
`state/<name>/device.json` — keep that file; one bridge per device.

## 5. UniFi in control

Unset, `control.ports` and `control.outlets` mean every port or outlet the
driver can write (`all`); `"off"` writes none, and a list such as `"2,5-8"`
tries a few first. Adoption takes the device as it is: with `api_url` and an
API key set, the bridge seeds the controller's port settings from the
device's live state, and the loop holds the first push while it would still
change a port's own settings or switch an outlet, until the controller
agrees with the device -- jumbo frames are a device-wide setting, like NTP
or the hostname, and follow the controller from the first push (every
switch driver plans its pushes; without `api_url`, set the
ports in UniFi to match, or `allow_initial_changes: true` to take UniFi's
defaults). **Check:** the log shows `reconciled ... 0 of N ports changed` (or
the changes it made to bring the device in line with the controller). From
now on the UniFi UI is the device's configuration: names, port state,
speed, VLANs, FEC, storm control, STP, aggregation, mirroring, and on a power
device the outlets (`control.outlets`).

With `api_url` and an API key set, a device claimed as anything but the USW
Leaf also takes the firmware updates the controller offers that model, with
no click: the bridge asks for the offered version a minute after it connects
and then checks hourly. Nothing is installed on your device. The update only
changes the version the UI shows, and the update badge goes away. A
`firmware:` you set in the config file switches this off for that device.

The device's own management address follows the controller's **IP
Settings** too, with no key, for every driver that can write it. A static
setting is applied; DHCP only when it replaces a static setting you chose,
never as the controller's default (so a fresh adoption moves nothing) — and
`arista-eos` and `podman` decline DHCP outright, because they cannot confirm
a lease (`podman` also applies a static address only if the host already
carries it: "make the lease permanent", never "move the host"). It is the
answer to a fact every adoption meets: adopting a device deletes the client
record it had, with its fixed-IP reservation and DNS name. It is also the
setting most likely to cut the bridge off from the device it manages: the
Arista makes the change inside a config session with a commit timer that
reverts unless the switch answers at the new address, but a device without
that safety net simply takes it, so have another way in before you change a
device's IP Settings in UniFi.

Two more UPS-only switches live in the same block: `control.outlets`
(switch outlet groups from the UI; off by default, an outlet is real load)
and `control.nut` (claim the NUT-server capability: the UI's **NUT Server**
switch then runs a NUT server in the bridge, which needs its port reachable --
`3493:3493` published, or the container on a macvlan network with its own
address; [`docs/drivers/apc-ups.md`](drivers/apc-ups.md) §12).
There is no per-setting opt-in for anything else UniFi configures on its
own devices: whenever the driver can apply it, ports written or not, the
controller's STP, IGMP snooping, NTP servers, time zone, remote logging host
(Settings → System), hostname, SSH keys, SNMP community and Restart (a real
reload) apply, and its lists **replace** the device's -- a server set on the device by hand is
removed, and remote logging switched off in UniFi leaves the device logging
nowhere, as a UniFi switch would. A driver applies what its device can take
and logs the rest ([`docs/feature-map.md`](feature-map.md)). On the APC
cards (`apc-pdu`, `apc-backups`) that is the syslog host, written into the
card's own syslog servers; `apc-ups` has none (the SmartConnect port has
no syslog client). Older configs' `address`, `igmp`, `ntp`, `syslog`,
`hostname`, `reboot`, `ssh_keys` and `snmp` keys are ignored.

Fan Mode is the one UniFi setting a switch cannot get from the controller,
which offers it only for a gateway. It is the device's own `fan_mode:
default | quiet` in `config.yaml` (unset leaves the fans alone), applied
each cycle where the driver can set it. What Quiet means differs by device.
On `sonic` (DX010) it is a lower floor that the fan control still raises.
On `arista-eos` it is EOS's fixed `environment fan-speed override 30`, and
the switch does not speed the fans up as it heats under it; only the
overheat shutdown is left
([`docs/drivers/arista-eos.md`](drivers/arista-eos.md) §8).

Things to know before you flip it: UniFi becomes the source of truth for
port config and VLAN membership on that device; the device's own VLAN list
is extended with the site's VLANs; IGMP snooping follows UniFi's
per-network setting (UniFi defaults it off, EOS on: the first push turns it
off on every network not enabled in UniFi). Read
[`docs/feature-map.md`](feature-map.md) for what each feature maps to on your driver.

## 6. Run it for good

- **Podman/Docker compose:** [`deploy/compose.yaml`](../deploy/compose.yaml) pulls the published
  image, so a directory holding that file, `config.yaml` and `env` (mode 600)
  is the whole install — its header has the three `curl` commands that fetch
  those files. `docker compose up -d`, or `podman-compose up -d`. Update with
  `docker compose pull && docker compose up -d --force-recreate`
  (without `--force-recreate` the old container keeps running). On Podman use
  `restart: always` and enable `podman-restart.service`; `unless-stopped`
  containers do not come back after a host reboot. The container runs as uid
  65532 and keeps state in the named volume.
- **Quadlet/systemd:** [`deploy/disunified.container`](../deploy/disunified.container); it sets
  `AutoUpdate=registry`, so `podman auto-update` (or its timer) picks up a new
  `:latest`. Pin `:v1.2.3` if you would rather update deliberately.
- Mount a volume at `/var/lib/disunified` (state and reply logs) and
  set `state_dir: /var/lib/disunified` in the config.
- An SSH driver (Proxmox, Podman, or Arista over SSH) in the container needs a key
  and a known_hosts file: mount them read-only and name them in the
  device's `options` (`ssh_key: /etc/disunified/id_ed25519`,
  `known_hosts: /etc/disunified/known_hosts`); the container has no
  home directory or agent. The key file must be readable by uid 65532.

**Check:** after a container restart the log says `resuming adopted state`
and the controller never showed the device as disconnected for more than
one inform interval.

## 7. On a SONiC switch

A SONiC switch can run its own bridge, as a systemd service on the switch,
instead of being reached over SSH from somewhere behind its own links
(the design: [`docs/drivers/sonic-on-switch.md`](drivers/sonic-on-switch.md)).
The bridge then runs as the login user (`admin`), executes the driver's
commands locally (`options.transport: local`) and keeps its config, API key
and state under `/host/disunified`, which survives an image install.

1. **Get the deb.** From a release: `disunified_<version>_amd64.deb` (the
   DX010 and most SONiC switches are x86_64; `arm64` for an Arm one). Or
   build it: `scripts/build-deb.sh amd64` writes `dist/disunified_*.deb`;
   on a host without `dpkg-deb` (a Mac) it stages the tree and prints the
   one `dpkg-deb` command to run on the switch, which has it.
2. **Install it** on the switch, and keep the file beside the config:

   ```sh
   scp disunified_<version>_amd64.deb admin@192.0.2.10:
   ssh admin@192.0.2.10
   sudo dpkg -i disunified_<version>_amd64.deb
   cp disunified_<version>_amd64.deb /host/disunified/
   ```

   The package installs `/usr/sbin/disunified` and the unit, enabled but not
   started (there is no config yet). On SONiC (`/host/machine.conf`) its
   postinst also makes `/host/disunified` (owner `admin`, mode 0700), links
   `/etc/disunified` to it and adds a drop-in that runs the service as
   `admin`. Elsewhere (a Proxmox node, §7b) the service runs as root and
   the postinst makes `/etc/disunified` and `/var/lib/disunified` (root,
   0700).
3. **Write the two files** in `/host/disunified`, as `admin`:
   `config.yaml` (the sonic entry below; `state_dir: /host/disunified`) and
   `env` (mode 0600) with `DUI_UNIFI_API_KEY=…` and nothing else.

   ```yaml
   controller:
     host: 192.0.2.1
     api_url: https://unifi.example.net
     api_key_env: DUI_UNIFI_API_KEY
     site: default

   state_dir: /host/disunified

   devices:
     - name: dx010
       driver: sonic
       ssh: admin@192.0.2.10      # the user the service runs as, and the switch's in-band address
       model: USWF07D
       options:
         transport: local         # exec on this switch; no key, no known_hosts
         # probe: 192.0.2.1:8080  # default: the controller's inform host:port
   ```

   `ssh:` must name the user the service runs as (`admin`), or the driver
   refuses to start. `options.probe` is the address the switch must keep
   reaching: with nothing to dial, an address move is proven by a TCP
   connection to it from the new address, and the first spanning-tree
   enable by one from anywhere. The bridge fills it from the controller's
   inform address; set it only to point elsewhere.
4. **Check it before it informs:** as `admin`,
   `disunified -config /host/disunified/config.yaml -collect-once` prints
   the switch's snapshot (ports, VLANs, addresses -- the configured one
   first) and sends nothing to the controller.
5. **Start it:** `sudo systemctl start disunified`, then
   `journalctl -u disunified -f`. A device already adopted from another
   bridge keeps its key when its `state/<name>/device.json` is copied into
   `/host/disunified/state/<name>/` first (and that bridge is stopped: one
   running bridge per adopted key).

**Check:** `systemctl is-active disunified` says `active`, the log says
`transport local: commands run on this switch as admin` and, for an adopted
device, `resuming adopted state`; the controller shows the switch connected.
Read the log with `sudo journalctl -u disunified -t disunified`: every
`sudo` the driver runs logs its own session lines into the unit's journal,
and `-t disunified` keeps only the bridge's.

**Image installs.** Only `/host` and `/etc/sonic` survive a SONiC image
install; the package itself is in the image's overlay and does not. SONiC
installs every deb found in the new image's `platform/<platform>/`
directory on its first boot (`rc.local`), so an upgrade that copies
`/host/disunified/disunified_*.deb` there before rebooting brings the
bridge back with its config and state (`sonic-on-switch.md` §5). Keep
exactly one deb in `/host/disunified`: the one installed. Updating the
bridge is `sudo dpkg -i` of the new deb and replacing that file.

## 7b. On a Proxmox node

A Proxmox VE node can run its own bridge, as a root systemd service on the
node, instead of being reached as `root@node` over SSH from a container
elsewhere (the design:
[`docs/drivers/proxmox-on-node.md`](drivers/proxmox-on-node.md)). Each node
runs one bridge whose config names that node only; the bridges share
nothing but pmxcfs (`/etc/pve`), where the guests' port numbers already
live in their tags ([`docs/drivers/proxmox.md`](drivers/proxmox.md) §1c).
The driver runs its commands on the node (`options.transport: local`), so
no host holds a root key to the nodes. Not yet verified live.

1. **Get the deb.** From a release: `disunified_<version>_amd64.deb`. Or
   build it as in §7 step 1; Proxmox VE has `dpkg-deb`, so a tree staged on
   a Mac can be packed on the node.
2. **Install it** as root on the node:

   ```sh
   scp disunified_<version>_amd64.deb root@192.0.2.11:
   ssh root@192.0.2.11 dpkg -i disunified_<version>_amd64.deb
   ```

   The package installs `/usr/sbin/disunified` and the unit, enabled but
   not started (there is no config yet), and makes `/etc/disunified` and
   `/var/lib/disunified` (root, mode 0700). The unit starts after
   `pve-cluster.service`; a collect that runs before `/etc/pve` is mounted
   fails and is retried, so nothing is provisioned from an empty guest list
   (`proxmox-on-node.md` §4.1).
3. **Write the two files** in `/etc/disunified`: `config.yaml` and `env`
   (mode 0600) with `DUI_UNIFI_API_KEY=…` and nothing else.

   ```yaml
   controller:
     host: 192.0.2.1                # the controller, an IP literal
     api_url: https://192.0.2.1     # its host must resolve on the node
     api_key_env: DUI_UNIFI_API_KEY
     site: default

   state_dir: /var/lib/disunified

   devices:
     - name: pve-1                  # as adopted; names the state directory
       driver: proxmox
       ip: 192.0.2.11               # this node's in-band address
       model: UDC48X6
       options:
         transport: local           # exec on this node as root; no ssh:, no key
         ports: "54"
       reply_log: {max_size: 8MB, keep: 3}
   ```

   `ssh:` is not needed; if it is set, it must name `root`, the user the
   service runs as, or the driver refuses to start. `ssh_key` and
   `known_hosts` are ignored. `ports`, `numbering` and `model` (and
   `guest_ports` where it is set) must be the same in every node's file:
   the nodes number their guests from the same tags
   (`proxmox-on-node.md` §3).
4. **Check it before it informs:**
   `disunified -config /etc/disunified/config.yaml -collect-once` prints
   the node's snapshot and sends nothing to the controller. It runs the
   driver's start, so on a node no other bridge serves it writes what the
   service would (guest port tags, lldpd's file).
5. **Start it:** `systemctl start disunified`, then
   `journalctl -u disunified -f`.

**Check:** `systemctl is-active disunified` says `active`, the log says
`transport local: commands run on this node as root` and, for an adopted
device, `resuming adopted state`; the controller shows the node connected.

**A node that is already adopted** through another bridge moves with its
state file and is not re-adopted, but only one bridge may hold its key at
any moment. The order -- the checks on the node before the running bridge
is touched, removing the node from that bridge, copying its state and reply
log, the first start, and the way back -- is
[`proxmox-on-node.md`](drivers/proxmox-on-node.md) §7.

**Updates.** `dpkg -i` of the new deb on a node restarts that node's bridge
only; update one node at a time. A Proxmox upgrade keeps the package,
`/etc/disunified` and `/var/lib/disunified`.

## 8. What the bridge keeps on disk, and the controller's secrets

The controller's pushes carry secrets: the adoption key, the device
login's password hash, a RADIUS server's shared secret, SNMP communities,
OSPF keys, a NUT login, and an uploaded BGP file that can hold neighbour
passwords. The bridge needs each one in memory to apply it; what it writes
keeps them out wherever it can, by one table of keys
([`internal/unificfg/secrets.go`](../internal/unificfg/secrets.go)).

| File | What it holds | Secrets | Mode |
|---|---|---|---|
| `state/<name>/device.json` | the adopted key, the last applied push and a pending one | **kept as they came**: a restart applies the stored push again and encrypts its informs with the key, so a placeholder there would be written to the device | 0600 |
| `inform-log/<name>/<mac>.ndjson` (and `.1` ...) | every reply, and what the bridge did with it | **redacted** | 0600, directory 0700 |
| `inform-log/<name>/payload-last.json` | the last inform sent | its `x_authkey` redacted | 0600 |
| `state/<name>/sonic-managed.json` (sonic) | what the driver owns on the switch | the SNMP community it set (it must remove it later) | 0600 |
| the log (stderr, the journal) | | every secret value of the current or the stored push masked as `<redacted>` | |

The secrets that reach a device are the device's own business: the sonic
driver keeps the RADIUS secrets in a root-only file on the switch and
OSPF's keys in FRR's 0600 file.

**Redacted, by key** (a value of 32 hex digits becomes 32 zeros, any other
non-empty value `<redacted>`, an empty one stays empty; every line stays):

- `mgmt_cfg` `authkey`; `system_cfg` `unifi.key`, `switch.wevent.key`,
  `users.N.password`, `switch.radius.*.N.secret`,
  `switch.snmp.community.*.name`, `switch.routing_interface.N.ospf.password`
  and `...ospf.md5_keys.N.password`, `nutserver.password`;
- `switch.bgp.N.raw_config_base64`: the file stays, each `password`,
  `authentication-key`, `md5` or `key-string` line loses the rest of the
  line;
- any key not captured yet whose last part is `password`, `passphrase`,
  `secret`, `psk`, `authkey` or `key` (or ends `_password`, `_secret`,
  `_key`): SNMPv3 credentials and the like;
- in a reply's JSON: `authkey`, `x_authkey`, `password`, `x_password`,
  `secret`, `x_secret`, `credential`, the TURN `username` of
  `build-ssh-session` (and a real device's `_authkey`, `guest_token`).

Not secret, so kept: the controller's SSH **public** keys, `sshd.auth.passwd`
(a flag), user names, the RADIUS server's address.

A redacted log still replays: each redacted value is the same placeholder
in every push, so it is never a change of its own
(`TestReplayControllerRepliesAristaRedacted`, `TestReplaySonicDX010Dot1XRedacted`).
There is no switch to turn redaction off. A capture for a fixture is
scrubbed anyway (`scripts/sanitize-controller.py` replaces the same keys),
so the redacted log is the capture source as before. If a new feature
needs to see a real secret's form, read that one key from
`state/<name>/device.json` on the device itself (`system_cfg` holds the
last applied push), and copy nothing off it.

**Logs written before this** (bridges before 2026-11-15) hold the secrets
as they came, in files 0644. A new build narrows the live file to 0600 when
it opens it, and the directory it creates is 0700; it does not rewrite old
lines or touch the rotated generations. They age out through rotation
(`reply_log.max_size` x `reply_log.keep`, 32 MB by default per device). To
be rid of them at once, stop the bridge, remove `inform-log/<name>/*.ndjson*`
(or keep a copy only where secrets may live), and start it again; nothing
the bridge needs is in them (the adopted state is `device.json`).

## Troubleshooting

- Device never appears: the inform URL must contain an IP literal, and the
  controller must be reachable on 8080 from the bridge.
- Adopted then pending again: the state file was lost or two bridges share
  one MAC. Forget the device in the UI and adopt again.
- A device shows **Disconnected** in UniFi while the bridge is running: the
  bridge could not read the device five times in a row (`collect from
  switch (N in a row): …` in the log) and has stopped informing on purpose,
  so the controller shows the truth rather than frozen numbers; it resumes
  the moment a read succeeds (`collect … recovered`). Fix whatever the log
  names -- credentials, a session limit, the device's address.
- A UI setting has no effect: look for `UNSUPPORTED` / `NOTE` lines in the
  log; the feature map says what that driver cannot do.
- Every reply from the controller is in `inform-log/<name>/*.ndjson`; that
  is the first thing to read when something unexpected happens. The file is
  capped (a device's `reply_log.max_size`, default `8MB`): past the cap it
  moves to `<mac>.ndjson.1`, older ones to `.2`, `.3` (`reply_log.keep`,
  default 3, the oldest dropped), always between two whole lines, and a
  fresh file starts. `max_size: off` lets it grow as before. Pushed
  secrets are redacted in it (§8).
- The controller's own log is the second: on a UniFi OS console,
  `/usr/lib/unifi/logs/server.log` (root over SSH), lines shaped
  `[<time>] <thread> LEVEL  category - message`. `grep -a "> ERROR "` and
  `grep -a "> WARN " | grep -i <device MAC>` find what concerns a bridged
  device; a rejected REST save shows up as `sanitize - Unsupported format
  exists in Device payload={...}` with the exact body the UI sent, which is
  how the UPS `nut_server: null` failure was pinned down. Ignore the flood
  of `PoE validation failed for supervisor …` / `PoE port validation
  failed for device …` lines: that is the controller's power-supervision
  check complaining that non-PoE devices are not drawing PoE, for real
  devices as much as bridged ones.
- The device lost its reserved address / DNS name after adoption: expected
  (the client record went with the adoption). Use the device's own IP
  Settings (applied when static), or make the address static on the device,
  and recreate the name as a static DNS entry (Settings → Routing → DNS):
  the gateway publishes no name for an adopted device, whatever its
  `hostname` or `name`.
- A container on a macvlan network cannot reach its own host (kernel rule):
  give it a second leg on a Podman bridge network and target the host there.
