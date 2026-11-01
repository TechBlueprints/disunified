# SONiC driver (`sonic`)

Status 2026-10-27: `internal/drivers/sonic` is built and every control
feature is verified live on the SONiC virtual switch, build
`202505.1238524-ff3c0c9f6`, as hardware SKU `Seastone-DX010`, adopted as
`USWF07D` (§6.6); STP is feature-gated (no daemon in community images).
The hardware -- a Celestica Seastone DX010 (32x QSFP28, Broadcom BCM56960
Tomahawk, Intel Atom C2538, ONIE) -- arrived 2026-10-31, runs build
`202505.1238526-ff3c0c9f6` and is adopted; §10 is what its first run
fixed. This file began as the plan for
building the driver on a virtual switch first (§2) and for the day the
unit lands (§3); §5 and §6 are the driver's specification, and the
write-up as it stands.

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
   a fictional Arista fabric) to L2: `sudo sonic-cfggen -k <hwsku> --preset
   l2 -p -H | sudo tee /etc/sonic/config_db.json`, `sudo config reload -y`.
   **`-k <hwsku>` is required on a fresh image** (`Seastone-DX010`):
   without it the preset has no port list and writes an empty config
   (seen on the DX010, 2026-10-31). This is also how the real DX010 is set
   up.
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
driver has hit this): static on the guest first, then UniFi's IP Settings.
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
2. On the switch: `sudo sonic_installer install http://<host>/sonic-broadcom.bin`
   (**the 2018 image spells it `sonic_installer`**, with an underscore;
   `sonic-installer` came later), then `sudo reboot`. On the DX010
   (2026-10-31) the 2018 installer took the 202505 image from a local file
   and kept the factory image in the second GRUB slot. Had it refused
   (image format and installer changed over seven years), the ONIE path:
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
4. First-boot setup: change the `admin` password (**the factory image's
   login may not be the documented `YourPaSsWoRd`**: the DX010's was
   another well-known default), add the bridge's SSH key, hostname, the L2
   preset with `-k` (A0 step 4), a VLAN interface on the uplink port for
   the bridge's in-band address with a static default route, and the
   uplink's FEC by hand if its module needs one (`config interface fec
   Ethernet0 rs` for a 100G CWDM4/SR4/CR4: the L2 preset sets none and such
   a link stays down without it; the bridge reaches the switch through that
   link, and from then on keeps FEC to the module, §10.3), then `config
   save -y`.
5. **Management `eth0` for rescue only, and never on the LAN at the same
   time as the in-band address.** SONiC gives `eth0` the system MAC that
   every VLAN interface also uses, so with `eth0` cabled to the LAN the
   upstream switches see the switch's MAC on two ports and the in-band path
   loses packets (20--40 % on the DX010's 100G uplink). The checklist, in
   this order:
   - **Keep `eth0` off the LAN segment**: unplug it, or disable the switch
     port it is cabled to.
   - **Give it a static link-local address**: `sudo config interface ip
     add eth0 169.254.1.1/16`, `sudo config save -y`. That is
     MGMT_INTERFACE, so SONiC runs no DHCP on `eth0` again, and no lease
     dhclient recorded can bring back a default route through it; a laptop
     on the OOB jack (169.254/16) still reaches the switch -- the rescue
     path. `sudo ip link set eth0 down` alone does not survive a reboot.
   - **Source NTP from the in-band interface**: `sudo sonic-db-cli
     CONFIG_DB hset 'NTP|global' src_intf Vlan4094` (202505 has no CLI for
     it), `sudo config save -y`. The image's default is `eth0`, and chrony
     binds to it as soon as `eth0` has a configured address. The bridge does
     this itself with the first push that carries NTP servers (§10.6); by
     hand it does not wait for adoption.
   - **After every reboot, check chrony** (`chronyc sources`): a boot hook
     marks every source offline before the in-band route exists, and
     nothing marks them online again; `sudo chronyc onoffline` does
     (`chronyc activity` says how many are offline). The bridge runs it
     while every source is offline and the in-band interface is up
     (§10.6); before adoption, type it.

   Why each matters (§10.6, 2026-10-31/11-01): SONiC leaves
   `ignore_routes_with_linkdown` at 0, so a default route through a dead
   `eth0` stays in use. Unplugging a live `eth0` with a DHCP lease
   black-holed the switch's own traffic; and after a reboot with `eth0`
   cabled to a disabled port, dhclient re-applied its recorded lease a few
   minutes after boot -- the address as `dynamic` and a `linkdown` default
   route with no metric, which beat the in-band route's metric 20, so
   everything off the LAN went dark while the LAN still answered. Bring a
   live `eth0` down before unplugging it.

**B4. Recapture on hardware.** Re-run A1 on the real unit: the platform
tables, transceivers with a real DAC in a cage, `platform.json` breakout
modes, FEC counters at 100G. Replace the VS fixtures for everything in
the "hardware only" column; keep the VS fixtures for the rest. Pin the
build id. **Done 2026-10-31** (build `202505.1238526-ff3c0c9f6`, §10.7;
the module in the capture is a 100G CWDM4, not a DAC).

**B5. Install.** Rack it on a metered PDU outlet; a 100G DAC or AOC from a
QSFP28 cage to the upstream aggregation switch (which has QSFP28 cages) or
to the Arista's spare cage; nothing on the OOB jack but the console cable.
Check UPS headroom first: the unit adds 150--200 W.

**B6. Prove it live**, in the order of [`docs/adding-a-device.md`](../adding-a-device.md) §4:
`-collect-once`; adoption with the address decided first and no `control`
block (UniFi owns every port from the first push; the loop holds that push
while it would change a port, until the seed from the switch makes it a
no-op); then, from the UI on one empty cage, disable/enable, name, speed,
VLANs, each verified on the switch, and a reconcile that changes nothing;
an aggregate to a real LACP partner, checked with `teamdctl PortChannel<n> state` (fast rate, min
ports 1) and on the partner's side (its LACP shows our fast timeout), and
a recapture with that PortChannel so `cfg_lag` holds `fast_rate` /
`min_links` from a real switch; record each push as a scrubbed
`system_cfg-*.txt`; update [`docs/feature-map.md`](../feature-map.md) and
[`docs/support-matrix.md`](../support-matrix.md). Deploy into the one
container on the Podman host like the others. `ports: all` is the whole
`control:` block: the address, NTP, syslog, SSH keys, SNMP, the time zone,
the hostname and Restart follow UniFi from the first push, ports written or
not (no per-setting opt-ins since 2026-10-31), and UniFi's NTP and syslog
lists replace whatever the factory image carries.

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

- ~~Whether `sonic-installer` on `20181130.77` accepts a `202505` image~~:
  it does, as `sonic_installer` (2026-10-31, §3 B3).
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
- **LACP fields (2026-10-28):** `cfg_lag` prints every `PORTCHANNEL`
  field already, so the control side's `fast_rate` / `min_links` (§6.2,
  §9) needed no collector change and the capture format is unchanged. No
  capture holds a PortChannel yet (the VS sections are `{}`); a recapture
  with one on hardware is a B6 item.

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

## 6. Driver specification: the control side (A4) -- finalised 2026-10-27

Goal: from the controller's point of view the SONiC switch behaves like
the Arista: every feature in [`docs/feature-map.md`](../feature-map.md)
§3 that the Arista driver honours is honoured here, and every one the
platform lacks is refused with a log and left unclaimed. Every write below
was probed on the VS guest (`202505.1238524`, SKU `Seastone-DX010`); the
CONFIG_DB effect of each is recorded in 6.2.

### 6.1 Mechanics

- **Transport:** the same SSH runner; writes are `sudo config …` commands
  (the `admin` user has passwordless sudo on SONiC), one `Run` per command
  or a `bash -s` batch joined with `&&`, followed by `sudo config save -y`
  once per apply batch so a switch reboot keeps the controller's intent.
  The FixtureRunner records every command and **applies it to the
  capture's CONFIG_DB sections** (PORT, VLAN, VLAN_MEMBER, PORTCHANNEL*,
  MIRROR_SESSION, PORT_STORM_CONTROL, STP*, NTP_SERVER, SYSLOG_SERVER,
  SNMP_COMMUNITY, VLAN_INTERFACE, STATIC_ROUTE) so the next Collect reads
  the change back and the idempotence tests mean something, the way the
  podman runner applies a slot-file write.
- **Diffs against the last snapshot**, idempotent; a second apply of the
  same intent runs no command. Every apply refreshes nothing itself; the
  loop collects after it.
- **`Start` runs every command the driver will use** in a harmless form
  (`config --help`-level checks cost nothing; the real test is the
  presence of each sub-command in `config interface --help`,
  `config vlan member --help`, `config portchannel member --help`,
  `config mirror_session span --help`, `config interface storm-control
  --help`, `config spanning-tree interface --help`, `config ntp --help`,
  `config syslog --help`, `config snmp community --help`, `config
  interface ip --help`, `config route --help`) and fails loudly on a
  missing one.
- **Capabilities** are computed from the snapshot, not constant:
  `FEC`, `Jumbo`, `LACP`, `StormControl`, `SNMP` true; `MirrorSessions`
  1 and `AggregateSessions` 8 (the Arista's claims; Tomahawk allows more,
  claim what is tested); `STP`, `BPDUGuard`, `STPPortCost` **true only
  when `FEATURE|stp` exists** (community images: no); `IGMPSnooping`,
  `LLDPMED`, `DHCPSnooping`, `PortIsolation` false.
- **Refused with a log, never claimed:** IGMP snooping, port isolation,
  802.1X, LLDP-MED off, egress rate limit, DHCP snooping, and STP on an
  image without the daemon. A refusal returns no error (the push is
  acknowledged, nothing is written), exactly as the Arista's isolation
  refusal.

### 6.2 Port controller (`Controller.ApplyPorts`, `Planner.PlanPorts`, `VLANController.EnsureVLANs`)

Order within one apply: mirror removals, LAG leaves, VLAN creation
(`EnsureVLANs` runs before `ApplyPorts` in the loop), per-port state,
VLAN membership, LAG joins and the LAG's own membership, mirror adds,
`config save -y`.

| Intent (`PortDesired`) | SONiC write | CONFIG_DB effect (probed) | Rules |
|---|---|---|---|
| `Enabled` | `config interface startup\|shutdown <port>` | `PORT\|<p> admin_status up\|down` | every lane of a split cage |
| `Description` | **no CLI on 202505** (`config interface description` does not exist); `sonic-db-cli CONFIG_DB hset "PORT\|<p>" description '<text>'`, `hdel … description` to clear | `PORT\|<p> description`, shown by `show interfaces description` | sanitise like the Arista (printable, no quotes/newlines, ≤ 64) |
| `SpeedMbps` (0 = auto) | `config interface speed <p> <Mbps>`; auto: `config interface autoneg <p> enabled` on a copper port only, **never on an optical/QSFP cage** (set the cage's native speed instead); a forced speed: `autoneg disabled` + `speed` | `PORT\|<p> speed`, `autoneg on\|off` | speed to lane 1 to split/join is **breakout** (below); a lane-speed change to every lane |
| FEC | `config interface fec <p> rs\|fc\|none` | `PORT\|<p> fec` | only when asked (`FEC != nil`), like the Arista |
| jumbo (`switch.jumboframes`/`switch.mtu`, device-level in UniFi) | `config interface mtu <p> 9100\|1500` on every front port | `PORT\|<p> mtu` | SONiC's default is 9100; "jumbo off" = 1500; reported `Jumbo` capability true |
| VLANs (`VLANSet`, `NativeVLAN`, `TaggedAll`, `TaggedVLANs`) | native: `config vlan member add -u <nat> <p>`; tagged: `config vlan member add <id> <p>`; removals: `config vlan member del <id> <p>` | `VLAN_MEMBER\|Vlan<id>\|<p> tagging_mode` | **UniFi VLAN 1 ↔ `Vlan<native_vlan>`** (§5.2a). "Allow All" = untagged native + tagged member of **every VLAN the controller lists** (SONiC has no "all" trunk; `EnsureVLANs` gives the list, the driver remembers it). "Block All" = native only. A port can be untagged in one VLAN only: change native by `del` then `add -u`. |
| `EnsureVLANs(ids)` | `config vlan add <id>` for missing ids; never deletes | `VLAN\|Vlan<id>` | **collision check:** if `ids` contains `native_vlan`, return an error naming it and write nothing; `ApplyPorts` likewise refuses any desired port that lists `native_vlan` as a tagged VLAN. This is how the §5.2a loop rule is met without UniFi knowledge in the driver. |
| `LAG` | `config portchannel add PortChannel<lag> --fast-rate true --min-links 1` (once; since 2026-10-28, §9), member: `config vlan member del` every membership of the port first, then `config portchannel member add PortChannel<lag> <p>`; leave: `member del`, then the port's own VLAN intent; `config portchannel del` when empty | `PORTCHANNEL\|PortChannel<n>`, `PORTCHANNEL_MEMBER\|…\|<p>`, `LAG_TABLE` | SONiC refuses a member with VLAN config ("Interface configured as VLAN_MEMBER"). The PortChannel carries the VLAN config of its members' intent (`config vlan member add [-u] <id> PortChannel<n>`), reported back on each member port as the Arista does. `lag` is the UniFi id (lowest member index). LACP active is SONiC's default; fast rate (1 s) and min-links 1 are what UniFi's own SONiC switches run (§9). A PortChannel the push names that holds anything else (one made by hand, or by the driver before 2026-10-28: `add` defaults to `fast_rate false`) is brought in line once with `sonic-db-cli CONFIG_DB hset 'PORTCHANNEL\|PortChannel<n>' fast_rate true min_links 1`; **teammgrd reads both only when it creates the team** (sonic-swss 202505 `teammgr.cpp`), so the running LAG takes them at its next start (reboot, `config reload`, teamd restart) -- the saved config holds them at once. Fixture- and replay-tested, **untested live**. |
| `MirrorSource` (this port is the destination) | session name `dui<dst>`: `config vlan member del` every membership of the destination, `config mirror_session span add dui<dst> <dst> <src> both`; removal: `config mirror_session remove dui<dst>` then the destination's VLAN intent | `MIRROR_SESSION\|dui<dst> {type SPAN, dst_port, src_port, direction BOTH}`; `STATE_DB MIRROR_SESSION_TABLE\|dui<dst> status active` | SONiC refuses a destination with VLAN config ("Destination Interface … has vlan config"); the driver strips it and restores it when the session goes. Report `MirrorFrom` from `MIRROR_SESSION`. |
| storm control (`StormCtrl`, percent) | `config interface storm-control add <p> broadcast\|unknown-multicast\|unknown-unicast <kbps>`; `del` to clear | `PORT_STORM_CONTROL\|<p>\|<type> kbps` | kbps = pct/100 × port speed (Mbps) × 1000, rounded; report back as a percent of the configured speed; UniFi's multicast = `unknown-multicast` |
| `BPDUGuard`, `STPEdge`, `STPPathCost` | `config spanning-tree interface bpdu_guard enable\|disable <p>`, `portfast enable\|disable <p>`, `cost <p> <n>`; "STP disabled on the port" (`STPEdge`) = `portfast enable` **and** `config spanning-tree interface disable <p>` | `STP_PORT\|<p> {bpdu_guard, portfast, enabled, path_cost}` | **only when `FEATURE\|stp` exists**; otherwise refuse with a log (not claimed, so not pushed) |
| breakout (speed on lane 1) | `config interface breakout <cage> <mode>` with the mode from `platform.json` (`1x100G[40G]`, `4x25G[10G]`, `2x50G`…) | `BREAKOUT_CFG\|<cage> brkout_mode`; child ports `Ethernet<n+1..3>` appear in `PORT` | **hardware only** (`platform.json` absent on the VS): refused with a log where absent; on the DX010 the first live test is on an empty cage with a 4x25G DAC |
| `PortCycler` | not implemented: no PoE, the controller never issues it | | |

**Corrections after pass 1 (2026-10-27), probed:**

- **Multi-VLAN forms.** `config vlan member add|del` take `-m <v1,v2,…>`
  (a VLAN list, *for one port*; a port list is rejected) and `-e` ("every
  existing VLAN except the listed ones"). So "Allow All" on a port is one
  command, `config vlan member add -e -m <native_vlan> <port>`; dropping
  every tagged membership is `config vlan member del -e -m <native_vlan>
  <port>`; a custom tagged set is `add -m 2,69 <port>`. `config vlan add
  -m 2,8,…` creates several VLANs at once. Each `config` call costs about
  200--250 ms on the VS, so a 31-port Allow-All push is about 8 s with
  these forms against about 60 s with one call per membership; the loop's
  apply budget is 20 s. Use them.
- **Clearing a description** is `hset … description ''` (APPL_DB follows)
  and then `hdel` to tidy CONFIG_DB; a bare `hdel` leaves APPL_DB's copy.
- **The guest's management NIC is owned by the Proxmox driver.** The
  `link_down` flag set by hand on the hypervisor was removed twice, a
  minute after each setting, by the Proxmox bridge reconciling the node's
  port "VM-999 net0" (enabled in the controller) -- the right behaviour
  for that driver. The port is disabled **in the controller** instead, so
  UniFi owns that too.

### 6.3 Device controller (`DeviceController.ApplyDevice`)

| Intent (`DeviceDesired`) | SONiC write | CONFIG_DB effect (probed) | Rules |
|---|---|---|---|
| STP mode/enabled/priority | `config spanning-tree enable pvst` / `disable pvst`, `config spanning-tree priority <n>` | `STP\|GLOBAL {mode pvst, priority, hello_time 2, forward_delay 15, max_age 20}`, `STP_VLAN\|Vlan<id>` and `STP_PORT\|<p>` for every VLAN and port | only with `FEATURE\|stp`; SONiC's only mode is PVST (per-VLAN 802.1D): UniFi `stp`/`rstp` → `pvst`, `mstp` → refused with a log. Report `STPMode` `stp` when enabled, `none` otherwise; `STPPriority` from `STP\|GLOBAL`; `STPRoot` unknown without the daemon's state tables. Without the feature: refuse with a log. |
| IGMP snooping | — | — | refused with a log (community SONiC has none); not claimed, so never pushed |
| NTP (`ManageNTP`, `NTPServers`) | `config ntp add\|del <ip>` (restarts chrony) | `NTP_SERVER\|<ip>` | manage the set as the Arista does: add the controller's, remove the ones the bridge added earlier (remembered per run + persisted in the state dir), leave others. Hostnames: SONiC accepts IPs; a hostname is resolved by the bridge first or logged. |
| syslog (`ManageSyslog`, `SyslogHosts`) | `config syslog add <ip> --port <n>` / `del` (restarts rsyslog) | `SYSLOG_SERVER\|<ip> {port}` | same ownership rule |
| SNMP (`SNMPSet`, `SNMPCommunity`) | `config snmp community add <name> RO` / `del` (restarts snmp) | `SNMP_COMMUNITY\|<name> {TYPE RO}` | UniFi's community is the only one: the image's `public` and any other are removed, and the site's SNMP off leaves none (2026-10-31; until then only the previously managed one was replaced); `public` from the image is left alone unless it is the managed one |
| DHCP snooping | — | — | refused with a log; not claimed |
| timezone | not written (as the Arista) | | |

### 6.4 Other interfaces

- **`SSHKeyInstaller`:** the controller's site keys go into the bridge
  user's `~/.ssh/authorized_keys` on the switch, in a marked block
  (`# disunified-managed-begin` … `-end`) rewritten as a whole; lines
  outside the block (the operator's own key, the bridge's own key) are
  never touched. Compare by `<type> <value>`. All keys, not the Arista's
  two. Written with one `bash -s` that rewrites the file atomically.
- **`Rebooter`:** `sudo reboot` whenever the controller asks. A session
  that drops once the command was sent -- SONiC's reboot script can take
  sshd down before the command returns (DX010, 2026-11-01) -- counts as the
  reload issued, and the dead connection is closed.
- **Locate:** no LED command on SONiC (nothing for a port or chassis
  LED in `config`); the session reports `locating` as every driver does.
  (Superseded by §7.5: the driver now blinks the status LED through the
  platform API.)
- **`AddressController.ApplyAddress`:** static only; DHCP is declined
  (a VLAN interface on SONiC has no DHCP client, and a lease could not be
  confirmed anyway). The target is the interface carrying the address the
  bridge dials (`Vlan<native_vlan>` on a switch set up as §3 B3 says).
  Sequence: `config interface ip add <iface> <new>/<len>` (SONiC keeps
  both addresses), `config route add prefix 0.0.0.0/0 nexthop <gw>` when
  the gateway changes (`STATIC_ROUTE\|default\|0.0.0.0/0`), DNS (below);
  dial the switch at the new address and run `true`. If it does not
  answer, remove the new address (and the route it brought) again and
  return an error: the old address never went away. If it answers, record
  the new address as a pending dial target (`sonic-managed.json`) and run
  one batch **over the new address**:
  `echo 1 | sudo tee /proc/sys/net/ipv4/conf/<iface>/promote_secondaries >/dev/null`;
  `config interface ip remove <iface> <old>/<len>`;
  `ip -4 -o addr show dev <iface> | grep -q ' <new>/<len> '`;
  the stale default route's `config route del`; `config save -y`.
  **Why the sysctl (corrected 2026-10-27, after a live move cut the switch
  off):** Linux's `promote_secondaries` is 0 on SONiC, so deleting a
  subnet's primary address deletes its secondaries too, and the new
  address, added second in the same subnet, is a secondary -- removing
  the old one left the interface with no IPv4 address at all. With the
  sysctl at 1 the secondary survives and keeps the FRR default route
  (probed both ways); `sysctl` is not on the admin user's PATH, hence the
  `/proc` write through `sudo tee`. The `grep` makes the batch fail
  (`set -e`) before anything is saved if the new address did not survive.
  Only when the batch succeeds does the runner switch, and the new address
  is persisted as the dial target (`dial`), so a restart keeps using it
  even if the config file still names the old one. When the current
  target cannot be dialled, the other known addresses (pending, persisted,
  the config file's) are tried once each and the first that answers is
  adopted -- a half-done move no longer leaves the bridge dialling a dead
  address. DNS servers: SONiC has `config dns nameserver add\|del`;
  written when present in the push, diffed against `DNS_NAMESERVER\|*`.
  Follows the podman/Arista guards in `internal/drivers/CLAUDE.md`.
- **Hostname (`resolv.host.1.name`):** queued repo-wide; SONiC's
  `config hostname <name>` exists and is the obvious implementation when
  the feature is turned on for every driver. Not part of A4. (Built in
  §7.5, from the controller -- `control.hostname` until 2026-10-31, on by
  default for sonic from 2026-10-28, §9.)
- **Firmware, fault reporting:** generic in `internal/device`; nothing
  driver-side beyond fans/PSUs/temperatures on hardware (§5).

### 6.5 Verification plan (what "done" means for A4)

1. Unit tests per apply sequence against the fixture, each verifying the
   recorded commands and that a second apply is a no-op; the LAG and
   mirror sequences verify the VLAN strip-and-restore; the collision
   refusal; storm-control arithmetic; the address sequence with a fake
   dial.
2. Replay tests: drive the driver through the loop with the recorded
   controller pushes under `docs/fixtures/controller-10.6.106/`
   (`system_cfg-port2-name-speed-vlans.txt`, `system_cfg-port54-fec-storm-
   bpdu-caps.txt`, `replies.ndjson`) and assert the SONiC commands.
3. Live on the VS guest, `control.ports` on the empty ports first, from the
   controller (UI or REST with the full combinations the notes record):
   disable/enable, rename, VLAN profile with native + tagged, Block All,
   Allow All, a two-port aggregate (ports 3+4), a mirror (4 ← 3), storm
   control, FEC, jumbo off/on, NTP and syslog hosts, SNMP community, SSH
   keys, reboot, and IP Settings → Static (the address change, dialled
   back). After each, the next reconcile reports zero changes.
4. `docs/feature-map.md` gets a `sonic` column or table per feature with
   the verified-on build; `docs/support-matrix.md` lists verified /
   untested / refused; the driver `CLAUDE.md` holds the facts that cost
   time; this file's §6 is updated with anything the implementation
   corrects.
5. Hardware-only items stay marked untested until the DX010 is here:
   breakout, optics, fans/PSUs/temperatures, real speeds and FEC, STP if
   an image with the daemon is ever installed.

### 6.6 Live results on the VS guest (2026-10-27, build 1238524, driven from the controller UI)

Bridge run from the `sonic` branch with `control.ports: 2-32`; every push
below reached the switch within one inform cycle and the following
reconcile reported zero changes. Exact SONiC commands as logged:

| Controller action | Commands the driver ran | Verified on the guest |
|---|---|---|
| first push after enabling control (seeded from the switch: 32 ports Block All) | `config vlan add -m 2,8,9,10,69,1000,2000,3000,4000,4010`; `config interface mtu <p> 1500` × 31 (jumbo off in the site config); VLAN memberships | VLANs created, every port untagged in the native VLAN, MTU 1500 on 31 ports, port 1 untouched. **The first batch (62 commands) hit the loop's 20 s apply budget**; the next cycle finished it. |
| port 2: Port State Disabled + name `lab-2` | `config interface shutdown Ethernet4`; `sonic-db-cli … hset 'PORT\|Ethernet4' description 'lab-2'`; `config vlan member add -e -m 4094 Ethernet4` (the UI's disabled port carries Allow All) | admin down, description shown by `show interfaces description` |
| port 2: Active again; port 4: Allow All | `config interface startup Ethernet4`; `config vlan member add -e -m 4094 Ethernet12` | both ports tagged in all ten VLANs, untagged native |
| port 2: Block All | `config vlan member del -e -m 4094 Ethernet4` | native only |
| port 3: Custom tagged kids (2) + visitor (69) | `config vlan member add -m 2,69 Ethernet8` | tagged 2 and 69, untagged native |
| ports 5+6: Aggregating | `config portchannel add PortChannel1`; `config vlan member del 4094 Ethernet16`; `config portchannel member add PortChannel1 Ethernet16` … | **failed**: "Port MTU of Ethernet16 is different than the PortChannel1 MTU size" (members at 1500, new PortChannel at SONiC's 9100 default; `config interface mtu PortChannel1` is "Invalid port"). Fix probed: `sonic-db-cli CONFIG_DB hset 'PORTCHANNEL\|PortChannel1' mtu 1500` (APPL_DB `LAG_TABLE` follows), then the members join and `config vlan member add -u 4094 PortChannel1` works. Completed by hand that way; the driver's next reconcile reported the LAG as applied (read side correct). |
| port 8: Mirroring from port 7 | `config vlan member del 4094 Ethernet28`; `config mirror_session span add dui8 Ethernet28 Ethernet24 both` | `MIRROR_SESSION\|dui8` SPAN, STATE_DB status active, destination with no VLAN config |
| port 9: Manual, 100G forced, RS-FEC, storm control broadcast 5 % + multicast 1 % | `config interface fec Ethernet32 rs`; `config interface storm-control add Ethernet32 broadcast 5000000`; `… unknown-multicast 1000000` | `fec rs`, `PORT_STORM_CONTROL` kbps exactly 5 % and 1 % of 100G |
| port 8 back to Switching | `config mirror_session remove dui8`; `config vlan member add -u 4094 Ethernet28` | session gone, membership restored |
| ports 5+6 back to Switching | `config portchannel member del PortChannel1 Ethernet16`, `… Ethernet20`; `config vlan member add -u 4094 Ethernet16`, `… Ethernet20`; `config vlan member del 4094 PortChannel1`; `config portchannel del PortChannel1` | PortChannel gone, both members untagged native again |

Two fixes follow from this (pass 3): the loop needs an apply budget of
its own, larger than the collect one (a SONiC `config` call is ~250 ms
and a full push is dozens of them, while a collect is 60 ms); and a new
PortChannel's MTU must be set to its members' MTU through CONFIG_DB
before members join, and follow the jumbo setting afterwards.

**Device level, same evening (builds 9c7cada → bfe12f7), driven from the UI:**

| Controller action | Commands / outcome | Verified |
|---|---|---|
| Jumbo Frames on (device setting, Global Switch Settings off) | `config interface mtu <p> 9100` × 31 at first; after pass 4 the pushed `switch.mtu`: `… 9216` × 31 | 31 ports, port 1 untouched, ~9 s |
| site NTP servers (hostnames + one IP, in every push) | `config ntp add <ip>` ×2; **churned one add + one del per cycle** because a pool hostname resolves differently each time -- pinned per hostname in pass 4 (one add at start since, then quiet) | chrony lists the pushed servers |
| site SSH keys (4, in every push) | one `bash -s` rewriting the managed block | 4 keys inside the block, the operator's 5 lines outside untouched |
| syslog | with remote logging off: `syslog.status=enabled`, `syslog.level=7` and no host → nothing written. Remote logging switched on in Settings → System (2026-10-27, after the session): the host arrives as **`syslog.remote.ip/port/status`**, which the parser had never seen (it read `syslog.ip`, from unifi-emu's notes; no driver had ever received a host) → parser fixed, `config syslog add <ip> --port 514` + save | `SYSLOG_SERVER\|<ip> {port 514}` in CONFIG_DB and the saved config, rsyslog forwarding to it; the capture is replay record 273 |
| SNMP enabled on the device (location/contact) | **nothing pushed**: the controller sends no `snmp.*` keys unless the site-wide SNMP setting is on | not exercisable without a site-wide change |
| Manage → Restart | arrived as a reply **`_type: reboot`, `reboot_type: soft`**, not a `cmd`; logged `UNHANDLED` and ignored until pass 6; then `sudo reboot` ("requested COLD shutdown"), guest back in ~2 min, stale `eth0` lease gone, bridge reconnected | uptime reset |
| IP Settings → Static .201 (from .200, same interface and subnet) | pass-2 sequence: `ip add`, `dns nameserver add`, dial new, `ip remove <old>`, save → **the interface lost both addresses** (`promote_secondaries` = 0: a subnet's secondaries go with its primary); recovered over the serial console with `config reload -y`; the bridge kept dialling the dead address | failure, fixed in pass 5 |
| IP Settings → Static .200 (back, pass 5) | `ip add <new>`; dial new; over the new session: `echo 1 \| sudo tee /proc/sys/net/ipv4/conf/Vlan4094/promote_secondaries`, `ip remove <old>`, `ip -4 -o addr show … \| grep -q ' <new>/16 '`, `config save -y`; runner switched, target persisted | guest on the new address only, default route kept, bridge continued, reconcile 0 changes |
| SSH agent | the Mac's 1Password agent holds the employer's key first; `sshrun` offered it and Go aborted on its refusal ("failed to sign challenge") -- pass 6: agent keys one at a time, and `ssh_key: <file>.pub` selects one identity | `authenticated with ssh-rsa SHA256:… Personal key` in the log |

**Controller version:** the reply log of these live tests says
`unifi.version=10.6.109`; the controller had moved on from the 10.6.106
the rest of this repo was verified against. Nothing in the protocol
differed, and the replay fixture `replies-sonic.ndjson` sits under the
10.6.106 directory with that note.

**After the review-fix and feature passes (same evening, build bfbbfe1 →
e5bfa83):** the first push after a restart applied `config clock timezone
MST7MDT` (from the pushed POSIX rule) and `config hostname
SONiCSeastone-DX010`, both verified on the guest;
`set-locate` through the API reached the driver, which tried the platform
LED, logged that the VS has none, and reported `locating`. **Jumbo with an aggregate present, live (20:14--20:24, build e5bfa83):**
aggregate 5+6 created entirely through the driver this time
(`config portchannel add PortChannel1`; `sonic-db-cli … hset
'PORTCHANNEL|PortChannel1' mtu 9216`; members out of their VLAN and into
the channel; the channel into the native VLAN; save) -- the sequence that
failed live before the MTU rule was known. Then Jumbo Frames off: `config
interface mtu <p> 1500` on 29 ports, `hset 'PORTCHANNEL|PortChannel1' mtu
1500`, `hset 'PORT|Ethernet16' mtu 1500`, `hset 'PORT|Ethernet20' mtu
1500`, 31 of 31 ports changed, APPL_DB LAG mtu 1500; then on again with
9216 the same way; then the aggregate dissolved with both members back in
the native VLAN. The review's high finding is closed live. (Since
2026-10-28 the create is `config portchannel add PortChannel1 --fast-rate
true --min-links 1`, §9; that form and the reconcile `hset … fast_rate
true min_links 1` are fixture- and replay-tested, **untested live**.)

Left open after this: the address the bridge **reports** to the controller
(`ip`, `connect_request_ip`) is the configured target from start-up, so the
controller kept showing the old address after a move until a restart -- a
real switch reports its current address (pass 7). STP remains untested (no
daemon on this image).

**STP on the real switch:** a community image has no STP daemon, so the
DX010 will not run spanning tree until an image is built with
`INCLUDE_STP=y` (the `sonic-stp` source and `docker-stp` rules are on the
202505 branch; only the flag is off). That is a build decision for later,
not a driver gap: the driver claims STP when the feature is present.

## 7. Feature evaluation against the SONiC source (2026-10-27)

Clint asked whether the source offers UniFi features the driver still
lacks. Repositories cloned locally for this (branch `202505`, the
switch's own): `sonic-utilities` (the whole `config`/`show` command tree,
7511bf2), `sonic-swss` (the Redis schema, e9dcd50) and the DX010 parts of
`sonic-buildimage` (ff3c0c9, sparse: the Seastone platform directory,
build rules, templates). Method: every `config` command group and its
sub-commands were listed from the source, the controller's pushed keys in
the recorded fixtures were listed, and the two were matched; the DX010
platform API and the YANG models on the guest filled in what the CLI
does not expose.

### 7.1 Pushed by the controller, implementable, not yet done → **implement**

| UniFi push / action | SONiC has | Decision |
|---|---|---|
| `system.timezone` / `locale.timezone` (in every push, 6 captures) | `config clock timezone <name>` (`DEVICE_METADATA timezone`; the guest runs `UTC`) | implement: `DeviceDesired.Timezone`, written when it differs; the Arista left it "todo (safe)" and can follow |
| `resolv.host.1.name` (the UniFi name without spaces, in every push) | `config hostname <name>` ("without impacting the traffic") | implement behind a new `control.hostname` flag (opt-in like the other writes; **on by default for sonic since 2026-10-28**, §9); the queued repo-wide feature's other half, a static DNS record for a static-address device, stays queued |
| `set-locate` / `unset-locate` | the DX010 platform API: `Chassis.set_status_led('green'\|'off')` on `/sys/devices/platform/leds_dx010/leds/dx010:green:stat`; no port LED control, no blink primitive; the VS platform API raises | implement an optional `devicemodel.Locator` (`SetLocate(ctx, on)`): the driver blinks the system LED by toggling green/off once a second while locating and restores green after; a platform whose API fails logs once and reports `locating` only. **Hardware-untested** until the DX010 |
| breakout (speed on lane 1 of a cage) | `config interface breakout [-y] [-f] [-l] <port> <mode>` with the modes from the SKU's `platform.json`: `1x100G[40G]`, `2x50G`, `4x25G[10G]` on the DX010; child ports `Ethernet<n+1..>` keep the parent's `index` | implement the mapping now (100000/40000 → `1x100G[40G]`, 50000 → `2x50G`, 25000/10000 → `4x25G[10G]`, lane speed then set on each child), fixture-tested with the DX010's `platform.json` interfaces block from the source tree (public, not site data); refused with a log where `platform.json` is absent (the VS). **Hardware-untested** |

### 7.2 Pushed, but SONiC cannot do it → stays refused or report-only (unchanged)

| UniFi push | Why not |
|---|---|
| `switch.vlan.N.igmp_*` (snooping, querier, fast-leave…) | no IGMP snooping in community SONiC (no `config igmp*`, no L2MC tables in the schema); logged, unclaimed. **An unmerged upstream implementation exists; what building it takes is §8** |
| `switch.port.N.lldpmed.opmode` / `topology_notify` | `LLDP_PORT` has `enabled`/`mode`/TLV flags, no LLDP-MED; report-only like the Arista |
| `switch.port.N.ld_mode` (loop detection) | nothing in SONiC; a real STP daemon would be the answer (see 7.4) |
| `switch.dhcp_snoop.status` | SONiC's `dhcp-mitigation-rate` is a per-port DHCP packet rate limit (`PORT dhcp_rate_limit`, default 300 pps), not rogue-server blocking; `dhcp_relay` is L3; unclaimed |
| isolation, 802.1X, port security (`port_security_mac_address`) | none in community SONiC (`sonic-acl.yang` exists, so an ACL-based emulation of isolation and MAC allow-lists is possible later; not now) |
| egress/ingress rate limits | the controller pushed no rate-limit key in any capture, so nothing to act on; SONiC could shape per port through `SCHEDULER`/`PORT_QOS_MAP` (YANG present) if a push ever carries it |
| `snmp.*` location/contact | not pushed unless the site-wide SNMP is on; `config snmp location\|contact` exist and are a one-line mapping once a capture exists |

### 7.3 Controller actions with no capture → not implemented (repo rule: capture first)

- **Reset Stats** (Port Manager): the control stays disabled for this device
  in the UI, with or without a port selected, so no command was observed;
  `sonic-clear counters` is the obvious implementation if one ever is.
- **ping / traceroute / speed-test** cmds: the host has `ping` and
  `traceroute`; the reply format for results was never captured (the
  Arista lists them "nice to have").
- **cable-test**: no TDR on SONiC.

### 7.4 Not a driver matter

- **STP**: `config spanning-tree` writes everything, but community images
  ship no daemon (`INCLUDE_STP` unset). An image built with
  `INCLUDE_STP=y` (the `sonic-stp` source and `docker-stp` rules are on
  `202505`) would make the driver's STP path, loop protection and BPDU
  guard real on the DX010. A build task for Clint, not code.
- **`switchport mode`**: `config switchport mode access\|trunk\|routed`
  exists on 202505 and would make the native/tagged model more explicit;
  the membership-based writes already behave correctly, so no change.

### 7.5 Implemented (2026-10-27)

The four 7.1 rows are built and fixture-tested; time zone and hostname
await a live check on the VS; locate and the 4x25G breakout with its join were verified on the DX010 on 2026-11-01 (§11.6), 2x50G and lane speeds on a split cage are still hardware-untested.
What each emits (from the tests), and where the build differs from 7.1:

| Feature | Commands | Notes |
|---|---|---|
| Time zone (always, with `control.ports`) | `sudo config clock timezone MST7MDT` ; `sudo config save -y` | The push carries a **POSIX rule** (`system.timezone=MST7MDT,M3.2.0,M11.1.0`), not a zone name, and `config clock timezone` takes only `timedatectl list-timezones` names. The driver reads the switch's zone list with each zone's rule (`bash -s -- disunified-zones`: `tail -n 1` of every TZif file, once per run; `zones.txt` in the fixture dir is that capture) and picks the zone named like the rule, else by its abbreviation, else the first match; the current zone is kept when its rule matches (America/Denver stays). Unmatched: refused with a log. `System.Timezone` reads `DEVICE_METADATA timezone`. |
| Hostname (`control.hostname`: **on by default for sonic** since 2026-10-28, because UniFi's own SONiC switches always take the UniFi name as their hostname (§9); `hostname: false` turns it off; off by default for every other driver; `-control-hostname` / `DUI_CONTROL_HOSTNAME`, unset = the driver's default, `0`/`1` explicit) | `sudo config hostname <resolv.host.1.name>` ; `sudo config save -y` | RFC 1123 labels and at most 63 characters in all (SONiC's YANG `stypes:hostname`); otherwise refused with a log. The log prefix and `System.Hostname` follow on the next collect. |
| Locate (`devicemodel.Locator`) | on: `sudo python3 -c 'import sys; from sonic_platform.platform import Platform; sys.exit(0 if Platform().get_chassis().set_status_led("off") else 1)'`, then `…("green")`, `…("off")` … once a second; off: the blink stops, `…("green")` once | The API returns False on a failed write, so the command exits on its result (7.1's bare call would exit 0). The VS raises `FileNotFoundError` (no `/etc/sonic/vs_chassis_metadata.json`, probed read-only): logged once per run, report-only after. The loop hands `set-locate`/`unset-locate` to a driver with `Locator`; others keep "reported as locating". The blink stops on `Close` and restarts on a second locate-on. |
| Breakout, cage 2 (`Ethernet4`) | split to 25G: `sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null`, then per child `Ethernet4..7` `config interface startup` (and the name), `config vlan member add -u 4094 <child>` (and the tagged VLANs), `config save -y`; lanes to 10G: `sudo config interface speed Ethernet4 10000` … `Ethernet7`, save; join: `sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null`, `config interface startup Ethernet4`, the name, `config vlan member add -u 4094 Ethernet4`, save; 50G: `… '2x50G'` with children `Ethernet4`, `Ethernet6` | **No `-l`** (7.1 asked for `-f -l -y`): `-l` loads `/etc/sonic/port_breakout_config_db.json`, which nothing in the SONiC source generates and which is absent on the VS; `config_mgmt.py` fails the breakout without it. The children are written the cage's intent by the same plan instead (state, name, MTU, VLANs, storm control, FEC), which is what the Arista does for lanes. `-f` removes the old ports' VLAN memberships (without it the breakout refuses); `</dev/null` keeps a possible "tables without YANG" prompt from reading the rest of the `bash -s` batch. Children per `portconfig.py get_config`: in 4x25G they are `Ethernet<n>..<n+3>`, in 2x50G `Ethernet<n>` and `<n+2>`, all on the parent's index, no `admin_status` (down). The mode is chosen from platform.json for the speed (its default speed preferred), not a fixed table. Refused for a cage in an aggregate or a mirror session. A split cage keeps the parent's alias as its name (`Eth2`) and claims every mode's speeds. |

`collect.py` now prints platform.json's `interfaces` block (each parent's
`index`, `lanes`, `breakout_modes`) instead of `yes`; `Start` also checks
`config clock timezone` and `config hostname` (`config-help.txt` gained
both captured helps).

## 8. IGMP snooping: what building it takes (assessed 2026-10-27)

Community SONiC 202505 has no IGMP snooping (§7). This section is the honest
account of what it would take to have it on the DX010, from the sources, so
the decision can be made on facts.

### 8.1 What exists today

**An implementation is in flight upstream, unmerged.** Jasper Hou of Micas
Networks opened a complete set of draft pull requests on 2025-10-23, all
against the **202511** branch, all still open, "draft", `mergeable_state:
blocked`, last touched 2026-09-10:

| Piece | Where | Size | State |
|---|---|---|---|
| `l2mcorch` (orchagent) + `l2mcmgrd` (cfgmgr): CONFIG_DB → APPL_DB → SAI | [sonic-swss #3950](https://github.com/sonic-net/sonic-swss/pull/3950) | 4,084 lines, 16 files (`l2mcmgr.cpp` 2,023, `l2mcorch.cpp` 1,573) | 30 review comments, nearly all answered "will further revise"; a real memory-safety bug (`memcpy` of short IP strings past `c_str()`) found 2026-08-12 and acknowledged (corrected 2026-11-01: fixed in the PR on 2026-08-19, `379cfb9`; the daemon carries the same bug, fixed in this build's series, §8.6); CI runs approved by maintainers |
| `docker-l2mcd` container, `INCLUDE_L2MCD`, YANG models (`sonic-igmp-snooping.yang`, `sonic-mld-snooping.yang`), CoPP trap group for IGMP/MLD to the CPU, rsyslog, feature table | [sonic-buildimage #24327](https://github.com/sonic-net/sonic-buildimage/pull/24327) | 772 lines, 22 files | draft; **its submodule points at `sonic-net/sonic-l2mcd`, which does not exist** -- the build cannot resolve it as submitted |
| `config igmp-snooping …` / `show igmp-snooping …`, and `mld-snooping` (corrected 2026-11-01; not under `ip`) | [sonic-utilities #4100](https://github.com/sonic-net/sonic-utilities/pull/4100) | 1,944 lines | draft |
| gNMI/translib for it | sonic-buildimage #24328 | 285 lines | closed, not merged |
| **The daemon itself (`l2mcd`)**: IGMPv1/2/3 and MLD state machines, querier, fast-leave, mrouter ports, timers, a `l2mcd_sync.cpp` bridge to swss-common | [`jasper-hou-micas/sonic-l2mcd-micas`](https://github.com/jasper-hou-micas/sonic-l2mcd-micas), branches `dev` / `l2mcd-pre` (master holds only LICENSE/NOTICE) | 62 files, 1.09 MB of C (`igmp.c` 113 KB, `igmp_fsm.c` 98 KB, `l2mcd_mld_port.c` 133 KB, …), autotools + `debian/`, depends on libswsscommon and libevent | last commit 2026-08-14; Apache-2.0, NOTICE says it derives from Microsoft's original (the 2020 Broadcom L2MCD lineage -- [buildimage #4213](https://github.com/sonic-net/sonic-buildimage/pull/4213), whose own source repo `Azure/sonic-l2mcd` was never populated) |
| Design | [SONiC HLD #426](https://github.com/sonic-net/SONiC/pull/426), `doc/l2mc/IGMP_Snooping_HLD.md`, 701 lines | closed 2025-01-29 **unmerged**; not in the SONiC doc tree |

So: roughly **7,900 lines of new C/C++/Python plus a 1 MB C daemon**, written by one vendor engineer, reviewed but not accepted, with the daemon's source published only on a personal fork and a known memory bug. It is real and runs on their hardware; it is not something anyone has shipped from the community tree.

**What 202505/202511 already provide that the design builds on.** `portsorch`
already creates a per-VLAN SAI L2MC group (for VXLAN flooding), so the
L2MC group API is exercised on Broadcom today. `intfsorch` already sets
`SAI_VLAN_ATTR_UNKNOWN_MULTICAST_FLOOD_CONTROL_TYPE`. The CoPP trap types
`igmp_query`/`igmp_leave`/`igmp_v{1,2,3}_report` exist in `copporch`; the
202505 `copp_cfg.j2` just never enables them (the PR adds an `l2mcd` trap
group, cir/cbs 600 pps, queue 3). The attributes the orch programs are all
in the **public** SAI headers (`inc/saivlan.h`, `sail2mc.h`, `sail2mcgroup.h`):
`SAI_VLAN_ATTR_CUSTOM_IGMP_SNOOPING_ENABLE` (standard despite the name;
Mellanox implements it too), `SAI_VLAN_ATTR_UNKNOWN_IPV4_MCAST_OUTPUT_GROUP_ID`
and the link-local variant, `SAI_L2MC_ENTRY_TYPE_XG/SG` with an output
group of `L2MC_GROUP_MEMBER`s. The virtual switch's SAI (`vslib`,
`SwitchBCM56850`) knows the L2MC object types, so the control path can be
proven on the VS guest; forwarding cannot.

**Images.** The Azure pipeline publishes nightly `sonic-broadcom.bin` for
`202511` (§1's URL with `branchName=202511`) and the DX010 platform
(`x86_64-cel_seastone-r0`) is on that branch. The PRs are against 202511, so a
custom image is a 202511 build, not 202505; the driver's fixtures would be
recaptured on a 202511 VS (`Start`'s `--help` checks are built for that drift).

### 8.2 How it fits together

```
UniFi switch.vlan.N.igmp_*  --driver-->  config igmp-snooping … (CLI, #4100)
                                               |
                                      CONFIG_DB L2MC|VlanN {enabled, querier, fast-leave,
                                                query-interval, query-max-response-time, …}
                                               |
                 l2mcmgrd (cfgmgr) -----> APPL_DB L2MC_VLAN / _MEMBER / _MROUTER
                 l2mcd container: IGMP packets trapped to the CPU (CoPP), joins/leaves/
                 queries parsed, group->ports learned, querier sends queries, timers;
                 writes members to APPL_DB
                                               |
                 l2mcorch (orchagent) --SAI--> VLAN igmp-snooping-enable, unknown-mcast
                                               output group, one L2MC entry per
                                               (VLAN, group) with its member ports
                                               |
                                      Tomahawk L2MC table: a group floods only to members
```

UniFi's eight per-VLAN keys (`igmp_snooping`, `igmp_querier_status`,
`igmp_querier` address, `igmp_fastleave`, `igmp_maxresponse` 10,
`igmp_groupmembership` 260, `igmp_mcrtrexpiretime` 180, `igmp_supression`)
map onto the `L2MC` table's `enabled`, `querier`, `fast-leave`,
`query-max-response-time`, `query-interval` (derived: group membership = 2 x
query interval + max response, so 125 s for UniFi's 260/10) and the
`L2MC_SUPPRESS` table; the querier address and the mrouter expiry have no
field and are logged. That driver side is the small part: the usual
captures, `control.go` mapping, tests -- a day or two in the established
pattern, once the switch has the commands. (Superseded 2026-11-01 by §8.10,
which maps every key against what Ubiquiti's own switch writes:
`L2MC_SUPPRESS` carries `forward_unknown_multicast`, not `igmp_supression`.)

### 8.3 The plan, if it is wanted

1. **Build host** (needed for the STP image too, §7.4): a Proxmox guest with
   8 vCPU, 16 GB, ~150 GB disk (the build tree is ~60 GB; NFS works, it is
   just slower), Docker, `sonic-buildimage` at `202511`. First build of an
   unmodified `sonic-vs.img.gz` and `sonic-broadcom.bin` to prove the
   pipeline: 3-6 hours each, the first one longer for the slave container.
   This also yields the `INCLUDE_STP=y` images on the same tree.
   (Corrected 2026-11-01: sonic-buildimage 202511 asks for 300 GB free, KVM
   -- the vs image is installed by a KVM guest during the build, so a Proxmox
   guest needs CPU type host -- Docker 20.10.10 or newer, `j2`, and a non-root
   user. §8.6 has the host as planned and the time estimate.)
2. **Integrate the three PRs** on a fork of 202511: #24327 with the
   submodule pointed at `jasper-hou-micas/sonic-l2mcd-micas@dev` (or a fork
   of it under TechBlueprints, so the build does not depend on a personal
   branch), #3950 into `src/sonic-swss`, #4100 into `src/sonic-utilities`;
   fix the `memcpy` bug and read the 30 review comments for anything else
   that matters on our path (IPv4 only; no MCLAG; no warm boot).
3. **Prove the control path on the VS guest**: enable on `Vlan4094`, static
   group, `show ip igmp snooping groups`, APPL_DB and ASIC_DB entries, a
   real IGMP join from a second guest on the same bridge (the VS's Linux
   bridge forwards; the ASIC_DB entry is the proof, not traffic).
4. **Hardware (the DX010, 202511 custom image)**: the one thing nothing
   above can check is whether the **Broadcom SAI for Tomahawk** in 202511
   accepts `SAI_VLAN_ATTR_CUSTOM_IGMP_SNOOPING_ENABLE` and L2MC entries. The
   L2MC *group* API is known to work (VXLAN uses it); the entry and the VLAN
   attribute are what Enterprise-class images rely on, and Micas tests on
   Broadcom, so the odds are good -- but if the SAI returns
   `SAI_STATUS_NOT_SUPPORTED`, snooping cannot be done in this ASIC's SAI
   and the whole thing stops there. Test: two receivers on one VLAN, one
   joined; multicast source; port counters show the unjoined port quiet.
5. **Driver**: map the keys (§8.2), claim the capability so the UI's
   per-VLAN IGMP controls apply, captures and tests, feature map.

### 8.4 Cost and risk, plainly

- **Steps 1-3** are a build-and-debug project on draft code: a day to set up
  the host, build time measured in hours per iteration, and an unknown
  number of iterations to get 7,900 lines of unreviewed vendor code to
  build and run on 202511 -- days at best, a couple of weeks if the daemon
  misbehaves. The memory bug is known; there may be others.
- **Step 4** cannot start before the switch arrives and carries the one
  hard unknown (SAI support on this ASIC's vendor library).
- **Ownership**: until upstream merges it, every SONiC upgrade on the DX010
  is a rebuild of this image with the patches re-applied. Upstream is
  active (CI runs approved through September 2026), so a merge in a 2026 or
  2027 branch is plausible, at which point a stock image replaces the custom
  one and the driver side carries over unchanged.
- **What it buys**: multicast confined to subscribed ports on the DX010 and
  UniFi's per-VLAN IGMP settings meaning something on it. Without it,
  multicast within a VLAN floods to every DX010 port in that VLAN; the UniFi
  switches downstream still snoop on their own access ports.

The decision is Clint's. The cheaper-first order is: build host and STP
image (step 1, useful regardless), then step 2 while waiting for the
hardware, then step 4 on arrival.

### 8.5 A reference implementation is already on the LAN (found 2026-10-28)

Clint asked whether the ECS line runs SONiC. It does. Read over SSH from the
site's UniFi **ECS Aggregation** (`USWF066`) with the site's own SSH key:

| | |
|---|---|
| OS | SONiC, Ubiquiti build `4.0.1`, `release: 202106`, Debian 10.13, kernel 4.19, `asic_type: marvell`, built 2026-09-08 by `ubnt` |
| Platform | ONIE `x86_64-ubiquiti_pcom_p701gt-r0`, Intel Atom C3558R; `docker-syncd-mrvl` -- a **Marvell** ASIC, so the ECS Core (same firmware line) is almost certainly Marvell too, not the DX010's Broadcom Tomahawk |
| Containers | `swss` (orchagent **with the L2MC orch**), `l2mcd` (the daemon **and `l2mcmgrd`**, the same split as the Micas container; corrected 2026-11-01, the process's cgroup is the l2mcd container's), `stp` (**mstpd 0.0.9** plus Ubiquiti's `ubnt-stpmgrd` and `UBNT_STP` tables, not the community `stpd`), `teamd`, `fpm-frr`, `mgmt-framework`, `dhcp_relay`, `radv`, `nat`, `sflow`, `macsec`, `snmp`, `pmon` |
| The snooping daemon | `/usr/bin/l2mcd`, 3.6 MB, **dated 2019-04-26**: the original Broadcom-era L2MCD -- the same code the 2020 HLD describes and the Micas `dev` branch (§8.1) carries forward. Package `l2mcd 1.0.0`, "L2MCD for SONiC project" |
| UniFi glue | `ubnt-lnxcfgd` writes the controller's config straight into CONFIG_DB (corrected 2026-11-01: `ubnt-lnxcfgd` is a small Python helper that runs scripts as interfaces come and go; no Python on the box mentions an `igmp_*` key, so the translation from the controller's keys is compiled -- most likely in `mcad`, Ubiquiti's management daemon, not read. §9's attribution of VLAN 1 to it is the same mistake); there is **no `show ip igmp snooping` CLI** on the box (`show ip` has no `igmp`). A feature file of Ubiquiti's switches those daemons on and off (MAC and DHCP snooping, the STP manager, LLDP, fan control). |

**UniFi's per-VLAN IGMP settings land in `CFG_L2MC_TABLE`**, one entry per
VLAN: on that switch the native VLAN has snooping and the querier on, the
gateway as querier address, IGMP version 2, the query, response,
membership, last-member and querier-timeout timers at UniFi's defaults, and
fast-leave and unknown-multicast flooding as switches. That is the HLD's
field set with a few additions of Ubiquiti's, and each field answers one of
UniFi's `switch.vlan.N.igmp_*` keys (group membership time, max response
time, querier address, fast leave, suppression). The mapping the driver would need (§8.2)
is therefore not a guess: it is what Ubiquiti's own glue writes, and the
switch's APPL_DB (`APP_L2MC_MEMBER_TABLE:Vlan:0.0.0.0:group:port`, 49 entries)
is a live reference to compare a DX010 build against.

**Ubiquiti floods the discovery protocols on purpose.** 21 reserved-group
entries in `CFG_L2MC_STATIC_GROUP_TABLE` per switch (SSDP, NTP, SLP, H.323,
PTP and more) carry every port as a static member, so
SSDP (239.255.255.250), NTP (224.0.1.1) and the rest flood to **all ports** on
a UniFi switch with snooping on. Only the remaining groups are pruned
dynamically: UniFi's own discovery `233.89.188.1` goes to the ports with
UniFi devices behind them, the controller's `239.254.127.63` and HP's
`224.0.1.60` toward the gateway's LAG only. So the SSDP chatter measured in
§8.4 already reaches every port of every UniFi switch by design; a DX010
without snooping changes nothing for those groups.

**What this does to the assessment.** The daemon is not experimental: it
runs in every ECS switch Ubiquiti sells and has run in this rack for months,
on a Marvell SAI. Micas runs the modern source of the same daemon on
Broadcom. The steps in §8.3 stand; the "draft vendor code" risk in §8.4 is
smaller than it read, and the SAI question in step 4 now has two vendors'
worth of precedent. The one thing the ECS build cannot lend the DX010 is a
binary: its `l2mcd` and orchagent are 202106 and link that branch's
`libswsscommon`; the Micas source on 202511 is the path.

### 8.6 The build, prepared (2026-11-01)

Steps 1-2 of §8.3 are done short of running the build. The build kit lives
outside this repo, beside the SONiC source clones: `series/` (the patches, one
directory per repository, `BASES`, and `CONFLICTS.md`, the rebase record),
`build.sh` (unattended, for the build host), `dry-run.sh` and a README.

**The series** (11 patches), each applying with `git am` on the **current
202511 head** of its repository -- which is also the commit sonic-buildimage
202511 pins for that submodule:

| Repository | Base | Patches |
|---|---|---|
| sonic-buildimage | `1354e0ce4630` (2026-11-01) | 0001 #24327 squashed (docker-l2mcd, rules, service, CoPP trap group, YANG); 0002 the daemon submodule; 0003 the PR's unrelated changes taken out; 0004, 0005 and 0007 DX010 platform fixes (§8.7); 0006 CoPP as the ECS does it (§8.11) |
| sonic-swss | `177c7a009f5e` (2026-09-19) | #3950 squashed (`l2mcorch`, `l2mcmgrd`) |
| sonic-swss-common | `e59457c6da43` (2026-07-21) | the `schema.h` part of [#1093](https://github.com/sonic-net/sonic-swss-common/pull/1093) (L2MC and MLD_L2MC table names; a fourth Micas draft §8.1 did not list, and swss does not compile without it) |
| sonic-utilities | `73f8c019ff7c` (2026-10-20) | #4100 squashed |
| sonic-l2mcd | `651a13bdf036` (dev = l2mcd-pre, 2026-08-14) | one fix: the memcpy bug, see below |

**Conflicts: none textual.** Each PR, squashed, three-way merged onto its base
cleanly (the PR merge bases are 7-154 commits behind). The semantic checks
found nothing to change in the merged code: 202511's dockers are still bookworm
(the PR's docker-l2mcd matches docker-stp's pattern), the orchagent interfaces
l2mcorch calls (`Orch`, the `PortsOrch` lookups, the port-change observer) are
unchanged in 202511, all 15 SAI identifiers it uses are in the SAI 1.17 headers,
and every CoPP trap name it adds is in copporch's map. What the series changes beyond the PRs:

- **The submodule.** #24327's `.gitmodules` names `sonic-net/sonic-l2mcd`, which
  does not exist, and the PR adds **no gitlink** at all, so `make init` would never
  check the daemon out. 0002 points it at `jasper-hou-micas/sonic-l2mcd-micas`
  pinned at `651a13b` and fixes two rule mistakes (`SMDEP_SPATH` named a
  directory that does not exist; `INCLUDE_L2MCD ?= y`).
- **Unrelated changes on the PR branch, removed (0003):** gNMI translib write
  access switched on, gNMI template and `x509` YANG changes (from the closed
  #24328), and an `init_cfg` default `MGMT_PORT|eth0 admin_status up` -- the
  last one would fight §10.6's deliberately-down eth0.
- **Left out:** sonic-sairedis #1672 (L2MC warm-reboot view comparison; this build
  is cold-boot only), sonic-mgmt-common #185 (gNMI YANG import), and two upstream
  commits plus four leftover debug-table defines on #1093's branch.
- **The memcpy bug.** The one acknowledged on #3950 (all ten IP-address copies in
  `cfgmgr/l2mcmgr.cpp` read 46 bytes from strings that guarantee `size() + 1`) was
  fixed upstream on 2026-08-19 (`379cfb9`, `strncpy` of size - 1); the series
  carries that. **The daemon has the same bug and upstream did not touch it**:
  `L2mcSync::dumpL2mcVlanEntry` in `l2mcd_sync.cpp` copies the port name and both
  addresses with `memcpy(dst, s.c_str(), FIXED_SIZE)` (the warm-restart path).
  `sonic-l2mcd/0001` bounds them the same way. The review's other crash-class
  findings (use-after-erase, an iterator that never advanced, `.at()` throws,
  unbounded start-up waits) are fixed on the PR head; what remains is minor (an
  early `return` on a malformed mrouter key, stale per-VLAN map entries after a
  VLAN is removed).

**Dry run** (`dry-run.sh`, passed): every series applies with no fuzz to its
base and reproduces the work branches exactly, also without 0005; the templates
the image renders (`copp_cfg.j2`, `init_cfg.json.j2`, the docker-l2mcd
Dockerfile, `l2mcd.service`) render to valid output with `FEATURE|l2mcd`
disabled and no MGMT_PORT default; both YANG models parse (pyang, with
sonic-yang-models' templated modules rendered); every make variable the new rules
use is defined (except `$(LIBEVENT)`, which expands empty -- harmless, libevent-dev
is in the bookworm slave and l2mcd links it statically); the CLI byte-compiles and
adds no undefined names; the daemon's pinned commit answers. **Not compiled**: the
Mac has no container runtime and creating one would mean a VM. `build.sh` builds
libswsscommon, l2mcd, swss and docker-l2mcd first in the bookworm slave, so a
compile error in the series shows within about an hour of the first run instead
of after the full image.

**The build host and the time.** Debian 12 x86_64, 16 cores, 64 GB, 400 GB, Docker
(20.10.10 or newer), `j2`, a non-root user in the docker and kvm groups, `/dev/kvm`
(nested virtualisation on a Proxmox guest: CPU type host). `build.sh` checks all
of it, prepares one patched tree (clone, `git am`, `make init`, the submodule
series), copies it per platform, and builds `sonic-vs.img.gz` and the Broadcom
ONIE images in separate trees (the dpkg cache keys include the platform, so
nothing is shared between them except the build slaves). Estimated, unattended:
**about 7-10 hours** for both (prepare 15-30 min, the two slaves 40-70 min once,
fail-fast packages 45-90 min, vs 2.5-4 h, broadcom 3-4.5 h with three ONIE images),
about 120 GB of disk per platform tree. The first run writes the measured
durations. The build id is `igmp-202511.<BUILD_NUMBER>-<sha>` (SONiC's own
`sonic_get_version` on the local branch); record it with the image as §1 asks.

### 8.7 202511 under a Tomahawk-1: the DX010 needs more than the snooping patches

Moving the DX010 from 202505 to 202511 changes its SAI and its platform
support, independently of snooping. Found while preparing the build:

- **Two Broadcom SAIs.** 202511 moved the XGS SAI to `14.3.0.0.0.0.41.1` and split
  Tomahawk-1 off to a "legacy-TH" SAI pinned at `13.2.1.100` (sonic-buildimage
  #25878, #26136: Broadcom "would still support TH SAI with current version, but
  no upgrade plan"). Only the Arista 7060CX-32S was moved; the DX010 -- the same
  BCM56960 -- still says `platform_asic broadcom` and would get 14.3, which nobody
  upstream reports running on a Tomahawk-1. The DX010 runs `13.2.1.36` on 202505
  today. `make target/sonic-broadcom.bin` builds one ONIE image per machine
  (`sonic-broadcom.bin`, `-dnx.bin`, `-legacy-th.bin`), each installing only on the
  platforms whose `platform_asic` names it. **Series 0005** moves the DX010 to
  `broadcom-legacy-th` and adds the legacy-TH BDE kernel modules the ONIE image
  never listed (#28125); the DX010's image is then
  **`sonic-broadcom-legacy-th.bin`**. `build.sh DX010_SAI=xgs` leaves 0005 out.
- **A start-up crash on any of those SAIs.** 202511's orchagent asks for
  streaming-telemetry capability at start (`HFTelOrch::isSupportedHFTel`), which
  202505's never did, and the Broadcom SAI null-dereferences on it on a Tomahawk-1
  (sonic-sairedis #1788): syncd SIGSEGV, swss crash-loop. All three libsaibcm
  builds export the crashing function. 202511's sairedis has the opt-out (#1795);
  **series 0004** sets `SAI_STATS_ST_CAPABILITY_SUPPORTED=0` and
  `SAI_STATS_EXT_SWITCH_SUPPORTED=0` in the eight Seastone `sai.profile`s (the
  DX010 part of the open #28160; a DX010 on 202605 was brought up this way, #28127).
- **No platform vitals on 202511 (#27291), found and fixed here.** A DX010 on 202511
  reports nothing in `show platform fan/psustatus/temperature/syseeprom`. The
  cause: 202511's kernel 6.12/trixie rework of the Celestica packaging moved the
  non-PDDF platforms to pybuild and dropped the line that installed the DX010's
  `sonic_platform` wheel into its device directory -- the file pmon's container
  installs the platform API from at start. Without it every pmon daemon starts with
  no platform. On 202505 this unit reports both drawers and both PSUs from exactly
  that file (read 2026-11-01). **Series 0007** builds the wheel the way the PDDF
  platforms build theirs and installs it there again; built and checked in the
  202511 trixie build slave (18 `sonic_platform` modules, the same size as
  202505's). H1 below is still where it is proven.
- The host OS under 202511 is Debian trixie with kernel 6.12 (the dockers stay
  bookworm); `platform-modules-cel` was adapted for it on the branch. The driver's
  fixtures are recaptured on the new build (`Start`'s `--help` checks exist for
  this drift).

### 8.8 The hard unknown: L2MC in the Tomahawk-1 SAI

Whether the Broadcom SAI programs L2MC entries and the VLAN snooping attribute
on a BCM56960 decides the whole effort. Inspected without hardware: the three
`libsaibcm` packages 202511 and 202505 can put on the DX010 (`13.2.1.36` running
now, `13.2.1.100` legacy-TH, `14.3.0.0.0.0.41.1` XGS, downloaded from the URLs in
the build rules, symbols and strings only) and `libsaibcm-dev` 14.3's headers.

**What is known:**

- All three implement the API l2mcorch calls: `brcm_sai_create/remove/set/get`
  `_l2mc_entry`, the L2MC group and group-member functions, and export the
  `l2mc_apis` table. The VLAN attributes it sets
  (`SAI_VLAN_ATTR_CUSTOM_IGMP_SNOOPING_ENABLE`, the unknown-multicast output groups)
  are in their metadata and in sairedis 202511's SAI headers (1.17.5).
- The SAI tracks snooping per VLAN ("L2MC not enabeld on vlan:%d" is one of its
  messages) and carries the SDK's `BCM_VLAN_IGMP_SNOOP_DISABLE` control, so the
  VLAN attribute is implemented, not just declared.
- Its L2MC feature gate, `_brcm_sai_l2mc_support`, is a byte initialised to 1 in
  `.data` in all three builds, and none of the per-chip feature-init functions
  (Tomahawk, Tomahawk-2, common, device, XGS) writes it. 14.3 adds the message
  "This device doesn't supports L2MC flow." for some other path.
- Tomahawk-1 code is present in all three (about 1,000 BCM56960 strings in 13.2.1,
  about 700 in 14.3).
- A refused SAI call does not take the switch down: l2mcorch logs
  "Failed to Enable L2mc snooping" / "L2MC entry create failed" and carries on, and a
  refused IGMP trap marks orchagent unhealthy without aborting it.

**What cannot be known before hardware:** whether the SDK accepts an L2 (VLAN,
group) entry on this chip in the mode the SAI uses, whether the VLAN control
actually stops unregistered-group flooding, whether the IGMP traps install on
it, how many L2MC entries it holds, and whether 202511 boots on a DX010 at all
(legacy-TH on 202511 is CI-tested only on the 7060CX). The virtual switch cannot
answer any of it -- `vslib` accepts every L2MC object. The first answer is H3
below: one syslog line and one ASIC_DB row.

### 8.9 Validation plan

**V -- the virtual switch** (the build's `sonic-vs.img.gz` as a Proxmox guest,
the way §2 A0 ran the 202505 one; creating it waits for Clint). Pass = every step's
evidence recorded and captured for fixtures (scrubbed).

1. Boot: `show version` prints the build id; all containers up; `show feature
   status` has `l2mcd disabled` and no l2mcd container runs; orchagent's log has no
   L2MC errors. The driver's normal collect and push work against it (fixtures
   recaptured on 202511).
2. `sudo config feature state l2mcd enabled`: the container runs `l2mcd` and
   `l2mcmgrd`; the `l2mcd` CoPP trap group and its five IGMP traps reach APPL_DB and
   ASIC_DB (`SAI_OBJECT_TYPE_HOSTIF_TRAP` with the IGMP types).
3. `sudo config igmp-snooping enable <vid>` on a test VLAN: `CONFIG_DB L2MC|Vlan<vid>`
   with the defaults, `APPL_DB L2MC_VLAN_TABLE:Vlan<vid>`, and the VLAN's ASIC_DB
   object gains `SAI_VLAN_ATTR_CUSTOM_IGMP_SNOOPING_ENABLE true`.
4. Joins from a test host (a second guest on the VS's bridge port, joining with
   any IGMPv2 client): `show igmp-snooping groups vlan <vid>` lists the group on
   that port; `APPL_DB L2MC_MEMBER_TABLE:Vlan<vid>:0.0.0.0:<group>:<port>` (the
   ECS's key shape, §8.5); ASIC_DB has the `L2MC_ENTRY` (type XG), its `L2MC_GROUP`
   and a `L2MC_GROUP_MEMBER` on the port's bridge port. A leave removes them, after
   the last-member query interval, or at once with fast-leave.
5. Querier: with no other querier on the segment, `config igmp-snooping querier
   enable <vid>` makes general queries appear on the test host every query interval;
   record their source (l2mcd uses the lowest IPv4 address of the VLAN's interface,
   `0.0.0.0` when it has none). With a second querier at a lower address, the
   switch must fall silent.
6. Static group, mrouter port, and `optimised_multicast_flood enable`: the VLAN's
   `SAI_VLAN_ATTR_UNKNOWN_IPV4_MCAST_OUTPUT_GROUP_ID` points at a group holding the
   mrouter ports.
7. Lifecycle: `disable` removes every entry; `config vlan del` is refused while
   snooping is on (the CLI's own check) and `vlan member del` while the port holds
   static entries; restarting the l2mcd container, then swss, then a reboot with the
   config saved rebuilds the same state.

**H -- the DX010**, under §11.3 (dead-man timer, Ethernet0 the only path, probes
never change state) and only after V passes and Clint says go:

0. Before rebooting into it: the image downloaded to the switch and its sha256
   checked; `sonic-installer install` adds it beside 202505 and the factory image
   (never `cleanup`) and makes it the default: `sonic-installer set-default` back
   to 202505 and `set-next-boot` the new one, so a power cycle returns to 202505. Confirm
   that eth0's down state is saved configuration, not an `ip link` change that a
   reboot undoes (§10.6).
1. Boot: syncd runs libsaibcm `13.2.1.100` (or 14.3 with `DX010_SAI=xgs`) without a
   segfault; its log shows `SAI_STATS_ST_CAPABILITY_SUPPORTED=0`; orchagent logs HFTel
   disabled; 32 ports created, Ethernet0 forwarding, the bridge's collect and push
   work. `show platform fan/psustatus/temperature/syseeprom` -- #27291 -- decides
   whether §10's hardware fields survive; if not, back to 202505 and that is its own
   problem first.
2. `config feature state l2mcd enabled`: the IGMP traps install (ASIC_DB, no
   "Failed to create trap" in syslog). Then confirm IGMP still crosses the switch on
   VLANs **without** snooping: the traps send every IGMP packet to the CPU, so
   l2mcd and the kernel bridge now carry what the ASIC used to forward.
3. **The hard unknown**: snooping on a test VLAN on two unused ports. Syslog has no
   "Failed to Enable L2mc snooping"; the VLAN's ASIC_DB object has the attribute.
4. Traffic: two receivers on the test VLAN, one joined, a multicast source; the
   unjoined port's multicast counters stay flat (`show interfaces counters`); after a
   leave the joined one goes quiet too. Repeat with `optimised_multicast_flood` on
   for an unjoined group.
5. Only then a production VLAN, observing first: the LAN's existing querier is
   learned as the mrouter port, the reports reach it, and nothing upstream loses a
   group.
6. Rollback at any step: `config igmp-snooping disable`, `config feature state l2mcd
   disabled`, or the 202505 image as default and a reboot under the timer.

### 8.10 The UniFi side: the controller's keys and their future write path

What the controller pushes to a SONiC-based UniFi switch with snooping on (the
ECS Aggregation's `system_cfg`, site data in `local-information/ecs-reference/`),
what Ubiquiti's own switch does with each key (read from its CONFIG_DB
2026-11-01, read-only, described here rather than copied), and where each would
land on the DX010 with the series. **Not implemented**: no capture of the DX010
side exists until the image runs (§8.9 V), and the controller withholds these
keys until the device claims the feature (§11.1). The driver claims IGMP snooping
(`feature_caps` 256, `IGMP_SNOOPING_ADVANCED`, `igmp_snoop_caps` 31, as the ECS)
only after H3 and H4 pass.

| UniFi key (`switch.vlan.N.…`, per VLAN) | On the ECS (one `CFG_L2MC_TABLE` entry per snooping VLAN) | DX010 with the series: CONFIG_DB `L2MC\|VlanX`, and the CLI that means the same |
|---|---|---|
| `igmp_snooping` true/false | an entry with snooping enabled; no entry for a VLAN without snooping | the row with every field (`config igmp-snooping enable <vid>` writes the defaults); removing it (`disable <vid>`) also drops the VLAN's static groups, mrouter ports and IPv4 suppression settings |
| `igmp_querier_status` | the querier switch | `querier` (`config igmp-snooping querier enable\|disable <vid>`) |
| `igmp_querier` (address, often empty) | the querier's source address: the pushed one, or, when the push leaves it empty, a made-up address in the benchmarking range 198.18.0.0/15 derived from the switch's own management address | **no field.** l2mcd sends queries from the lowest IPv4 of the VLAN's interface (STATE_DB `INTERFACE_TABLE`), 0.0.0.0 without one. Logged; the patch that closes this is specified in §8.11 |
| `igmp_fastleave` | fast leave | `fast-leave` (`config igmp-snooping fast-leave enable\|disable <vid>`) |
| `igmp_maxresponse` (s) | the max response time | `query-max-response-time` (1-25 s; must stay below the query interval, CLI and YANG check) |
| `igmp_groupmembership` (s) | the membership time, stored as pushed, beside a query interval the switch chooses itself (shorter on the VLAN whose push names a querier address) and a querier timeout derived from it | **no field**; l2mcd derives membership as 2 x query interval + max response, so `query-interval` = (membership - response) / 2: 125 for UniFi's 260/10, the RFC 3376 default |
| `igmp_mcrtrexpiretime` (s) | nothing that carries it | none; logged |
| `igmp_supression` | nothing that carries it (every VLAN there pushes false) | none; meaning not established; logged |
| `forward_unknown_multicast` true/false | flood unknown multicast in the VLAN, or send it to the multicast-router ports only | `L2MC_SUPPRESS ipv4-optimised-multicast-flood` `disable`/`enable` (`config igmp-snooping optimised_multicast_flood …`, snooping must be on): unknown IPv4 multicast to the mrouter ports only (`SAI_VLAN_ATTR_UNKNOWN_IPV4_MCAST_OUTPUT_GROUP_ID`) |

Global keys:

- `switch.multicast.flood_known_protocols=true` → on the ECS, seven static groups
  per snooping VLAN in `CFG_L2MC_STATIC_GROUP_TABLE` (SSDP, NTP, PTP, H.323 and three
  SLP groups), every port a member, named symbolically and resolved by its own
  daemon (§8.5). On the DX010: `L2MC_STATIC_GROUP|VlanX|<group>` plus
  `L2MC_STATIC_MEMBER|VlanX|<group>|<port>` for each VLAN member, with IANA's
  addresses for those protocols (239.255.255.250, 224.0.1.1, 224.0.1.129, 224.0.1.41,
  224.0.1.35, 224.0.1.22, 239.255.255.253), kept in step with VLAN membership.
- `switch.multicast.forward_unk_mcast_router_ports=true` → nothing separate on the
  ECS; read as "unknown multicast always reaches the mrouter ports", which the
  optimised flood above already does. Logged if it is ever false.

Order rules the driver inherits from the CLI: enable snooping before writing
suppression or static entries; disable snooping (or delete the statics) before
removing a VLAN or a member; UniFi's VLAN 1 settings land on `Vlan<native_vlan>`
(§5.2a). As everywhere else, UniFi's list replaces the switch's: a snooping VLAN the
push does not name is disabled.

The CLI column says what each key means on this build; the write path itself
follows the ECS (§8.11): whole rows straight into CONFIG_DB.

### 8.11 Following the ECS (2026-11-01)

Clint, 2026-11-01: for this work, follow how the site's ECS switch, which runs
Ubiquiti's SONiC, does the same things, wherever possible. Compared piece by piece
from what the ECS shows read-only; where this build departs, the reason is the
code, not a preference:

| Piece | The ECS (Ubiquiti SONiC 4.0.1, 202106) | This build | |
|---|---|---|---|
| Daemon and manager | `l2mcd` (2019 Broadcom lineage) and `l2mcmgrd` share the `l2mcd` container; orchagent's L2MC orch is in `swss` | the same layout (Micas's l2mcd is the same lineage, modernised) | same |
| Feature | `l2mcd` enabled, auto-restart on, always | the image ships it disabled, so a first boot of new code changes nothing; the driver enables it (auto-restart on) once it claims IGMP snooping and leaves it on, as the ECS does | same once claimed |
| CoPP | the five IGMP trap types (and PIM) in one rate-limited group that traps to the CPU; **no MLD traps** | **series 0006**: the five IGMP types only, no MLD, in a group of its own (so it follows the feature) with the ECS's rate limit, queue and priority -- about a fifth of the rate the PR used. The PR also trapped MLD, which would send every IPv6 neighbour-discovery join on every VLAN to the CPU | same, without PIM: this l2mcd's socket filter accepts IGMP only, so trapping PIM would only take it off the ASIC |
| Who writes the config | no snooping CLI on the box; the controller's keys reach its `CFG_L2MC_*` tables from Ubiquiti's compiled management side | the driver writes complete `L2MC`, `L2MC_SUPPRESS` and static-group rows with one swsscommon script per apply (the collector's transport, §5.1), not one `config igmp-snooping` call per field | same |
| What a VLAN's entry carries | every timer and switch in one entry, including the querier address, the membership time and unknown-multicast handling | one `L2MC` row with every field this l2mcd reads; unknown-multicast handling in the `L2MC_SUPPRESS` row | same values; Micas's table names (`L2MC`, not `CFG_L2MC_TABLE`) kept, because they are what upstream would merge |
| Membership time | stored as pushed, independent of the query interval | derived (above); the push's 260 s comes out the same | same result; the ECS's shorter query interval on one VLAN not copied (with derived membership it would age groups out in about a minute) |
| Querier source address | the pushed address, or a synthetic one; never 0.0.0.0 | the VLAN interface's address, else **0.0.0.0** -- the DX010 has an address only on its native VLAN | **differs**; the next patch, below |
| Reserved groups | `flood_known_protocols` → seven reserved groups static on every port | the same seven as static groups by address (§8.10) | same |
| Unknown multicast | to the mrouter ports only when `forward_unknown_multicast` is false | `ipv4-optimised-multicast-flood enable` | same |
| Kernel bridge | `Bridge` with multicast snooping on | 202511's vlanmgrd never sets it, so the kernel default, on (not read on the DX010) | same |

**The querier-address patch (specified, not written).** Without it, on every VLAN
where the DX010 has no address, its queries leave from 0.0.0.0, which RFC 4541
permits but some hosts and snoopers ignore; the ECS never sends one. The change,
for the round after the first image (it touches the daemon's query and election
paths and should be written with a compiler at hand, by the Opus pass that
implements §8.10):

1. swss `cfgmgr/l2mcmgr.cpp`: read a `querier-address` field from `L2MC|VlanX`
   beside the other fields (the three places that parse them) into a new
   `char querier_addr[L2MCD_IP_ADDR_STR_SIZE]` in `L2MCD_CONFIG_MSG`
   (`sonic-l2mcd/include/l2mcd_ipc.h`, before the `count`/`ports[]` tail; both
   sides compile from that one header).
2. l2mcd `port/l2mcd_system.c`: store it per VLAN (a `uint32_t` in the VLAN node of
   `mld_vlan_db.h`) when the config message is applied.
3. l2mcd `src/igmp_io.c`: where the source is chosen -- `igmp_send_general_query`
   (no interface address → today 0) and `ip_get_lowest_ip_address_on_port` (used by
   group queries and by querier election) -- fall back to the configured address
   before 0.0.0.0, so the switch both sends from it and elects with it.
4. YANG: a `querier-address` leaf (`inet:ipv4-address`) in `L2MC_LIST`.

The driver then writes the pushed `igmp_querier`, or, when it is empty, an address
in 198.18.0.0/15 built from the DX010's own management address, as the ECS does.
Validation: §8.9 V5, with and without an interface address on the VLAN.

## 9. What Ubiquiti's own SONiC does, and what the driver takes from it (2026-10-28)

The ECS Aggregation's CONFIG_DB (§8.5; read-only, one sample per table) is
the only first-party answer to "how does UniFi lay its config out in SONiC".
Table by table against what this driver writes on the DX010:

| UniFi concept | Ubiquiti's SONiC (ECS, 202106, Marvell) | This driver (202505, Broadcom) | Verdict |
|---|---|---|---|
| Port name | `PORT\|EthernetN description = "<UniFi name>"` (`SFP28 1`, `QSFP28 3`) | the same `description` | same |
| Allow All | every site VLAN **tagged** on every port (490 tagged `VLAN_MEMBER`s for 11 VLANs); native 1 untagged | the same shape | same |
| VLAN 1 (native LAN) | **`VLAN\|Vlan1` exists**, 52 untagged members, kernel PVID 1, `MGMT_VLAN\|Vlan1`; written straight into CONFIG_DB by `ubnt-lnxcfgd`, bypassing the CLI | `Vlan<native_vlan>` (default 4094) stands in for 1 | **keep the substitute.** Community 202505 cannot take VLAN 1: `config vlan add 1` fails "is default VLAN", the YANG model's `vlanid` range is `2..4094`, `vlanmgrd` strips VID 1 from the bridge and every port at start, and `orchagent` empties the SAI **default VLAN, which is 1 on the Broadcom SAI**, at init -- creating a second VLAN object with that id is not something the community orchagent is written to do. Ubiquiti's orchagent and Marvell SAI are their own. Revisit only with an orchagent change (the same image work as §8). |
| Aggregate | each aggregate a `PORTCHANNEL` entry running LACP at the **fast rate** with a **minimum of one link** (`fast_rate`, `min_links`); members in `PORTCHANNEL_MEMBER` | `config portchannel add PortChannelN` with the defaults (fast_rate false; min_links 1 on 202505, whose `--min-links` is `IntRange(1,1024)` defaulting to 1) | **adopted 2026-10-28, untested live**: `config portchannel add PortChannelN --fast-rate true --min-links 1` (202505's `add_portchannel` takes both); a managed PortChannel holding anything else gets `sonic-db-cli CONFIG_DB hset 'PORTCHANNEL\|PortChannelN' fast_rate true min_links 1` once, which teamd applies at its next start only (teammgrd reads both at team creation, §6.2). UniFi's LACP runs fast timers (1 s) end to end; a slow-rate DX010 on the core LAG would be the odd partner (LACP tolerates mixed rates, but failover detection on our side would be 90 s instead of 3 s). |
| Hostname | `DEVICE_METADATA\|localhost hostname = <UniFi name>` (`aggregation`) | `config hostname <resolv.host.1.name>` with `control.hostname` on | **adopted 2026-10-28**: `control.hostname` unset is on for sonic (`hostname: false` turns it off), off for every other driver; config-tested. The command itself was verified live on the VS (§6.6). |
| NTP | `NTP_SERVER\|<ip>` for the IP only; the site's pool **hostname is dropped** (SONiC takes addresses) | both: the hostname resolved once and pinned (`sonic-managed.json`) | keep ours (a pinned pool member beats no second server). |
| Jumbo | `PORT mtu 1500` with the site's jumbo off | the pushed `switch.mtu` (9216) when on, 1500 when off | same semantics. |
| Speed / FEC on optics | an **`AUTO_DETECT` table per port** (speed, adv_speeds, fec, interface_type from the module) kept by Ubiquiti's `autodetectd`; the UniFi UI's "auto" means "what the optic says" | speed/FEC only as pushed; "auto" leaves the switch's setting | **hardware-day item**: on the DX010, when UniFi says auto and the cage holds a module, choose FEC from `TRANSCEIVER_INFO` (RS for 100G DAC/SR4/CWDM4, none for 40G) the way `autodetectd` does. Not provable on the VS. |
| STP | **RSTP via mstpd 0.0.9** in the `stp` container, driven by Ubiquiti's `ubnt-stpmgrd` from a table of their own (`UBNT_STP`): RSTP switch-wide at priority step 2 (8192), per-VLAN STP off, and an admin-edge flag per port | none on the VS; the community `INCLUDE_STP` image is **PVST only** (`config spanning-tree enable <pvst>` is the only mode in 202505; no RSTP PR in sonic-stp or swss) | the gap is bigger than "build the STP image": a UniFi LAN runs RSTP, and a PVST+ switch in the middle of it interoperates through the IEEE-BPDU-on-VLAN-1 compatibility path only. Ubiquiti solved it by running mstpd with their own glue into the STP orch. Noted for §8's image work: the STP piece should be RSTP, which means mstpd + a manager, not the community `stpd`. |
| Loop protection (`ld_mode`) | a **MAC-move guard** (`MAC_MOVE_GUARD`, switch-wide and per port): a port whose MACs move 10 times within 5 s is disabled for 150 s -- Ubiquiti's own daemon errdisables a port whose MACs flap | refused with a log | **candidate, bridge-side, no SONiC change**: the collector reads the FDB every cycle already; a MAC seen moving between ports N times in the window → anomaly on the port, and with `ld_mode` pushed, `config interface shutdown` + a timed re-enable. Same semantics as Ubiquiti's, implemented where we can. Worth it on a hub switch with no STP. |
| Storm control | errdisable on a broadcast storm (`ERROR_DISABLE`), with no automatic recovery | `PORT_STORM_CONTROL` rate limits in kbps from the UI's percentages | different mechanisms; ours matches what the UI's sliders promise. keep. |
| DHCP guard / snooping | `ACL_TABLE\|global_ip_acl` (L3) and `global_mac_acl` (ANY) bound to every port, no rules while the site feature is off; `dhcpsnpmgrd` in the glue | refused with a log | an ACL-based path exists in community SONiC too (`sonic-acl.yang`); same shape as Ubiquiti's. Later, if wanted. |
| Management address | on a Linux bridge `br0` taking DHCP, in a management table of their own (`UBNT_MGMT_INTERFACE`), with VLAN 1 as the management VLAN | `VLAN_INTERFACE\|Vlan<native>` address, static or DHCP | Ubiquiti's is their own construct; ours is standard SONiC. keep. |
| Features | `l2mcd`, `stp`, `teamd`, `bgp` (FRR), `dhcp_relay`, `radv`, `mgmt-framework`, `snmp` enabled; `macsec`, `nat`, `sflow` present but off | community image: no `l2mcd`, no `stp` | §8. |

Platform detail: the platform is a Marvell Prestera 98CX8514 reference
design (`x86_64-marvell_db98cx8514_10cc-r0`, HwSKU `ECSAGG-48x25G6x100G`)
with Ubiquiti's ONIE (`ubiquiti_pcom_p701gt`). Its `PORT` entries carry
Marvell-only cut-through fields (`switching_mode`, `ct_untagged`) that have
no Broadcom counterpart in the community schema.

**To do from this (small, driver-side):** the MAC-move guard as the
`ld_mode` implementation; FEC-from-module on hardware. Done 2026-10-28:
LACP fast rate and min-links 1 on the aggregates the driver creates (and
brought in line on one that lacks them), and the hostname from UniFi. The VLAN 1 question is closed for the community image.

## 10. The first hardware run, and what it fixed (2026-10-31)

The DX010 arrived 2026-10-31 and runs `202505.1238526-ff3c0c9f6` -- the
VS guest's commit (`ff3c0c9f6`), two builds later -- on the L2 preset,
adopted as `USWF07D`, uplinked by one 100G port to a UniFi device. The first real-hardware run of the driver showed eight gaps. Each
is specified here before its code; what is verified live is in
[`docs/support-matrix.md`](../support-matrix.md). §10.9 is a gap the
first live speed tests found the next night.

### 10.1 SNMP off never stuck: `/etc/sonic/snmp.yml`

Seen live: the reconcile ran `config snmp community del <community>` and
`config save -y` every ~80 s, restarting the snmp container each time.
The container's `start.sh` runs `snmp_yml_to_configdb.py`, which copies
every community named in `/etc/sonic/snmp.yml` (`snmp_rocommunity`,
`snmp_rocommunities`, `snmp_rwcommunity`, `snmp_rwcommunities`) into
CONFIG_DB `SNMP_COMMUNITY` on every container start, adding only, and
`config snmp community add|del` restart that container -- so the deleted
community came straight back. Every 202505 image ships the file with
`snmp_rocommunity: public` and `snmp_location: public` (the importer exits
1 without a location); `/etc/sonic` survives `sonic_installer`, so the
factory image's copy came along. `snmpd.conf` itself is rendered from
CONFIG_DB only.

- **Read.** `collect.py` prints a `snmp_yml` section: the communities the
  file names under those four keys, names only (`{community: {key}}`,
  like `cfg_snmp`), `{}` when the file is absent, `error` when it cannot
  be parsed. The file is `0644 root`; the login user reads it.
  `System.SNMPCommunities` is CONFIG_DB's set plus the file's: a community
  in the file is configured, the next snmp start imports it.
- **Write.** The file's communities minus UniFi's one are dropped from it
  **before any `config snmp community` call** (each restarts the
  container, which imports the file): one `bash -s --
  disunified-snmp-yml` script; if the file exists, `sudo python3 -` loads
  it with PyYAML (on every SONiC host; the importer needs it), removes the
  dropped names from the four keys (a scalar key goes, a list keeps the
  rest, an emptied list goes), and only if that changed anything writes it
  back atomically (a temporary file in `/etc/sonic`, mode 0644,
  `os.replace`). Every other key stays (`snmp_location`). The names travel
  inside the script on stdin, never on a command line; the log says
  `<community>`. Then the CONFIG_DB diff runs as before. UniFi's own
  community is never written into the file (CONFIG_DB and `config save`
  hold it).
- **Tests.** The FixtureRunner treats the capture's `snmp_yml` section as
  the file: the script's drop list is applied to it, and every `config
  snmp community add|del` re-imports it into `cfg_snmp` as `start.sh`
  does, so deleting before rewriting brings the community back and fails
  the idempotence test. The factory file's section
  (`snmp_yml-factory.txt`, produced by `collect.py`'s own code from the
  unit's backup of the factory file, scrubbed) is the input.

### 10.2 The first-push hold and jumbo

Seen live: the first push carried `switch.jumboframes=disabled` /
`switch.mtu=9216` (a new device's default) while every port ran SONiC's
9100 (the L2 preset sets no `mtu`), so `PlanPorts` saw 32 changes and the
loop held every push; the adoption seed (`internal/unifiapi`) covers port
overrides, not the device's Jumbo Frames setting. Released on the unit by
setting 1500 by hand. The choice was (A) seed the device's Jumbo Frames
setting from the switch at adoption -- a capture of the UI's save first,
and 9100 counted as UniFi's jumbo or the hold still sticks -- or (B) leave
MTU out of the hold. **Clint chose B (2026-10-31).** Jumbo is a
device-wide setting in UniFi, like NTP, syslog and the hostname, which
follow UniFi from the first push; the hold keeps protecting a port's own
config (state, VLANs). The loop plans the hold, and the fresh-port
withholding that has the same sticking problem, on the push with every
port's MTU left alone (`withoutMTU`); the push itself still carries the
MTU. On adoption a SONiC switch therefore moves from 9100 to UniFi's value
(1500 with jumbo off, a new device's default) with its first push. Test:
the DX010's real seeded push (`controller-10.6.109/system_cfg-dx010-held.txt`,
held 37 times live) against the switch put back to 9100 goes through --
1500 on 32 ports, RS on cage 1 from its module -- and the same push with
one port shut down on the switch is held for that port alone
(`internal/informloop/hold_sonic_test.go`).

### 10.3 FEC follows the module when UniFi says "auto"

Seen live: Ethernet0 (a 100G CWDM4 module to a UniFi device's 100G
port) stayed down until `config interface fec Ethernet0 rs`; the L2
preset writes no `fec`, Broadcom's default is none, and UniFi pushes
`port.N.fec` only when someone sets it (once in the 274 recorded pushes),
so the driver never wrote one. UniFi's own SONiC switches keep an
`AUTO_DETECT` table per port (speed and FEC from the module, §9): on
UniFi, no FEC setting means "what the optic calls for". The driver now
does the same:

- When the push has no FEC for a port (`PortDesired.FEC == nil`) and its
  cage holds a module (`TRANSCEIVER_INFO|<first interface>`), each of the
  cage's interfaces gets the FEC that module calls for at the interface's
  configured speed (table below); an explicit `port.N.fec` always wins.
- Only a mode in STATE_DB's `supported_fecs` when it lists any (the DX010:
  `none,rs`); `none` against an absent CONFIG_DB `fec` is no change, as for
  a pushed FEC.
- An empty cage, or a module or speed the table does not know: nothing is
  written (logged once), the port keeps what it has.
- Not counted by `PlanPorts`: the first-push hold and the fresh-port
  withholding ask whether the controller's config would change a port, and
  the module's FEC is the driver's reading of UniFi's "auto". (The hold
  writes nothing at all while it holds, this included.)

The module is named by its SFF-8024 compliance code as xcvrd prints it
(sonic-platform-common 202505, `sff8024.py` `EXT_SPEC_COMPLIANCE`,
`sff8636.py` `ETHERNET_10_40G_COMPLIANCE`); the FEC is what the IEEE
clause or MSA for that module requires of the host:

| Interface speed | Module | FEC |
|---|---|---|
| 100G | `100G CWDM4` (**verified live**), `100GBASE-SR4 or 25GBASE-SR`, `100G PSM4 Parallel SMF`, `100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS`, `100G AOC (Active Optical Cable) or 25GAUI C2M AOC` and `100G ACC (Active Copper Cable) or 25GAUI C2M ACC` (BER 5x10^-5), `100G SWDM4`, `100G PAM4 BiDi` | `rs` |
| 100G | `100GBASE-LR4 or 25GBASE-LR`, `100GBASE-ER4 or 25GBASE-ER`, `100G AOC or 25GAUI C2M AOC` and `100G ACC or 25GAUI C2M ACC` (BER 10^-12) | `none` |
| 40G, 10G | any | `none` |
| 25G (a 4x25G child) | `100GBASE-SR4 or 25GBASE-SR`, CA-25G-L, the 5x10^-5 AOC/ACC | `rs` |
| 25G | `25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R` | `fc` |
| 25G | `25GBASE-CR CA-25G-N or 50GBASE-CR2 with no FEC`, 25GBASE-LR/ER, the 10^-12 AOC/ACC | `none` |
| 50G (a 2x50G child) | codes 11 / 12 / 13 (CR2 with RS / BASE-R / no FEC) | `rs` / `fc` / `none` |
| anything else | | not written |

`internal/drivers/CLAUDE.md`'s "only write FEC when asked" gains this
exception for drivers whose platform reads the module.

### 10.4 The optic's media type

xcvrd's `specification_compliance` for an SFF-8636 module is Python's
`str()` of a dict (`"{'10/40G Ethernet Compliance Code': 'Extended', …,
'Extended Specification Compliance': '100G CWDM4'}"`), and the read side
put that whole string in `Optic.MediaType` (the inform's
`sfp_compliance`). It is now parsed as the Python literal it is: the
`Extended Specification Compliance` value when the 10/40G code says
`Extended`, else the 10/40G code (`40GBASE-SR4`); for any other shape (an
SFP's codes) the first value that is not `Unknown`/`Unspecified`/
`Extended`; a plain string stays as it is. That is what UniFi's own SONiC
switch reports: the ECS Aggregation's inform carries `sfp_compliance:
"100G CWDM4"` for the same module type
(`docs/fixtures/controller-10.6.106/inform-ecs-aggregation-uswf066.json`).

### 10.5 Naming the SSH key for a flag-run device

`-device-ssh` had no way to name the key, so a run from the flags fell
back to the agent, which offered the wrong key first. New: `-device-ssh-key`
/ `DUI_DEVICE_SSH_KEY`, the driver option `ssh_key` for a device run from
the flags -- a private key file, or a `.pub` that selects that one agent
identity, exactly as the config file's `options.ssh_key`. Every SSH driver
that reads `ssh_key` (sonic, podman, proxmox) takes it.

### 10.6 The out-of-band warning for an administratively down eth0

The DX010's `eth0` is cabled but taken down on purpose (§3), and keeps its
last DHCP address; the loop warned "out-of-band management port eth0
carries address …" every start. `OOBInterface` gains `AdminDown` (zero:
not known to be down, so the Arista is unchanged); SONiC sets it for an
`eth0` without the `UP` flag in `ip -j addr`. The "carries address"
warning needs the port to be administratively up (it answers on that
address the moment a cable has link) or to have carrier; the warning for
a bridge that dials the OOB address is unchanged.

**The trap a reboot set (2026-11-01).** Taking `eth0` down by hand does not
survive a reboot. After one, with `eth0` cabled to a disabled switch port
(no carrier), dhclient re-applied its recorded lease a few minutes after
boot: `eth0` got its old LAN address as `dynamic` and a `default via
<gateway> dev eth0 linkdown` route **with no metric**, which beat the
in-band `Vlan4094` default route (`metric 20`), because SONiC leaves
`ignore_routes_with_linkdown` at 0. Everything off the LAN -- the internet,
the NTP server -- was black-holed while the LAN still answered. And SONiC's
`NTP|global` (from `init_cfg.json`) names `src_intf eth0`:
`chrony.conf.j2` writes `bindacqdevice <src_intf>` only while that
interface has an address in its CONFIG_DB table (MGMT_INTERFACE for
`eth0`, VLAN_INTERFACE for `Vlan*`), so once `eth0` had a configured
address chrony bound to the dead port and every source went unreachable
(`^?`, Reach 0). Fixed on the unit by hand: `config interface ip add eth0
169.254.1.1/16` (MGMT_INTERFACE, saved: no DHCP on `eth0` ever again, and a
laptop on the OOB jack still reaches the switch) and `sonic-db-cli
CONFIG_DB hset 'NTP|global' src_intf Vlan4094` (202505 has no CLI for it),
which regenerated chrony.conf with `bindacqdevice Vlan4094`; it synced. The
arrival checklist is §3 B3 step 5. What the driver does about it:

- **NTP from the in-band interface.** When it applies UniFi's NTP servers
  it also sets `NTP|global src_intf` to the interface that carries the
  address the bridge dials, when that is an in-band interface whose
  address CONFIG_DB holds (`Vlan<native_vlan>` on a switch set up as §3
  says): `sonic-db-cli CONFIG_DB hset 'NTP|global' src_intf <iface>`, then
  `config save -y` (hostcfgd regenerates chrony.conf). `collect.py` reads
  `NTP|global` (`cfg_ntp_global`) and an interface already right is left
  alone. Nothing is written when the switch has no `NTP|global` (an image
  without `init_cfg.json`'s entry; a hash with `src_intf` alone is not
  what hostcfgd expects) or when the bridge dials `eth0` (its default is
  then right). Tests: the image's `init_cfg.json` entry
  (`ntp_global-init_cfg.txt`, read from the unit) in place of the live one,
  the driver and the DX010's real first push through the loop.
- **No out-of-band warning for a link-local address without carrier.** An
  `eth0` with `169.254.1.1/16` and no link is the setup above, not a risk;
  the "carries address" warning now needs a routable address (an IPv4
  link-local one names nothing on the LAN), and a link-local `eth0` with
  carrier gets the milder "connected" note an addressless one gets. A
  routable address on an `eth0` that is up without carrier still warns:
  that is the trap. Test: the unit's capture after the fix
  (`collect-20261101.txt`).
- **chrony left offline by its boot hook (found 2026-11-01, after a UI
  Restart).** chronyd started, and two seconds later a boot hook ran
  `chronyc onoffline`, which marks every source with no route to it
  offline (journal: "Source <address> offline" for both servers): the
  in-band route did not exist yet -- `Vlan4094` and FRR's static default
  come up about 30 s later -- and nothing runs it again, because SONiC's
  `/etc/network/if-up.d/chrony` fires only for ifupdown interfaces and
  `Vlan4094` is not one. So after every reboot NTP stayed dead (`^?`,
  Reach 0) until `sudo chronyc onoffline` was typed; it then synced within
  about two minutes. The driver now does that: `collect.py` reads chrony's
  view read-only as the login user (`chronyc -n -c sources` as
  `chrony_sources`, `chronyc -n activity` as `chrony_activity`), and when
  NTP is managed, servers are configured, the interface chrony sends from
  (`ntpSource`, the dialled in-band one) has carrier and **`activity`
  counts no source online and some offline**, it runs `sudo chronyc
  onoffline` -- idempotent, it only sets online a source that has a route
  now. The decision is `activity`'s, not `sources`': there an offline
  source reads `?` with reach 0, exactly like one that is online and has
  not answered yet -- right after `onoffline` both sources still read so
  while `activity` already counts them online. At most once per collect (a
  push and the reconcile in the same cycle nudge once), not in an apply
  that writes NTP itself (`config ntp` and the `src_intf` hset restart
  chrony anyway), not counted as a changed setting, and logged once per
  episode, with a line when they are online again. A source that stays
  offline (no route) keeps getting the nudge once a cycle, silently.
  Nothing happens without the `activity` section (older captures; chronyc
  unable to reach chronyd). Fixtures, all read from the unit and scrubbed:
  `chrony-synced.txt`; `chrony-offline.txt`, every source offline (set
  with `sudo chronyc offline`, the state the boot hook leaves);
  `chrony-after-onoffline.txt`, right after `sudo chronyc onoffline`.

### 10.7 Hardware fixtures

`docs/fixtures/sonic-202505.1238526-ff3c0c9f6/collect.txt` is the DX010's
collector run (scrubbed: the sanitiser now also rewrites hardware serials,
the LLDP neighbour's system name and the managed authorized_keys block).
The hardware-only tables are tested against it -- 10 fan modules and the
two PSU fans, two PSUs with power and temperature, five temperature
sensors (the ASIC table reads all zeros on this platform), the CWDM4
module with DOM, the FEC codeword counters, `supported_speeds`
`40000,100000` and `supported_fecs` `none,rs`, a real LLDP neighbour (a
UniFi device announcing bridge + router) as the uplink -- and the VS fixtures stay
for everything else (the control tests run on them). Both builds are the
same commit, so `config-help.txt` and `zones.txt` stay with the VS
capture.

### 10.8 Docs

§3 now records the day's facts: the 2018 image spells the installer
`sonic_installer`; the L2 preset needs `-k <hwsku>` on a fresh image; the
factory password may not be the documented one; eth0 shares the system MAC
with every VLAN interface.

### 10.9 Speed "auto" follows the module (2026-11-01)

Seen live on the DX010: a cage split to 4x25G through UniFi (a 25G speed on
the port, §7.5) stayed split when UniFi set it back to auto. On auto,
`speed()` leaves a split cage alone, by design for a cage split by hand, so
only a manual 100G joined it again. On UniFi's own SONiC switches "auto"
means what the module says (`AUTO_DETECT`, §9). Verified live the same
night, by hand: `config interface speed Ethernet8 40000` and back to
100000, and `config interface breakout -f -y Ethernet4 '4x25G[10G]'` then
`'1x100G[40G]'`, each without a swss or syncd restart and with Ethernet0
unaffected.

The rule, the companion of the FEC rule (§10.3):

- When the push carries no manual speed for a port (`autoneg` enabled:
  `PortDesired.SpeedMbps == 0`) and its cage holds a module the table
  below knows, the cage's configuration -- the number of ports it is split
  into and their lane speed -- is checked against what that module runs.
  A configuration the module runs stays as it is: a cable that can be
  either a 100G cable or a 4x25G breakout (the CR4 / CA-25G-L code, the
  AOC/ACC codes, `100GBASE-SR4 or 25GBASE-SR`) keeps a split made for it.
  Any other configuration becomes the module's native one, through a
  breakout to the mode with that many ports that runs that speed where
  platform.json has one (§7.5), else as a lane speed.
- These writes are the driver's own, like the module's FEC: `PlanPorts`
  plans without them at all, so the first-push hold and the fresh-port
  withholding never wait on them -- nor on what a module-driven join leaves
  to re-apply (a rejoined port is admin down until the plan's own
  `startup`), which would otherwise count as a change on every cycle. A
  fresh adoption changes a cage only when its module cannot run what the
  cage runs.
- No module (an empty cage), or a module the table does not know (logged
  once): the rule from before, unchanged -- a split cage stays split, an
  unsplit one runs its native (highest) speed. So the night's case, an
  **empty** cage split through UniFi and set back to auto, stays split: no
  module, no opinion. It joins with a manual 100G, or by itself once a
  module that cannot run 4x25G is in the cage.
- A breakout the module asks for is refused, with a log, for a cage in an
  aggregate or a mirror session, as every breakout is (§7.5).

xcvrd's SFF-8636 info carries no "far end broken out" field, so the
compliance code is all there is to go on. Ports x lane speed, the native
configuration first:

| Module (xcvrd compliance name) | Runs | Native |
|---|---|---|
| `100G CWDM4`, `100GBASE-LR4 or 25GBASE-LR`, `100GBASE-ER4 or 25GBASE-ER`, `100G SWDM4`, `100G PAM4 BiDi`, `100G CLR4`, `100GBASE-DR`, `100G-FR or 100GBASE-FR1`, `100G-LR or 100GBASE-LR1` | 1x100G | 1x100G |
| `100GBASE-SR4 or 25GBASE-SR`, `100G PSM4 Parallel SMF` (parallel optics break out over a fan-out), `100G AOC …` and `100G ACC …` (both BER grades) | 1x100G, 4x25G | 1x100G |
| `100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS` | 1x100G, 4x25G, 2x50G | 1x100G |
| `25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R`, `25GBASE-CR CA-25G-N or 50GBASE-CR2 with no FEC` (25G/50G-only codes: in a QSFP28 cage, a breakout cable) | 4x25G, 2x50G | 4x25G |
| `40GBASE-LR4`, `40GBASE-ER4`, `40G SWDM4` | 1x40G | 1x40G |
| `40GBASE-SR4`, `40GBASE-CR4`, `40G Active Cable (XLPPI)`, `40G PSM4 Parallel SMF` | 1x40G, 4x10G | 1x40G |
| `4 x 10GBASE-SR` | 4x10G | 4x10G |

On the DX010 that is `1x100G[40G]` at 100000 or 40000, `4x25G[10G]` at
25000 or 10000, `2x50G` at 50000. Tests: the capture's CWDM4 in cage 1
after a split and after a forced 40G (`speed_test.go`), the codes above
substituted into the capture's own compliance string, and the DX010's real
first push through the loop with cage 1 at 40G (`hold_sonic_test.go`).
Untested live: the driver's own join and lane-speed writes, and every
module but the CWDM4.

## 11. Parity with a UniFi switch (planned 2026-10-31)

Clint, 2026-10-31: keep working on the DX010 until it is on parity with a
UniFi switch. "Parity" is measured, not guessed, against the one first-party
SONiC switch on the site, an ECS Aggregation (`USWF066`, Ubiquiti SONiC 4.0.1),
three ways: its device record in the controller against the DX010's (field by
field), the capability bits each claims (decoded with unifi-emu's
`capability_bits.json`), and the `system_cfg` the controller pushes to each
(key shapes). The raw captures are site data and stay in
`local-information/ecs-reference/`; anything a test needs goes through
`scripts/sanitize-controller.py` first.

### 11.1 What the controller withholds, and why

The controller pushes a feature's keys only to a device that claims it. The
ECS's push carries 44 key shapes the DX010's lacks; apart from the LCD and
Etherlighting (hardware the DX010 does not have) every one belongs to a
feature the DX010 does not claim:

| Feature (claim) | ECS claims | DX010 claims | Keys the ECS receives |
|---|---|---|---|
| STP, RSTP (`feature_caps` 64; `stp_caps` BPDU_GUARD 1, AUTO_EDGE 2) | yes, `stp_caps` 3 | no, 0 | `switch.stp.status/version/priority`; `switch.port.N.stp.port_mode/edge/bpdu_guard` |
| Loop protection (`KEEPALIVE_LOOP_PROTECTION` 1048576) | yes | no | `switch.port.N.ld_mode`, `switch.port.N.keepalive` |
| IGMP snooping (256; `IGMP_SNOOPING_ADVANCED` 67108864; `igmp_snoop_caps` 31) | yes | no | `switch.vlan.N.igmp_*` (snooping, querier, fast leave, membership, max response, mrouter expiry, suppression), `switch.vlan.N.forward_unknown_multicast`, `switch.multicast.*` |
| DHCP snooping (128) | yes | no | `switch.dhcp_snoop.status` |
| Port isolation (4), port security (512) | yes | no | `switch.port.N.port-security` (isolation keys only when set) |
| LLDP-MED (16) | yes | no | `switch.port.N.lldpmed.opmode/topology_notify` |
| Egress rate limit (32) | yes | no | none while unset |
| L3 routing (1), BGP (33554432), `max_l3_intf` 64, `max_static_routes` 256 | yes | no | `switch.routing_enabled` only while no L3 network is on the switch |
| Custom ACLs (`max_custom_ip_acls`/`mac_acls`/`global_acls` 512) | yes | no | none while no rule exists |
| MC-LAG (16777216) | yes | no | needs a second switch; out of scope |
| LACP, FEC, jumbo, storm control | yes (storm by errdisable) | yes | the same |

Reported state the ECS carries and the DX010 did not (the LACP and
hardware items are reported since 2026-11-01, §11.4): `stp_version`,
`stp_priority`, `root_switch`, per-port `stp_edge_state`,
`stp_bpdu_guard_enabled`, `stp_pathcost`; `ld_status`;
`multicast_router_mode`; on an aggregate `lacp_state[]`, `partner_system_id`,
`aggregate_members`, `lag_idx`, `link_aggregation_groups[]`; `fan_table[].direction`;
`psu_table[]` detail (fan, temperatures, `psu_caps`, `psu_type`, EEPROM);
`mac_table_capability` (capacity, count, thresholds); `ipv6[]`;
`displayable_version`; `sfp_rev`. The ECS's `fw2_caps` carries
`SONIC_BASED_FIRMWARE` (8192) and `SUPPORT_FILE_UPLOAD` (65536); what the first
changes in the controller is unknown -- claim it only after a capture shows it
changes nothing that breaks the system_cfg path.

### 11.2 How each gap closes on the DX010

| Gap | Mechanism | Image | Work |
|---|---|---|---|
| **RSTP** | Ubiquiti's own design (§9): **mstpd** on the kernel bridge `Bridge` (`stp_state 2` hands it to mstpd through `/sbin/bridge-stp`), BPDUs to the CPU by a runtime CoPP trap (`COPP_TRAP\|stp`, trap id `stp`, which `copporch` knows), and a **manager** on the switch that mirrors mstpd's port states into the stock `stporch` through APPL_DB (`STP_VLAN_INSTANCE_TABLE` VLAN→instance -- one instance per VLAN as built, §11.5 --, `STP_PORT_STATE_TABLE`, `STP_FASTAGEING_FLUSH_TABLE` on a topology change). Both are compiled into the stock swss; community `stpd` (PVST only) is not used. The driver installs and supervises both (the Proxmox driver keeps files on a node the same way), maps `switch.stp.*` and the per-port keys onto mstpd, and reads mstpd for `root_switch`, roles, states, path costs and edge state. Claims STP, BPDU guard and auto edge only once live-verified. **Built 2026-11-01; the mechanism verified live by hand that night and the driver's own path live after the deploy; on by default (`l2d: off` to opt out): §11.5.** | stock 202505 | large |
| **Loop protection** | Ubiquiti's `MAC_MOVE_GUARD` semantics (threshold 10 moves in 5 s → errdisable for 150 s) on the switch, from FDB move events, driven by `ld_mode`; `ld_status` reported. With RSTP in place this is the second line of defence, as on the ECS. **Built 2026-11-01 in disunified-l2d, driven by `switch.port.N.keepalive` (not `ld_mode`); verified live by hand that night: §11.5.** | stock | medium |
| **LACP state** | teamd's per-member state, which tlm_teamd copies out of `teamdctl <pc> state dump` into STATE_DB `LAG_MEMBER_TABLE` → `lag_member`, and on the aggregate's lowest member `aggregate_num_ports`, `lacp_state[]`, `partner_system_id`. `aggregate_members`, `lag_idx` and `link_aggregation_groups[]` are the controller's own (from its port overrides; the ECS's inform carries none of them). Needed for the planned two-cable aggregate from the core. | stock | **built 2026-11-01** (§11.4), held to the ECS's own inform on the ECS's own tables; live on the DX010 once the second cable is in |
| **Hardware tables** | PSU, fan, MAC-table capacity, IPv6 and firmware fields from pmon's STATE_DB tables, `crm`, the kernel. | stock | **built 2026-11-01** (§11.4), fixture- and contract-tested on a second DX010 capture |
| **DHCP snooping, isolation, port security, custom ACLs** | SONiC ACL tables (`ACL_TABLE`/`ACL_RULE`, `sonic-acl.yang`), the same shape Ubiquiti uses (`global_ip_acl`, `global_mac_acl`, §9); each needs its push captured from the UI first. | stock | medium each |
| **LLDP-MED** | SONiC's `lldpd` speaks LLDP-MED (`configure med ...`); the `lldp` container's config is the write path. | stock | small |
| **Egress rate limit** | `SCHEDULER` + `PORT_QOS_MAP` (YANG present, §7.2); capture the push first. | stock | small |
| **L3 routing, static routes, BGP** | VLAN interfaces and FRR are native to SONiC; capture an L3 network and a static route pushed from the UI first. | stock | medium |
| **IGMP snooping** | §8: the Micas l2mcorch/l2mcd series on a **custom 202511 image**. Needs a build host (sonic-buildimage wants ~200 GB disk, 8+ cores, Docker). | custom | large |

Order: RSTP and loop protection first -- the DX010 is to become the LAN's hub
with the aggregation switch as the backup path, so it must run spanning tree
before any second path is cabled. Then LACP state (the core aggregate), then
the hardware tables, then the ACL family, LLDP-MED and rate limits, then L3,
and the IGMP image in parallel once a build host exists.

### 11.3 Rules for live work on the DX010

The DX010 has one link and no one on site: Ethernet0 is the only path, eth0 is
administratively down on purpose (§10.6), and the console is not attached.
Every step that can stop Ethernet0 forwarding (spanning tree above all) runs
under a dead-man timer on the switch (`systemd-run --on-active=300`) that
restores forwarding on Ethernet0, removes what the step added, and brings eth0
up; the timer is cancelled only after a fresh connection over Ethernet0 and the
bridge's own collect both succeed. Probes never contain a state-changing
command, and a wait loop starts only after what it waits for is confirmed
started. Captures come from the switch with read commands only, or from the
ECS (`admin@` the aggregation, read-only) for UniFi's own behaviour.

### 11.4 LACP state and the hardware tables (built 2026-11-01)

The reference is the ECS's own inform, printed with `mca-dump` on the
switch (`controller-10.6.109/inform-ecs-aggregation-uswf066.json`). Where
each value comes from on the SONiC side was worked out by a read-only look
at the ECS's tables in the same minute; that look is described here in our
own words and **nothing read off the ECS is kept in the tree** (Clint,
2026-11-01: no text or code copied from it that does not come from an open
source upstream). The DX010 side is a second collector run with the new
sections (`sonic-202505.1238526-ff3c0c9f6/collect-hwtables.txt`, 0.84 s for
the whole run).

**New `collect.py` sections** (all read-only, no sudo): `state_lag_member`
(STATE_DB `LAG_MEMBER_TABLE|*`), `crm` (COUNTERS_DB `CRM:STATS`) and
`hwmon` (each `/sys/class/hwmon` chip's name, its device -- i2c
`<bus>-<addr>` for a PMBus supply -- and its `*_input`/`*_label` files).
Captures from before lack them; they read as empty (`laterSections`).

**LACP.** The ECS's inform puts `lag_member: true` on every member and
the aggregate on its lowest member: `aggregate_num_ports`, `lacp_state`
(per member `{active, member_port, speed}`), `partner_system_id`, and an
empty `service_mac_table` (on every head in both ECS captures, nowhere
else). No top-level aggregate table: `aggregate_members`, `lag_idx` and
`link_aggregation_groups` in the controller's record come from its own
port overrides. The source is teamd: tlm_teamd (sonic-swss, the same code
in 202106 and 202505) copies each member out of `teamdctl <pc> state dump`
into STATE_DB `LAG_MEMBER_TABLE|<pc>|<member>` (`link.up`,
`runner.selected`, `runner.aggregator.selected`,
`runner.partner_lacpdu_info.{system,state,port}`, ...), so the collector
needs no `docker exec`. A member is **active** when teamd enables it --
selected, its aggregator the selected one, the partner's SYNCHRONIZATION
bit (0x08) set (libteam's `lacp_port_should_be_enabled`) -- and link up.
The partner is `runner.partner_lacpdu_info.system`; teamd's
`00:00:00:00:00:00` (no LACPDU yet) is no partner. The member's speed is
the port's. Tests: the driver's reading of teamd's records against
upstream SONiC's own test data (`docs/fixtures/sonic-utilities-202505/`,
from sonic-utilities 202505, Apache-2.0: a bundled member, one without
link, one defaulted with no partner; `hwtables_test.go`), and the payload
from the DX010's own capture with its first two cages put in an aggregate
at the model level -- the core aggregate before its second cable, one
member bundled with the core as partner, one not -- held to the ECS
inform's aggregate head key for key, the aggregate keys on its member and
the `lacp_state` rows' keys and types (`contract_sonic_hw_test.go`,
`TestWireContractSonicAggregate`). Live on SONiC: never -- the DX010 has
one cable. The Arista reads the same bundling from `show port-channel summary`
(`lagMember`, already captured); its partner id needs `show lacp
neighbor`, which has no capture yet.

**Fans.** The ECS lists one row per fan drawer (`Fan1`..`Fan5` for its
`Drawer 1..5`), `speed`/`target_speed` the PWM duty in percent, and
`direction` as SONiC's thermalctld names it (`exhaust`); its supplies'
fans are in the PSU rows, not here. The DX010's community thermalctld
gives `speed`/`speed_target` in percent per rotor (two per drawer,
`drawer_name` `Drawer1..5`) and `direction: intake` (F2B, port side in):
a drawer is the mean of its present rotors, the highest target, OK only
while every rotor is present and healthy, `present: false` when none is.
A fan in no drawer named after a supply (`PSU-1 FAN-1`) belongs to the
supply.

**Supplies.** The ECS's row: `psu_idx`, `label`, `present`, `online`,
`power`/`watt` (the supply's **input** power: its PSU_INFO power matches
the chip's `pin`, not `pout`), `maxwatt`/`power_capacity`, `temp`
`{"0","1","2"}` (its PSU_INFO `temp_list`, the chip's three sensors),
`fan` `{"0": <RPM>}`, `psu_caps` 4 (HOT_SWAPPABLE in unifi-emu's
vocabulary; POWER_METER 2 is not claimed though it meters), `psu_type`
131072 (an unnamed vocabulary), `psu_eeprom_valid`, and `serial_no`,
`part_no`, `revision` 0, `datecode`, `invol_type`, `max_invol`,
`min_invol` sent empty although its STATE_DB knows serial and model.
The DX010's community psud keeps one temperature and the output power
(`pout1`, the platform's `get_power`), so the driver reads the supply's
own PMBus chip as well: the platform's `psu.py` maps `PSU-1`/`PSU-2` to
i2c `10-005a`/`11-005b` (`psuChips`; hwmon numbers move across boots, the
device does not). From it: three temperatures, the fan's RPM, the input
power. The row sends every ECS key but `psu_type`, with the DX010's real
model and serial, `psu_caps` 4 from `is_replaceable`, `power` the input
power (78.5 W against psud's 67.5 W output). `total_max_power` is the
supplies' summed draw, as the ECS sends it (116 beside 54.4 + 62.3 W; a
PoE switch sends its budget there); it was their capacity (1800 W).

**MAC table.** `mac_table_capability` comes from CRM: capacity = used +
available (the DX010: 8044 + 152 = 8196 -- Tomahawk's L2 table), count =
used; the warning/critical thresholds keep the UniFi ratios the
presentation already used (66 % / 79 %, the ECS's 79000/95000 of
120000). The ECS reports a fixed 120000 against its CRM's 131072: its
capacity is Ubiquiti's number, ours is the ASIC's. The VS capture has no
`crm` section (the omission stays for it).

**Small fields.** `ipv6`: the dialled interface's IPv6 addresses,
`<address>/<prefix>`, global first (the ECS: its management bridge's
SLAAC and link-local; the DX010's Vlan4094 has only its link-local).
`displayable_version`: a UniFi version without its build number (the
ECS's `4.0.1.995` -> `4.0.1`), any other version as it is (the DX010's
build id). `sfp_rev`: xcvrd's `vendor_rev`, padding trimmed (`1` on the
CWDM4), `""` on an empty cage as the ECS sends; every driver now sends
the key on its optical ports, `""` where it does not read revisions.

**Not claimed, and why.** `psu_type` (vocabulary unknown); `fw2_caps`'s
`SONIC_BASED_FIRMWARE` (§11.1); `root_switch`/`stp_priority` until
spanning tree runs (§11.2).

**Seen on the way:** the controller sanitiser's IPv6 pattern could not
match `fe80::…` (the `::` right after `fe80`), so link-local addresses
went through unscrubbed; it now matches every IPv6 shape and maps a
link-local into `fe80::N`. The same gap had let 237 link-local addresses
(84 of them EUI-64, the low half a real MAC) and one global address with
the site's prefix into six fixtures committed since 2026-09-20 (the
10.6.106 informs, the podman and proxmox captures): rewritten in place,
`sanitize-podman.py`/`sanitize-proxmox.py` fixed the same way, and
`check-site-info.sh` now refuses any IPv6 outside `2001:db8::/32`,
`fe80::N` and Docker's `fd00::/8`. Git history still holds the originals.
`sanitize-sonic.py` now rewrites the DNS
domain of the switch's own LLDP name (`<hostname>.<domain>` once a DNS
domain is set).

### 11.5 RSTP and loop protection: the design as built (2026-11-01)

Built on branch `claude/sonic-rstp-66e3ee` against the SONiC 202505 source
(`sonic-swss` `orchagent/stporch.cpp`, `copporch.cpp`, `portsorch.cpp`,
`fdborch.cpp`, `cfgmgr/coppmgr.cpp`), mstpd 0.2.0's source, read-only probes of
the DX010 and a read-only look at the ECS Aggregation's `stp` container. Off
by default behind the driver option `l2d: on` until the driver's own path
passed live on the DX010 (2026-11-01, below); **on by default since**, with
`l2d: off` as the escape for a switch the bridge must install nothing on.

**Pieces on the switch** (installed and kept current by the driver, `l2.go`;
the binaries in the image's overlay, the config in `/etc/sonic`, which an image
upgrade keeps):

| Piece | Path | What it is |
|---|---|---|
| mstpd, mstpctl 0.2.0 | `/usr/local/sbin/` | built **on the switch** from the upstream release tarball (sha256-pinned) in a throwaway `alpine:3.20` container (digest-pinned), statically linked against musl (`build-mstpd.sh`; ~40 s on the DX010's Atom, 149 KB + 121 KB). Debian 12 has no mstpd (Debian's 0.2.0 needs glibc 2.38; bookworm has 2.36), and building it on the switch means the bridge redistributes no GPL binary. A transient unit (`disunified-mstpd-build`) runs the build so it outlives the SSH session; the next collects read its outcome. With libtool installed, `-static` silently links against musl's loader, which Debian lacks: the build refuses a binary with an `INTERP` header. |
| `disunified-l2d` | `/usr/local/sbin/disunified-l2d` (`l2d.py`) | the manager, Python with swsscommon, run as root by `disunified-l2d.service` |
| the kernel hook | `/usr/sbin/bridge-stp` (`bridge-stp.sh`; `/sbin` is `usr/sbin` on Debian 12) | answers 0 to `Bridge start\|stop` and 1 for any other bridge. Without the file the kernel runs its own 802.1D STP on `Bridge` (`br_stp_start`: no helper = kernel STP), so l2d refuses to set `stp_state` unless it exists, and turns it off again if the bridge does not read back `2` |
| config | `/etc/sonic/disunified/l2.json` | written by the driver: STP on/off, version, priority, per port `enabled`/`edge`/`bpdu_guard`, the loop-protected ports; `deadman` set by the switch when the dead-man fired |

**Kernel side.** `Bridge` (vlanmgrd's, VLAN filtering on, every switched port
and vlanmgrd's `dummy` a member) runs with `stp_state 0`. l2d writes `1`; the
kernel calls the hook, which answers 0, so the bridge is in user-space mode
(`2`) and l2d runs `mstpctl addbridge Bridge`. In user-space mode the kernel
changes no port state by itself (`br_port_state_selection`), and mstpd sets
each port's state over netlink. `dummy` is set admin edge with BPDU filter.
SONiC's ebtables already drops `-d BGA` in `FORWARD`, so the kernel never
forwards a BPDU between ports, STP on or off. The hostif netdevs report their
speed (`/sys/class/net/Ethernet0/speed` = 100000), so mstpd's 802.1t path
costs need no help (200 at 100G, as the ECS's mstpd shows).

**BPDUs.** The image's `/etc/sonic/copp_cfg.json` has no `stp` trap and the
ASIC lists `stp` in `COPP_TRAP_CAPABILITY_TABLE`; l2d writes
`COPP_TRAP|stp {trap_ids stp, trap_group queue4_group1, always_enabled true}`
to CONFIG_DB before mstpd starts (`always_enabled` installs it without
`FEATURE|stp`, `coppmgr.cpp`; `queue4_group1` is LACP's: trap, not copy, 6000
pps) and deletes it with STP off, so the switch floods its neighbours' BPDUs
again and stays transparent to their spanning tree. Probed: no BPDU reaches
Ethernet0's netdev today (`tcpdump`, 6 s, nothing).

**Into the ASIC: StpOrch, and why each VLAN has its own instance.** The stock
orchagent always builds `StpOrch` on `STP_VLAN_INSTANCE_TABLE`,
`STP_PORT_STATE_TABLE` and `STP_FASTAGEING_FLUSH_TABLE` (APPL_DB). Facts from
`stporch.cpp` that shape everything:

- `STP_PORT_STATE_TABLE:<port>:<instance> {state}` (0 disabled, 1 blocking,
  2 listening, 3 learning, 4 forwarding; 0-2 are SAI blocking). An instance id
  maps to a SAI STP object **created on first use** (`m_stpInstToOid`); the
  switch's default instance is never one of them.
- `STP_VLAN_INSTANCE_TABLE:Vlan<id> {stp_instance}` moves the VLAN into that
  object. A port with no state there yet is not forwarding, so l2d writes every
  bridge port's state into a VLAN's instance first, waits until ASIC_DB holds
  that many `SAI_OBJECT_TYPE_STP_PORT` objects, and only then maps the VLAN.
  Mapping Vlan4094 first would have cut the DX010 off.
- Deleting a VLAN (`portsorch` `removeVlan` → `removeVlanFromStpInstance`)
  calls `removeStpInstance`, which **removes every port state of that
  instance**. With one instance for all VLANs, the driver deleting any UniFi
  network would strip the states the management VLAN depends on. So **each
  VLAN gets its own instance, id = the VLAN id**, all holding the same
  (RSTP) port states; a port-state change is one write per VLAN.
- A port-state key naming a port orchagent does not know makes
  `doStpPortStateTask` `return` without erasing it: that table's queue stops
  for good. l2d writes only ports present in APPL_DB `PORT_TABLE`/`LAG_TABLE`
  and ready in STATE_DB, never `dummy`, and drops the keys of a vanished port
  or VLAN from APPL_DB directly (a plain `Table` delete, which orchagent never
  sees: it already removed the objects with the bridge port or the VLAN).
  `portsorch` removes a port's STP ports with its bridge port.
- `STP_FASTAGEING_FLUSH_TABLE:Vlan<id> {state true}` flushes the VLAN's dynamic
  FDB in the ASIC (`flushFdbByVlan`); every write of `true` is a new flush.

The ECS's own glue (`ubnt-stpmgrd`, read for reference, not copied) writes
instance 0 with no VLAN mapping and per-port flush keys: Ubiquiti's orchagent
is patched for both. The community one is not, hence the per-VLAN instances.

**Reacting in milliseconds.** l2d listens on rtnetlink (`RTMGRP_LINK`,
`AF_BRIDGE` messages, `IFLA_PROTINFO/IFLA_BRPORT_STATE`) and writes a changed
state into every VLAN's instance at once; a full reconcile runs every 2 s
(a port that joined `Bridge`, a new or deleted VLAN, an orchagent restart --
fewer STP ports in ASIC_DB than written -- or APPL_DB flushed). mstpd flushes a
port's kernel FDB on a topology change by writing `brport/flush` in sysfs, and
the kernel announces every bridge-port sysfs write with an `RTM_NEWLINK`
(`brport_store` → `br_ifinfo_notify`): a notification whose state did not
change is taken as that flush and turned, coalesced over 20 ms, into a flush
of every mapped VLAN in the ASIC. (Over-flushing costs a moment of flooding;
missing one would black-hole a moved MAC until it ages out.)

**Teardown.** STP off in the config, or `disunified-l2d --teardown`: every
`STP_VLAN_INSTANCE_TABLE` key deleted through the producer (each VLAN back to
the default instance, which forwards everywhere; orchagent removes the
instances), the port keys dropped, mstpd stopped, every kernel bridge port set
forwarding (the kernel leaves user-space ports as they were when STP stops),
`stp_state 0`, the trap removed. Stopping the service does **not** tear down
(`KillMode=process`): mstpd keeps running and the next start adopts it, so a
restart or an upgrade of l2d never runs the LAN without spanning tree.

**The controller's keys** (the ECS's real push,
`fixtures/controller-10.6.109/system_cfg-ecs-aggregation.txt`, scrubbed):
`switch.stp.status/version/priority` → l2d's `mstpctl setforcevers` (stp,
rstp, mstp) and `settreeprio Bridge 0 <priority/4096>`; per port
`stp.port_mode=disabled` → BPDU filter + admin edge (the port forwards and
ignores BPDUs, as `ubnt-stpmgrd` does), `stp.edge` auto/enabled/disabled →
`setportautoedge`/`setportadminedge`, `stp.bpdu_guard` → `setbpduguard`
(mstpd shuts the port's netdev on a BPDU; the bridge port goes disabled and
StpOrch blocks it in the ASIC; the port reports `errdisabled: bpduguard`, the
UI's BPDU-guard anomaly; a port toggle in UniFi brings it back). A LAG member's
settings go to its PortChannel, the bridge port. The controller pushes these
only to a device claiming `feature_caps` STP (64) and `stp_caps` BPDU_GUARD |
AUTO_EDGE (3).

**Loop protection.** The UI's Loop Protection is `switch.port.N.keepalive`
(`port_keepalive_enabled` in the device record), pushed only with
`KEEPALIVE_LOOP_PROTECTION` (1048576) claimed. `ld_mode`, which the brief
named beside it, is not loop protection: in the ECS's push it appears on
exactly the 15 ports with a forced speed, always `disabled`, and `ld_status`
is a port-override field the controller keeps, never in a switch's inform
(the ECS's own inform, `fixtures/controller-10.6.106/inform-ecs-aggregation-uswf066.json`,
carries neither). The ECS runs its MAC-move guard (10 moves in 5 s disable
the port for 150 s) on all 54 ports with every `keepalive` disabled, and
lists keepalive as an errdisable cause of its own: Ubiquiti's guard is not
driven by the toggle. Here the toggle decides,
as everything else follows UniFi: l2d subscribes to STATE_DB `FDB_TABLE`
(fdborch writes the new port on a MOVE), and a MAC that moves 10 times in 5 s
takes the protected port it moved to down for 150 s -- APPL_DB `PORT_TABLE
admin_status down`, so CONFIG_DB, the saved config and the driver's own
reconcile keep the operator's setting -- never the STP root port nor the port
the default gateway is learned on (the management path; the other side of the
loop is taken instead, or nothing, logged). The port reports
`errdisabled: loop-protect (…)`, the UI's loop anomaly (512). A held port is
recorded in STATE_DB `DUI_LOOP_GUARD_TABLE` and restored by the next l2d start
if l2d dies meanwhile.

**Read side.** l2d publishes mstpd's view every 2 s: STATE_DB
`DUI_STP_TABLE|Bridge` (version, bridge and root ids, root port, topology
changes and time since the last) and `DUI_STP_PORT_TABLE|<port>` (role, state,
path cost, admin/auto/oper edge, BPDU guard and its error, BPDU filter,
counters). `collect.py` reads those tables (`l2_bridge`, `l2_port`,
`l2_guard`) and its own `l2d` section (installed files' sha256, mstpd's
version, the units' states, docker, `stp_state`, the config). The model gets
`stp_version`, the priority (the bridge id's nibble × 4096), `root_switch`,
the bridge's own topology-change count and the last change's time (as the ECS
reports `stp_last_topology_change_timestamp`), and per port `stp_state`,
`stp_role`, `stp_pathcost`, `stp_edge_port` (configured or detected), BPDU
guard and the faults above. On the wire the ECS line reports `stp_priority`
as the step, priority/4096 (the ECS Aggregation: 1 at 4096 in its inform of
2026-09-19, 2 at 8192 on 2026-10-31), so the payload sends the step for
`USWF066`/`USWF07D` and the priority itself for every other model (the
USW-XG16 sends 32768; `internal/device` `stpPriorityWire`).

**Settings reach mstpd by reconciliation.** Every 2 s l2d reads mstpd's own
report (`mstpctl -f json showbridge` and `showportdetail`, which lists every
bridge port, a down one included), compares the force version, the priority
nibble and each port's BPDU filter, admin edge, auto edge and BPDU guard with
the config, sets only what differs, and logs it (`settings: priority 32768 ->
61440; Ethernet8 bpdu guard on`); a refused `mstpctl` call is retried on the
next pass. A reload (`systemctl reload`) only wakes the loop. Until
2026-11-01 the settings were applied on a reload alone, and the flag was
cleared at the end of the sync it started: the driver writes the config twice
for one push (the bridge from `ApplyDevice`, the ports from `ApplyPorts`, a
second apart), the second reload arrived during the first one's sync, and
Ethernet8's BPDU guard never reached mstpd while the priority did (seen live;
`l2d_reconcile_test.go` runs the reconcile on the live capture's mstpd state
with the config the driver writes for that push, through
`testdata/l2d_reconcile.py`). Whether the ECS's
`stp_last_topology_change_timestamp` is UTC or local time is unconfirmed (no
zone in the string, and the two reads of it could not be lined up); the
payload sends UTC.

**Driver.** `ApplyDevice` records the bridge-level intent and `ApplyPorts` the
ports' (the loop calls them in that order); the config is written once both
are known. mstpd missing: the build is started and nothing else is installed
until it is there (retried every 10 minutes after a failure). Then one
`bash -s -- disunified-l2` batch installs what differs (by sha256), writes the
config and restarts or reloads the service. **A first enable runs under a
180 s dead-man timer** armed in the same batch before the service starts
(`systemd-run --on-active=180 … disunified-l2d --deadman`: stop, tear down,
mark `deadman` in the config, start again for loop protection). The driver
then dials the switch afresh (`Redial`), checks that mstpd runs, and cancels
the timer through that connection. If the timer fires, the driver does not
enable again until the controller has turned STP off and on. Claims: STP,
BPDU guard, auto edge, port cost and loop protection where the switch has
mstpd or docker, `l2d` not off, and no `FEATURE|stp` (an image with the
community daemon keeps the PVST path). Tests: `l2_test.go` (capabilities,
the build, the install and the dead-man's arming and cancelling, port
settings, STP off, the dead-man path and its re-arming, a failed build) on
`fixtures/sonic-202505.1238526-ff3c0c9f6/collect-l2.txt` -- the DX010
collected read-only with this collector before anything was installed -- and
`internal/informloop/replay_sonic_l2_test.go` (the DX010's real push with the
ECS's spanning-tree keys through the loop).

**Live status (2026-11-01).** The mechanism was verified by hand on the DX010
(build 1238526, 04:28-04:44 MST, Clint's approval in the session), each step
under §11.3's 300 s dead-man timer, cancelled only after a fresh SSH
connection over Ethernet0 and the bridge's own next collect (its kept SSH
connection's `lastrcv` in `ss -ti`) both succeeded; the bridge never lost the
switch:

| Step | Seen |
|---|---|
| mstpd built on the switch with `build-mstpd.sh` (twice, by hand) | static, identical BuildIDs, `mstpd -V` 0.2.0; with libtool installed the link went dynamic against musl's loader, hence the `INTERP` check |
| `COPP_TRAP\|stp` alone | `COPP_TRAP_TABLE\|stp` installed, `queue4_group1` = `bgp,bgpv6,lacp,stp`; the core's RSTP BPDUs (802.1w, every 2 s) on Ethernet0's netdev (tcpdump shows them tagged 4094; mstpd parses them, below) |
| l2d starts mstpd on `Bridge` | `stp_state` 2; designated root the ECS (`2.000.<its MAC>`, 8192), root port Ethernet0, path cost 200; Ethernet0 blocking → learning → forwarding 1.5 s after start |
| port states, then the VLAN | after the 6 s settle: a new SAI STP object, 32 `STP_PORT` objects (Ethernet0 FORWARDING, the 31 down ports BLOCKING), Vlan4094's `SAI_VLAN_ATTR_STP_INSTANCE` on it; the FDB stayed full (145 entries), the gateway answered |
| l2d restarted, mstpd kept (`KillMode=process`) | the same mstpd adopted, no port transition, the mapping re-asserted (a restart that also restarts mstpd cost Ethernet0 0.4 s) |
| a topology change: a veth port added to `Bridge` (kernel only), non-edge, forwarding after the timers | mstpd's TC count 1 → 2, mstpd flushed Ethernet0, l2d wrote `STP_FASTAGEING_FLUSH_TABLE:Vlan4094`: the ASIC's FDB 150 → 20 entries, 58 six seconds later |
| BPDU guard on the veth port, one inferior BPDU (root 61440, never a root candidate) injected from its peer | `bpdu guard error yes`, the port shut (admin down); `ip link set up` cleared the error |
| auto edge on the veth port | edge and forwarding 3 s after link up with no BPDU; one BPDU cleared oper edge; the root never moved |
| loop protection on the two empty cages Ethernet4/Ethernet8, a test MAC moved between them 12 times in STATE_DB `FDB_TABLE` | the port it moved to went down through APPL_DB (`admin_status down`, CONFIG_DB still `up`), `DUI_LOOP_GUARD_TABLE` recorded the MAC and the peer, restored after the hold (20 s set for the test; ~22 s with the 2 s tick) |
| a recapture with l2d running and a port held (`collect-l2-running.txt`) | the read side's fixture: `l2_read_test.go`, `internal/device/contract_sonic_stp_test.go` |
| `disunified-l2d --teardown` (with a port still held) | Vlan4094 back in the default instance, no `STP_PORT` objects, the instance and the APPL_DB keys gone, mstpd stopped, `stp_state` 0, Ethernet0 forwarding, the trap removed, the held port restored at once |

Found live and fixed: the false "ASIC lost STP ports" warning on the first
sync (fresh writes not yet processed by orchagent are no sign of a restart:
now only a drop below the count seen at mapping), a "topology change" logged
while no VLAN was mapped (mstpd flushes every port when it takes the bridge),
the teardown leaving the running view beside `enabled no` in `DUI_STP_TABLE`
(a set merges), and `docker rmi name:tag@digest` removing nothing. The switch
was left as found: every file, unit, timer and `DUI_*` key removed, the image
deleted; the saved `config_db.json` never held the trap.

**The driver's own path, live (2026-11-01, main 6803b78 deployed with `l2d:
on` for the DX010).** After a force-provision the push carried `switch.stp.*`
(the device claimed STP, BPDU guard, auto edge and loop protection), and the
bridge logged `sonic apply l2: building mstpd 0.2.0 on the switch` (12:04:43
UTC), `install disunified-l2d … first enable under a 3m0s dead-man timer`
(12:05:51) and `spanning tree up, the switch answers a fresh connection;
dead-man timer cancelled` (12:05:58). mstpd: designated root the ECS, root
port Ethernet0, cost 200, rstp; the controller showed `root_switch` the ECS
and port 1 root, forwarding, 200; no loss. From the UI, priority 61440 and,
on cage 3 (empty), BPDU guard and loop protection: the config was written
right and mstpd took the priority, but not the port's BPDU guard -- the lost
reload above, fixed by the reconciliation. Re-tested from the UI after the
fix was deployed (main 4eee9d8, 12:39 UTC): the bridge replaced the manager by
checksum (`sonic apply l2: update disunified-l2d`) while mstpd kept running
(started at boot, no port transition); BPDU guard on cage 3 from the UI was
logged `settings: Ethernet8 bpdu guard on` and shown by mstpd within one
push, and off again the same way. Two reboots through the UI's Restart
brought mstpd back with Ethernet0 the root port. With that the option went:
RSTP is the driver's default.

### 11.6 Live checks on the DX010, 2026-11-01

Run from the controller against the deployed bridge (the 2026-10-31 build),
on empty cages, every override restored to its pre-test value afterwards
and a zero-change push confirmed. The uplink stayed up throughout; the
breakout ran under a dead-man timer that would have joined the cage back
and brought eth0 up had the uplink dropped.

| Feature | What UniFi sent | What the switch did |
|---|---|---|
| Locate | `set-locate`, `unset-locate` | the status LED `dx010:green:stat` toggled about once a second, steady green after |
| Forced speed | cage 3 manual 40G, then auto | `config interface speed Ethernet8 40000`, then `... 100000` (auto on a QSFP cage is its native speed) |
| Breakout | cage 2 manual 25G | `config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null`; four children `Ethernet4`-`Ethernet7` (aliases `Eth2/1`-`Eth2/4`, lanes 69-72) admin up, MTU 1500, untagged in the native VLAN; no prompt, no swss or syncd restart |
| Join | cage 2 auto, then manual 100G | auto changed nothing (the deployed build left a split cage split; §10.9 now has auto follow the module); manual 100G: `'1x100G[40G]'` and `config vlan member add -u 4094 Ethernet4` |
| Restart | the UI's Restart (`cmd/devmgr restart`) | `reboot requested`, the reload issued about 50 s later (SONiC's reboot script stops the services first), SSH back 112 s after the box went down, on the same image with the saved config intact; the bridge logged one failed collect and recovered; the next push wrote nothing. eth0 came back up on DHCP and had to be taken down again (§10.6) |

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
