# SONiC driver -- plan (hardware on order, nothing built yet)

Status 2026-10-27: a Celestica Seastone DX010 (32x QSFP28, Broadcom
BCM56960 Tomahawk, Intel Atom C2538, ONIE) is ordered and some weeks out.
This file is the plan for building `internal/drivers/sonic` before it
arrives, on a SONiC virtual switch, and for what happens the day it lands.
When the driver exists this file becomes its write-up, like the others in
this directory.

The driver targets **community SONiC**, not the DX010: SONiC is Debian
with the switch state in Redis, and every SONiC switch exposes the same
databases and `config` CLI. The DX010 is the first unit it will run on.

## 0. The device

| | |
|---|---|
| Ports | 32x QSFP28; each cage breaks out 4x25G, 2x50G, 4x10G (SONiC hardware SKUs `Seastone-DX010`, `Celestica-DX010-C32`, `-D48C8`, `-25-50`, `-10-50`, …) |
| ASIC | Broadcom BCM56960 Tomahawk, 3.2 Tbps |
| CPU | Intel Atom C2538 "Rangeley", 8 GB |
| Management | RJ45 OOB Ethernet (upper jack), RJ45 serial console (lower jack, **9600 baud** on SONiC; the Celestica manual says 115200), USB |
| Power | 2x Delta 800 W; 150--200 W in use |
| Cooling | 5 hot-swap 40 mm counter-rotating Nidec modules, 68 dBA each at full speed; SONiC runs them at 35 % duty below 26 °C board temperature, 100 % at 45 °C. Loud. |
| Airflow | two chassis variants; the serial number carries `F2B` or `B2F` after `DX010`, fan FRUs are `F302-R0872` (exhaust) / `F301-R0872` (intake), PSUs `DPS-800AB-16` / `-25` |
| UniFi model | **`USWF07D`, "ECS Core", 32x QSFP28** -- the one catalogue entry whose layout is exactly this switch ([`docs/unifi-models.md`](../unifi-models.md)). The Proxmox nodes wore it first; its records carry the `oob_port_config` quirk already handled in [`internal/unifiapi`](../../internal/unifiapi), and it has real firmware in the controller so update prompts are expected and emulated. |

**AVR54.** The Atom C2000 B0 stepping has a Low Pin Count clock output
that degrades with operating hours; when it fails the system no longer
boots (Intel C2000 Specification Update, erratum AVR54, fixed in C0,
April 2017). A running system keeps running; the failure appears at the
next boot. A 2016 DX010 predates C0. Check on arrival, from SONiC:

```bash
sudo setpci -s 00:00.0 08.B
```

`02` is B0, `03` is C0 (PCI revision ID, bus 0 device 0 offset 8; the
CPUID stepping field reads 8 on both and cannot tell them apart). A B0
unit is a return-or-accept decision inside the seller's return window;
the mitigation if kept is a 100--130 ohm pull-up on the LPC clock line
under the heatsink, documented by ServeTheHome forum members for this
board, plus a UPS so it rarely reboots.

## 1. Images: what to install and where it comes from

The community does not publish tagged release downloads. The Azure build
pipeline (definition 138 for Broadcom, the `vs` pipeline for the virtual
switch) serves the **latest successful build of a branch** through one
anonymous redirect, verified downloadable from a Mac with `curl` on
2026-10-24 (HTTP 200, `Content-Disposition` with the filename; the server
refuses range requests, so one uninterrupted download):

| Artifact | URL (`<branch>` = `202505`, `202411`, `master`) | Size |
|---|---|---|
| Switch image, Broadcom ASICs (the DX010) | `https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=<branch>&platform=broadcom&target=target%2Fsonic-broadcom.bin` | ~950 MB |
| Virtual switch, KVM disk image | `…?branchName=<branch>&platform=vs&target=target%2Fsonic-vs.img.gz` | ~1.9 GB |
| Virtual switch, container image tarball | `…?branchName=<branch>&platform=vs&target=target%2Fdocker-sonic-vs.gz` | ~230 MB |

Rules:

- **Target branch `202505`.** The DX010 platform (`x86_64-cel_seastone-r0`)
  is on `202411`, `202505` and master; master moved to kernel 6.12 and
  Debian trixie on 2025-10-29 and a `202511` branch exists, but `202505` is
  the newest branch with a year of fixes behind it. The floor is `202305`:
  the DX010's fan, PSU, EEPROM and temperature commands were all broken on
  `202111`/`202205` (sonic-buildimage issues 11316--11321, fixed January
  2023 by PR 12567) and PSU fan speed was fixed May 2024 (PR 18787).
- **The redirect is rolling.** Record the build id that `show version`
  prints after installing and keep the installed file:
  `images/sonic/<branch>/<build-id>/sonic-broadcom.bin` plus its
  `sha256`, on the deployment host next to the stacks (the exact path is in
  `local-information/site.md`). The driver is pinned to that build the
  way `arista-eos` is pinned to EOS 4.26.14M; the fixtures directory is
  `docs/fixtures/sonic-<build-id>/`.
- **Never discard the factory image.** The seller's own photos show the
  unit shipping with `SONiC.20181130.77` (Debian 9, kernel 4.9, built
  2020-05-13). SONiC keeps two images; `sonic-installer install` adds one
  and leaves the other bootable. Do not run `sonic-installer cleanup`.
  That build cannot be downloaded again.
- The Broadcom SAI is inside the image; nothing to license or build.

## 2. Phase A -- before the hardware arrives: build on a virtual switch

**Recommendation: the KVM image as a Proxmox guest.** It is the real
SONiC stack -- the same Redis databases, `show`/`config` CLI,
`sonic-installer` dual-image mechanism and GRUB -- on a software ASIC. Its
default hardware SKU is `Force10-S6000`, 32 front-panel ports
(`Ethernet0`…`Ethernet124`, four lanes each), the same shape as the DX010.
The container image (`docker-sonic-vs`) is the same stack without the
installer or a kernel; it would do for a CI job later, not for the first
build.

What the virtual switch proves and what it cannot:

| Proves on the VS | Hardware only (Phase B) |
|---|---|
| Every Redis table the read side parses: ports, admin/oper state, speed, FEC, MTU, lanes, counters, LLDP, FDB, VLANs, port channels, device metadata | fans, PSUs, temperatures, system EEPROM (`pmon` is empty on the VS) |
| the one-script-per-poll SSH collector and its `FixtureRunner` | transceiver info and DOM (no optics) |
| every `config` write the driver makes, and `config save` | real speeds, FEC modes, Tomahawk breakout, the exact `platform.json` breakout modes |
| the controller round trip: adoption as `USWF07D`, `port_table`, `speed_caps`, LLDP, placement, port and VLAN pushes | link state from real cables |
| **the firmware upgrade procedure itself** (install a second VS image with `sonic-installer`, `list`, `set-default`, roll back) | ONIE fallback |

### A0. Stand up the VS guest

1. Download `sonic-vs.img.gz` for `202505` (table above) onto a Proxmox
   node, `gunzip`, `qemu-img convert -O qcow2`.
2. `qm create` with 4 vCPU, 8 GB (the VS swaps below 4 GB), a serial
   console (`-serial0 socket`), NIC 1 on `vmbr0` for management (`eth0`,
   DHCP). Add further VirtIO NICs as needed: VM NIC N+1 is front-panel
   `Ethernet<4N>`; a port's link comes up only when its NIC exists and is
   up, so two or three extra NICs give linked ports with real traffic and
   LLDP neighbours behind them. `qm importdisk`, attach as the boot disk.
3. Boot, log in `admin` / `YourPaSsWoRd` (the image default; change it),
   `show version`, `show platform summary`, `show interfaces status`.
4. Switch the default config from the router preset (every port a /31 to
   a fictional Arista fabric) to L2: `sudo sonic-cfggen --preset l2 -p -H
   | sudo tee /etc/sonic/config_db.json`, `sudo config reload -y`. This is
   also how the real DX010 will be set up.
5. Put the bridge's SSH key on the `admin` account (the Proxmox and Podman
   drivers' pattern: [`internal/sshrun`](../../internal/sshrun)).

The guest is a Proxmox port, so it also appears, truthfully, as a client
behind its node in the controller -- the Podman host precedent.

### A1. Capture first

One `collect.sh` run over SSH per poll, printing tagged sections, reduced
on the host to the fields the driver reads (the Podman rule: never ship a
raw dump that may carry secrets). Sources, all `sonic-db-cli` or `show …
--json` where it exists:

| Section | Source |
|---|---|
| identity | `show version` (SONiC build, platform, HwSKU, ASIC, serial), `CONFIG_DB DEVICE_METADATA|localhost` (hostname, mac, hwsku, platform) |
| ports | `CONFIG_DB PORT|*` (index, lanes, alias, speed, fec, mtu, admin), `APPL_DB PORT_TABLE:*` (oper_status, speed), `STATE_DB PORT_TABLE|*` |
| counters | `COUNTERS_DB COUNTERS_PORT_NAME_MAP` then `COUNTERS:<oid>` (SAI port stats incl. FEC codeword counters), `RATES:` for rates |
| optics | `STATE_DB TRANSCEIVER_INFO|*`, `TRANSCEIVER_DOM_SENSOR|*` (hardware only) |
| LLDP | `APPL_DB LLDP_ENTRY_TABLE:*` (chassis id, port id, system name, mgmt address) |
| MAC table | `fdbshow` / `show mac` (ASIC_DB FDB entries) |
| VLANs, LAGs | `CONFIG_DB VLAN|*`, `VLAN_MEMBER|*`, `PORTCHANNEL|*`, `PORTCHANNEL_MEMBER|*`; `STATE_DB LAG_TABLE` |
| platform | `STATE_DB FAN_INFO|*`, `PSU_INFO|*`, `TEMPERATURE_INFO|*`, `CHASSIS_INFO` (hardware only) |
| STP | `show spanning_tree` **if** the image carries `sonic-stp` (PVST); verify on the VS, expect thin |
| address | `CONFIG_DB MGMT_INTERFACE|eth0|*`, `MGMT_PORT`, `VLAN_INTERFACE|*`, `INTERFACE|*` |

Scrub with a new `scripts/sanitize-sonic.py` (MACs, IPs, serials,
hostnames, descriptions, optic serials); `scripts/check-site-info.sh
--staged` before every commit. Fixtures under `docs/fixtures/sonic-<build-id>/`.

### A2. Read side

`internal/drivers/sonic`: `Driver` registered as `sonic`, an `sshrun.Runner`,
a `FixtureRunner` serving the captures. Port index = `CONFIG_DB PORT` `index`
(1..32), name `Ethernet<n>`, alias `Eth<i>` as the vendor name; four lanes
fold into one cage, split cages set `LanesDiverge`; media QSFP28; speed caps
from the hardware SKU's `platform.json` breakout modes (hardware) with a
sane default on the VS; `Port.Health` from the SAI counters; LLDP, FDB,
VLANs, LAGs, STP as available. `System`: hostname, platform, HwSKU,
build id as the firmware version, uptime, management interface as the
OOB interface in `System.OOBInterfaces` (the loop warns if it carries the
bridge's address), fans/PSUs/temperatures when present. Tests cover
parsing, idempotence and the wire-contract/replay suites.

### A3. Adopt the VS

Model `USWF07D`. The VS has a real VM MAC, so it is placed behind its
Proxmox node's port the way the Podman host is. Decide the address before
adopting (adoption deletes the client record and any reservation, every
driver has hit this): static on the guest first, then `control.address`.
Walk the UI: port table, speed pickers, LLDP neighbours behind the linked
ports, topology. Forget the device when done, or keep it as the lab unit.

### A4. Control

Writes are diffs against `CONFIG_DB`, idempotent, followed by `config
save -y`:

| Feature | SONiC |
|---|---|
| enable/disable | `config interface startup/shutdown <port>` |
| name | `config interface description` |
| speed, FEC | `config interface speed`, `config interface fec` (`rs`/`fc`/`none`; never auto on an optic) |
| VLANs | `config vlan add`, `config vlan member add [-u]`; a port's native VLAN is its untagged membership |
| breakout | `config interface breakout <port> <mode>` (dynamic port breakout; needs the hardware SKU's `platform.json`) |
| LAG | `config portchannel add/member add` |
| mirroring | `config mirror_session` (SPAN) |
| STP, BPDU guard | `config spanning_tree …` only if `sonic-stp` is in the image |
| address | `config interface ip add/remove` on the in-band VLAN interface, with the `AddressController` guards (apply only when the new address is verifiably reachable; refuse DHCP) |
| refused with a log | IGMP snooping, storm control, isolation, 802.1X, egress rate limit -- not in community SONiC; same treatment as the Arista's refusals |

Rehearse the upgrade here too: install a second VS build with
`sonic-installer install`, reboot into it, `sonic-installer list`,
`set-default` back, so the Phase B commands have been typed once already.

## 3. Phase B -- the day it arrives

Do B1--B3 on the bench, within the seller's return window, before anything
goes in the rack.

**B1. Inspect.** Serial console (lower RJ45, 9600 baud), OOB management
jack on the lab VLAN, power. Record: the serial number and its airflow
code (`show platform syseeprom`, or `decode-syseeprom`; also on the label),
the CPU stepping (`setpci` above), `show version` (expect
`SONiC.20181130.77`), `show platform summary`, fan direction from `show
platform fan`, idle power from the PDU outlet it is plugged into. Decide
keep or return on stepping and airflow.

**B2. Preserve the factory state.** `sonic-installer list` (note the
installed image name), `config save -y` and copy `/etc/sonic/config_db.json`
off the box, copy `/host/machine.conf` (ONIE version, platform string).
The factory image stays in its slot after the upgrade.

**B3. Upgrade to the pinned 202505 build.**

1. Serve the kept image over plain HTTP from the deployment host
   (`python3 -m http.server` in the images directory) or the Mac.
2. On the switch: `sudo sonic-installer install http://<host>/sonic-broadcom.bin`,
   then `sudo reboot`. The 2018 installer may refuse a 2025 image (image
   format and installer changed over seven years). Then the ONIE path:
   reboot, pick "ONIE: Install OS" in GRUB, in the ONIE shell `ifconfig eth0
   <addr> netmask <mask>`, `ip route add default via <gw>`, `onie-nos-install
   http://<host>/sonic-broadcom.bin`. ONIE wipes the switch; the factory
   image is then gone from the box, which is why B2 comes first.
3. After boot: `show version` (record the build id and pin it in this
   file and the driver's doc comment), `show platform summary` (platform
   `x86_64-cel_seastone-r0`, HwSKU `Seastone-DX010` or
   `Celestica-DX010-C32`), `show platform fan`, `psustatus`,
   `temperature`, `syseeprom` -- all must answer, that is the point of the
   floor; `show interfaces status` shows 32 ports at 100G.
4. First-boot setup: change the `admin` password, add the bridge's SSH
   key, hostname, the L2 preset (A0 step 4), a VLAN interface on the
   uplink port for the bridge's in-band address, management `eth0` left
   for rescue only.

**B4. Recapture on hardware.** Re-run A1 on the real unit: the platform
tables, transceivers with a real DAC in a cage, `platform.json` breakout
modes, FEC counters at 100G. Replace the VS fixtures for everything in
the "hardware only" column; keep the VS fixtures for the rest. Pin the
build id.

**B5. Install.** Rack it on a metered PDU outlet; a 100G DAC or AOC from a
QSFP28 cage to the upstream aggregation switch (which has QSFP28 cages) or
to the Arista's spare cage; nothing on the OOB jack but the console cable.
Check UPS headroom first: the unit adds 150--200 W.

**B6. Prove it live**, in the order of [`docs/adding-a-device.md`](../adding-a-device.md) §4:
`-collect-once`; read-only adoption with the address decided first;
`-control-ports <one empty cage>` for disable/enable, name, speed, VLANs,
then `all` with a zero-change reconcile; record each push as a scrubbed
`system_cfg-*.txt`; update [`docs/feature-map.md`](../feature-map.md) and
[`docs/support-matrix.md`](../support-matrix.md). Deploy into the one
container on the Podman host like the others.

## 4. Phase A log

**A0 done 2026-10-27.** The VS guest runs on a Proxmox node (guest id,
addresses and MACs in `local-information/site.md`): image
`SONiC.202505.1238524-ff3c0c9f6` (Debian 12.14, kernel 6.1), 4 vCPU,
8 GB, disk on the cluster's NFS store, serial console socket, first
login over the serial socket with the image default, the Mac's key
installed, hostname set, factory config kept beside the live one. Found
along the way:

- **The VS image carries the DX010's hardware SKUs** (`Seastone-DX010`,
  `Celestica-DX010-C32`, the `-D48C8`/`-25-50`/`-50`… variants, with
  `lanemap.ini` for the VS), so the guest runs as `Seastone-DX010`: 32 ports
  `Ethernet0`…`Ethernet124`, aliases `Eth1`…`Eth32`, 100G, the DX010's lane
  numbers. Generated with `sonic-cfggen -H -k Seastone-DX010 --preset l2 -p`,
  installed as `config_db.json`, `config reload -y`. The reload drops the
  hostname; set it again and `config save -y`.
- **No `platform.json` on the VS platform** (`x86_64-kvm_x86_64-r0`), so
  dynamic port breakout is hardware-only, as expected.
- **`config spanning-tree` and `show spanning-tree` exist** in the 202505
  image. Whether to report STP is still a Phase A1 decision.
- The factory config is the "t1" router preset: every front port a /31
  from the bottom of the RFC 1918 10/8 space, BGP peers named
  `ARISTA01T2`…. Those overlap a home LAN on the same range, so no
  front-panel NIC may touch the LAN bridge until the L2 preset is in. With the L2 preset in, one front-panel NIC on the
  LAN bridge is loop-free; a second one on the same bridge is a Layer 2
  loop (no STP on either side by default).
- A front-panel port binds to its VM NIC when `syncd` starts: add the NIC,
  then reboot the guest (hot-plug alone leaves the port down). On the VS
  `show interfaces status` reports a nonsense speed (`4294967.3G`) for a
  linked port -- do not parse the CLI, read the Redis tables.
- `pmon` and `lldp` start about a minute after boot; `show lldp table`
  stays empty because a Linux bridge does not forward LLDP frames and the
  nodes announce on their bond only. LLDP neighbours need a second guest on
  the same bridge, or the hardware.
- The default `admin` password is the image's; change it before the guest
  stays on a reachable network.

**In-band management done 2026-10-27.** The guest now carries its
bridge-facing address on `Vlan<native_vlan>` (the Default network on the
wire, with a saved `STATIC_ROUTE` default via the gateway) and the
management NIC's link is held down on the hypervisor, so the guest has
exactly what the hardware will have: an in-band address on the system MAC
and a console. The old DHCP address lingers on the down `eth0` until a
reboot and shows in `OOBInterfaces` with `Up: false`. Adoption deletes a
device's client record and reservation, so the address was made static
on the device first, as `docs/adding-a-device.md` §4 says.

**A3 done 2026-10-27: the VS guest is adopted.** Read-only run from the
Mac against the in-band address, model `USWF07D`; `cmd/devmgr adopt` on
the system MAC; `mgmt_cfg` → ADOPTING → first `system_cfg` → CONNECTED in
about a minute; the provisioner named the device and its 32 ports after
the switch (`Eth1`…); the pushed config was accepted without applying.
**Placement took two inform cycles**: the controller set the uplink to the
Proxmox node's port that carries the guest's front-panel NIC, because that
is where the system MAC is seen -- the in-band address on
`Vlan<native_vlan>` is what makes the device placeable
([`docs/topology-placement.md`](../topology-placement.md)). The guest's
own NIC MACs remain as ordinary client records on the node's ports; the
dead management port's stale address keeps the loop's OOB warning alive
until the guest reboots. First-party UI walk-through of the device page
is still to do.

**Still open**

- Whether `sonic-installer` on `20181130.77` accepts a `202505` image, or
  B3 goes straight to ONIE. The VS rehearsal cannot answer this; only the
  unit can.
- Whether the guest stays as a permanent lab switch after the DX010 is
  adopted.

## 5. Driver specification (A1 + A2, read side) -- finalised 2026-10-27

Investigated on the VS guest (`202505.1238524`, SKU `Seastone-DX010`, L2
preset). This is the contract the implementation follows; the Podman
driver ([`internal/drivers/podman`](../../internal/drivers/podman)) is the
pattern for everything not said here.

### 5.1 Transport and collector

- `internal/drivers/sonic`, registered as `sonic`. Config: `ssh:
  admin@<host>` (key auth), options `ssh_key`, `known_hosts` as podman.
- **One SSH exec per poll: `python3 -` with `collect.py` on stdin**
  (embedded with `go:embed`). The script runs as the SSH user, needs no
  sudo, and reads Redis in one process through `swsscommon.swsscommon.
  SonicV2Connector(use_unix_socket_path=True)` (`connect`, `keys`,
  `get_all`), present on every SONiC host. Measured: every table below in
  56 ms, ~100 KB. Fallback if `swsscommon` is missing: the host's
  `redis` Python module on `/var/run/redis/redis.sock` with the db ids
  from `/var/run/redis/sonic-db/database_config.json` (APPL 0, ASIC 1,
  COUNTERS 2, CONFIG 4, STATE 6).
- Output: tagged sections `@@@ <name>` as the other collectors print, each
  Redis section one JSON object `{key: {field: value}}`. Linux facts
  (`/proc/uptime`, `/proc/stat` line 1, `/proc/loadavg`, `/proc/meminfo`,
  `ip -j addr show`, `ip -j route show default`, `ip -j neigh show`,
  `hostname`, `/etc/sonic/sonic_version.yml`) as text/JSON sections.
  `decode-syseeprom` needs root: not run; the serial comes from
  `STATE_DB DEVICE_METADATA|localhost chassis_serial_number` (hardware
  fills it; `N/A` on the VS).
- **Reduce on the host.** Only the tables listed are dumped. `CONFIG_DB`
  carries secrets elsewhere (`TACPLUS_SERVER` passkeys, `SNMP_COMMUNITY`
  names); the script never dumps whole databases, and the SNMP section is
  key names only. Never widen it to a full `redis-dump`.

Redis sections (key pattern → use):

| Section | DB / pattern | Use |
|---|---|---|
| `metadata` | CONFIG `DEVICE_METADATA\|localhost` | hostname, `hwsku`, `platform`, `mac` (system MAC) |
| `state_metadata` | STATE `DEVICE_METADATA\|localhost` | `chassis_serial_number` |
| `cfg_port` | CONFIG `PORT\|*` | index, lanes, alias, speed, fec, mtu, admin_status, description |
| `appl_port` | APPL `PORT_TABLE:*` | `oper_status`, `speed`, `flap_count`, `last_up_time`, `description` |
| `state_port` | STATE `PORT_TABLE\|*` | `supported_speeds` (hardware), `fec` -- **not** `speed` (`4294967295` on the VS) |
| `cfg_vlan`, `cfg_vlan_member` | CONFIG `VLAN\|*`, `VLAN_MEMBER\|*` | VLAN list; per-port `tagging_mode` (`untagged`/`tagged`) |
| `cfg_lag`, `cfg_lag_member`, `appl_lag` | CONFIG `PORTCHANNEL\|*`, `PORTCHANNEL_MEMBER\|*`; APPL `LAG_TABLE:*` | LAG name/id and member state |
| `cfg_mgmt` | CONFIG `MGMT_INTERFACE\|*`, `MGMT_PORT\|*`, `VLAN_INTERFACE\|*`, `INTERFACE\|*`, `LOOPBACK_INTERFACE\|*` | configured addresses; absence of `MGMT_INTERFACE` = DHCP on eth0 |
| `cfg_stp` | CONFIG `STP\|*`, `STP_VLAN\|*`, `STP_PORT\|*`, `STP_VLAN_PORT\|*` | STP mode/priority/per-port, when configured (`show spanning-tree` says "not configured" on a fresh L2 preset) |
| `cfg_mirror`, `cfg_ntp`, `cfg_syslog`, `cfg_snmp` | CONFIG `MIRROR_SESSION\|*`, `NTP_SERVER\|*`, `SYSLOG_SERVER\|*`, `SNMP_COMMUNITY\|*` (keys only) | device-level reports |
| `cfg_breakout`, `cfg_feature` | CONFIG `BREAKOUT_CFG\|*`, `FEATURE\|*` | breakout mode per cage; which daemons exist (`lldp`, `pmon`) |
| `lldp_loc`, `lldp` | APPL `LLDP_LOC_CHASSIS`, `LLDP_ENTRY_TABLE:*` | own chassis id/name; neighbours per port |
| `fdb` | STATE `FDB_TABLE\|*` | key `FDB_TABLE\|Vlan<id>:<mac>` → `{port, type}` (no timestamps) |
| `counters_map`, `counters`, `rates` | COUNTERS `COUNTERS_PORT_NAME_MAP`, `COUNTERS:oid:0x10*`, `RATES:oid:0x10*` | SAI port stats; port OIDs are `oid:0x1` + 15 hex digits (queues are `0x15…`, so the `0x10*` glob is ports only) |
| `xcvr`, `dom`, `fan`, `psu`, `temp`, `chassis`, `asic_temp` | STATE `TRANSCEIVER_INFO\|*`, `TRANSCEIVER_DOM_SENSOR\|*`, `FAN_INFO\|*`, `PSU_INFO\|*`, `TEMPERATURE_INFO\|*`, `CHASSIS_INFO\|*`, `ASIC_TEMPERATURE_INFO` | hardware only; empty or zero on the VS, the parser must treat absence as unknown |

### 5.2 Mapping to the model

- **Ports.** One `Port` per `CONFIG_DB PORT` entry: `Index` = its `index`
  (1..32), `IfName` = the key (`Ethernet0`), `Interfaces` = `[key]`,
  `Description` = `description`, `Media` QSFP28 (hardware SKU with 100G
  cages; QSFP+ for a 40G SKU), `Lanes` = the lane count (4). Breakout: a
  `BREAKOUT_CFG` mode other than `1x100G` folds the child ports (`Ethernet0`,
  `Ethernet1`, `Ethernet2`, `Ethernet3`) into one cage the way the Arista
  driver folds lanes, `LanesDiverge` when their config differs. `Enabled` =
  `admin_status == up`, `Up` = APPL `oper_status == up`, `SpeedMbps` =
  APPL `speed` (kbps/1000 → Mbps: `100000` is 100G), `AdminSpeed` =
  CONFIG `speed`, `MTU` = `mtu`, `FEC`/`FECConfig` from STATE/CONFIG `fec`
  (`rs` → `rs-fec`, `fc` → fire-code, `none` → disabled, `N/A` → unknown),
  `SpeedCaps` from STATE `supported_speeds` when non-empty, else the SKU's
  speed only. `Health.LinkChanges` = `flap_count`. No PCS/FEC counters on
  the VS (`PORT_COUNTER_CAPABILITIES|PORT_FEC_COUNTER` is empty); on
  hardware `SAI_PORT_STAT_IF_IN_FEC_CORRECTABLE_FRAMES` /
  `_NOT_CORRECTABLE_FRAMES` fill `FECCorrected`/`FECUncorrected`.
- **Counters** from `COUNTERS:<oid>` via the name map:
  `IF_IN/OUT_OCTETS`, `IF_IN/OUT_UCAST_PKTS + NON_UCAST_PKTS` → packets,
  `IF_IN/OUT_ERRORS`, `IF_IN/OUT_DISCARDS`, `IF_IN/OUT_MULTICAST_PKTS`,
  `IF_IN/OUT_BROADCAST_PKTS`.
- **VLAN** per port from `VLAN_MEMBER`: the `untagged` membership is the
  native VLAN; `tagged` ones are `Allowed`; `Mode` is `trunk` when any
  tagged membership exists, else `access`; `AllowAll` is never true (SONiC
  has no "all VLANs" trunk). `Snapshot.VLANs` from `VLAN|*` ids ascending.
- **MAC table** from `FDB_TABLE`: `PortIndex` via the port's index,
  `LastMove` zero. `MACTableUsed` = entry count; capacity unknown.
- **LLDP** `LLDP_ENTRY_TABLE:<port>` → `Neighbor` (`lldp_rem_sys_name`,
  `lldp_rem_sys_desc`, `lldp_rem_chassis_id`, `lldp_rem_port_id`,
  `lldp_rem_port_desc`, `lldp_rem_man_addr`; `IsBridge`/`IsRouter` from
  `lldp_rem_sys_cap_enabled` bits 2 and 4 of the hex string).
- **System.** `Vendor` = `Celestica` for a `Seastone`/`DX010` SKU, else the
  platform's vendor token; `Model` = `hwsku`; `Serial`; `MAC` = metadata
  `mac`; `MgmtMAC` = `eth0`'s link address; `Hostname`; `Version` =
  `build_version` from `sonic_version.yml` (`202505.1238524-ff3c0c9f6`);
  `Uptime`, `LoadAvg`, `CPUPercent` (delta of `/proc/stat` between polls,
  as podman), memory from `/proc/meminfo`; `Addresses` = every IPv4 on
  `eth0`, `Vlan*`, `Loopback*`; `Gateway` from the default route,
  `GatewayMAC` from `ip neigh`; `DHCP` = `eth0` has a `dynamic` address
  and no `MGMT_INTERFACE` entry; **`OOBInterfaces` = `eth0`** (name, up,
  its IPv4) so the loop warns when the bridge dials it; `STPMode` =
  `none` unless `STP|GLOBAL` exists (then its `mode`, `priority`);
  `NTPServers`, `SyslogHosts`, `SNMPCommunities` from the config tables;
  `Fans`/`PSUs`/`TemperatureC`/`HasTemperature` from the pmon tables when
  present (hardware).
- **Uplink.** `UplinkHint` = the port whose LLDP neighbour is a bridge, else
  0; on the lab guest LLDP is empty, so the loop's own MAC-based placement
  does the work.
- **Naming** (`Namer`): `DeviceName` = `"SONiC " + hwsku` (`SONiC
  Seastone-DX010`); `PortName` = description if set, else the SONiC alias
  (`Eth1`…`Eth32`, the faceplate labels); `DefaultPortNames` = alias and
  interface name.
- **Capabilities** (read side): `Capabilities{}` like podman, until the
  control side claims STP, FEC, LACP, mirroring.
- **Identity for the controller.** The device MAC is the system MAC. On
  hardware it is the base MAC the in-band VLAN interface uses, so the
  upstream switch sees it and the controller places the device. On the VS
  the generated system MAC is never seen on the LAN while the bridge dials
  `eth0`; placement needs an in-band `Vlan1000` address on the guest (a
  static address, Clint's call) -- also what the OOB rule asks for.

### 5.2a The native VLAN (decided 2026-10-27)

SONiC has no VLAN 1: the config CLI rejects it as "default VLAN" and
accepts 2--4094 only, and a port in no VLAN is a routed port, not an
access port. UniFi's untagged "Default" network is VLAN 1. So the driver
needs a **substitute id for the native, untagged network**, and it must be
one the controller never uses, or a reconcile would treat a real UniFi
VLAN as the native one -- on Clint's site the L2 preset's `Vlan1000` is a
real UniFi VLAN (an ISP handoff). Decided:

- Driver option `native_vlan` (CONFIG `PORT`-independent), **default
  4094** (confirmed by Clint 2026-10-27 after asking for 0: 802.1Q reserves
  0 and 4095, SONiC's CLI, kernel bridge and SAI all accept 2--4094 only,
  and UniFi keeps the top of the range for itself -- its network editor
  stops around 4009/4010 and it runs its own AP traffic on 4093 -- so 4094
  is a number SONiC takes and the controller cannot hand out). UniFi native VLAN 1 ↔ SONiC `Vlan<native_vlan>` with the port an
  `untagged` member; every other UniFi VLAN keeps its id and is a `tagged`
  member. The read side reports a port whose untagged membership is
  `Vlan<native_vlan>` as native VLAN 1.
- The lab guest moves from `Vlan1000` to `Vlan4094` (add, move all 32
  ports untagged, delete 1000, `config save -y`); the in-band address for
  adoption lives on `Vlan4094`, which on the wire is the Default network.
- An untagged member leaves frames untagged, so the id is never seen on
  the wire; the hazard is only a *tagged* membership of a colliding id.

Implemented on the read side 2026-10-27 (`native_vlan` option, default
4094, validated 2..4094; the substitute id is replaced by 1 in the port's
native VLAN, in `Snapshot.VLANs` and in every MAC entry, so it never
reaches the controller). Edge cases the implementation surfaced, decided
for the control side:

| Case | Read side today | Control side rule |
|---|---|---|
| Port in no VLAN (a routed port on SONiC) | `PortVLAN{}` (mode "", native 0) | The first push gives it the pushed config: an untagged member of `Vlan<native_vlan>` plus the tagged VLANs. Nothing on a DX010 after the L2 preset is routed. |
| Tagged membership of `Vlan<native_vlan>` | reported as tagged `<native_vlan>` (the driver alone cannot tell it from a real UniFi VLAN of that id) | Remediated in two places. **The loop checks the controller's networks**: if any has a VLAN equal to `native_vlan`, that is a configuration collision -- log it and hold port pushes for the device until the option is changed (the driver never sees UniFi state, so this check lives in `informloop`, from the pushed network list or `rest/networkconf`). With that in place a tagged membership of the native id is never legitimate: the driver never writes one, and a stray one typed on the switch is removed by the ordinary reconcile diff like any VLAN the controller did not push. |
| Tagged VLANs with no untagged membership | native 0 | Written only if the controller ever pushes a port without a native network; not seen in any capture yet -- capture first, then decide. |
| `Vlan<native_vlan>` absent on the switch | VLANs without 1 | Created before the first port push (`config vlan add`), never deleted by a reconcile. |
| Changing `native_vlan` on a running bridge | -- | Not migrated: the operator moves the memberships by hand first (what was done on the lab guest 1000 → 4094). Documented, not automated. |

### 5.2b Corrections from the implementation (2026-10-27)

The read side was built from this spec by an Opus 5.5 agent in one pass
(`internal/drivers/sonic`, branch `sonic`). What it found the spec had
wrong, now the rule:

- **Speeds are Mb/s already** (`100000` = 100G); no conversion.
- **`Lanes` is 1 for an unsplit cage**, the model's meaning ("1, or the
  number of lanes a breakout was folded from"); a cage is every `PORT`
  entry sharing an `index`, so a split cage folds its children by index.
- **The L2 preset's `CONFIG_DB PORT` has no `mtu`, `fec` or
  `description`**; MTU comes from `APPL_DB` (9100). A missing CONFIG field
  is the default, not an error.
- **`APPL_DB PORT_TABLE` holds two non-port markers** (`PortConfigDone`,
  `PortInitDone`); ports come from `CONFIG_DB PORT` only.
- **LLDP capability bits are the LLDP-MIB BITS string, most significant
  bit first**: bridge is `0x20`, router `0x08` of the first byte (`"28 00"`
  = both).
- **The LLDP local chassis id is `eth0`'s MAC**, not the system MAC.
- **`RATES:oid:0x10*` also matches `RATES:<oid>:PORT` markers**; the
  parser ignores them. `COUNTERS:oid:0x10*` is exactly the ports.
- **`UplinkHint` has one fallback when LLDP is silent**: the up port on
  which the default gateway's MAC was learned (port 1 on the lab guest).
  The contract test requires an uplink, and a MAC-learned gateway is a
  truthful signal.
- **A stock SONiC has only an RSA host key** and refuses SHA-1
  `ssh-rsa`; `internal/sshrun` now offers `rsa-sha2-512/256` for an
  `ssh-rsa` known_hosts entry (commit on the `sonic` branch).
- The sanitiser rewrites compressed and link-local IPv6 too (EUI-64
  carries the MAC); the podman one did not.
- **Found when the lab guest went in-band (2026-10-27):** two Linux-side
  rules the first pass got wrong. (1) `System.DHCP` describes **the
  address the bridge dials** (the host in the driver's `ssh` config),
  not `eth0`: it is true only when that address is a `dynamic` one in
  `ip -j addr`, or belongs to `eth0` with no `MGMT_INTERFACE` entry; a
  static in-band address with a stale DHCP lease on a down `eth0` reports
  `DHCP: false`. (2) `OOBInterface.Up` is **carrier**, from `ip -j link`'s
  `operstate == "UP"` (equivalently the `LOWER_UP` flag), not the
  administrative `UP` flag: a management port with no cable, or a VM NIC
  with `link_down`, reports `Up: false`. `Addresses` keeps every
  configured address including one on a down interface; `OOBInterfaces`
  carries the carrier state the loop warns from.

**A1 and A2 done 2026-10-27**: fixture
`docs/fixtures/sonic-202505.1238524-ff3c0c9f6/collect.txt` (138 MACs,
captured while the guest was still on `Vlan1000`), `go test ./...` green,
contract test as `USWF07D`, replay test read-only against the Proxmox
replies, live `-collect-once` against the guest. Still to do on the read
side: the `native_vlan` option of §5.2a (the guest now runs `Vlan4094`), a
recapture on 4094, and hardware-only parsing (STP modes, optics, fans,
PSUs, temperatures) that has never seen real data.

### 5.3 Fixtures, tests, sanitiser

- Capture: `docs/fixtures/sonic-202505.1238524-ff3c0c9f6/collect.txt`,
  the script's output from the VS guest, through
  `scripts/sanitize-sonic.py` modelled on `sanitize-podman.py`: MACs →
  `02:00:00:xx:xx:xx` (consistent, lower/upper case preserved per
  occurrence form), IPv4 → `192.0.2.0/24` (keep prefix lengths), global
  IPv6 → `2001:db8::/32`, hostname → `host-1`, LLDP management-address
  lists element-wise, SNMP community keys → `community-N`, port
  descriptions kept unless they contain a mapped value, build info and
  SKU kept. Container-free, so no app/net renaming.
- `FixtureRunner` answers the `python3 -` command with the capture and
  records any other command (none on the read side).
- Tests: parsing of every section, the full snapshot from the fixture
  (32 ports, port 1 up with the learned MACs, the others down, VLAN 1000
  untagged everywhere, system fields), idempotence (two collects equal),
  and the per-driver contract and replay test files the other drivers
  have under `internal/device` and `internal/informloop`.
- Done means: `go test ./...` green, `-collect-once` against the guest
  prints the snapshot, `scripts/check-site-info.sh` clean, a local commit
  on a `sonic` branch, nothing pushed.

## Sources

- SONiC build pipelines: <https://sonic-build.azurewebsites.net/ui/sonic/pipelines>
- sonic-buildimage, DX010 platform: `device/celestica/x86_64-cel_seastone-r0`
  (`platform.json`, hardware SKUs, `fancontrol-F2B`/`-B2F`, `thermal_policy.json`)
- sonic-buildimage issues 11316--11321 (DX010 platform commands broken on
  202111/202205), 18767 (fan command on 202311), PRs 12567 and 18787
- Intel Atom C2000 Product Family Specification Update, June 2018
  (erratum AVR54; Table 8, SoC stepping by PCI revision ID)
- ServeTheHome, "Inside a Celestica Seastone DX010", and the STH forum
  threads "Rework for Celestica AVR54 bug" and "Celestica DX010 --
  Replacement Fans/PSUs"
- ManiAm/net-lab-dx010 (cooling and setup notes: fan module data, console
  baud rate, port layout)
- SONiC Quick Start wiki (ONIE install, default login)
