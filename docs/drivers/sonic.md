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

**Since 2026-11-02 the DX010 runs this repo's patched 202511 image (§8) with
the bridge on the switch itself (§12).** This file is the working record, in
the order things happened. The reader's map -- the two deployments (the
bridge over SSH, or on the switch) and the image patches by what each
enables -- is [`sonic-running.md`](sonic-running.md).

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
| identity | `show version` (SONiC build, platform, HwSKU, ASIC, serial), `CONFIG_DB DEVICE_METADATA|localhost` (hostname, mac, hwsku, platform); the chassis serial (22 characters on the DX010) is logged, and the MAC's hex digits are reported as the serial, as on every bridged device (`device.SerialFor`, CLAUDE.md §5) |
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
build id (logged, never reported: claimed as an ECS the switch shows
UniFi firmware numbers only, `docs/unifi-models.md`), uptime, management interface as the
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
   - **On 202505 only, give it a static link-local address**: `sudo config
     interface ip add eth0 169.254.1.1/16`, `sudo config save -y`. That is
     MGMT_INTERFACE, so SONiC runs no DHCP on `eth0` again, and no lease
     dhclient recorded can bring back a default route through it (202505's
     DHCP route had no metric, §10.6); a laptop on the OOB jack (169.254/16)
     still reaches the switch. `sudo ip link set eth0 down` alone does not
     survive a reboot. **On 202511 leave eth0 on DHCP**: the CLI writes the
     row without a gateway (`{"NULL": "NULL"}`), and sonic-yang, which every
     `config interface breakout` runs over the whole CONFIG_DB, refuses
     every breakout while it is there (§11.8); 202511's template gives a
     DHCP eth0 `metric 202`, behind the in-band default's 20. The rescue
     path is then eth0 plugged into a LAN port, which takes a lease. A unit
     upgraded from 202505 with the row has it removed (`sudo config
     interface ip remove eth0 169.254.1.1/16`, `sudo config save -y`, then
     `sudo systemctl restart interfaces-config`, which hostcfgd skips for
     a field-less row: §11.11). If a row is kept, it needs `MGMT_PORT|eth0`
     too (`sudo sonic-db-cli CONFIG_DB hset 'MGMT_PORT|eth0' alias eth0
     admin_status up`), which the driver adds itself before a breakout.
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
  `LearnedAt` and `LastSeen` zero (no timestamps): the loop's learn-time
  tracker gives `uptime`, `age` is 0 (`docs/mac-table-times.md`).
  `MACTableUsed` = entry count; capacity unknown.
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
| breakout (speed on lane 1) | `config interface breakout <cage> <mode>` with the mode from `platform.json` (`1x100G[40G]`, `4x25G[10G]`, `2x50G`…) | `BREAKOUT_CFG\|<cage> brkout_mode`; child ports `Ethernet<n+1..3>` appear in `PORT` | **hardware only** (`platform.json` absent on the VS): refused with a log where absent; on the DX010 every mode verified live on an empty cage (§11.6, §11.11); a speed no mode runs that every current lane supports (1G on a 25G lane, STATE_DB `supported_speeds`) is a lane speed |
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
| DHCP snooping (`DHCPSnooping`, `DHCPGuards`) and the switch ACL rules (`IPACLs`, `MACACLs`) | `config acl add table` / `remove table`, `sonic-db-cli CONFIG_DB hset\|del 'ACL_RULE\|DUI_…'` | the driver's own `ACL_TABLE\|DUI_DHCP_GUARD`, `ACL_TABLE\|DUI_IP_ACL` and their rules | built 2026-11-01 (§11.7.6); MAC rules into `DUI_MAC_ACL` (type `L2`) on an image with the kit's sonic-swss 0003, refused with a log on any other (§11.7.8) |
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

**Device level, same evening (builds b173fa0 → 0e89a64), driven from the UI:**

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

**After the review-fix and feature passes (same evening, build 62f1228 →
f225d13):** the first push after a restart applied `config clock timezone
MST7MDT` (from the pushed POSIX rule) and `config hostname
SONiCSeastone-DX010`, both verified on the guest;
`set-locate` through the API reached the driver, which tried the platform
LED, logged that the VS has none, and reported `locating`. **Jumbo with an aggregate present, live (20:14--20:24, build f225d13):**
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
| `switch.dhcp_snoop.status` | SONiC's `dhcp-mitigation-rate` is a per-port DHCP packet rate limit (`PORT dhcp_rate_limit`, default 300 pps), not rogue-server blocking -- and on 202505 and 202511 only a field: nothing enforces it (§11.17, where disunified-l2d counts the rate instead); `dhcp_relay` is L3; unclaimed. **Superseded 2026-11-01 (§11.7):** the push, captured, is the flag plus per-VLAN trusted-server groups (`switch.dhcp_guard.N.*`), which an L3 ACL table can enforce in hardware |
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
await a live check on the VS; locate and the 4x25G breakout with its join were verified on the DX010 on 2026-11-01 (§11.6); on the 202511 image every mode -- 4x25G, the lanes at 10G and at 1G, 2x50G, the join -- and the ACL rebind on a split and a join were verified live on 2026-11-07 (§11.11).
What each emits (from the tests), and where the build differs from 7.1:

| Feature | Commands | Notes |
|---|---|---|
| Time zone (always, with `control.ports`) | `sudo config clock timezone MST7MDT` ; `sudo config save -y` | The push carries a **POSIX rule** (`system.timezone=MST7MDT,M3.2.0,M11.1.0`), not a zone name, and `config clock timezone` takes only `timedatectl list-timezones` names. The driver reads the switch's zone list with each zone's rule (`bash -s -- disunified-zones`: `tail -n 1` of every TZif file, once per run; `zones.txt` in the fixture dir is that capture) and picks the zone named like the rule, else by its abbreviation, else the first match; the current zone is kept when its rule matches (America/Denver stays). Unmatched: refused with a log. `System.Timezone` reads `DEVICE_METADATA timezone`. |
| Hostname (`control.hostname`: **on by default for sonic** since 2026-10-28, because UniFi's own SONiC switches always take the UniFi name as their hostname (§9); `hostname: false` turns it off; off by default for every other driver; `-control-hostname` / `DUI_CONTROL_HOSTNAME`, unset = the driver's default, `0`/`1` explicit) | `sudo config hostname <resolv.host.1.name>` ; `sudo config save -y` | RFC 1123 labels and at most 63 characters in all (SONiC's YANG `stypes:hostname`); otherwise refused with a log. The log prefix and `System.Hostname` follow on the next collect. **Syslog follows one cycle later** (2026-11-11): rsyslog.conf's templates carry the hostname as rendered text and nothing re-renders them on a rename, so once the Linux hostname matches CONFIG_DB and a template (collect.py's `rsyslog_tmpl`) does not, the plan adds `sudo systemctl restart rsyslog-config` (it renders again and restarts rsyslog when the file changed); a plain `systemctl restart rsyslog` reloads the stale file. |
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

Steps 1-2 of §8.3 are done short of running the build. The build kit lived
outside this repo, beside the SONiC source clones, until 2026-11-02; it is
now [`contrib/sonic-image/`](../../contrib/sonic-image/) (`series/` -- the
patches, one directory per repository, `BASES`, and `CONFLICTS.md`, the
rebase record -- `build.sh` (unattended, for the build host), `dry-run.sh`,
`sonic-upgrade.sh`, the fuzz harness and a README; the work clones under
`src/` stay outside).

**The series** (22 patches), each applying with `git am` on the **current
202511 head** of its repository -- which is also the commit sonic-buildimage
202511 pins for that submodule:

| Repository | Base | Patches |
|---|---|---|
| sonic-buildimage | `1354e0ce4630` (2026-11-01) | 0001 #24327 squashed (docker-l2mcd, rules, service, CoPP trap group, YANG); 0002 the daemon submodule; 0003 the PR's unrelated changes taken out; 0004, 0005, 0007 and 0011 DX010 platform fixes (§8.7, 0007 and 0011 being upstream's own); 0006 CoPP as the ECS does it (§8.11); 0008-0010 fixes the first build needed (below) |
| sonic-swss | `177c7a009f5e` (2026-09-19) | #3950 squashed (`l2mcorch`, `l2mcmgrd`) |
| sonic-swss-common | `e59457c6da43` (2026-07-21) | 0001 the `schema.h` part of [#1093](https://github.com/sonic-net/sonic-swss-common/pull/1093) (L2MC and MLD_L2MC table names; a fourth Micas draft §8.1 did not list, and swss does not compile without it); 0002 upstream's port to libyang 3 (`1431c3d`, §8.12) |
| sonic-mgmt-common | `b5f84b0644d9` (2025-12-05) | upstream's port to libyang 3 (#157, `cdb612e`; §8.12) |
| sonic-utilities | `73f8c019ff7c` (2026-10-20) | #4100 squashed |
| sonic-l2mcd | `651a13bdf036` (dev = l2mcd-pre, 2026-08-14) | 0001 the memcpy bug, see below; 0002-0007 the receive path: packet parsers and a socket leak (§8.13); 0008-0009 the querier election and the query source address, 0010 the per-packet syslog lines (§8.14) |

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

**What the first build needed (2026-11-01).** The series' own code compiled first
time (swss with l2mcorch and l2mcmgrd, the daemon, the container). Three more
patches came out of the build itself, none of them snooping logic:

- **0008, 0009: libyang 1 and 3.** 202511 builds most things against libyang 3 but
  sonic-mgmt-common, sonic-mgmt-framework and sonic-gnmi compiled Go validation code
  against libyang 1's headers. The two libyang-dev packages replace each other, and
  which one is installed when those compile depends on build order: "too many
  arguments to function `ly_set_log_clb`", first under parallel jobs, then with the
  packages built alone (upstream #26165). The first answer left the gNMI and
  management-framework containers out of the image; the series now carries the
  real fix instead, worked out and proven in its own session (§8.12): 0008 builds
  swss-common, mgmt-common, gNMI and the management framework against libyang 3 (as
  master and 202605 do), 0009 retires the ordering gate nothing needs any more, and
  upstream's own libyang 3 ports of swss-common and mgmt-common come with them. The
  image keeps both containers. (An image built with the earlier 0009, as the first
  vs run in §8.14 was, needs `gnmi` and `mgmt-framework` disabled after an upgrade
  from 202505.)
- **0010: the snooping YANG models.** With #24327's two models installed, 7
  sonic-utilities tests of the generic config updater fail, and those tests run in
  the image build. Bisected in the build slave: any new module that imports
  `sonic-vlan` (one VLAN-name leafref is enough) changes the references the
  updater's libyang 1 walk reports for existing tables, i.e. how `config
  apply-patch` orders changes; an unrelated module does not. 0010 makes the models'
  interface and VLAN-name fields plain strings without those imports: all 214 tests
  of the two files pass. Lost: the schema's check that a named VLAN or port exists,
  which nothing here uses (the snooping CLI writes unvalidated, the driver writes
  rows directly).

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
  that file (read 2026-11-01). **Series 0007** is upstream's own fix, open as
  [sonic-net/sonic-buildimage#27355](https://github.com/sonic-net/sonic-buildimage/pull/27355)
  ("Restore Celestica platform API wheels", review required, base master),
  cherry-picked unchanged from its head `748ad3bb4c40` with its author and title,
  so this image tests what upstream is reviewing. It applies to 202511 without
  conflict and builds the wheel with `setup.py bdist_wheel` for every non-PDDF
  Celestica board; all five build in the 202511 trixie slave, and the DX010's
  comes out with its 18 `sonic_platform` modules. H1 below is still where it is
  proven, and any word on the PR waits for that.
- **The CPLD driver panics 6.12 on its first register write (#26885), found on the
  DX010 (§8.14).** `dx010_cpld`'s `setreg_store` copies the sysfs buffer into
  `char clone[count]` with `strcpy`, one byte short for the NUL. Kernel 6.1 never
  noticed; 6.12's FORTIFY_SOURCE checks the dynamic size of the array and BUG()s
  (`lib/string_helpers.c:1040`), and the first write comes from the platform
  watchdog's disarm at boot. **Series 0011** is upstream's fix,
  [sonic-net/sonic-buildimage#26983](https://github.com/sonic-net/sonic-buildimage/pull/26983)
  ("Fix VLA size in setreg_store for DX010", open, base master, labelled for a 202511
  backport), cherry-picked unchanged from its head `894c85517024` with its author:
  `char clone[count + 1]`, `strcpy` kept. Other Celestica boards' store handlers
  (seastone2, ds3000, haliburton, silverstone and the rest) have the same array;
  that is for a note upstream, not this series.
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
land on the DX010 with the series. **Implemented 2026-11-03** (`igmp.go`, the
end of §8.14) behind `options.igmp`, off until H4 passes; the controller
withholds these keys until the device claims the feature (§11.1), and the
driver claims it (`feature_caps` 256, `IGMP_SNOOPING_ADVANCED`,
`igmp_snoop_caps` 31, as the ECS) only with the option on.

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
| `forward_unknown_multicast` true/false | flood unknown multicast in the VLAN, or send it to the multicast-router ports only | `L2MC_SUPPRESS ipv4-optimised-multicast-flood` `disable`/`enable` (`config igmp-snooping optimised_multicast_flood …`, snooping must be on): unknown IPv4 multicast to the mrouter ports only (`SAI_VLAN_ATTR_UNKNOWN_IPV4_MCAST_OUTPUT_GROUP_ID`). **Refused by the DX010's SAI** (§8.14): false is logged, the VLAN keeps flooding |

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
- **The IGMP version: no key, and the driver must write 3.** The CLI's default
  for a snooping VLAN is version 2, and l2mcd at version 2 ignores IGMPv3 reports
  -- which is what a Linux host sends until a querier tells it otherwise. On the VS
  (§8.14) a host's join was never learned until `config igmp-snooping version
  <vid> 3`; at 3 l2mcd took v3 and v2 joins alike. So every snooping VLAN gets
  `version 3` with the rest of its row.

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
| Unknown multicast | to the mrouter ports only when `forward_unknown_multicast` is false | `ipv4-optimised-multicast-flood enable`, which the DX010's Tomahawk-1 SAI refuses (§8.14) | **differs on the DX010**: unknown multicast keeps flooding; logged |
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

### 8.12 libyang 1 and 3: the gNMI and management-framework containers stay in (2026-11-01)

The first builds of the series failed with "too many arguments to function
`ly_set_log_clb`" in sonic-mgmt-common, sonic-mgmt-framework or sonic-gnmi,
depending on the order of the run. The series then left both containers out
(the first 0009). Clint, 2026-11-01: fix the build so it works with them
on. This section replaces that 0008 and 0009 with four patches, so
`INCLUDE_SYSTEM_GNMI` and `INCLUDE_MGMT_FRAMEWORK` stay `y` as on the stock
image. The ECS runs `mgmt-framework` too (§8.5).

**The cause.** 202511 builds libyang 1 (1.0.73) and libyang 3 (3.12.2) side
by side, and their `-dev` packages are the same Debian package,
`libyang-dev`. Installing one replaces the other, so a compile finds
whichever was installed last. Three things in the build rules leave that to
chance:

- The build runs each package's `-install` once per make run and does not
  check it again. A package that declared libyang-dev 3 can still compile
  against 1, if something installed 1 later in the same run.
- The conflict declaration in `rules/libyang.mk` does nothing. It runs
  before `rules/libyang3.mk` has defined `$(LIBYANG3_DEV)`, so the "wait
  for the conflicting package to be uninstalled" loop has an empty list.
  The IGMP tree's log shows the downgrade from 3.12.2-1 to 1.0.73 going
  straight through.
- `libyang3-dev-done` (upstream #25427) orders the *build* of libyang 1
  after the libyang 3 consumers. That holds only in one run from a clean
  `target/`. In a later or resumed run the debs already exist, and the
  order of the two installs is up to make.

Two packages in 202511 compile against libyang 1's headers: sonic-swss-common
(the `DefaultValueProvider`) and sonic-mgmt-common's cvl. sonic-gnmi and
sonic-mgmt-framework compile cvl too, through cgo, and declare no
libyang-dev at all. gnmi's own `$(LIBSWSSCOMMON_DEV)` dependency installs
swss-common's build dependencies, libyang-dev 1 among them, while
mgmt-common's `sonic_yang_models` wheel dependency installs libyang-dev 3
through python3-libyang. That is why the first 0008 (declare libyang-dev 1
on both) was not enough. Makefile.work also starts a fresh slave container
**per goal**. So `make a.deb b.deb`, the kit's fail-fast stage before the
image, and every rebuild after a failure are separate make runs. In each
one, whichever libyang-dev was installed last decides.

**Upstream's PR first: #26165 does not fix it on 202511.** sonic-net/sonic-buildimage#26165 ("[build] Fix
sonic-mgmt-common build failure due to libyang-dev conflict", xq9mend,
base master) adds `$(SONIC_MGMT_COMMON)_PHONIES += $(LIBYANG3-DEV-DONE)`.
Its only review, from the developer leading the libyang 3 move, said it
should be redundant, because mgmt-common already waits on the gate through
`$(LIBYANG)`. They had seen the failure only on rebuilds, and were removing
libyang 1 instead. The PR was closed unmerged on 2026-04-15. Tried on
202511 before anything else (cherry-picked unchanged with its author; 202511's
`rules/sonic-mgmt-common.mk` is byte-identical to the PR's base), containers
on:

| Run (each a fresh slave) | Result |
|---|---|
| swss-common and mgmt-common, 4 jobs | passes (in mgmt-common's run libyang-dev 1 happened to be installed last) |
| mgmt-framework alone, 1 job | **fails**: libyang-dev 1 at 17:35:05, then 3 "over (1.0.73)" at 17:35:06; `util.go:33:9: error: too many arguments to function 'ly_set_log_clb'` |
| sonic-gnmi alone, 1 job | **fails** the same way, the installs one second apart |

The failures are deterministic. They do not need parallel jobs. The PR's gate is a file in
`target/phony/`, already up to date from the first run, so it orders
nothing. The obvious extension is to gate and declare libyang-dev 1 on gnmi
and mgmt-framework too. That is the first 0008, and it failed the same way,
because that install has already run when their build starts. The reverse
order would break the libyang 3 consumers (FRR, python3-libyang) instead.
While both `-dev`s are installed in one tree, no ordering in the rules
fixes it. Hence the backport.

**The fix: backport upstream's own ports, then move the two build
dependencies.** On master, libyang 1 left in steps. sonic-swss-common
`1431c3d` (2026-05-22) and sonic-mgmt-common `cdb612e` (#157, 2026-06-01)
made both C/cgo consumers compile against either libyang, using libyang 3's
`LY_ARRAY_COUNT` to tell them apart. sonic-yang-mgmt moved to libyang 3's
Python bindings (#26584). #27629 then removed libyang 1 and the shim
(202605 has it as #27783). This series takes the first two and the
dependency half of the last:

| Patch | What | Source |
|---|---|---|
| `sonic-swss-common/0002` | port `defaultvalueprovider` to libyang 3 with a libyang 1 fallback | `1431c3d` cherry-picked unchanged onto the 202511 pin, author kept; the one conflict is a CI job that never runs (`azure-pipelines.yml`) |
| `sonic-mgmt-common/0001` (new series) | port cvl to libyang 3 with a libyang 1 fallback | `cdb612e` (#157) cherry-picked unchanged onto the 202511 pin `b5f84b0`, author kept; its CI hunk is redone on bookworm paths |
| `sonic-buildimage/0008` | swss-common and mgmt-common build against `$(LIBYANG3_DEV) $(LIBYANG3)`; sonic-gnmi and sonic-mgmt-framework declare it (and the libyang 3 runtime their binaries now link) | ours; mgmt-common's rule is master's after #27629 |
| `sonic-buildimage/0009` | retire `libyang3-dev-done` | as in #27629 |

After 0008, no package depends on libyang-dev 1, so it is never installed,
and the order of a run no longer decides anything. The patches are
byte-identical to upstream's apart from the CI files (`git patch-id` of
the rest matches). Nothing in gnmi or mgmt-framework needed changing: the
only cvl functions whose signatures changed are internal to mgmt-common.

**What stays on libyang 1, and why.** Its runtime and python3-yang, which
202511's sonic-yang-mgmt uses, and with it `config` validation and the
generic config updater. Moving that is #26584, a rewrite of
sonic-yang-mgmt's schema walk, and 0010 was bisected against the current
walk. Not a backport for a release branch. libswsscommon keeps libyang 1 in
its runtime dependencies next to libyang 3, master's state between #26584
and #27629, so no image or container loses a package. **The one runtime
change**: swss-common's default-value lookups and the gNMI and REST
servers' validation now run on libyang 3, as on master and 202605.

**Proof.** Built on the build host from the IGMP kit's prepared 202511
tree: series 0001-0007, these four patches, 0010. Each run was in a fresh
slave pinned to 4 cores, next to the IGMP build:

| Run | Result |
|---|---|
| the eight targets as eight goals, 4 jobs (libyang 1 and 3, swss-common, mgmt-common, mgmt-framework, gnmi, `docker-sonic-gnmi`, `docker-sonic-mgmt-framework`); mgmt-framework and gnmi each built alone after mgmt-common, the case that failed | all built; libyang-dev 1 never installed |
| `docker-sonic-gnmi.gz` as **one** goal from a clean tree, 4 jobs (one make run, mgmt-common and swss-common in parallel); the slave **killed** with mgmt-common compiling; the same goal **resumed**; then `docker-sonic-mgmt-framework.gz` as one goal | killed run rc 2 as intended; resume rc 0 (mgmt-common rebuilt beside swss-common, then gnmi and the container); mgmt-framework container rc 0; libyang-dev 1 never installed |
| serial (1 job): `sonic-mgmt-framework` as one goal from a clean tree, then `sonic-gnmi` alone, then the trixie `libswsscommon` (the host image's; gcc and `-Werror` of trixie) | all rc 0; libyang-dev 1 never installed, in either slave |

`libswsscommon.so`, `rest_server`, `telemetry` and `dialout_client_cli`
link `libyang.so.3`. Their packages depend on `libyang3 (>= 3.12.2)`. Both
container images carry libyang3 3.12.2 next to libyang 1.0.73. Started
inside their images from `/usr/sbin`, `telemetry` and `rest_server` parse
the CVL schema with libyang 3 and exit 0 on `--help`. They warn about three
type-only modules (sonic-extension, sonic-types, sonic-common), and the
libyang 1 `telemetry` from the IGMP tree prints the same warnings in the
same image: 202511's behaviour, not the port's.
**On the DX010 (2026-11-02 02:02 UTC).** The IGMP image built with these
patches (broadcom-legacy-th) was installed on the DX010, and gNMI and
mgmt-framework ran on hardware for the first time:

- `show feature status` lists both as enabled.
- `docker ps` shows `gnmi` and `mgmt-framework` up.
- The last 200 lines of each container's log hold no libyang, CVL, error,
  fail or panic lines.
- **Not yet seen:** `monit summary`, which was still in its first-boot delay
  when checked.

**Upstream (2026-11-01).** Posted as sonic-net/sonic-buildimage#30482 (the
issue) and #30483 (the PR, with #26165's commit first), with
sonic-net/sonic-swss-common#1269 and sonic-net/sonic-mgmt-common#273 for the
two cherry-picks. #1269 needed one more commit, for 202511 only: its CI
installs libyang 1's python3-yang again, because 202511's sonic_yang_mgmt
still imports it.

**For the IGMP kit.** `build.sh` applies submodule series only to
`sonic-swss sonic-swss-common sonic-utilities sonic-l2mcd`. Add
`sonic-mgmt-common` to that loop, and the line
`sonic-mgmt-common b5f84b0644d925828dc0a4e290695554d890ca4f 2025-12-05` to
`BASES`.

**On #26165**, a comment answers the review's question with the
reproduction and points to the new issue and PR.

### 8.13 The daemon's packet parsers: checked, fixed, fuzzed (2026-11-01)

A read of l2mcd's receive path said its IGMPv3 and MLDv2 record walks trust the
counts in the packet. Checked in-process, never on a network: `l2mcd-fuzz/` in the
build kit compiles the daemon's own C sources with clang's AddressSanitizer and
UndefinedBehaviorSanitizer, configures it through its own IPC handler as l2mcmgrd
does (port list; snooping on one VLAN, IGMPv3 and MLDv2; membership), makes one
port a router port with a general query, and feeds each packet to
`receive_igmp_packet()` / `receive_mld_packet()` in a heap buffer of exactly the
received size, built as the socket handlers build it. Only the APPL_DB sync layer is
stubbed and transmits are counted, not sent; it runs in the bookworm build slave
with no network and dummy links. One named case per finding (24) is the regression
suite (`run.sh regress`).

**Every finding was real**, and there were more than reported:

| Where | What a host on a snooping VLAN could do |
|---|---|
| IGMPv3 report walk | `num_grps` and each record's `num_srcs` trusted; the sources are converted to host order **in place**, so a report claiming 65535 sources **writes** up to 256 KB past the packet |
| `NEXT_GRP_REC` | `aux_data_len` (one byte, a count of words) byte-swapped as 16 bits: one aux word moves the walk 1 KB, so even a well-formed RFC 3376 report with aux data overruns |
| Record types outside 1-6 | type 0 counts at index -1; higher types read past the action-label table (with the VLAN's debug log on) and reach the group state machine, which indexes by type (heap over-read) |
| Queries | IGMPv3 and MLDv2 source counts read in network order (one source walks 256); a 9-11 byte IGMP query counts at index -1; an 8-byte IGMPv1 query naming a group reads bytes 10-11 (found by the fuzzer after the first round of fixes) |
| IP lengths, checksums | `tot_len` and `ihl` never compared with the bytes received, and a zero IGMP or ICMPv6 checksum skips the check, so the parse runs over whatever length the header claims. `l2mcd_fwd_pkt()` copies `tot_len` bytes to every router port: a 50-byte report claiming 300 is forwarded as 300 bytes, the rest being whatever an earlier packet left in that receive slot |
| `receive_mld_packet()` | hop-by-hop length unset without a hop-by-hop header and unchecked with one, `payload_length` unchecked; a bad checksum, a failed validation and a down port logged but the packet processed anyway |
| MLDv2 report walk | each record advanced by the running total of all record sizes, so a well-formed three-record report overruns; aux data ignored |
| Group state machine | `i <= src_cnt` reads one source past every record's list -- past the packet for the last record of any well-formed report with sources |
| VLAN-tagged MLD frames | the IPv6 header taken from the 19th receive buffer, not this one |
| Port updates (found on the VS, §8.14) | every resend of the port list and every link change opened another IGMP receive socket on the port -- the "already open" test read a field nothing sets -- and each copy parsed and forwarded every packet: after a few VLAN membership changes one report reached the mrouter port a dozen times |

The MLD findings are unreachable in this image (0006 traps IGMP only) and are fixed
anyway. On a switch the receive buffer is a 1 MB static ring (500 slots of 2 KB), so
most over-reads land in other packets' bytes rather than faulting; the in-place
source conversion can write past the ring.

**Fixed in sonic-l2mcd 0002-0007** (the build kit's series; posted to the
daemon's author 2026-11-05 as PRs against the Micas `l2mcd-pre` branch once the
image had run on the hardware -- links and the prior art found for each in
[`prior-art.md`](../prior-art.md) §6.1):

- 0002: a malformed packet is dropped, counted (`malformed-igmp`/`malformed-mld` in
  the daemon's debug dump) and logged at most once per family every 10 s with the
  number dropped since, so a flood of them cannot flood the log.
- 0003, IGMP: every record, with its sources and aux words, is checked to lie inside
  the message before anything past the header is read; unknown record types are
  skipped (RFC 3376 4.2.12); 9-11 byte queries dropped (RFC 3376 7.1); query sources
  read in host order and checked; the checksum always verified; every record
  advances the walk (a `continue` used to re-read the same record).
- 0004: the IPv4 lengths checked against the bytes received; the tagged-MLD offset.
- 0005, MLD: the same, and an 8-byte hop-by-hop header followed by ICMPv6 required
  -- RFC 2710 and RFC 3810 send every MLD message with a router alert there, and the
  handlers parse at those fixed offsets; bad checksums and down ports dropped.
- 0006: the state machine's loop.
- 0007: one IGMP receive socket per port (the field the socket is stored in).

The daemon's own validators keep their advisory verdict: the IGMP one rejects a
whole report for one 224.0.0.x record, which Linux hosts send by default, so
honouring it would drop real joins. gcc `-Wall` reports nothing new.

**Results.** Unfixed (`651a13b` + 0001): 21 of the 25 cases overrun or leak with the
debug log off, 23 with it on. Fixed: all 25 clean either way, LeakSanitizer included;
the malformed ones counted as drops, the valid ones still forwarded to the router
port. (The harness configures the daemon's event priorities and AF_PACKET receive
sockets as its init does; without them every per-port socket failed to register,
which is why the socket leak only showed on the VS first.)

**Fuzzing.** libFuzzer with ASan and UBSan, one target per family whose input is a
whole IPv4 or IPv6 packet (the checksum made valid wherever the header chain lets the
harness find it), seeded with the regression cases, the debug log's arguments
evaluated. Its first minute on the fixed tree found the IGMPv1 read above (case
`igmp-v1-query-group`, fixed in 0003). Then 4 hours with 4 workers per family on
the fixed tree (before 0007, which changes socket bookkeeping, not parsing), at the lowest priority beside the image build: **71.0 million IGMP
and 49.3 million MLD packets, about 1,100 and 1,160 code edges reached, no crash, no
leak, no undefined behaviour**. Two inputs were saved as slow (10 s, once each) and
replay in under 10 ms: the harness never runs the daemon's timers, so its group
state only grows over a run (the daemon caps it with its group pool), and the build
host was at a load near 35.

### 8.14 The image on the virtual switch and the DX010 (2026-11-01/02)

The vs build of the series ran on the VS guest (§2 A0's guest, 4 vCPU / 8 GB,
installed beside its 202505 image): first as built before the l2mcd parser fixes
(build `igmp-202511.20261101-de9675d44`, buildimage = the series without 0007,
which touches only the Broadcom platform), then rebuilt with them (same build id:
the version string does not see submodule commits, so the build's MANIFEST
records the l2mcd commits). Debian 13.7, kernel 6.12.41.

**The install path, rehearsed as §8.9 H0 will run it** (the build kit's
`sonic-upgrade.sh`):

- `arm` (sha256, `sonic-installer install`, 202505 kept as the default, the new
  image once): verified by `verify-next-image`; the new image's overlay carried the
  SSH host key, admin's authorized_keys and password. Booted in under 90 s; the
  bridge's SSH (strict known_hosts) logged in with no host-key change.
- **The dead-man rollback (R2):** with an 8-minute timer and no confirm it fired
  on time and the guest was back on 202505 two minutes later, 202511 still
  installed. Re-armed with 20 minutes, checked and `confirm`ed: 202511 the
  default, 202505 kept.
- Found and fixed in the script: arming an image installed by an earlier arm
  failed with a misleading message, and re-arming it in place would boot the
  config it migrated the first time, not today's -- `arm` now refuses and says to
  `sonic-installer remove -y <image>` first; `status` misreported a confirmed run;
  and the new image had to rebuild mstpd (docker, DNS and the internet inside the
  20-minute window -- on the guest a stale lab nameserver made that fail as
  "Run 'docker run --help'") -- `arm` now carries the switch's own static `mstpd`
  and `mstpctl` into the new image, so the bridge only reinstalls l2d.
- **Installing deletes every other image.** The image's `install.sh`, run from a
  running SONiC as `sonic-installer install` runs it, removes every `/host/image-*`
  except the running image and the new one (202505 and 202511 alike). On the DX010
  the factory 20181130.77 image went that way with the first H0; only ONIE can put
  one back. `arm` now names what an install will delete before it starts.
- **Disk:** the 202511 vs image takes 6.8 GB installed (5.8 GB for 202505), plus
  the installer (2.1 GB) while it installs. `/tmp` is no way round that: on SONiC it
  is the running image's overlay on `/host`, not memory. Delete the installer once
  `arm` has installed it, before the reboot. Measure the Broadcom image the same way
  before H0.

**§8.9 V1-V7, all passed.** The guest has a NIC behind Ethernet0 only, so the
other front-panel ports are down and l2mcd programs nothing on them. The build
kit's `vs-hosts.sh` gives Ethernet8 and Ethernet12 veth pairs (`eth3`/`eth4`, the
VS lane map's names) whose far ends are network namespaces h1 and h2 -- two test
hosts inside the VM, nothing on the LAN -- and `vs-join-test.sh` drives them:

| Step | Result |
|---|---|
| V1 | the build id; `l2mcd` shipped disabled, no l2mcd container; no L2MC/IGMP errors |
| V2 | `config feature state l2mcd enabled`: the container with `l2mcd` and `l2mcmgrd` running; `COPP_TABLE:l2mcd_group` and exactly the five IGMP hostif traps in ASIC_DB; no MLD traps |
| V3 | `L2MC\|Vlan100`, APPL_DB `L2MC_VLAN_TABLE:Vlan100`, the VLAN's `SAI_VLAN_ATTR_CUSTOM_IGMP_SNOOPING_ENABLE true` |
| V4 | h1's IGMPv3 join and h2's IGMPv2 join (the hosts' own kernels, a socket joining 239.1.2.3) learned on their own ports and nowhere else; APPL_DB `L2MC_MEMBER_TABLE:Vlan100:0.0.0.0:239.1.2.3:Ethernet8` (the ECS's key shape); ASIC_DB `L2MC_ENTRY` with one group member per port; both leaves cleared everything within 2 s. **Only at IGMP version 3** (§8.10) |
| V5 | no queries with the querier off; on (query interval 12 s), h2 saw them from 0.0.0.0 (no address on the VLAN, §8.11); silent again once off |
| V6 | a static group, an mrouter port and optimised flood: APPL_DB and ASIC_DB entries, `SAI_VLAN_ATTR_UNKNOWN_IPV4_MCAST_OUTPUT_GROUP_ID` set (with the ports down: stored, not programmed) |
| V7 | restored after a container restart; `vlan del` and the static member's removal refused by the CLI; disable leaves no L2MC row or ASIC object |

**The bridge on 202511** (G2): the driver's own loop and collector, run from the
Mac over SSH against the guest, the controller replaced by the recorded pushes.
`Start` ran every command the driver uses once (none missing from 202511's CLI)
in 7 s. Then the VS lab session's pushes (§6.6: the first push with port control,
port state and name, Allow All, Block All, a custom tagged set, the aggregate of
ports 5+6, the mirror, forced FEC and storm control, the removals, jumbo, syslog)
each applied, and the reconcile after each wrote nothing; the IP Settings were
declined with the right reason (the guest is dialled on eth0's lease). Then the
DX010's own push with the ECS's spanning-tree keys: mstpd built on the switch,
l2d installed and enabled under its 3-minute dead-man (cancelled over a fresh
connection), RSTP up at priority 8192, port states in APPL_DB and 352 STP port
objects in ASIC_DB, two quiet reconciles after.

**What 202511 changed, and what is only the VS:**

- **The earlier 0009's leftovers** (this run's image; the series has since replaced
  it, §8.6). A 202505 config enables the `gnmi` and `mgmt-framework` features, and
  an upgrade keeps them: featured fails to start the units the image no longer has,
  monit's container check reports them every cycle, systemd says "degraded".
  `config feature state gnmi disabled`, the same for `mgmt-framework`, and `config
  save -y` clear it (verified).
- Only the VS, and the same on its 202505: orchagent's `SAI_API_PORT` set failures
  at start, pmon's missing `sonic_platform`, and the system-health and
  watchdog-control units failing on the VS platform's missing chassis metadata.

**The image rebuilt with the l2mcd fixes** (0002-0006; 0007 came out of this run),
installed the way H0 will run it: from 202505, with mstpd in 202505's overlay as on
the DX010. `arm` from 202505 (the installer, then deleted), boot, `confirm`: the host
key and mstpd carried. One more script fix: `confirm | head -n 1` stopped `confirm`
halfway (the image the default, the state file not marked) -- SIGPIPE is now ignored
and sonic-installer's output goes to `/host/sonic-upgrade.log`. Then:

- **V1-V7 all passed again** (l2mcd not running at boot; V2-V7 as above).
- **The parser fixes end to end**, through the daemon's real socket path: test host
  h1 sent the regression cases' packets (§8.13) as tagged frames into a snooping
  VLAN on Ethernet8. All eight malformed ones were dropped and counted
  (`malformed-igmp` in l2mcd's dump), the six well-formed ones processed, and l2mcd
  ran on under the same pid. Each drop was counted 6-9 times, and a valid report sent
  once reached the mrouter port a dozen times: l2mcd held 14 receive sockets on
  Ethernet8 against 3 on an untouched port. That is the socket leak in §8.13's table,
  fixed in 0007, which the Broadcom image carries.
- **The bridge on the fixed image:** the same recorded pushes, all applied with quiet
  reconciles. 202505's stale lab nameserver came over with the config, so building
  mstpd would have failed; the carried binaries meant the push installed and enabled
  l2d at once, RSTP up under its dead-man in six seconds.

**On the DX010 (H0, 2026-11-01/02): two failed boots, the cause, and a confirmed
install.** The Broadcom build `igmp-202511.20261101-22cac574e` (the series through
0010), the `legacy-th` image (§8.7), installed from 202505 with `sonic-upgrade.sh`.

- **Trial 1** booted 202511 once and came back on 202505 about three minutes after
  the reboot. Every log stopped together about 22 s after `watchdog-control` started
  `watchdogutil disarm`, with no lockup or oops message on disk. One fact explained
  the timing: 202505's `reboot` arms the DX010's CPLD watchdog for 180 s
  (`watchdogutil arm`), the CPLD keeps counting through the CPU reset, and only the
  next boot's `watchdogutil disarm` turns it off -- its `Watchdog()` constructor
  writes 0x141 = 0 through `dx010_cpld/setreg`. Any hang before that point ends in
  a CPLD reset into GRUB's default image.
- **Trial 2: the same image, instrumented, no rebuild.** `arm ... trace` (the kit)
  adds to the new image's overlay only:
  - a boot trace synced to `/host` every 2 s (unit states, whether `setreg`/`getreg`
    exist, the CPLD's 0x141, the last SDK step and kernel error);
  - a feeder that arms the CPLD watchdog for 120 s once the CPLD driver is up and
    feeds it every 20 s, so a hang still resets into 202505;
  - `watchdogutil disarm` run under a timeout with its output in a file of its own;
  - `hardlockup_panic=1` beside SONiC's `softlockup_panic=1` and `panic_on_oops=1`.

  `confirm` turns the CPLD watchdog off, reads 0x141 back as 0 before it touches
  the dead-man or GRUB, and removes all of it. The mechanics were tested on the VS,
  the CPLD branches against a stub. The trace answered it at 29 s:
  `kernel BUG at lib/string_helpers.c:1040`, with the platform module still
  activating and the feeder's first `setreg` write the last thing it did.
  202505's systemd-pstore had archived an ERST panic record after each trial. The
  cause is §8.7's `setreg_store` overflow (#26885), so trial 1 was the disarm's own
  write. The watchdog code, syncd and the SAI were never involved.
- **Trial 3: the fixed platform package injected into the installed image.** Only
  `platform-modules-dx010` was rebuilt from the same tree (vermagic
  `6.12.41+deb13-sonic-amd64`, all imported symbol CRCs matching the kernel's
  `Module.symvers`, the other five modules byte-identical). rc.local's first boot
  installs `platform/<platform>/*.deb` with apt from a `[trusted=yes]` flat repo on
  `platform/common`: a same-version deb swapped in fails with "Hash Sum mismatch"
  unless its `Packages.gz` entry (Size, MD5sum, SHA1, SHA256) is rewritten. A
  re-versioned deb in the platform directory installs from the file itself (both
  checked on the VS with rc.local's own commands). Trial 3 ran a re-versioned
  build, `1.0+dx010cpld1`.
- **Trial 3 passed and was confirmed.**
  - The boot: no BUG. The feeder fed throughout, the disarm returned in 39 s, and
    `confirm` read 0x141 back as 0x00, removed the trial pieces and restored the
    sysctls. 202505 stays installed.
  - The window checks: SOC unit 0 attached, SDK init in 3 s on libsaibcm
    13.2.1.100, the 100G uplink up with RS FEC, the management address on its VLAN
    with the default route through it. pmon showed fans, both PSUs and
    temperatures (0007). NTP was synced. l2d was active with the uplink as the RSTP
    root port, forwarding.
  - The bridge recovered and applied the controller's push with 0 of 32 ports
    changed. The fan floor was re-applied.
  - l2mcd is still off: §8.9's V-steps on hardware come next.
- **What runs is not exactly 0011.** The installed `platform-modules-dx010` is
  `1.0+dx010cpld1` (`dx010_cpld` srcversion `B61467287F95373235ED7FB`), built from a
  fix written here before #26983 turned up. It sizes the array the same way but
  copies with `strscpy(clone, buf, sizeof(clone))`, and it also changed the other
  Celestica boards' handlers (not on this unit). Series 0011 is #26983 unchanged, so
  the next image build carries upstream's version. A 1.0 package built from it
  (srcversion `9A732FC0E3A48B6C025AE37`) is kept for a swap if the running module
  should match upstream before then.

**§8.9 H2-H3 on the DX010 (2026-11-03, image `igmp-202511.20261102-e53777446`,
libsaibcm 13.2.1.100, Clint's go in the session).** Only Ethernet0, the
uplink, has a link and no test host was cabled, so this is the chip's half
without traffic. The bridge on the switch was stopped for the window
(its reconcile would have undone the test VLAN's memberships; the switch
showed offline in the controller for about 21 minutes), every step went
through the CLI or `bcmcmd` reads, Ethernet0 was never touched, and nothing
was saved.

| Step | Result |
|---|---|
| H2: `config feature state l2mcd enabled` | the container with `l2mcd` and `l2mcmgrd`; `COPP_TABLE:l2mcd_group` (queue 1, 128 pps) and exactly the five IGMP hostif traps added to ASIC_DB (13 → 18 traps); no "Failed to create trap" |
| H3: test VLAN 3999 on Ethernet4 and Ethernet8 (tagged), `igmp-snooping enable`, `version 3` | orchagent "Enabled L2mc Snooping on Vlan3999"; the VLAN's ASIC_DB object `SAI_VLAN_ATTR_CUSTOM_IGMP_SNOOPING_ENABLE true`; no SAI error. In the chip nothing per VLAN changed: every VLAN, snooping or not, uses `VLAN_PROFILE` 1, which already has `IPMCV4_L2_ENABLE=1` and `L2_PFM=1` (known groups by their entry, unknown multicast flooded) |
| A static group (`static-group add 3999 Ethernet4 239.1.2.3`) on a port without link | CONFIG_DB only: l2mcd programs members on oper-up ports alone |
| **Optimised flood** (`optimised_multicast_flood enable 3999`, UniFi's `forward_unknown_multicast=false`) | **refused**: the wildcard entry (0.0.0.0, 0.0.0.0, 3999) `brcm_sai_create_l2mc_entry ... Invalid parameter`, and `SAI_VLAN_ATTR_UNKNOWN_IPV4_MCAST_OUTPUT_GROUP_ID` `SAI_STATUS_ATTR_NOT_IMPLEMENTED`. orchagent rolls back and **retries twice a second for as long as the row says enable**; `disable` stops it. Its group object stayed in ASIC_DB until snooping was disabled |
| **A group entry in the chip**: Ethernet4 reduced to VLAN 3999 only, then `bcmcmd "port ce1 lb=mac"` (MAC loopback: oper up with no cable, nothing it reflects can reach another VLAN) | the static group programmed: syncd `L2MC table add: mc-ip:0xef010203 src:0x0 vlan:3999`, orchagent "Added l2mc entry (0.0.0.0,239.1.2.3,3999)"; ASIC_DB `L2MC_ENTRY` (type XG), its `L2MC_GROUP` and a `L2MC_GROUP_MEMBER`; in the chip `ipmc table show` has (0.0.0.0, 239.1.2.3, VLAN 3999) → group 0x2000004, `mc show` that group with port ce1, `show igmp-snooping groups vlan 3999` the same group on Ethernet4 |
| The querier | on, its own query looped back through ce1 and l2mcd learned Ethernet4 as a dynamic mrouter port. The mrouter add for the wildcard key failed (`l2mc_gid:0`, "object on list [0] is NULL"), left by the refused optimised flood; orchagent adds a router port to every group entry of its VLAN as well (`addMrouterPortToL2mcEntries`), so without optimised flood that path is not taken |
| The driver's write path, by hand | `L2MC_STATIC_GROUP` and `L2MC_STATIC_MEMBER` rows written straight through swsscommon's `ConfigDBConnector` as the admin user program the entry exactly as the CLI's do; l2mcmgrd reads `L2MC_STATIC_MEMBER` only (deleting that row alone removed the entry; the group row is the CLI's bookkeeping). The CLI's delete removed the entry from ASIC_DB and from the chip's IPMC table |
| Teardown | `lb=none`, `igmp-snooping disable`, VLAN 3999 deleted, Ethernet4 back untagged in Vlan4094, `l2mcd` disabled: no L2MC row or ASIC object, 13 traps, one multicast group in the chip; the saved config untouched. The one error: orchagent removing the wildcard entry it never created. The bridge restarted, "0 of 32 ports changed", inform HTTP 200 |

So the hard unknown of §8.8 is answered for (*,G) entries: the Tomahawk-1 SAI
programs snooping and group entries. It cannot keep unknown multicast to the
router ports, which leaves UniFi's `forward_unknown_multicast=false`
unhonoured: such a VLAN keeps flooding unregistered groups, as one without
snooping does. **Still open (H4):** traffic between two cabled hosts (a
joined stream reaching only its port, the queries on the wire, a leave),
IGMP of VLANs without snooping still crossing the switch with the traps in
place, and the switch's own address as the query source on its native VLAN.
The captures are `docs/fixtures/sonic-igmp-202511.20261102-e53777446/`
(`collect-snooping.txt`, taken with the test VLAN and its static group in
place, and `config-help.txt` with `config feature` and `config
igmp-snooping`).

**The driver's write path (`igmp.go`, built 2026-11-03, after H3).** As the
ECS keeps its own: whole rows, never the CLI per field.

- **Gate:** `options.igmp: on` (off by default until H4) on an image with
  `FEATURE|l2mcd` whose `L2MC*` tables collect.py prints (`cfg_l2mc`); then
  the driver claims `feature_caps` 256 and `IGMP_SNOOPING_ADVANCED`
  (67108864) and `igmp_snoop_caps` 31, the ECS's own inform's values, and
  `Start` checks `config feature state`. Otherwise a push with snooping on
  is logged ("options.igmp is off" or "the image has no l2mcd").
- **Rows:** per snooping VLAN (UniFi's 1 on `Vlan<native_vlan>`) one `L2MC`
  row: `enabled`, `querier` and `fast-leave` as pushed, `version` 3,
  `query-max-response-time` the pushed max response (1-25 s),
  `query-interval` (membership - max response) / 2 (125 for UniFi's
  260/10), `last-member-query-interval` 1000 ms. With
  `flood_known_protocols` on, the seven reserved groups (§8.10) as
  `L2MC_STATIC_GROUP` + `L2MC_STATIC_MEMBER` rows on every member of the
  VLAN. Everything else in those tables goes: a VLAN the push does not turn
  on (its statics, suppression and router-port rows first, the CLI's
  order), and every `L2MC_SUPPRESS` row, because optimised flood is refused
  and a row with it on makes orchagent retry forever.
- **One swsscommon script per apply** (`sudo python3 - disunified-l2mc`,
  the ops as a JSON string on its last line, `ConfigDBConnector.set_entry`),
  then `config save -y`: a `sonic-db-cli` call per row would put a 32-port
  VLAN's 224 static rows past the apply budget. `config feature state l2mcd
  enabled` first, with the first snooping VLAN; it then stays on, as on the
  ECS.
- **VLAN membership:** the CLI refuses `vlan member del` of a port a static
  group names in that VLAN, so the port plan deletes those rows before any
  membership removal, and `ApplyPorts` re-runs the IGMP apply after its
  batch, so the reserved groups follow the ports.
- **Logged, not written:** the querier address (no l2mcd field, §8.11;
  the line names the source the queries will use, the VLAN's own address or
  0.0.0.0), the multicast-router expiry and report suppression (no
  setting), `forward_unknown_multicast=false` (refused by the SAI, above),
  `forward_unk_mcast_router_ports=false`.
- **Read side:** `System.IGMPSnooping` from the `L2MC` rows.
- **Tests:** `igmp_test.go` over the H3 capture with the ECS's settings
  (rows, order, the test VLAN removed, idempotence, the feature enable,
  statics following a port's VLAN move); the FixtureRunner refuses a static
  member outside its VLAN or without snooping, and the CLI's `vlan member
  del` check (a mutation that drops the port plan's cleanup fails the test);
  `TestReplaySonicIGMPSnooping` drives the DX010's own push with the ECS's
  networks and snooping keys through unificfg and the loop;
  `TestSwitchCapsIGMPAdvancedAsTheECS` holds the claim to the ECS's inform.

**Live through the driver (2026-11-05, deb `f0accd9`, `options.igmp: on` on
the DX010; Clint: on without H4).** The new claim was in the next inform
(`feature_caps` with 256 and 67108864, `igmp_snoop_caps` 31), but the
controller pushed nothing new until a `cmd/devmgr force-provision`, as with
STP (§11.5). That push (`controller-10.6.109/system_cfg-dx010-igmp.txt`,
replayed in `TestReplaySonicDX010IGMPPush`) carries VLAN 1 only, the DX010
being on no other network: snooping, querier on with **`igmp_querier`
empty** (the ECS's VLAN 1 names the gateway), max response 10, membership
260, router expiry 180, `forward_unknown_multicast=true`, and both
`switch.multicast.*` keys on. The bridge applied it in 4 s: `config feature
state l2mcd enabled`, `L2MC|Vlan4094` (v3, querier, 125/10), the reserved
groups' 7 rows and 224 members, `config save`; the router expiry logged;
0 ports changed; every reconcile after it wrote nothing. On the switch:
l2mcd and l2mcmgrd running (0.4 % CPU), 18 hostif traps, no ERR in syslog,
the saved config with l2mcd enabled. `show igmp-snooping groups vlan 4094`,
APPL_DB, ASIC_DB and the chip's `ipmc table show` agree on 10 entries: the 7
reserved groups (static, Ethernet0) and 3 learned from the LAN on Ethernet0
(233.89.188.1, 239.254.127.63, 224.0.1.60 -- the groups the ECS holds).

What the uplink showed in two minutes of `tcpdump` on Ethernet0, and what
it means:

- The traps hand l2mcd the IGMP of **every** VLAN the trunk carries (2, 8,
  9, 69, 4094 seen), not only the snooping one or the ones Ethernet0 is a
  member of. Trapped frames show the ingress VLAN as an 802.1Q tag (4094
  for the native one), as BPDUs do (§11.5).
- The gateway is VLAN 1's querier; its general
  queries reach l2mcd on Vlan4094. **l2mcd learned no mrouter port from
  them** (`Multicast Router ports:` empty throughout).
- **l2mcd queries from 0.0.0.0 although Vlan4094 has an address**
  (`INTERFACE_TABLE|Vlan4094|<addr>/16` in STATE_DB, the same in
  VLAN_INTERFACE and INTF_TABLE). §8.11 assumed it uses the VLAN interface's
  lowest IPv4; on this build it does not. At 0.0.0.0 it wins every
  election, so with UniFi's querier on it never defers to the gateway: it
  sent group-specific queries for 233.89.188.1 (v3, 1 s max response,
  untagged into VLAN 1) about six a minute, after the leaves UniFi devices
  send for that group -- by the count, leaves seen on other VLANs as well.
  The LAN already carries such 0.0.0.0 group queries on VLANs 2 and 69 from
  two other switches. No general query from the DX010 was seen in the
  window.
- With only Ethernet0 linked nothing transits the switch, so none of this
  has yet decided where a stream goes. It will once ports carry hosts:
  without a learned mrouter port, a group joined behind the DX010 reaches
  the upstream router only by flooding or the static groups. That is H4's
  question, and the querier-address patch (§8.11) now also has to fix the
  address lookup and the election.

**The cause, and `sonic-l2mcd/0008` (2026-11-05).** Two faults, both in
`src/igmp_io.c`; l2mcd's own dump (`l2mcd_ctl -v 4094`) showed `querier : 1`,
`rtr_port_list` nil, and `querier_router` the switch's own address:

- **The gateway queries in IGMPv2** (every general query captured was v2),
  and `igmp_process_query()` returned on any version mismatch *before* the
  election and the router-port learning. The driver runs every snooping
  VLAN at version 3 (§8.10: at 2 l2mcd ignores hosts' v3 reports), so an
  IGMPv2 querier was invisible: no router port, the switch querier for
  good, answering leaves with its own group-specific queries. 0008 processes
  an older-version query (RFC 3376 §7, RFC 4541 §2.1.1) and drops only one
  newer than the VLAN's version or of no version; both are still counted.
- **The source address** was looked up only through `vlan_node->ve_ifindex`,
  although SONiC binds no VE and l2mcmgrd's INTERFACE_TABLE update stores the
  VLAN interface's addresses in `ve_portdb_tree` under the VLAN id. 0008
  looks there when `ve_ifindex` is 0, for the election and for the general
  query's source -- which turned out not to be the DX010's state (0009,
  below). A VLAN without an address still queries from 0.0.0.0: that is the
  querier-address field of §8.11, still to write.

Built on VM 116 as the series' l2mcd package (the tree's
`target/debs/bookworm/l2mcd_1.0.0_amd64.deb`; the binary differs from the
0007 build the DX010 runs, which matched byte for byte, only by the patch).
The harness's 25 regression cases pass on it (`l2mcd-fuzz/run.sh regress`).

**0008 on the DX010 (2026-11-05 17:48 MST, installed by hand into the
running l2mcd container).** `cp` over the running binary fails ("Text file
busy"); `mv` works. Ethernet0 was learned as the multicast-router port
within 30 s, and over 135 s (more than the 125 s query interval) the switch
sent no general query of its own: the gateway, querying in IGMPv2 every
30 s, is the querier. `show igmp-snooping vlan` still prints `Querier -
true`: that is the configured flag, not the election (l2mcd's own dump,
`querier : 0`, is the election). But the group-specific queries the switch
sends after a leave still left from 0.0.0.0.

**Why, and `sonic-l2mcd/0009`.** l2mcd's VLAN table (`l2mcd_ctl -s <file>
-c 3`) showed `4094 0x48000ffe 0xffe`: `ve_ifindex` is the bare VLAN id, not
0 and not the SVI index. `mld_if_snoop_set()` creates the VLAN node as a
forward reference through the router-port path (both fields the bare id)
and then rewrites `ifindex` alone. The bare id fails
`l2mcd_ifindex_is_svi()`, so every reader took the router-port branch and
searched `mld_portdb_tree`, which holds no address, while
`ve_portdb_tree` held `4094 <address>/16` (`-c 9`). That is the state when
the address update arrives after snooping is enabled, the DX010's order; in
the other order the snoop-enable handler sets `ve_ifindex` to the SVI index
and the SVI branch finds the address, though only because
`l3_get_port_from_ifindex()` is the identity on SONiC and the result is cut
to a 16-bit port id (0x48000ffe -> 4094). 0009 falls back to the VLAN id's
entry in every reader of the address (group queries and the election,
general queries, the MLD query source, static reports sent to router ports,
the querier's own address) whatever `ve_ifindex` holds. The harness has a
case for each order and one that forces the DX010's state: on the 0008 tree
the after-order and the forced case query from 0.0.0.0, on 0009 all three
from the VLAN's address; the 25 packet cases stay clean.

**The log flood, and `sonic-l2mcd/0010`.** With snooping on, l2mcd sent
~75 lines/s to the remote log host (820 in 11 s; ~20/s tagged ERR), which
tripped the log host's error-rate alert. docker-l2mcd starts `l2mcd -n 1`
-- so does the ECS (read 2026-11-05) -- and `-n 1` routes every per-VLAN
debug line to syslog at NOTICE. l2mcd starts at DEBUG, but l2mcmgrd sends it
its own swss level (`L2MCD_CFG:PARAMS loglevel:5`, NOTICE by default) about
a second later, so a start-up level in l2mcd would not hold; at NOTICE every
routed line passes. The ECS logs the same way (~30 lines/s) but only to its
own `/var/log/syslog`: its rsyslog has no remote rule. SONiC forwards to the
syslog host the controller pushes, which the driver writes. 0010 routes the per-VLAN lines at INFO and
counts the IPv6 receive path's two per-frame ERRs ("vlan_node is not set"
for every IPv6 multicast frame on a VLAN without MLD snooping, "hbh nexthdr
is not ...") as `mld-off` and `non-mld` in `l2mcd_ctl -c 10`; `-n 1` is
unchanged. To see the lines again: `swssloglevel -l INFO -c l2mcmgrd` and an
l2mcd restart, or `l2mcd_ctl -s <file> -d 6` until the next restart. The
runtime mitigation before it was `l2mcd_ctl -s /tmp/l2mcd_ctl.out -d 4`:
**`-s` is required** in the container (the default output file is
`/dev/pts/1`, which it lacks), the output file is opened in append mode, and
the `debug IGMP-Snooping loglevel` CLI of the sonic-utilities patch is not on
the DX010's image. l2mcd writes to `/var/log/l2mcd.log` on the host, not
`/var/log/syslog`.

**0009+0010 on the DX010 (2026-11-05 20:37 MST, by `mv`; `l2mcd.0007` and
`l2mcd.0008` kept beside it in the container).** The deb from the series
tree differs from the 0008 build in `l2mcd` and `l2mcd_ctl` only; the daemon
was swapped. After `supervisorctl restart l2mcd l2mcmgrd`, with no runtime
level: l2mcd at level 5 with `-n 1`'s routing on, **0 l2mcd lines to the
remote host in 10 s** and about 9 a minute in `l2mcd.log` (port-state
refreshes), one ERR; a one-off burst of ~3300 lines at the restart itself
(l2mcmgrd replaying the static members at NOTICE, and l2mcd's DEBUG lines in
the second before l2mcmgrd's level arrives). 605 IPv6 frames counted as
`mld-off` in the first two minutes, each formerly an ERR line. Over 150 s:
every IGMP frame the switch sent was a group-specific query from the VLAN's
address, none from 0.0.0.0, and no general query; Ethernet0 the router
port, l2mcd `querier : 0` with the gateway as `querier_router`; all 10
groups in l2mcd and 10 `L2MC_ENTRY` objects in ASIC_DB. The install lives in
the container's writable layer until the next image build carries 0008-0010.

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
| DHCP guard / snooping | `ACL_TABLE\|global_ip_acl` (L3) and `global_mac_acl` (ANY) bound to every port, no rules while the site feature is off (they hold the custom ACL rules); snooping on adds a table of its own that traps DHCP server replies to the CPU for a snooping daemon (§11.7.3) | refused with a log | captured and specified 2026-11-01 (§11.7): the guard as hardware ACL rules (no trap action in community SONiC), the IP rules as an L3 table; MAC rules need an orchagent patch. |
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
arrival checklist is §3 B3 step 5. **The address row was removed again on
2026-11-07** (§11.11): on 202511 a gateway-less MGMT_INTERFACE row fails
the YANG check every breakout runs, NTP no longer binds to eth0 (below),
and on 202511's template a DHCP eth0 has `metric 202` behind the in-band
default's 20 -- the trap above needed a route with no metric. eth0 is on
DHCP now, without carrier. What the driver does about it:

- **NTP from the in-band interface.** When it applies UniFi's NTP servers
  it also sets `NTP|global src_intf` to the interface that carries the
  address the bridge dials, when that is an in-band interface whose
  address CONFIG_DB holds (`Vlan<native_vlan>` on a switch set up as §3
  says): `sonic-db-cli CONFIG_DB hset 'NTP|global' src_intf <iface>`, then
  `config save -y` (hostcfgd regenerates chrony.conf). **For a VLAN the
  field is deleted instead** (`hdel 'NTP|global' src_intf`, since
  2026-11-02): sonic-ntp.yang has no VLAN among `src_intf`'s types (the
  branch is commented out in 202505 and 202511), and the VLAN the driver
  wrote failed the sonic-yang check every breakout runs (§11.8). Without
  the field chrony.conf.j2 binds nothing and the kernel sends from the
  interface its route uses, which is the in-band one on a switch set up as
  §3 says. `collect.py` reads
  `NTP|global` (`cfg_ntp_global`) and an interface already right is left
  alone. Nothing is written when the switch has no `NTP|global` (an image
  without `init_cfg.json`'s entry; a hash with `src_intf` alone is not
  what hostcfgd expects) or when the bridge dials `eth0` (its default is
  then right). Tests: the image's `init_cfg.json` entry
  (`ntp_global-init_cfg.txt`, read from the unit) in place of the live one,
  the driver and the DX010's real first push through the loop.
- **The MGMT_PORT row the hand fix never made.** `config interface ip add
  eth0 ...` writes MGMT_INTERFACE and nothing else, and sonic-yang's model
  of MGMT_INTERFACE has a leafref to a MGMT_PORT row of the same name.
  Nothing on the switch minds (interfaces.j2 and hostcfgd do not read
  MGMT_PORT) until `config interface breakout` validates the whole
  CONFIG_DB and refuses every mode on every cage with `Leafref ... of value
  "eth0" points to a non-existing leaf` (§11.8, 2026-11-02). The driver
  adds the row (`sonic-db-cli CONFIG_DB hset 'MGMT_PORT|eth0' alias eth0
  admin_status up`, a name and nothing more) before the breakout that
  needs it, once, with a note; the arrival checklist (§3 B3 step 5) gets
  it by hand. Test: the 202511 capture, which lacks the row.
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
| IGMP snooping (256; `IGMP_SNOOPING_ADVANCED` 67108864; `igmp_snoop_caps` 31) | yes | with `options.igmp: on` on the l2mcd image (§8.14), off until H4 | `switch.vlan.N.igmp_*` (snooping, querier, fast leave, membership, max response, mrouter expiry, suppression), `switch.vlan.N.forward_unknown_multicast`, `switch.multicast.*` |
| DHCP snooping (128) | yes | yes since the 2026-11-01 build (§11.7.6) | `switch.dhcp_snoop.status`, and with it on `switch.dhcp_guard.N.server/vlan/status` (captured 2026-11-01, §11.7) |
| Port isolation (4), port security (512) | yes | yes since 2026-11-07 (§11.12) | `switch.port.N.port-security` (isolation keys only when set) |
| LLDP-MED (16) | yes | yes since 2026-11-07 (§11.12) | `switch.port.N.lldpmed.opmode/topology_notify` |
| Egress rate limit (32) | yes | yes since 2026-11-07 (§11.12) | none while unset |
| L3 routing (1), BGP (33554432), `max_l3_intf` 64, `max_static_routes` 256 | yes | L3 routing, 64/256, and BGP since 2026-11-07 (§11.13, §11.14) | `switch.routing_enabled`, and `switch.routing_interface.N.*` once a network is routed on the switch (captured 2026-11-07, §11.13); `switch.bgp.1.*` once a BGP router names the switch (§11.14) |
| OSPF (`feature_caps2` 4) | no (`feature_caps2` 128) | yes since 2026-11-07 (§11.14) | `switch.ospf.*`, and `switch.routing_interface.N.ospf.*` for each routed network in an area (captured 2026-11-07, §11.14) |
| Custom ACLs (`max_custom_ip_acls`/`mac_acls`/`global_acls` 512; no feature bit) | yes | IP 128, MAC 0 since the 2026-11-01 build (§11.7.6) | none while no rule exists; `switch.acl.mac.N.*`, `switch.acl.ip.N.*` once one does (captured 2026-11-01, §11.7) |
| Routed ports and aggregates (`feature_caps2` 16, `PORT_ROUTING`) | no | yes since 2026-11-08 (§11.16) | `switch.routing_interface.N.type=port` / `=lag` with `port` / `lag` (captured 2026-11-08, §11.16) |
| VRRP (268435456) | no | no: offered only as an MC-LAG group's L3 Automatic Failover (§11.16) | `switch.routing_interface.N.vrrp.*` from the MC-LAG group's context |
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
| **DHCP snooping, isolation, port security, custom ACLs** | SONiC ACL tables (`ACL_TABLE`/`ACL_RULE`, `sonic-acl.yang`), the same shape Ubiquiti uses (`global_ip_acl`, `global_mac_acl`, §9); each needs its push captured from the UI first. **DHCP snooping and the ACL rules captured and specified 2026-11-01 (§11.7)**: the guard and the IP rules on the stock image, MAC rules only with an orchagent patch (custom image); isolation and port security captured, built and verified live 2026-11-07 (§11.12: an isolation group and learn mode drop + static FDB, not ACLs). **The guard and the IP rules built 2026-11-01 (§11.7.6) and verified live on the DX010 the same evening (§11.7.7).** | stock (MAC rules: custom) | medium each |
| **LLDP-MED** | lldpd's custom TLVs per port, as the ECS's lldp manager writes them. **Built and verified live 2026-11-07, with the Voice VLAN (§11.12).** | stock | small |
| **Egress rate limit** | `SCHEDULER` + `PORT_QOS_MAP` port shaper. **Built and verified live 2026-11-07 (§11.12).** | stock | small |
| **L3 routing, static routes, BGP** | VLAN interfaces and FRR are native to SONiC. **Built and verified live 2026-11-07 (§11.13): routed networks, the transit to the gateway, switch static routes (next hop, interface, black hole), DHCP server (busybox udhcpd) and relay, Isolate Network.** BGP and OSPF since the same evening (§11.14): the operator's uploaded FRR file loaded into the bgp container; ospfd run by the driver on the controller's router. | stock | medium |
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
ECS's `4.0.1.995` -> `4.0.1`). The DX010's build id is no longer sent
(2026-11-13): it reports the ECS Core's catalogue version, then the
controller's upgrade targets. `sfp_rev`: xcvrd's `vendor_rev`, padding trimmed (`1` on the
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
| config | `/etc/sonic/disunified/l2.json` | written by the driver: STP on/off, version, priority, per port `enabled`/`edge`/`bpdu_guard`, the loop-protected ports, the DHCP rate detector (§11.17); `deadman` set by the switch when the dead-man fired |

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
restart or an upgrade of l2d never runs the LAN without spanning tree. The
script is embedded in the bridge (`l2.go`) and reinstalled whenever its sha256
differs from the switch's copy, which restarts the unit: so a new bridge
deb that carries a changed `l2d.py` restarts l2d, and the restart adopts
mstpd -- `stp_up` finds it running, `stp_state` already 2 and `Bridge` already
mstpd's, so nothing is started, `addbridge` is skipped and no sysfs is
written; the first sync rewrites each port's kernel state (the state it
holds) and re-maps the VLANs after the settle (`TestL2dRestartAdoptsRunningMstpd`,
from the capture with l2d running). **mstpd's liveness is read in-process**
(`/run/mstpd.pid` and `/proc/<pid>/comm`, else a `/proc` scan), not with
`pidof`: sysvinit's pidof logs a process that exits between its `/proc`
listing and its read of `<pid>/stat` (`can't read from <pid>/stat`) straight
to syslog at ERR -- `_TRANSPORT=syslog` in the journal, so redirecting its
stderr would not have helped. Three such lines in 36 h on the DX010, every one
a false alarm (mstpd found, NRestarts 0); fixed 2026-11-07.

**The controller's keys** (the ECS's real push,
`fixtures/controller-10.6.109/system_cfg-ecs-aggregation.txt`, scrubbed):
`switch.stp.status/version/priority` → l2d's `mstpctl setforcevers` (stp,
rstp, mstp) and `settreeprio Bridge 0 <priority/4096>`; per port
`stp.port_mode=disabled` → BPDU filter + admin edge (the port forwards and
ignores BPDUs, as `ubnt-stpmgrd` does), `stp.edge` auto/enabled/disabled →
`setportautoedge`/`setportadminedge`, `stp.bpdu_guard` → `setbpduguard`
(mstpd shuts the port's netdev on a BPDU; the bridge port goes disabled and
StpOrch blocks it in the ASIC; the port reports `errdisabled: bpduguard`,
with no `anomalies` bit -- that field's table has none for BPDU guard
(`docs/feature-map.md` §2); a port toggle in UniFi brings it back). A LAG member's
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

**The driver's own path, live (2026-11-01, a local main since squashed into 7616b6c, deployed with `l2d:
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
fix was deployed (a local main since squashed into 7616b6c, 12:39 UTC): the bridge replaced the manager by
checksum (`sonic apply l2: update disunified-l2d`) while mstpd kept running
(started at boot, no port transition); BPDU guard on cage 3 from the UI was
logged `settings: Ethernet8 bpdu guard on` and shown by mstpd within one
push, and off again the same way. Two reboots through the UI's Restart
brought mstpd back with Ethernet0 the root port. With that the option went:
RSTP is the driver's default.

**Gap found 2026-11-02: a swss restart left the switch without spanning
tree.** An orchagent crash (the mis-encoded probe in §11.9) restarted swss,
which deletes and recreates the `Bridge` netdev. The new bridge came up
with `stp_state 0`, mstpd logged `delete_br_byindex` and dropped it, and
l2d, seeing mstpd still running, took its only recovery branch (mstpd
died) never: it rebuilt the APPL_DB states from kernel ports that were all
forwarding, and `mstpctl` answered `Couldn't find bridge` every two seconds
for ten minutes. A `systemctl restart disunified-l2d` fixed it in one
second (stop does not tear down, start re-adopts: `addbridge`, settle, every
VLAN remapped). ae2ce0e adds that branch to the sync loop: a present bridge
whose `stp_state` is not 2 is a recreation, and l2d runs the fresh-enable
path with its port, state and VLAN memory cleared. Live-proven the same
afternoon by the day's second swss restart (§11.9): `Bridge was recreated
(swss restart?): re-enabling spanning tree` 20 s after the netdev came
back, mstpd `create_br`, the VLANs mapped 16 s later (27 ports at that
moment, the rest joining as portsorch brought them up), Ethernet0
blocking -> learning -> forwarding, one topology change.

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

### 11.7 DHCP snooping and switch ACLs: the captured push and the plan (2026-11-01)

The controller pushes neither feature to the DX010, which claims neither
(§11.1), so both were captured on the ECS Aggregation. Clint approved two
site-wide changes for it: Rogue DHCP Server Detection on, and a harmless set
of ACL rules. Both were saved from the UI on Network 10.6.109 and restored
within minutes. Snooping was on for about 2.5 minutes. The rules matched only
documentation addresses (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`)
and test MACs (`02:00:00:00:00:0x`), on one unused VLAN, scoped to the ECS
alone. After each restore the ECS's push was byte-identical to the one before,
and the controller objects matched the before-state. No switch rebooted, and
every DHCP request in the window got its ACK. The capture session wrote no
driver code; the build that followed it is §11.7.6.

#### 11.7.1 Where UniFi keeps them

- **Rogue DHCP Server Detection** is the 10.x name for DHCP snooping. It is
  site-wide, under Settings → Networks → Global Switch Settings, stored as
  `global_switch.dhcp_snoop` and saved by the UI with
  `POST api/s/<site>/set/setting/global_switch`. The UI's tooltip says it
  "collects client IP addresses, hostnames, and DHCP guarding information".
- **The trusted servers are per network.** A network's DHCP Guarding is
  `networkconf.dhcpguard_enabled` with up to three servers in
  `dhcpd_ip_1..3`. The site had it on, with servers, for three networks
  before the capture. Snooping turns those lists into rules. There is no
  per-port trust setting and no "trusted uplink" concept in the push.
- **ACL rules** are under Settings → Policy Table → ACL Rules (v2 API
  `acl-rules`; removal is `acl-rules/batch-delete`). There are two kinds:
  - **MAC** ("Device Access Control", traffic within one VLAN): needs a VLAN,
    with source and destination each Any, a Device or a MAC, and an
    optional netmask prefix length.
  - **IPv4** ("Network Access Control", "between VLANs"): protocol All,
    TCP/UDP, TCP or UDP; source and destination each Any, a Network or
    IPs, plus a port Any or a comma list. The UI refuses a range
    ("Please enter a valid port between 1 and 65535").

  A rule applies to All switches or to Specific ones (`specific_enforcers`,
  by MAC). The picker listed only switches that claim ACLs. On every save the
  controller logged `Switches[...] do not support Switch ACL, but assigned
  with Switch ACL rule[...]` naming the 29 switches that do not claim them,
  even though the rule named the ECS alone.

#### 11.7.2 What the controller pushes

Both pushes are scrubbed in `docs/fixtures/controller-10.6.109/`.
`system_cfg-ecs-dhcp-snoop.txt` and `system_cfg-ecs-acl.txt` are each the
ECS's ordinary push (`system_cfg-ecs-aggregation.txt`) plus the lines below,
scrubbed with the same address mapping. The `switch.acl.*` lines are kept as
captured, because they hold only the values typed for the capture. The
scrubber keeps a MAC-shaped mask, and the site-info checker accepts one,
since no device has an address of ones followed by zeros (both taught on
2026-11-01). The
parser types are `unificfg.Config.DHCPGuards`, `.MACACLs` and `.IPACLs`
(`internal/unificfg/parse_test.go`).

**DHCP snooping on.** `switch.dhcp_snoop.status` flips to `enabled`, and one
`switch.dhcp_guard` group follows for every trusted server of every guarded
network, slot by slot. A network with three servers is three groups. An
unguarded network has none, which means no server list for its VLAN. Off is
the bare `switch.dhcp_snoop.status=disabled` with no groups.

```
switch.dhcp_snoop.status=enabled
switch.dhcp_guard.1.server=192.0.2.1      (the gateway; the same address as switch.vlan.1.igmp_querier)
switch.dhcp_guard.1.vlan=1
switch.dhcp_guard.1.status=enabled
switch.dhcp_guard.2.server=192.0.2.5
switch.dhcp_guard.2.vlan=69
...                                        (6 groups: VLAN 1 x1, VLAN 69 x2, VLAN 2 x3)
```

**ACL rules.** MAC rules come first, then IP rules, each numbered from 1 in
the UI's rule order. Each group's name is the rule's name plus `-<n>`. One UI
rule naming two destination ports was pushed as two IP groups (`-1`, `-2`).

| Key | Values seen | Meaning |
|---|---|---|
| `switch.acl.mac.N.action` | `permit`, `deny` | Allow, Block |
| `switch.acl.mac.N.vlan` | `4010` | the rule's VLAN (the network's VLAN ID) |
| `switch.acl.mac.N.src.mac` / `.src.mask` | `02:00:00:00:00:01`/`ff:ff:ff:ff:ff:ff`; `02:00:00:00:00:00`/`ff:ff:ff:ff:ff:00` (UI netmask 40) | one side; the UI's Any is `any` with mask `00:00:00:00:00:00` |
| `switch.acl.mac.N.dst.mac` / `.dst.mask` | as above, `any`/`00:00:00:00:00:00` | |
| `switch.acl.ip.N.action` | `permit`, `deny` | |
| `switch.acl.ip.N.type` | `ip` (protocol All), `tcp`, `udp` | the UI's TCP/UDP is two groups, `-1` tcp and `-2` udp (DX010 capture, below) |
| `switch.acl.ip.N.src.ip` / `.dst.ip` | CIDR; `any` | `any` is the UI's Any (DX010 capture); a Network side is a CIDR too |
| `switch.acl.ip.N.src.port` / `.dst.port` | `9`, `1001` | one port per group; the source port from the DX010 capture |
| `switch.acl.ip.N.src.port_range` / `.dst.port_range` | `1-65535` | on a TCP or UDP rule, the side with no port; absent on an `ip` rule |

IP rules carry no VLAN.

**The shapes the ECS round left out, captured on the DX010 (2026-11-01).**
Once the DX010 claimed `max_custom_ip_acls` 128 (§11.7.6), five Block rules
were saved scoped to it alone, documentation addresses throughout, one per
shape; each save reached the DX010 as its own push within ~25 s, the driver
held the set and wrote nothing, and the rules were removed seven minutes
later (the controller's rule list `[]` again, the DX010's next push equal to
the one before apart from the two lines its new claims add). The last push is
`docs/fixtures/controller-10.6.109/system_cfg-dx010-acl.txt`:

- **UDP** is `type=udp`, shaped as `tcp`.
- **TCP/UDP** is two groups, `<name>-1` of type `tcp` and `<name>-2` of type
  `udp`, the rest identical. There is no TCP/UDP type on the wire.
- **Any** is `<side>.ip=any`, a literal value, not an absent key.
- **A source port** is `src.port=<n>`, the other side `port_range=1-65535`.
- **A Network side** is pushed as a CIDR like an IP side, the network's
  gateway address with its prefix length (host bits set). The network
  picked, a VLAN-only network with no subnet, was pushed as the gateway
  address of the controller's default LAN (a private /24), not anything of
  that network's. A rule naming a VLAN-only network therefore matches an address
  range the network does not have; that is UniFi's push, and the driver
  follows it.
- **A MAC rule cannot target the DX010**: with `max_custom_mac_acls` 0 the
  UI lists the switch under "ACL Rules are not applied to" for type MAC and
  greys it out in the Specific picker. (Since 2026-11-06 the DX010 claims
  MAC rules on the patched image; their push and the Device side are
  §11.7.8.)

**What reached the DX010.** The snooping save gave every switch a new
cfgversion. The DX010's push was otherwise byte-identical, with no
`dhcp_*` keys, so the driver's diffed apply wrote nothing. The ACL rules,
scoped to the ECS, did not reach the DX010 at all.

#### 11.7.3 What Ubiquiti's SONiC does with it (ECS, read-only)

From the ECS's CONFIG_DB and process list during the capture, described
here and not copied:

- **DHCP snooping is done in software.** With the setting on, the ECS gains
  one more L3 ACL table bound to every port and aggregate. Its one rule
  traps UDP 67 → 68 (server replies) to a CPU queue, and a snooping daemon
  starts. The per-VLAN server lists are then applied on the CPU. With the
  setting off, the table, the rule and the daemon are gone.
- **ACL rules map one to one.** Rule N of each kind becomes rule N in a
  table that exists from boot and stays empty while there are no rules: an
  L2-capable table for MAC rules and an L3 table for IP rules, both bound
  ingress to every port and aggregate.
  - Priority falls from just under 10000 in rule order.
  - `permit`/`deny` become FORWARD/DROP, and `tcp` becomes protocol 6.
  - A MAC is written with its mask, and the VLAN becomes a VLAN match.
  - The any-port range is kept as a range.

#### 11.7.4 What community SONiC 202505 can express on the DX010

Read from `sonic-swss` 202505 (e9dcd50) and from the switch's own STATE_DB,
read-only (`ACL_STAGE_CAPABILITY_TABLE|INGRESS`, `SWITCH_CAPABILITY|switch`,
CRM):

- **The L3 table type has every field the IP rules and the DHCP guard
  need.** It matches ether type, outer VLAN ID, IP type, source and
  destination IPv4, ICMP, IP protocol, L4 source and destination port and
  TCP flags. On Broadcom ingress it also takes L4 port ranges. It binds to
  ports and PortChannels.
  - Actions on this ASIC: packet action FORWARD, DROP and COPY, plus
    redirect, mirror and do-not-NAT.
  - There is **no trap action** (no user trap queue as on the ECS), so a
    software snooping path would need image work. COPY sends a copy to the
    CPU while the ASIC still forwards the original.
- **There is no outer MAC match and no L2 table type in orchagent.** Only
  the tunnel-inner MACs (`INNER_SRC_MAC`/`INNER_DST_MAC`) are in the match
  lookup, on 202505 and on the 202511 IGMP tree alike.
  - `sonic-acl.yang` on the switch already defines an `L2` table type with
    `SRC_MAC`/`DST_MAC` (`mac-addr-and-mask`), so YANG would accept a MAC
    rule. orchagent would not create it.
  - **MAC rules therefore cannot be honoured on a stock image.** The
    image kit's sonic-swss 0003 adds both (§11.7.8).
- **Three ingress ACL tables are free.** CRM shows `acl_table_available 3`
  for each of the PORT and LAG bind points, none used.
- **`config acl update full|incremental` must not be used.** It runs
  `acl-loader`, which adds a `DEFAULT_RULE` dropping all IPv4 at the lowest
  priority to every ingress L3 table it loads. UniFi's rules are
  default-allow, so that would black-hole the switch.
  - Tables: `config acl add table <name> L3 -p <ports> -s ingress` and
    `config acl remove table <name>` are safe.
  - Rules: write the `ACL_RULE|<table>|<rule>` hashes into CONFIG_DB
    directly (`PRIORITY` 0..999999, `VLAN_ID` 1..4094 per the YANG).
  - orchagent reports its verdict in STATE_DB:
    `ACL_TABLE_TABLE|<table>` and `ACL_RULE_TABLE|<table>|<rule>`,
    `status` `Active`/`Inactive`.

#### 11.7.5 The mapping (to build next)

Every table below is the driver's own (a `DUI_` prefix), bound ingress to
every front-panel port and PortChannel the switch has. The port list is
recomputed when a breakout or an aggregate changes it. Rules are a diff
against CONFIG_DB, read each collect from `ACL_RULE` with that prefix,
and verified through the STATE_DB status. Nothing else in `ACL_*` is
touched.

**DHCP guard → `DUI_DHCP_GUARD` (L3).** The table exists only while snooping
is on and at least one guard applies to a VLAN the switch has. For each
guarded VLAN v, there is one FORWARD rule per trusted server s, then one DROP
rule below them:

```
ACL_RULE|DUI_DHCP_GUARD|V<v>_S<k>  PRIORITY 9000-…  PACKET_ACTION FORWARD
    IP_PROTOCOL 17  L4_SRC_PORT 67  L4_DST_PORT 68  SRC_IP <s>/32  VLAN_ID <v'>
ACL_RULE|DUI_DHCP_GUARD|V<v>_DROP  (lower)          PACKET_ACTION DROP
    IP_PROTOCOL 17  L4_SRC_PORT 67  L4_DST_PORT 68  VLAN_ID <v'>
```

- `v'` is the switch's VLAN. UniFi's VLAN 1 is the `native_vlan` stand-in
  (§5.2a, 4094 on the DX010). A VLAN the switch does not carry is skipped.
- A VLAN with no guard group gets no rules, because UniFi has no server list
  for it.
- This is the guard done in hardware: a reply from an untrusted address is
  dropped at the ingress port, while the gateway's replies pass on every
  port.
- What it leaves out of Ubiquiti's version is the software part: no binding
  table and no client IP or hostname learning. The gateway knows both
  anyway.
- `DHCPSnooping` with no guards writes nothing.
- The model needs `DeviceDesired.DHCPGuards` (VLAN, server) beside the
  existing `DHCPSnooping`, filled by the loop from
  `unificfg.Config.DHCPGuards`.

**IP rules → `DUI_IP_ACL` (L3).** The table exists only while a rule applies.
Group N becomes `RULE_N` with `PRIORITY` 9999−N:

| Push | Rule field |
|---|---|
| `action` permit/deny | `PACKET_ACTION` FORWARD/DROP |
| `type` ip / tcp / udp | `IP_TYPE IPV4ANY` / `IP_PROTOCOL 6` / `IP_PROTOCOL 17` |
| `src.ip` / `dst.ip` | `SRC_IP` / `DST_IP` (absent = any) |
| `src.port` / `dst.port` | `L4_SRC_PORT` / `L4_DST_PORT` |
| `*.port_range` `1-65535` | nothing: it means any port, and a range would spend a range checker |
| any other `*.port_range` | `L4_SRC_PORT_RANGE` / `L4_DST_PORT_RANGE` (not seen; the UI refuses ranges) |

There is no catch-all rule: UniFi's ACLs are default-allow, and an Allow
matters only above a Block it would otherwise hit.

- **Between VLANs vs within.** The UI says IP rules filter traffic "between
  VLANs". The ECS's table nonetheless matches every IPv4 packet entering a
  bound port, bridged or not. The DX010 does the same (§9's rule: follow the
  ECS).
- **Model.** The model needs neutral `devicemodel` ACL types (permit, the
  protocol, both sides' CIDR, port and range; a MAC rule's VLAN, MACs and
  masks) in `DeviceDesired`. They are filled from `unificfg.Config.IPACLs`
  and `.MACACLs`, and the driver keeps no `system_cfg` names.

**MAC rules → refused on the stock image.** These need an orchagent patch:
an `L2` table type (bind PORT/LAG; match `SRC_MAC`, `DST_MAC`,
`OUTER_VLAN_ID`, ether type) and `SRC_MAC`/`DST_MAC` in the match lookup with
mac/mask parsing. SAI has the attributes (`SAI_ACL_ENTRY_ATTR_FIELD_SRC_MAC`,
`..._DST_MAC`), and the YANG model is already in place. The patch belongs in
the custom-image series (§8) and would use the third free ingress table.
Until then a `switch.acl.mac` push is logged and refused, and the claim
below keeps the controller from sending one. (Built 2026-11-06 as the
kit's sonic-swss 0003; §11.7.8.)

**Claims.**
- **DHCP snooping** is `feature_caps` 128
  (`UNIFI_SWITCH_CAP_DHCP_SNOOPING`). It is already wired from
  `Capabilities.DHCPSnooping` in `internal/device/caps.go`, and is claimed
  once the guard path is built.
- **Custom ACLs have no feature bit.** The ECS claims them with
  `switch_caps` numbers: `max_custom_ip_acls` 512, `max_custom_mac_acls`
  512, `max_global_acls` 512 and `max_acl_port_range` 4.
- **Claim for the DX010:** `max_custom_ip_acls` at a size the one table
  holds (start at 128; CRM's per-table entry counters on the switch are the
  authority once the table exists), and `max_custom_mac_acls` 0 on the
  stock image.
- **The gate (captured 2026-11-01).** `max_custom_ip_acls` alone (128,
  with `max_custom_mac_acls` 0 and no `max_global_acls` or
  `max_acl_port_range`) makes the UI offer the switch for IP rules and the
  controller push them; `max_custom_mac_acls` 0 keeps MAC rules away (the
  switch is greyed out in the picker). What `max_global_acls` and
  `max_acl_port_range` change is still unknown. The claim alone produced no
  push; the next rule save did.

**Hardware checks before claiming** (documentation-range rules, under §11.3's
dead-man timer; all three run 2026-11-01, §11.7.7):
- that `VLAN_ID` matches untagged frames in the native-VLAN stand-in, since
  Broadcom's ingress field processor qualifies the VLAN after VLAN
  assignment;
- which wins when a DHCP reply is dropped by `DUI_DHCP_GUARD` and forwarded
  by a `DUI_IP_ACL` rule, two tables being separate lookups (expected: the
  drop);
- the table's entry capacity.

**Cannot be done (stock image):**
- MAC rules (above).
- Ubiquiti's software snooping (binding table, client learning).
- Stopping a rogue server that spoofs a trusted server's address (UniFi's
  model is address-based too).
- Anything for DHCPv6 or router advertisements: the push carries no v6 guard
  keys.

#### 11.7.6 As built (2026-11-01)

`internal/drivers/sonic/acl.go`, on the mapping above. Departures and
details the spec left open:

- **Model.** `DeviceDesired.DHCPGuards` (UniFi VLAN, server), `IPACLs` and
  `MACACLs` with `IPACLSet`/`MACACLSet`; `Capabilities.IPACLs`/`MACACLs`
  render `max_custom_ip_acls`/`max_custom_mac_acls` (only from a driver that
  claims a count, so no other payload changes). The loop
  (`internal/informloop/acl.go`) translates from `unificfg`; the driver
  never sees a `system_cfg` key.
- **Shapes not captured are held, not guessed.** `unificfg` records every
  line of an ACL group it does not model and an action other than
  permit/deny (`Unmodelled`), and `IPACL.Uncaptured()` /
  `MACACL.Uncaptured()` name what no capture shows: a type other than
  `ip`/`tcp`/`udp`, a side without an `ip` line, an `ip` that is neither an
  IPv4 prefix nor `any`, a port on an `ip` rule, a range other than
  `1-65535`, a port and a range on one side, and a MAC side that is neither
  a MAC under a mask nor `any`. (Until the DX010 capture UDP, TCP/UDP, Any
  and a source port were held too.) One such rule holds **every rule of its
  kind** (`IPACLSet` false: the switch keeps what it has), because the
  rules are ordered and leaving one out changes what the others match; the
  loop logs the held rules when they change. The loop writes `any` as no
  address and every prefix as its network address (a Network side arrives
  with host bits set).
- **The rules never cut the bridge off.** They bind to the uplink too, so a
  Block matching the bridge's TCP to the address it dials (port 22) would
  isolate the switch from the push that removes it again -- the DX010 has
  one link and no console (§11.3). `collect.py` reads `SSH_CONNECTION` (the
  `ssh` section: the bridge's address and the switch address it dialled,
  as the switch sees them), and such a set is held with a log unless an
  earlier Allow lets that traffic through. A rule from a prefix that does
  not hold the bridge, of another protocol or port, or naming a source
  port (the bridge's is ephemeral) passes. Without the section every
  source counts.
- **Guard priorities are constant**: every FORWARD at 9000, every DROP at
  8000. Rules of one priority never overlap (one VLAN, one server each), and
  a server added or removed then writes one rule, not a renumbering. Rule
  names as specified (`V<v>_S<k>`, `V<v>_DROP`, v UniFi's VLAN). Since
  2026-11-14 a counted DROP per port sits between them at 8500
  (`V<v>_IN_<port>`) and the table has a type of its own (§11.7.9).
- **The bound ports** are the CLI's own rule (`get_acl_bound_ports`): every
  port that is not a PortChannel member, and every PortChannel with members;
  orchagent refuses to bind a member. The list is compared as a set (the CLI
  stores it in Python set order) and rewritten with `config acl add table`
  (the CLI's `set_entry` replaces the entry; orchagent updates a table whose
  type and stage are unchanged, `updateAclTablePorts`). `ApplyPorts` re-runs
  the ACL apply after its own batch, so a breakout's children and a new
  PortChannel are bound in the same push. A breakout's `-f` takes the parent
  out of the list first (the leafref is a DPB dependency).
- **Write order.** A table that goes loses its rules first (lowest priority
  first), then `config acl remove table` (which leaves rules in CONFIG_DB).
  A table that stays is bound before its rules change; stale rules go
  lowest priority first and new ones are written highest first, so a
  guard's FORWARD never trails its DROP. A changed rule is `del` then `hset`
  of the whole hash (orchagent re-creates a rule from the hash on every
  SET). One `bash -s` batch, then `config save -y`.
- **Verification.** `collect.py` reads `ACL_TABLE|DUI_*`, `ACL_RULE|DUI_*`,
  STATE_DB `ACL_TABLE_TABLE|DUI_*` / `ACL_RULE_TABLE|DUI_*` and CRM's
  `CRM:ACL_STATS:INGRESS:*` / `CRM:ACL_TABLE_STATS:*`. A table or rule not
  `Active` is logged once per status; the first collect after a write logs
  the tally and CRM's counters (the entry capacity once a table exists).
- **Claims**: `feature_caps` 128, `max_custom_ip_acls` 128,
  `max_custom_mac_acls` 0 (128 since 2026-11-06 when Start finds the
  kit's sonic-swss 0003 in orchagent, §11.7.8), wherever the collector reads the ACL sections
  (and Start then checks `config acl add|remove table`). `max_global_acls`
  and `max_acl_port_range` are not claimed: the capture showed the IP count
  alone gates the push (§11.7.5).
- **More rules than claimed** (128) hold the IP set with a log.
- **Fixtures** (read-only on the DX010, scrubbed):
  `sonic-202505.1238526-ff3c0c9f6/collect-acl.txt` (no DUI table yet, CRM
  three free ingress tables per PORT and LAG bind point) and the DX010's own
  `config-help.txt` (every group the driver checks, `config acl` included;
  the same commands as the VS's, the help flags in another order). The
  FixtureRunner models `config acl add|remove table` with the CLI's port
  check, the raw rule writes, the breakout dependency, and orchagent's
  verdict (`Active`, `Inactive`, or none while the table is missing).
  Tests: `acl_test.go` (the ECS's two pushes; guards on Vlan4094/2/69 and
  off, the IP rules, a deleted first rule, the aggregate and breakout
  rebind, the verdict, the DX010's shapes, the management guard),
  `internal/informloop/acl_test.go` (translation, holds),
  `replay_sonic_acl_test.go` (the DX010's own push with the ECS's lines,
  and its own ACL push, end to end), `unificfg` `TestACLUncaptured` and
  `TestParseDX010ACLRules`, `device` `TestSwitchCapsACLCounts`.
- **Captured live** (2026-11-01): the gate and the shapes above
  (`TestParseDX010ACLRules`, `TestDesiredDeviceDX010ACLShapes`,
  `TestIPRulesDX010Shapes`, `TestReplaySonicDX010ACLPush`).
- **Verified live** (§11.7.7): the guard and an IP rule written, Active,
  enforced and removed again; `VLAN_ID` on the native stand-in; guard drop
  over ACL forward; the entry capacity.
- **Untested live:** a rebind on a live breakout or aggregate (fixture- and
  replay-tested), a guard on a VLAN other than the native one (the DX010
  carries only VLAN 1).

#### 11.7.7 Hardware checks on the DX010 (2026-11-01)

Approved by Clint, run on build `33797d8` under §11.3's dead-man timer: a
300 s transient timer whose script deleted every `ACL_RULE|DUI_*`, removed
both tables, saved and brought eth0 up, re-armed over a fresh connection
before each stage and cancelled at the end (it never fired). Test traffic
was one-off UDP datagrams to the switch's own address from a LAN host, seen
or not seen by `tcpdump` on `Vlan4094` (the switch's CPU).

| Check | Done | Result |
|---|---|---|
| The guard's push | Rogue DHCP Server Detection on for 7 min | The DX010's push carried **one** guard group, VLAN 1 trusting the gateway (the ECS got six for its three guarded VLANs: the controller sends a switch only the guards of VLANs it carries). The bridge wrote `DUI_DHCP_GUARD` 2 s after the push: `V1_S1` (FORWARD from the gateway/32) and `V1_DROP`, `VLAN_ID 4094`, bound to all 32 ports; table and rules `Active`, `show acl rule` lists both. |
| `VLAN_ID` on the native stand-in | UDP 67 → switch:68 from a host that is not the gateway; control 66 → 68 | The 67s never reached the CPU, the 66s did: **`VLAN_ID 4094` matches untagged frames** of the stand-in (the field processor qualifies after VLAN assignment), and the guard drops an untrusted server's replies. |
| Guard drop vs ACL forward | Forged 192.0.2.10:67 → switch:68 with the guard alone, then with a `DUI_IP_ACL` Allow (UDP from 192.0.2.0/24 to any port 68) Active; control from port 66 | Dropped both times; the 66s arrived both times: **the guard's DROP wins** over a FORWARD in the other table, as expected. A UniFi Allow cannot open a hole in DHCP guarding. |
| Entry capacity | CRM `CRM:ACL_TABLE_STATS:<oid>` (polled every 300 s) | `DUI_DHCP_GUARD` 2 used / 766 available while it was the only table; `DUI_IP_ACL` 1 / 511. **At least 512 entries per table**; the claim stays at 128 (a fourfold margin, rules of one UI rule per port included). Ingress tables free before: 3 per PORT and LAG bind point. |
| Removal | Snooping off; the Allow rule deleted | `DUI_DHCP_GUARD` went in the designed order (DROP, FORWARD, table) 74 s after the save, `DUI_IP_ACL` 23 s after its delete; CONFIG_DB, STATE_DB and `config_db.json` hold no `DUI_` key afterwards. |

Side findings: the image's ACL counter polling is off (`counterpoll` ACL
disable), so `aclshow` reports N/A per rule; the driver did not need the
counters then (since 2026-11-14 the guard turns the polling on for its
per-port count, §11.7.9). The Allow rule with destination Any and
a destination port arrived as `dst.ip=any` plus `dst.port=68`, a shape the
loop already takes (Any on either side, a port on either side).

The site afterwards: the controller's ACL rules `[]`, Rogue DHCP Server
Detection off, the three networks' DHCP Guarding as before; the DX010's
push identical to its push before the checks (with the two lines its
claims add); no switch rebooted.

#### 11.7.8 MAC rules on the patched image (2026-11-06)

**The patch.** The image kit's `sonic-swss` 0003, a reduced port of
sonic-net/sonic-swss#2554 (open since 2022, approved, never merged). It adds
`SRC_MAC`/`DST_MAC` to orchagent's match lookup, parsed as `<mac>` or
`<mac>/<mask>`, and a built-in table type `L2`: bind PORT and LAG, match
`SRC_MAC`, `DST_MAC`, `ETHER_TYPE` and `OUTER_VLAN_ID`, with packet action
and redirect, ingress and egress.
- **Why the type is literally `L2`.** `sonic-acl.yang` allows
  `SRC_MAC`/`DST_MAC` only when the table's type is `L2`, and 202511
  validates the whole CONFIG_DB with sonic-yang before a breakout.
- **Why no `ACL_TABLE_TYPE` instead.** A CONFIG_DB `ACL_TABLE_TYPE` cannot
  stand in, because its matches are looked up in the same table the patch
  extends.
- **Its test.** The mock test `AclOrchTest.L2Acl_Mac_Matches` covers a
  masked source, an exact destination and refused malformed values. All
  19 `AclOrchTest` cases pass in the build's slave container. The package
  build does not run the mock tests.
- **The image.** `igmp-202511.20261106-f2e25f54c` (legacy-th), built from
  the kit with sonic-swss 0001-0003, sonic-l2mcd 0001-0010 and
  sonic-buildimage 0001-0012.

**The driver** (`internal/drivers/sonic/acl.go`).
- **Detecting the patch.** `Start` checks for it read-only:
  `docker exec swss grep -c -a -F 'expected <mac> or <mac>/<mask>' /usr/bin/orchagent`
  looks for the patch's own error string.
  - **Found:** the driver claims `max_custom_mac_acls` 128 and writes the
    rules to `DUI_MAC_ACL` (type `L2`, every port, ingress) beside
    `DUI_IP_ACL`, with the same batch, reconcile and verdict.
  - **Not found:** nothing is claimed, so no push should come; a push that
    does is refused with a log naming the patch.
- **Rule shape**, from the ECS's push (§11.7.3):
  - `RULE_N` with `PRIORITY` 9999-N, so the first rule in the UI's order
    wins;
  - `PACKET_ACTION` `FORWARD`/`DROP`;
  - `VLAN_ID`, with UniFi's VLAN 1 written as the native stand-in;
  - `SRC_MAC`/`DST_MAC` as `<mac>/<mask>`, a side that is Any being no
    field at all.
- **Holds.** More than 128 rules, a rule on the stand-in's own VLAN number,
  or a VLAN out of range holds the whole MAC set with a log.
- **Management guard** (`cutsManagementMAC`, like the IP one). A Block
  that would match frames from the gateway or from the far end of the
  bridge's connection, sent to the switch's MAC or to broadcast on its
  management VLAN, holds the whole MAC set unless an Allow matches first.
  The UI already refuses the management network for a MAC rule ("The
  network used to manage UniFi devices cannot be selected"). The guard is
  a second line, for a push that reaches the switch some other way.

**Captured** (2026-11-06, `controller-10.6.109/system_cfg-dx010-mac-acl.txt`;
`TestParseDX010MACACLRules`).
- **The picker.** The claim alone lists the DX010 in the Specific picker
  for type MAC. The rule save produced the push.
- **The groups** are the ECS's shape (§11.7.2), in UI order:
  - a Block from a typed MAC to Any (`dst.mac=any`,
    `dst.mask=00:00:00:00:00:00`);
  - an Allow from a Device to a typed MAC under a /45 netmask, a mask not
    on a byte boundary (`ff:ff:ff:ff:ff:f8`).
- **A Device side is just its MAC.** It is pushed as the client's MAC with
  mask `ff:ff:ff:ff:ff:ff`. The v2 API stores both kinds of side as
  `CLIENT_MAC` with `specific_mac_addresses`, and the editor redraws a
  typed MAC as a Device.
- **The fixture's MACs.** The typed MACs are kept as entered. The Device's
  real MAC is mapped to `02:00:00:00:00:03`.

**Verified live on the DX010** (2026-11-06, bridge `cc276ad` on the image
above):
- **The claim.** The controller stored `switch_caps.max_custom_mac_acls`
  128.
- **The write.** The push was applied 3 s after it arrived: `config acl add
  table DUI_MAC_ACL L2` on all 32 ports, then the rules.
- **The state.** CONFIG_DB, the saved `config_db.json`, STATE_DB `Active`
  for the table and both rules, and `show acl rule` all agree.
- **The chip.** The ASIC_DB entry holds `SAI_ACL_ENTRY_ATTR_FIELD_SRC_MAC`
  `02:00:00:00:00:01&mask:FF:FF:FF:FF:FF:FF`, `FIELD_OUTER_VLAN_ID`
  `2&mask:0xfff` and `PACKET_ACTION_DROP`. The /45 destination mask was
  accepted as written.
- **Deleting the first rule** renumbered the other (`del RULE_2`, then
  `RULE_1` rewritten, still `Active`).
- **Deleting the last** removed the rule and the table. CONFIG_DB, STATE_DB,
  `config_db.json` and ASIC_DB then hold no `DUI_` key and no ACL table.
- **The site afterwards.** The controller's ACL rules are `[]`.

**Traffic check** (2026-11-06 07:07-07:13 MST, Clint's go). No UI rule can
reach a frame on this switch: its one link carries only the native VLAN,
which is the management network the UI refuses for a MAC rule. So a
hand-made table stood in.
- **The setup.** A table `MACTEST` of type `L2`, bound to Ethernet0 alone,
  with one rule: `SRC_MAC 02:00:00:00:00:01/ff:ff:ff:ff:ff:ff`,
  `VLAN_ID 4094`, `DROP`. A 300 s systemd timer on the switch removed it.
- **The traffic.** A LAN host sent raw UDP frames from the test MAC and
  from a control MAC (`02:00:00:00:00:02`): three to the switch's own MAC
  and address, then three to broadcast. `tcpdump` watched `Vlan4094`, the
  switch's CPU.
- **The match works in the chip.** The rule's field-processor entry
  (`bcmcmd "fp show group"`: qualifiers SrcMac and OuterVlanId, action Drop)
  counted **6 packets, 384 bytes**. Those are exactly the six frames from
  the test MAC; none of the control MAC's six hit it.
- **Frames addressed to the switch still reached its CPU**, the blocked
  MAC's included. The to-CPU traps are field-processor entries in other
  groups whose action is `GpCopyToCpu`. On Broadcom a Drop in one group
  does not cancel a CPU copy made by another, so the drop removes only the
  forwarding copy. The broadcast frames reached the CPU from neither MAC,
  so they show nothing either way.
- **Not observed: a dropped switched frame.** That is the case UniFi's MAC
  rules are for ("traffic between devices on the same VLAN"), and seeing
  it needs a second port with a link. Every other port of the DX010 is
  down, so the check stops at the hit counter.
- **Open difference.** In §11.7.7 an L3 rule (`DUI_DHCP_GUARD`) did keep
  DHCP replies from the switch's CPU. Whether that trap's action differs,
  or the L3 table's drop does, was not traced.
- **Afterwards.** The timer removed the table (no `MACTEST` key in any DB,
  no ACL table or entry in ASIC_DB). Ethernet0 stayed up and the bridge
  informed every minute throughout.

**Not verified live:**
- **A switched frame dropped**, which needs two linked ports (above).
- **A rule on the native stand-in from the UI**, which the UI does not
  offer here.

Tested from fixtures: `acl_mac_test.go` (the ECS's push, refusal without the
patch, VLANs and holds, the management guard) and
`TestReplaySonicDX010MACACLPush` (the captured push through the loop: the L2
table, both rules as written live, the verdict, a quiet reconcile).

#### 11.7.9 Which port a rogue server is behind: the per-port count (2026-11-14)

**The goal.** UniFi raises "Rogue DHCP server detected on <device> Port N"
from the per-port `anomalies` bit 64, which the controller reads as what the
switch itself detected (seen live 2026-11-13: the alert came up while the
bridge still set that bit for a link flap; `docs/feature-map.md` §2). The bridge sets the bit when a port's `Health.RogueDHCPDrops` grows
between informs (`internal/device`). The guard of §11.7.6 drops an untrusted
server's replies silently, and its rules are bound to every port: a rule's
counter can say that replies were dropped, not where they came in. The alert
is per port, so the count must be too.

**What the ECS does.** Its guard is software (§11.7.3): a table traps UDP
67 -> 68 to a CPU queue and a snooping daemon applies the server lists, so
the daemon sees each reply's ingress port. The ECS's inform printed on the
switch (`controller-10.6.109/inform-ecs-aggregation-uswf066.json`, detection
off) has no `anomalies` key on any port, so no capture shows the ECS setting
bit 64. A read-only look at the ECS on 2026-11-14 was refused at login
(`Permission denied (publickey,password)` for the key that worked on
2026-10-28), so how its daemon reports was not read. Copying the pattern
needs a trap to a CPU queue, which this ASIC's ACL has no action for
(§11.7.4): the code reason to do it differently.

**The options weighed.**

| Option | Attribution | Guard semantics | Entries | Breakout, aggregate | Cost per poll |
|---|---|---|---|---|---|
| A DROP per port in place of the shared DROP (an ingress-port match) | true | **weakened**: a port without its rule (a breakout's new lanes, until orchagent has them) is unguarded | VLANs x (servers + ports) | the rules must follow every port change before the port is guarded | the rules' counters |
| **One counted DROP per port above the shared DROP** (chosen) | true (verified live, below) | **unchanged**: the shared DROP stays and catches whatever no counted rule matched | VLANs x (servers + ports + 1); 34 on the DX010 with one guarded VLAN; capped at 256 | a breakout's lanes lose their rules before it and get new ones after it; an aggregate's members are still physical ports, so nothing changes | two small Redis reads plus one per counted rule |
| A trap or mirror of the dropped replies to the CPU, counted by disunified-l2d | true | unchanged | one more table | none | none, but every reply (a flood too) reaches the CPU |
| SONiC's per-port drop counters (`PORT_INGRESS_DROPS`, reason `ACL_ANY`) | **no**: they count every ACL drop on the port, the IP and MAC rules' too | unchanged | none | none | one counter per port |

The trap is out for three reasons. The ASIC has no trap action. COPY
forwards the original, so the rogue's reply would pass. A mirror needs a
session to the CPU and a table, and the driver's three tables (guard, IP
rules, MAC rules) use the three ingress tables the DX010 has free (§11.7.4).
Whether this SAI offers `ACL_ANY` drop counters was not checked: they would
not be the guard's count.

**Why a table type of its own.** The guard's table was type `L3`, and
202511's aclorch (`initDefaultTableTypes`) gives `TABLE_TYPE_L3` no
`IN_PORTS` match. `L3V4V6` has one, but this SAI does not support it: STATE_DB
`ACL_STAGE_CAPABILITY_TABLE|INGRESS` says `supported_L3V4V6 false`. A
CONFIG_DB `ACL_TABLE_TYPE` row defines a type orchagent builds the table
from (`AclTableTypeParser`):

```
ACL_TABLE_TYPE|DUI_DHCP_GUARD  MATCHES@     IN_PORTS,VLAN_ID,IP_PROTOCOL,L4_SRC_PORT,L4_DST_PORT,SRC_IP
                               ACTIONS@     PACKET_ACTION,COUNTER
                               BIND_POINTS@ PORT,PORTCHANNEL
```

- **The `@`** marks a list in CONFIG_DB, as `ports@` does. swss-common strips it
  before orchagent sees the field name (`Table::stripSpecialSym`).
- **`config acl add table`** takes any type name (202511's
  `parse_acl_table_info` does not check it). There is no CLI for the type
  row, so it is a `sonic-db-cli` write like the rules.
- **sonic-acl.yang** makes `ACL_TABLE.type` a leafref to
  `ACL_TABLE_TYPE_NAME`, and `IN_PORTS` a plain string.
  - The leafref means the type row must exist before the table, and the
    table must go before the type.
  - Being a string, `IN_PORTS` is no dependency that `config interface
    breakout -f` would clear.
- **The whole CONFIG_DB with the row passed `validate_data_tree`** (live,
  below). That matters on 202511, which validates everything before a
  breakout (§11.8).

**As built** (`internal/drivers/sonic/acl_count.go`, beside `acl.go`).

- **Rules.** Per guarded VLAN v, unchanged: `V<v>_S<k>` FORWARD at 9000 and
  `V<v>_DROP` at 8000. New between them: one `V<v>_IN_<port>` DROP at 8500
  per front-panel port. It is the shared DROP's match plus `IN_PORTS <port>`.
  Rules of one priority never overlap, one port each.
- **Only ports orchagent has.** A counted rule is written only for a port in
  STATE_DB `PORT_TABLE`. aclorch refuses a rule whose `IN_PORTS` names a port
  it does not have ("Failed to locate port"), marks it `Inactive` and erases
  the task: it never retries (202511 `doAclRuleTask`). So a counted rule that
  is `Inactive` is written again (`del`, `hset`) at the next apply.
- **A breakout or a join.** The port batch deletes every rule whose `IN_PORTS`
  names one of the cage's lanes, before `config interface breakout`. Each such
  rule's SAI entry holds the port's object id (aclorch passes `IN_PORTS` as an
  object list). The plan also forgets those lanes in its copy of STATE_DB's
  ports: a child can have its parent's name (`Ethernet4`) and is still a new
  port. The new lanes get their rules at the first apply after STATE_DB lists
  them. The shared DROP guards the cage throughout.
  - Untried live: whether a rule left naming a leaving port would hold up
    the breakout. The rules simply go first.
- **An aggregate.** The table binds the PortChannel, as before. The counted
  rules name its members, which stay physical ports. `config portchannel
  member add` checks table bindings (`get_port_acl_binding`), not rules, so
  a join changes nothing here.
- **The cap.** The guard table holds at most 256 entries (`guardEntries`).
  Counted rules are added VLAN by VLAN in number order while they fit; a VLAN
  past the cap keeps its FORWARDs and its shared DROP, with a note naming it.
  - Measured: the stock L3 table took 512+ entries (§11.7.7). The guard's own
    type showed 2557 available beside 3 used (CRM, live, below).
  - A quarter leaves the IP rules' claim of 128 its margin.
  - One VLAN on 32 cages is 34 entries; on 128 lanes (every cage split in
    four) it is 130.
- **Counter polling.** Rule counters reach COUNTERS_DB only while orchagent
  polls ACL counters, which is off on the image (§11.7.7). With the guard
  written, the driver sets `FLEX_COUNTER_TABLE|ACL FLEX_COUNTER_STATUS
  enable`, the field `counterpoll acl enable` writes (202511 `counterpoll`
  `acl_enable`, `mod_entry`). It leaves it on: polling costs one SAI read per
  ACL rule every 10 s. A switch where polling was turned off again is logged
  ("orchagent does not poll ACL counters"), and the next apply turns it back on.
- **The read.** `collect.py` prints three sections:
  - `cfg_acl_type`: `ACL_TABLE_TYPE|DUI_*`;
  - `cfg_flex_acl`: `FLEX_COUNTER_TABLE|ACL`;
  - `acl_counters`: for each counted rule of a `DUI_` table, from COUNTERS_DB's
    `ACL_COUNTER_RULE_MAP` (`<table>:<rule>` -> counter oid), the counter's
    `SAI_ACL_COUNTER_ATTR_PACKETS`.
  The collector sums the packets of the guard's counted rules by each rule's
  own `IN_PORTS` (CONFIG_DB) over a port's lanes and the guarded VLANs. That
  sum is `Health.RogueDHCPDrops`. A rule with no value yet adds nothing.
- **Counters reset.** A rule written again starts from 0, and a deleted rule
  takes its counter out of the map (live, below). `internal/device` sets
  nothing on a decrease.
- **An earlier build's guard** (type `L3`, no counted rules) is replaced
  whole: its rules, then its table, then the type, the table and the rules.
  For a moment there is no guard.

**Verified live on the DX010** (2026-11-14 02:41-02:59 MST, image
`igmp-202511.20261106-f2e25f54c`, the deployed bridge running and left
alone). The reply came from a LAN host: its own UDP from port 67 to the
switch's address, port 68, as in §11.7.7 but unforged.
- **The test tables.** Every DROP matched only that host's address, so no
  real DHCP was touched.
  - The table was bound to Ethernet0 (the uplink) and cages 2-3 (empty).
  - A reply can only arrive on a linked port, and Ethernet0 is the DX010's
    only one. A MAC loopback on an empty cage (§8.9) would have needed the
    cage out of `Vlan4094`. The running bridge reconciles VLAN membership
    every cycle, and a looped port in the LAN's VLAN would send the LAN's
    floods back into it.
  - The first table was `DUI_RDTEST` (the deployed bridge leaves a `DUI_`
    table it does not manage alone). The read-path check used
    `DUI_DHCP_GUARD` itself, built by hand just after a bridge inform. The
    deployed bridge would remove it within a cycle (the push says snooping
    off), and it did not get to.
- **Cleanup.** A 15-minute and then a 10-minute systemd timer held the
  cleanup. It was run by hand after each stage and the timer stopped.

| Check | Done | Result |
|---|---|---|
| The type and IN_PORTS | type row as above; rules at 8600 `IN_PORTS Ethernet4`, 8500 `IN_PORTS Ethernet0`, 8000 no port | table and every rule `Active`; `show acl rule` lists `IN_PORTS` |
| Attribution | 5 replies | Ethernet0's rule **5 packets (1410 bytes)**; the empty cage's rule, one priority **above** it, **0**; the shared DROP 0. The port match holds in the chip. |
| A trusted server | a FORWARD for the host at 9000, 5 replies | the FORWARD 5, Ethernet0's counted rule still 5: trusted replies are not counted |
| The shared DROP still guards | the FORWARD and Ethernet0's rule deleted, 3 replies | the shared DROP 3, the empty cage's rule 0. A deleted rule's counter left `ACL_COUNTER_RULE_MAP`. |
| Capacity | CRM `CRM:ACL_TABLE_STATS:<oid>` | 3 used, **2557 available** on the custom type |
| YANG | `sonic_yang` `loadData` + `validate_data_tree` over `sonic-cfggen -d --print-data` with the type row | passes; no table without a model |
| The capture | the rules renamed to the design's (`V1_S1`, `V1_IN_Ethernet0`, `V1_IN_Ethernet4`, `V1_DROP`), 4 replies, `collect.py` from the branch | `acl_counters`: Ethernet0 4, Ethernet4 0; scrubbed as `sonic-igmp-202511.20261106-f2e25f54c/collect-rdtest.txt` |
| The driver's read | `DUI_DHCP_GUARD` of the same shape, 3 replies, `disunified -driver sonic -device-ssh ... -collect-once` built from the branch | `RogueDHCPDrops` **3 on port 1, 0 on the other 31** |
| Restore | cleanup after each stage | no `DUI_` ACL key in CONFIG_DB or STATE_DB, `ACL_COUNTER_RULE_MAP` empty, ACL polling `disable` running and in `config_db.json` (which never held a test key), no ACL table, CRM 0 tables; Ethernet0 up, the bridge informing throughout |

**Tested from fixtures:**
- `acl_count_test.go`: the live capture's counters, the per-port rules and
  count, the replacement of an `L3` guard with polling turned on, the wait for
  orchagent's ports, the `Inactive` rewrite, a breakout, the cap, polling off.
- The ECS-push tests in `acl_test.go` and `TestReplaySonicDHCPGuardAndACL`,
  updated.
- `TestReplaySonicRogueDHCPPerPort`: the push through the loop, replies
  counted on cage 3, the next inform's `anomalies` 64 on port 3 alone, then 0.

**Not verified live:**
- A reply on a port other than the uplink, which needs a second linked port
  (rack day).
- An aggregate member's count.
- A breakout with counted rules in place.
- The replacement of a deployed `L3` guard (detection is off on the site).
- The cap.
- The controller's alert from this bit, which is the end-to-end check after
  the deploy.

### 11.8 Full retest on the 202511 IGMP image (2026-11-02)

The DX010 was reinstalled with the custom 202511 image (§8,
`igmp-202511.20261101-22cac574e`) and every control the driver implements
was driven again from the controller UI (Network 10.6.109) through the
deployed bridge, overnight and unattended, on the empty cages 2-8 (Ethernet0,
the uplink, was touched only by the site-wide jumbo setting). Each push was
checked on the switch (CONFIG_DB, STATE_DB, mstpd, chrony, the sudo
journal) and every override and site setting was put back afterwards, the
restore confirmed in the controller's objects and in every affected device's
next push. The capture of each finding is a fixture, the fix a test, and the
driver fixes are **committed but not deployed**: the session's permission
check refused the rebuild on the deployment host, so the rows marked *fixed*
below are fixture-tested and wait for a deploy and a re-run.

**202511 against 202505, as seen from the driver:** `config interface
breakout` now validates CONFIG_DB with sonic-yang (two dangling leafrefs
refused the split, below); `config mirror_session span add` is gated by the
SAI's `PORT_INGRESS/EGRESS_MIRROR_CAPABLE` flags (`'false'` on the DX010,
so the CLI refuses every SPAN direction, and orchagent applies the same
answer to a row written past it -- the re-run below); every PORT write adds `dhcp_rate_limit: 300` (wrong: the field was on every port before, §11.17); `config
spanning-tree interface` spells `bpdu-guard`; `config` gains
`igmp-snooping`, `mld-snooping` and `dhcpv4_relay`. Everything else the
driver sends behaved as on 202505. The first `config save` on the new
image rewrote the saved `config_db.json` as `version_202511_01`.

| # | Feature (UI action) | What the switch did | Result |
|---|---|---|---|
| 1 | Port state and name (cage 2 Disabled + a name, then back) | `config interface shutdown Ethernet4`, `hset 'PORT\|Ethernet4' description <name>`, save (3 s); restore `startup`, `hset description ''`, `hdel description` | pass |
| 2 | VLANs on cage 2: native kids; Allow All on a non-Default native; Allow All on Default; Block All; Custom kids+visitor; back | `config vlan add 2`, membership moves (`del 4094`, `add -u 2`); Allow All with a non-Default native logged as the stand-in's limit (VLAN 1 tagged cannot be expressed) and left the port; Allow All on Default created the nine other site VLANs (`config vlan add -m ...`) and tagged them; Block All removed them; Custom `add -m 2,69`; each 3-7 s. l2d gave each new VLAN its own STP instance | pass (the ten site VLANs stay on the switch; the driver never deletes a VLAN) |
| 3 | Forced speed and FEC on cage 3: 40G (the UI picks FC-FEC itself), FEC off, 100G, RS-FEC, FEC off, auto | `config interface speed Ethernet8 40000` then `config interface fec Ethernet8 fc` **refused** (`fec fc is not in ['none', 'rs']`, portconfig's text on stdout, exit 1: the cage's SAI runs none and rs only); the controller re-pushed the same cfgversion every ~70 s until FEC was unticked. 100G, RS, none and auto each 3 s | pass except FC-FEC: **fixed** -- a FEC the cage's `supported_fecs` lacks is skipped with a note and the rest of the port applies (`TestPushedFECTheCageCannotRun`); the bridge's error now carries the command's stdout tail (`sshrun`, `TestRunErrorCarriesStdout`) |
| 4 | Breakout of cage 2 to 4x25G (REST, no `fec_mode`: the Port drawer refuses every FEC at 25G on a QSFP28 port, `api.err.InvalidFecMode`, and insists on one) | `config interface breakout -f -y Ethernet4 '4x25G[10G]'` **refused by sonic-yang** (202511 validates CONFIG_DB before a breakout): `COPP_TRAP\|stp` → no `COPP_GROUP\|queue4_group1` (l2d wrote the trap only; the group lives in `copp_cfg.json`) and `MGMT_INTERFACE\|eth0\|169.254.1.1/16` → no `MGMT_PORT\|eth0` (the §10.6 hand fix made no port row). Eight retries, nothing changed; reverted through REST. The lane speeds, 2x50G and the join could not run | fail: **fixed** -- l2d puts the COPP group in CONFIG_DB with the trap (`TestL2dWritesTheTrapGroupWithTheTrap`); the driver adds the `MGMT_PORT\|eth0` row before a breakout when the address row has none (`TestBreakoutAddsTheManagementPortRow`). The hand fix for the row was refused by the session's permission check (a root write) |
| 5a | Storm control on cage 3: 1 % broadcast + multicast (Level), then Rate mode, then off | `config interface storm-control add Ethernet8 broadcast 1000000`, `unknown-multicast 1000000` (kbps: 1 % of 100G); Rate mode made the driver **remove** storm control silently (SONiC storms are kbps only) | pass; Rate mode **fixed** to a note per port (`TestDesiredPortsStormRateIsNotedOnce`) |
| 5b | Mirror of cage 5 onto cage 4 (the picker defaults to port 1, the uplink -- changed before Apply) | `config vlan member del 4094 Ethernet12` then `config mirror_session span add dui4 Ethernet12 Ethernet16 both` **refused** (`Port mirror direction 'both' is not supported by the ASIC`: 202511's `is_port_mirror_capability_supported` reads the SAI's two `'false'` flags; a SPAN row written by hand programs the ASIC and reads back Active); nine retries; back to Switching restored the VLAN | fail: **fixed** -- where the flags refuse, the `MIRROR_SESSION` row is written through `sonic-db-cli` and its STATE_DB status read back (`TestMirrorWrittenDirectWhereTheCLIRefuses`, on the 202511 capture and its own `config --help`) |
| 6 | Aggregation of cages 6+7, then its removal | `config portchannel add PortChannel1 --fast-rate true --min-links 1`, the MTU hset, members out of their VLAN and in, the channel into the native VLAN, save (9 s); removal the reverse (10 s); teamd ran the channel (both members down: empty cages); l2d reconciled both times | pass (the UI renamed the second member to the profile default "QSFP28 7" on aggregation and kept it after; restored by hand) |
| 7 | Per-port STP on cage 3: Edge (UniFi turns BPDU guard on with it), Infrastructure with BPDU guard, STP off, STP on with loop protection, back to auto | each an l2d config apply (0.2 s): mstpd admin/auto/oper edge and BPDU guard as asked, BPDU guard set on a down port (the 2026-11-01 fix holds), STP off = edge + BPDU filter, loop protection in `l2.json` with the port listed; `DUI_STP_PORT_TABLE` mirrored each | pass |
| 8a | STP priority 61440, then 32768 | `spanning tree rstp priority 61440`: mstpd bridge id `F.000.<mac>`, root and root port (Ethernet0) unchanged; back to `8.000` | pass |
| 8b | Locate | `dx010:green:stat` blinking (sampled 1 1 0 0 0 1 1 0), steady after Stop Locating | pass |
| 8c | Rename the device | `config hostname <UniFi name>` + save (3 s; `service hostname-config restart`), `hostname`, `DEVICE_METADATA` and `/etc/hosts` followed; the first live run of the hostname apply; restored | pass |
| 8d | Restart | the reply `_type: reboot` → "reboot requested: reloading the switch"; the SSH session dropped under `sudo reboot` (context deadline exceeded) and the bridge logged "reboot: switch reload issued" (the 2026-11-01 fix, now live); SSH answered 78 s after the request, on the same image with the saved config intact (32 ports at 1500, the eleven VLANs, the hostname, Ethernet0 up with RS-FEC); `eth0` came back `UP` without carrier on its static link-local address and **no DHCP lease or default route** (the §3 B3 hand fix holds across a reboot); the in-band default route and `NTP\|global src_intf Vlan4094` intact; chrony had both sources online two minutes after boot; l2d up with Ethernet0 the root port; the controller showed the switch Online within a cycle | pass |
| 8e | Upgrade (emulated) | the reply `_type: upgrade`, "upgrade to 4.0.1.995 requested (emulated reboot)", the next push 0 changes; the UI shows 4.0.1 with a Revert button; the switch untouched | pass |
| 9a | Rogue DHCP Server Detection on, with cage 4 carrying a tagged guarded network (kids) | `config acl add table DUI_DHCP_GUARD L3 -p <all 32>`; `V1_S1`/`V1_DROP` (VLAN_ID 4094) **and `V2_S1`-`V2_S3`/`V2_DROP` with `VLAN_ID 2`** -- the guard of every VLAN the switch carries, each network's own server list; all Active. When cage 4's tag was removed the V2 rules went in the same cycle; detection off removed the table (DROP, FORWARD, table) and the Proxmox nodes' and the PDU's next pushes carried the restore | pass (closes the register's "guard on a tagged VLAN" item) |
| 9b | One IP ACL rule (Allow UDP any → any:68, switch-specific) | `config acl add table DUI_IP_ACL L3 -p <all 32>`; `RULE_1` {IP_PROTOCOL 17, L4_DST_PORT 68, FORWARD, PRIORITY 9998}, Active; the delete removed rule then table | pass |
| 9c | Aggregation of cages 5+6 **while both tables were bound** (the register's rebind check) | `config portchannel member add PortChannel1 Ethernet16` **refused**: `Port Ethernet16 is already bound to following ACL_TABLES: ['ACL_TABLE\|DUI_DHCP_GUARD', 'ACL_TABLE\|DUI_IP_ACL']` (sonic-utilities' `get_port_acl_binding`, on 202505 too; test 6 passed only because no table existed then). Ten retries; the batch had already made the PortChannel and taken Ethernet16 out of its VLAN. Removing the aggregation restored the VLAN but left the memberless `PortChannel1`, which the driver deleted only when it had emptied it itself | fail: **fixed** -- the joining lanes leave the driver's tables inside the batch, before the member add, and `applyACL` binds the PortChannel after as before; a memberless PortChannel with the driver's own LACP settings that no port wants is removed (`TestACLTablesFollowThePorts` fails on the old order, `TestOrphanPortChannelOfOurOwnGoes`; the push is `system_cfg-dx010-aggregate-acl.txt`). The rebind itself (PortChannel in, members out, Active) still waits for the deploy; **`PortChannel1` is still on the switch** until then or a `config portchannel del` |
| 10a | Jumbo Frames on, then off (site-wide) | `config interface mtu <p> 9216` on all 32 ports, Ethernet0 first (47 s for 33 commands), then `1500` x32; the uplink's MTU moved 1500→9216→1500 with no link event (zero `up to down` on this boot); the Proxmox nodes and the PDU got fresh pushes with 0 changes | pass (run without the §11.3 dead-man: a SONiC port MTU is a SAI attribute set, the adoption push had already moved Ethernet0's MTU over that link, and the dead-man's `systemd-run` is itself a root write the session could not make) |
| 10b | NTP, syslog, time zone, SSH keys read back against the push | `NTP_SERVER` = the push's two servers, both reached in chrony; `SYSLOG_SERVER\|<host>` (514); `MST7MDT,M3.2.0,M11.1.0` → `MST7MDT`; the managed `authorized_keys` block holds the push's four keys, the operator's two lines outside it untouched | pass |
| 11 | Static address move | not run (Clint's call: the risk of losing the only path) | skipped |
| 12 | The read side in the UI and the stored record | PSUs 77/74 W of 900, five fans, 56 °C, memory, uptime, the module's DOM (100G CWDM4, 29 °C, -3.3/+0.6 dBm), parent core port 27 at 100G; `mac_table_capability` is **dropped from the stored record for the `USWF07D` too** (only the real USWF066 keeps it); the 24 h anomaly counts on cages 4 and 5 are the guard period's DHCP toast and the LAG attempt | pass |

**Blocked by the session's permission check (root writes, never worked
around):** the `MGMT_PORT\|eth0` hand fix, the redeploy of the fixes, the
orphan `PortChannel1`'s removal, and `Ethernet8`'s `fec: none` leftover
(CONFIG_DB keeps the explicit `none` from test 3; harmless). **Cannot be
closed on this unit yet:** the second core cable (LACP state), IGMP
snooping's hosts (§8), MAC ACL rules (an orchagent patch; built and
verified 2026-11-06 on the next image, §11.7.8). What tonight
left on the switch: the ten site VLANs, `fec none` on Ethernet8,
`dhcp_rate_limit 300` on every written port (there before the writes, §11.17), and
`PortChannel1` without members.

#### Re-run after the deploy (2026-11-02, 14:25-14:56 UTC)

66bf6c8 and f723e82 were deployed at 14:21 UTC (HEAD 4c511fd). The first
apply removed the orphan `PortChannel1` (`ownOrphan`, live) and the l2d
reinstall put `COPP_GROUP|queue4_group1` in CONFIG_DB. Only what the deploy
changed, or what had never run, was driven again, one pass each, restored
and followed by a zero-change cycle. Rogue DHCP Server Detection, the
site-wide half of (e)'s precondition, was **refused by the session's
permission check** (a shared-setting change); the rebind needs any of the
driver's tables bound, so (e) ran with one IP ACL rule scoped to the DX010
alone, and no other device got a push. The live lines are
`docs/fixtures/sonic-igmp-202511.20261101-22cac574e/rerun-20261102.txt`.

| # | Re-run | What the switch did | Result |
|---|---|---|---|
| 3 | FC-FEC at 40G on cage 3 (REST: speed 40000, `fec_mode fc-fec`), then auto | the note "the controller asks for FEC fc, which Ethernet8 does not support (none,rs); FEC left as it is" and `config interface speed Ethernet8 40000`, save (3.5 s); no failed batch, no re-push; auto: `speed 100000` | **pass** (fixed, verified live) |
| 5a | Storm control Rate mode on cage 3 (Multicast 100 pkts/s), then off | one NOTE ("storm control in Rate mode (packets/s) is not something this switch can apply ...; storm control left off on the port"), 0 of 32 changed; off: 0 changed | **pass** (fixed, verified live) |
| 5b | Mirror of cage 5 onto cage 4 (REST: `op_mode mirror`, `mirror_port_idx 5`), then off | the row went in past the CLI (`hset 'MIRROR_SESSION\|dui4' ...`, 3 s) and the controller read the mirror back; removal through the CLI restored the destination's VLAN. **But nothing was mirrored:** orchagent logged `setUnsetPortMirror: Port ingress mirror is not supported by the ASIC`, `Failed to configure mirror session dui4 port Ethernet16`, `Failed to activate port mirror session dui4`, then `Created mirror session dui4` -- 202511's MirrorOrch applies the same SAI answer (`querySwitchPortMirrorCapability` 0/0) and creates the SAI session without binding the source port. STATE_DB's `inactive` meant that, not "destination down". The session object stayed in ASIC_DB after the removal (orchagent skips deactivating a session that never activated). The hand test of the night before (`duitest`) had failed the same way | **fail, not fixable in the driver on this image**: b26fb67 refuses the mirror with one note where SWITCH_CAPABILITY says no; port mirroring needs an ACL-based mirror or a SAI that answers the capability query (follow-up) |
| 4 | Breakout of cage 2 to 4x25G (REST, no `fec_mode`) | the driver added `MGMT_PORT\|eth0` (logged once) and the split was **refused by the next sonic-yang rule**: `Must condition "(contains(current(), ':') and contains(../gwaddr, ':')) or (contains(current(), '.') and contains(../gwaddr, '.'))" not satisfied` on `MGMT_INTERFACE\|eth0\|169.254.1.1/16` -- every MGMT_INTERFACE row needs a gateway of its family, and `config interface ip add eth0 <ip>` without one writes `{"NULL": "NULL"}`. One attempt, nothing else written; reverted through REST. A read-only `sonic_yang.loadData()` then showed the next refusal behind it: the driver's own `NTP\|global src_intf Vlan4094` (sonic-ntp.yang allows no VLAN there). With both changed in memory the unit's config validates | **fail**: 7dca204 clears `src_intf` for a VLAN instead of setting it (chrony then sources from the kernel's route, in-band); b26fb67 refuses the breakout with a note while eth0's row has no gateway. **The eth0 row is a site decision** (a gateway becomes a default route on eth0 in the management table, which the syslog server's forced rule follows; removing the row returns eth0 to DHCP, `metric 202` on 202511). The lanes at 10G, 2x50G and the join still never ran. **Closed 2026-11-07**: the row removed, every mode run live (§11.11) |
| 9c | Aggregation of cages 5+6 while `DUI_IP_ACL` was bound (UI) | **`config acl add table DUI_IP_ACL L3 -p <30 ports, without Ethernet16/20>` first**, then the PortChannel, its MTU, the members, the native VLAN, save (11 s); then `DUI_IP_ACL` rebound with `PortChannel1` (31 entries); table and rule Active, LAG in STATE_DB. Removal: members out, back in VLAN 4094, `portchannel del`, the table rebound to all 32. The controller deleted port 6's override when it joined (the UI showed "QSFP28 6"); restored by REST | **pass** (fixed, verified live) |

Not deployed: b26fb67 and 7dca204 (the session's permission check refused
the rebuild). The switch after the re-run: the ten site VLANs, `fec none`
on Ethernet8, `dhcp_rate_limit 300` on 31 ports (§11.17), `MGMT_PORT|eth0`
(added by the driver, now saved), `NTP|global src_intf Vlan4094` (until
7dca204 is deployed), and the leaked SAI mirror session in ASIC_DB (no
port bound to it; gone at the next swss restart or reboot). No ACL table,
no PortChannel, no mirror row, cage 2 unbroken.

Both commits were deployed later the same day (15:11Z, by the
coordinating session): the DX010's first apply dropped NTP's source
interface, chrony restarted unbound and re-selected both servers at
stratum 2 within two minutes.

### 11.9 Port mirroring on the DX010: the ACL route (spec, 2026-11-02)

**Why the SPAN path cannot work on this unit.** orchagent asks the SAI at
start whether a port can carry `SAI_PORT_ATTR_INGRESS_MIRROR_SESSION` /
`EGRESS_MIRROR_SESSION` (`SwitchOrch::querySwitchPortMirrorCapability`,
`switchorch.cpp`). A query the SAI cannot answer is recorded as
**`true`**; `false` is written only when the SAI answers and says the
attribute is not implemented. The DX010's `SWITCH_CAPABILITY|switch`
carries `PORT_INGRESS_MIRROR_CAPABLE false` and `PORT_EGRESS_MIRROR_CAPABLE
false`, and syncd logged no "capability query not supported" line for the
port object at boot: Broadcom's `libsaibcm 13.2.1.100` (the legacy
Tomahawk branch) itself denies port-bound mirroring. `MirrorOrch` and the
CLI both honour that answer (§11.8), so no row the driver writes can bind a
source port. The only way to learn whether the SAI would in fact accept the
attribute is an orchagent build that ignores the answer -- a probe patch in
the 202511 series (`switchorch.cpp`, treat `set_implemented false` as
capable), hours of build on VM 116 and an image install; kept as a register
item, not part of this spec (the probe below then showed the SAI binds;
§11.10 is that patch).

**How the ECS does it (read 2026-11-02 on the USWF066).** Ubiquiti's glue
writes a port-based SPAN session: a `MIRROR_SESSION` row with `type SPAN`,
`dst_port`, the `src_port` list and `direction BOTH` -- the row shape this
driver already writes (§6) -- and nothing through ACLs. Its SONiC 4.0.1
(202106) orchagent predates the capability gate (its `SWITCH_CAPABILITY`
has no `PORT_*_MIRROR_CAPABLE` rows) and Marvell's SAI (`mrvllibsai
1.8.1`) binds the ports. Its ACL capability rows read like the DX010's:
`MIRROR_INGRESS_ACTION` at ingress, none at egress. So the ECS pattern is
port SPAN in both directions, and the DX010 falls short of it only because
Broadcom's legacy-TH SAI answers the port-mirror capability query with no.

**What the same SAI does offer.** `SWITCH_CAPABILITY|switch` has `MIRROR
true` and `MIRRORV6 true` (the ACL table types; orchagent logs
`TABLE_TYPE_MIRROR: yes` at init), and `ACL_STAGE_CAPABILITY_TABLE|INGRESS`
lists `MIRROR_INGRESS_ACTION` among the ingress actions. The `EGRESS` stage
lists `PACKET_ACTION` only. So the ASIC can mirror **frames received on a
port** through an ingress ACL rule, and nothing can mirror frames the
switch sends out of a port. **Mirroring on the DX010 is therefore
ingress-only**, and the driver says so in a note once per session; a
UniFi switch mirrors both directions, and this stays short of parity until
the probe patch (or a newer SAI) says otherwise.

**Design.**

1. *Session.* `MIRROR_SESSION|dui<dst>` with `type SPAN` and `dst_port`
   only: no `src_port`, no `direction`. `MirrorOrch::activateSession`
   binds ports only when both are set (`mirrororch.cpp`, the
   `!session.src_port.empty() && !session.direction.empty()` branch), so a
   destination-only session is created and reported `active` in
   `STATE_DB MIRROR_SESSION_TABLE|dui<dst>` as soon as the destination's
   link is up, `inactive` while it is down. Written with `sonic-db-cli`
   (`hset ... type SPAN dst_port <dst>`), not the CLI: the CLI's gate is
   about port binding and the row never asks for one. Removal: `config
   mirror_session remove dui<dst>` as today. The destination keeps today's
   treatment (every VLAN membership stripped before, restored after).
2. *Table.* One ACL table **`DUI_MIRROR`**, type `MIRROR`, stage
   `ingress`, bound to every source port of every session
   (`AclOrch::addAclTable` allows one `MIRROR` table per stage; it binds
   to the `PORT` and `LAG` bind points). Created and rewritten with
   `config acl add table DUI_MIRROR MIRROR -p <ports> -s ingress` exactly
   as `DUI_DHCP_GUARD`/`DUI_IP_ACL` are (`set_entry` replaces the row,
   rules survive); removed with `config acl remove table DUI_MIRROR` when
   the last session goes. On Broadcom XGS orchagent builds the combined
   v4+v6 mirror table (`m_isCombinedMirrorV6Table true`), which changes
   nothing for rules that match ports only. CRM: the ingress `PORT` bind
   point reports **3 ACL tables available** on the DX010 (`CRM:ACL_STATS:
   INGRESS:PORT`); guard + IP ACL + mirror is exactly three, so the three
   tables fit and a fourth of any kind would not -- the driver logs the
   `STATE_DB ACL_TABLE_TABLE|DUI_MIRROR` status after the write, as it does
   for the other two.
3. *Rules.* One per source port: `ACL_RULE|DUI_MIRROR|SRC_<src>` with
   `PRIORITY` (9999 downwards, any distinct values), `IN_PORTS <src>` and
   `MIRROR_INGRESS_ACTION dui<dst>`. `AclRuleMirror::validate` needs at
   least one match, and `IN_PORTS` is that match: the rule mirrors every
   frame the port receives, whatever its type. `AclRuleMirror::activate`
   programs the SAI entry only while the session is `active`, and
   `AclOrch::update` re-runs it on every `SUBJECT_TYPE_MIRROR_SESSION_CHANGE`,
   so a rule written while the destination is down takes effect when it
   comes up -- the driver writes rows and does not wait. Rule status is in
   `STATE_DB ACL_RULE_TABLE|DUI_MIRROR|SRC_<src>`.
4. *Choosing the route.* The ACL route is used when the collect shows
   `PORT_INGRESS_MIRROR_CAPABLE` or `PORT_EGRESS_MIRROR_CAPABLE` present
   and not `true` (today's `portMirrorUnsupported`), **and** `MIRROR` is
   `true` in `SWITCH_CAPABILITY|switch`, **and** `ACL_STAGE_CAPABILITY_TABLE|
   INGRESS` lists `MIRROR_INGRESS_ACTION`. The VS and 202505 (no flags)
   keep the SPAN CLI path unchanged; a unit with the flags false and no
   ACL mirror support keeps today's refusal note. `collect.py` gains the
   `ACL_STAGE_CAPABILITY_TABLE|INGRESS` row (a later section, like
   `state_switch_cap`), and the `DUI_*` ACL sections already cover the
   table, its rules and their status.
5. *Read side.* `Port.MirrorFrom` comes from the `DUI_MIRROR` rules (the
   `IN_PORTS` of the rule whose action names `dui<dst>`), since the session
   row no longer carries `src_port`; the SPAN path keeps reading
   `MIRROR_SESSION`. The note, once per session per run: "port N mirrors
   what port M receives; this switch cannot mirror what it sends out of
   port M (SAI: no port-bound mirror; ACL mirror is ingress-only)".
6. *Interactions.* `aclUnbind` (f723e82) takes a lane out of the driver's
   tables before a `portchannel member add`; it must accept the
   `MIRROR`/`ingress` shape as the driver's own, and a rule whose
   `IN_PORTS` is a lane that joins a LAG is removed with the unbind (a
   mirrored port that becomes a LAG member is no longer a port the
   controller can mirror; the loop already handles the override). A
   breakout of a mirrored cage follows the same rebind step the other two
   tables use (`applyACL` after `ApplyPorts`). Order within an apply stays
   as §6 lists it: mirror removals (rule, then the table if empty, then
   the session) first; session, table, rules after the VLAN and LAG work.
   A source that is a PortChannel: the table binds the PortChannel (a
   `LAG` bind point) and `IN_PORTS` lists its members; whether orchagent
   accepts the member list is checked on the fixture first and, if
   refused, a LAG source is declined with a note.

**Captures first (Clint's rule), on empty cages, by hand, before any
code:** the session `dui4` with `dst_port Ethernet12` only; the table
`DUI_MIRROR MIRROR -p Ethernet16 -s ingress`; the rule `SRC_Ethernet16`
as in 3. Capture CONFIG_DB, `STATE_DB` (`MIRROR_SESSION_TABLE`,
`ACL_TABLE_TABLE`, `ACL_RULE_TABLE`), the ASIC_DB ACL entry once the
session is active, `CRM:ACL_STATS`, and orchagent's syslog lines; then
remove in the reverse order and capture again. With no link on cage 4 the
session stays `inactive` and the rule unprogrammed; the capture then
proves orchagent accepts the rows, and **packet-level proof waits for two
cabled devices** on the DX010 (a sender on the source cage, a capture
host on the destination) -- the first time a cage is cabled, mirror it
and look. The fixture runner models `config acl add table ... MIRROR`, the
`MIRROR_INGRESS_ACTION` rule, the status rows and the
`ACL_STAGE_CAPABILITY_TABLE` section; tests on the 202511 capture
(`rig202511`): the route chosen, the writes in order, the read-back of
`MirrorFrom`, removal, the LAG-join unbind, the breakout rebind, and the
SPAN path untouched on the VS capture (`TestApplyMirror`). Docs: §6's
mirror row, the support matrix row ("ingress-only on the DX010"), the
feature map, this driver's `CLAUDE.md`.

**Asking the SAI without a rebuild (2026-11-02).** Whether Broadcom's
`false` is honest can be tested on the running switch: orchagent never
calls the SAI itself, it pushes a record into ASIC_DB's operation queue,
syncd pops it, calls the vendor library and, in synchronous mode (`syncd
-s`; `DEVICE_METADATA|localhost synchronous_mode enable` on this image),
writes the SAI status to the response queue. So the one record a patched
orchagent would write can be pushed by hand with `redis-cli -n 1` and
syncd's answer read back:

```
EVAL "redis.call('LPUSH', KEYS[1], ARGV[1], ARGV[2], ARGV[3]); redis.call('PUBLISH', KEYS[2], ARGV[4])" 2 \
  ASIC_STATE_KEY_VALUE_OP_QUEUE ASIC_STATE_CHANNEL@1 \
  SAI_OBJECT_TYPE_PORT:oid:<source port vid> \
  '["SAI_PORT_ATTR_INGRESS_MIRROR_SESSION","1:oid:<mirror session vid>"]' \
  Sset G
```

The port and session VIDs come from `COUNTERS_PORT_NAME_MAP` and
`ASIC_STATE:SAI_OBJECT_TYPE_MIRROR_SESSION:*`; the undo is the same push
with `"0:null"`. The status lands in `GETRESPONSE_KEY_VALUE_OP_QUEUE`; a
reply orchagent did not ask for is discarded by its `RedisChannel::wait`
with a warning. **The op is `Sset`, not `set`**: swss-common's
`ProducerTable` prefixes every op with `S` (set) or `D` (del) and the
consumer's Lua strips that first character. A bare `set` reaches syncd as
`et`, which `processSingleEvent` throws on -- syncd enters shutdown-wait,
tells orchagent, orchagent exits, and swss, syncd, teamd and bgp restart:
forwarding was gone for about 80 s (16:49:11-16:50:35Z; the bridge missed
one collect) and, because swss recreates the kernel bridge, spanning tree
was gone for ten minutes until `disunified-l2d` was restarted by hand
(ae2ce0e makes l2d do that itself, §11.5). That first attempt was mine,
with the wrong op. In sync mode a *failed* SAI set is only logged (`api
set failed in syncd mode`) and answered; syncd throws on failure only in
asynchronous mode, so the correctly encoded probe cannot repeat the crash.
A `SAI_STATUS_SUCCESS` means the capability query lied and the orchagent
probe patch gives ECS-style SPAN; `NOT_SUPPORTED`/`NOT_IMPLEMENTED` means
the ACL route above is all there is. Frames actually copied still need two
cabled devices, or a read of the SDK's mirror table through the Broadcom
shell.

**Result (2026-11-02 17:19Z): the Broadcom SAI binds a port to a mirror
session; the capability query lied.** With a SPAN row written to
CONFIG_DB (`type SPAN`, `dst_port Ethernet12`, no source) orchagent created
and activated the SAI session as before. The push of
`SAI_PORT_ATTR_INGRESS_MIRROR_SESSION = 1:oid:<that session>` on
Ethernet16's port object came back `SAI_STATUS_SUCCESS` in under two
seconds, and the SDK shell (`bcmcmd "mirror show"`) showed the silicon
programmed: `ce4: Mirror ingress to local port ce3` (ce4 is Ethernet16,
ce3 Ethernet12). The undo (`0:null`) answered `SAI_STATUS_SUCCESS` and the
shell reported `No mirror ports configured`. So port-based SPAN, the ECS's
pattern, works on this SAI once orchagent stops believing the query: the
orchagent probe patch (`switchorch.cpp`, treat the query's `false` as
capable, or skip the query) in the 202511 series gives mirroring in both
directions without a driver change, and the ACL route above stays the
fallback for images that cannot be rebuilt. **The egress attribute is
unprobed**: the bridge's own reconcile removed the probe row between the
two pushes (UniFi owns the session list; `config mirror_session remove
duiprobe` ran at 17:19:52Z), so the egress push named a session object
that no longer existed and syncd stopped again (`translateVidToRid:
unable to get RID for VID`, a throw before any SAI call, so synchronous
mode does not soften it: **a stale object id in a hand-pushed op kills
syncd whatever the mode**) -- the second restart of the day, both mine
(see §11.5's gap for the first). This time l2d re-enabled spanning tree
by itself 20 s after the bridge reappeared (17:21:52Z; the fix below,
deployed an hour earlier, live-proven). A future probe writes the row,
probes and removes it in **one** script, with the row re-read before
every push.

### 11.10 Port mirroring on the DX010: the orchagent capability patch (2026-11-02)

The ECS's way (§11.9: a port-based SPAN session, both directions, no ACL)
on the DX010, by taking out the gate that §11.9's probe showed to be wrong.
No driver change: with the capability rows `true` the driver's existing
SPAN path (`config mirror_session span add dui<dst> <dst> <src> both`, §6)
runs, and `portMirrorUnsupported` (b26fb67) stays for images without the
patch.

**The patch** (`series/sonic-swss/0002-switchorch-do-not-record-port-mirroring-as-unsupport.patch`
in the IGMP image kit, after 0001's l2mc series, base `177c7a0`). In
`SwitchOrch::querySwitchPortMirrorCapability`, an answer of
`set_implemented=false` for `SAI_PORT_ATTR_INGRESS_MIRROR_SESSION` or
`SAI_PORT_ATTR_EGRESS_MIRROR_SESSION` is recorded as `true`, the same as
the existing branch for a query that fails:

```diff
     else
     {
-        if (capability.set_implemented)
-        {
-            fvVector.emplace_back(SWITCH_CAPABILITY_TABLE_PORT_INGRESS_MIRROR_CAPABLE, "true");
-            m_portIngressMirrorSupported = true;
-        }
-        else
-        {
-            fvVector.emplace_back(SWITCH_CAPABILITY_TABLE_PORT_INGRESS_MIRROR_CAPABLE, "false");
-            m_portIngressMirrorSupported = false;
-        }
-        SWSS_LOG_NOTICE("port ingress mirror capability %d", capability.set_implemented);
+        // The Broadcom legacy-TH SAI (libsaibcm 13.2.1.100, Tomahawk-1) answers
+        // set_implemented=false here, yet it sets SAI_PORT_ATTR_INGRESS_MIRROR_SESSION
+        // and programs the port. Take a "no" as a failed query: record the
+        // capability, and let MirrorOrch::setUnsetPortMirror log a set the SAI
+        // refuses.
+        fvVector.emplace_back(SWITCH_CAPABILITY_TABLE_PORT_INGRESS_MIRROR_CAPABLE, "true");
+        m_portIngressMirrorSupported = true;
+        SWSS_LOG_NOTICE("port ingress mirror capability %d (recorded as supported)", capability.set_implemented);
     }
```

and the same for egress. Nothing else: MirrorOrch reads
`m_portIngress/EgressMirrorSupported` and the CLI reads the STATE_DB rows,
so both follow. The query arrived with sonic-net/sonic-swss#3934
(2025-11-21; the 202511 backport is #4137) and the CLI's gate with
sonic-net/sonic-utilities#4089 (202511: #4204); #4323 later made the CLI
skip the flags for ERSPAN and treat an absent key as supported, but an
explicit `false` still refuses SPAN. The port-mirroring HLD
(`doc/port-mirroring/SONiC_Port_Mirroring_HLD.md`, rev 0.2) has the system
assume full support only when detection *fails*. 202505's orchagent has no
query and asks every SAI to bind, and so does the ECS's 202106.

**A known upstream defect, not a new one.** sonic-net/sonic-buildimage#28139
(open, Triaged) is the same false answer on Broadcom Tomahawk-5 (Micas
M2-W6940), with Broadcom's reply "fixed in SAI 15.2, backport to 14.3".
That does not reach the DX010: it is on the separate legacy-Tomahawk SAI
train (`platform/broadcom/sai-legacy-th.mk`: 13.2.1.100 on 202511,
13.2.1.120 on master via sonic-net/sonic-buildimage#27875, an ABI rebuild
against OCP SAI 1.18.1 only), so the orchagent patch stays the route for
this unit. No PR upstream; a comment on #28139 is Clint's. The swss unit tests (`switchorch_ut.cpp`) check only that the
two rows exist. The orchagent log line keeps the SAI's own answer
(`port ingress mirror capability 0 (recorded as supported)`), so a boot log
still shows that the SAI said no.

**If the SAI does refuse a bind (the egress attribute was never probed).**
`MirrorOrch::setUnsetPortMirror` logs `Failed to configure TX session on
port …` and hands the status to `handleSaiSetStatus`, which for any status
but the retry ones (`INSUFFICIENT_RESOURCES`, `TABLE_FULL`, `NO_MEMORY`,
`NV_STORAGE_FULL`) calls `handleSaiFailure(…, abort_on_failure=false)`: an
error log, a structured `sai-operation-failure` event, a SAI dump requested
from syncd, and `gOrchUnhealthy`, which makes orchagent's main loop repeat
the error line about once a second until it restarts. No abort.
`parseHandleSaiStatusFailure(task_failed)` returns true, so the session is
still reported active with the ingress half bound. On the syncd side a
failed set in synchronous mode (`synchronous_mode enable`, read on the
DX010 2026-11-02) is answered and logged, not thrown (`Syncd.cpp`: the
throw is only `if (!m_enableSyncMode)`). So a genuinely unsupported egress
bind degrades to a noisy log, not a crash; no second patch. The read-back
below shows which case the DX010 is in.

**Build.** The kit's own script on the build host, Broadcom only, in a
fresh work directory so the tree the running image came from stays:
`SKIP_PREFLIGHT=1 WORK=~/sonic-mirror OUT=~/out PLATFORMS=broadcom ./build.sh`
(preflight's disk check budgets for two trees; this run builds one, and
every other check passed). The series is the running image's plus this
patch and sonic-buildimage 0011 (sonic-net/sonic-buildimage#26983, the
`dx010_cpld` `setreg_store` fix the running image carries as a hand-swapped
platform deb; now built in). The kit's `dry-run.sh` passed on the new
series first. Build id: **`igmp-202511.20261102-e53777446`** (series applied 17:42Z); the DX010 takes
`sonic-broadcom-legacy-th.bin`.

**Install (Clint's call: a reboot of a switch with one live link).** With
the kit's `sonic-upgrade.sh` as for the running image: `arm` installs
beside the running image, which stays GRUB's default, boots the new one
once under a 20-minute dead-man, and `confirm` makes it the default. The
image's own `install.sh` deletes every image but the running one and the
new one, so the 202505 image leaves `/host` (a copy is kept off the
switch); `sonic-installer cleanup` is never run. After `confirm`, redo the
settings that live in the image's overlay (the fan floor, on an image
built before series patch 0012 carried it; the DIMM sensor's limits, before
0013; the follow-up register lists them). Then accept the controller's update
badge once: the emulated version (`4.0.1.995`) is dropped whenever the
switch's own version changes (`internal/device/session.go`), so after every
install the controller offers the upgrade again. The UI's Upgrade, or
`cmd/devmgr {"cmd": "upgrade", "mac": <mac>}` through the API, arrives as a
reply `_type: upgrade`: the bridge records the version and reports it, and
writes nothing to the switch (2026-11-07 after the `20261106` image: the
controller went upgrading (state 4) -> provisioning (5), the next informs
carried `4.0.1.995`, its push changed 0 of 32 ports, the badge cleared).

**Installed and verified 2026-11-02 (03:53-04:05Z).** The image went in
with `sonic-upgrade.sh arm` (the 202505 image deleted as announced, its
copy on the Podman host; the on-switch bridge's deb, mstpd and the platform
deb carried), `sudo reboot`, SSH back after 185 s with swss, syncd, l2d
(RSTP, the same root) and the bridge all active -- the bridge reinstalled
by the image's first boot, its start-up reconcile setting the VLAN MTUs,
the first inform answered 200 with 0 of 32 ports changed -- and `confirm`
at 03:58Z. STATE_DB `SWITCH_CAPABILITY|switch` reads
`PORT_INGRESS_MIRROR_CAPABLE true` and `PORT_EGRESS_MIRROR_CAPABLE true`
(orchagent's boot log still says `port ingress mirror capability 0
(recorded as supported)`). Then cage 4 set to mirror cage 5 through the
REST API (`op_mode mirror`, `mirror_port_idx 5`, both cages empty): the
push arrived on the next cycle and the driver's ordinary path ran
(`config vlan member del 4094 Ethernet12 ; config mirror_session span add
dui4 Ethernet12 Ethernet16 both`), orchagent logged `Activated mirror
session dui4` with no `setUnsetPortMirror` refusal, STATE_DB `status
active`, and `bcmcmd "mirror show"` listed **both** `ce4: Mirror ingress
to local port ce3` and `ce4: Mirror egress to local port ce3` -- so the
egress attribute, never probed by hand, binds on this SAI too. The
removal (`config mirror_session remove dui4 ; config vlan member add -u
4094 Ethernet12`) left `No mirror ports configured` and the cage back in
its VLAN. No new fixture was captured: the build differs from
`22cac574e`'s capture only in the two capability rows. **Packet-level
proof is out of scope**: it needs two cabled devices on the DX010 (a
sender on the source cage, a capture host on the destination); the SDK's
mirror table is the proof available without them.

### 11.11 Breakout on the 202511 image, every mode live (2026-11-07)

**eth0's address row removed.** Every breakout on 202511 had been refused
by sonic-yang on `MGMT_INTERFACE|eth0|169.254.1.1/16` = `{"NULL": "NULL"}`
(§11.8: a row needs a gateway of its family). Of the two ways out, a gateway
or no row, the row went: a gateway becomes `default via <gw> dev eth0 table
default metric 201` in the management table. The template's forced rule
`to <syslog server>/32 lookup default` (pref 32764, skipped only for a
server with a `vrf`) would then have sent syslog out an eth0 without
carrier. Checked first, read-only:
- `sonic_yang.loadData()` over `sonic-cfggen -d --print-data` passes with
  the row removed in memory, with and without an in-memory 4x25G split of
  `Ethernet4`.
- `interfaces.j2` then renders `iface eth0 inet dhcp` with `metric 202`
  (against the in-band default's 20), without the policy-routing rules.
- Nothing binds to eth0: `NTP|global` has no `src_intf` (§10.6),
  `SYSLOG_SERVER` has no `vrf`, `TELEMETRY|gnmi` names no interface, no
  `SNMP_AGENT_ADDRESS_CONFIG`, `DNS_NAMESERVER` is static (so
  `resolv-config.sh` keeps it).

Then `sudo config interface ip remove eth0 169.254.1.1/16 && sudo config
save -y`. **On 202511 that removes the row and nothing else.** The CLI no
longer restarts any service, and hostcfgd's `MgmtIfaceCfg` compares the
deleted row's data (`{}`) with what it loaded at start, where
`ConfigDBConnector` had already turned `{"NULL": "NULL"}` into `{}`. It sees
no change, so `interfaces-config` is not restarted, and eth0 kept its
address, routes and rules until `sudo systemctl restart interfaces-config`
was run by hand: an hostcfgd bug for any field-less MGMT_INTERFACE row. The
restart regenerated `/etc/network/interfaces` and restarted networking
(lo and eth0 only). eth0 now runs dhclient (no carrier, no lease, no
route), the forced syslog rule and the `from 169.254.1.1` rule are gone,
and syslog to its server leaves from Vlan4094. The in-band default (FRR,
metric 20), chrony (two sources online), DNS and the bridge's informs were
untouched; the next push changed 0 ports. `MGMT_PORT|eth0` (§10.6) stays:
harmless and valid. **Rescue path from now on:** plug eth0 into a LAN port
and it takes a DHCP lease (in the in-band subnet: a second connected
route, for rescue only).

**The run, from the controller's REST exactly as the UI sends it** (port
2's override: `setting_preference manual`, `autoneg false`, `speed`; no
`fec_mode`), on cage 2 (`Ethernet4`, empty), deployed build bf64692. Each
step checked in CONFIG_DB, APPL_DB and STATE_DB, in mstpd and the STP
port-state tables, in the L2MC static tables, and in the controller's
port table. Each was followed by a cycle whose reconcile wrote nothing.

| # | Push | What the switch did | Result |
|---|---|---|---|
| 1 | 25G | `config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null`, then per child `startup` + `mtu 1500`, `vlan member add -u 4094` x4, save (20 s); IGMP: the 7 reserved groups extended to the three new lanes (21 rows). Children `Eth2/1`-`Eth2/4`, lanes 69-72, 25G; STATE_DB `supported_speeds 1000,10000,25000`, `supported_fecs none,fc`; mstpd added the four ports, each with a state in all 11 VLANs' instances; Ethernet0 stayed the root port, forwarding. mstpd's "Couldn't set kernel bridge state blocking" for each new port is the kernel refusing a state on a down port, logged for every down port at boot too. Controller: "Eth2" at 25G, `speed_caps` gained 1G | pass |
| 2 | 10G | `config interface speed Ethernet4..7 10000`, save (8 s) | pass (first time on hardware) |
| 3 | 50G | `… '2x50G'`, children `Ethernet4` (lanes 69,70) and `Ethernet6` (71,72) at 50G, no `fec` field, `supported_speeds 20000,40000,50000`, `supported_fecs none,rs`; IGMP removed the 14 rows of the lanes that went; Ethernet5/7 gone from mstpd and the STP tables (15 s) | pass (first time on hardware) |
| 4 | 100G | `… '1x100G[40G]'`, `startup`, `mtu`, `vlan member add -u 4094 Ethernet4`, save; IGMP removed 7; CONFIG_DB, APPL_DB, STATE_DB, VLAN and L2MC rows identical to before the run (field order aside) | pass |
| 5 | one IP ACL rule (Allow, 192.0.2.0/24 → 198.51.100.0/24, `specific_enforcers` = the DX010 only) | `config acl add table DUI_IP_ACL L3 -p <32 ports> -s ingress`, `RULE_1`; table and rule `Active` | pass |
| 6 | 25G with the table bound | the breakout's `-f`: `ConfigMgmt: Deleting …/ACL_TABLE_LIST[ACL_TABLE_NAME='DUI_IP_ACL']/ports[.='Ethernet4']` (the FixtureRunner's model); then `applyACL` rebound the table to 35 ports with `Ethernet4`-`Ethernet7`; `Active` throughout; orchagent logged warnings only ("Cannot remove port as bridge port OID is present", retried; FDB-flush noise) | pass |
| 7 | 100G pushed within a cycle of 6 | **withheld for good**: "port 2 … is new since the controller's config and would be changed by it; not written". The first collect after the split marked the cage fresh (its interfaces changed). The join arrived in that same cycle, so the planner said port 2 would change. The switch (four lanes) and the controller (one port) never come to match, so it stayed. Unstuck by setting 25G (matches, clears the flag), then 100G a cycle later: the join and the rebind to 32 ports, `Active` | fail: **fixed** (below) |
| 8 | the rule deleted | rule `del`, `config acl remove table DUI_IP_ACL`, save; no `DUI_` row left in CONFIG_DB or STATE_DB | pass |

**Fixed, deployed (29e71ae) and verified live the same hour:**

- *The join after a split* (`informloop`): `applyPorts` records the ports
  the plan writes from the controller's config, and the next collect does
  not mark a layout change on one of them fresh ("port layout changed as
  the controller's config asked"). Live: a split, then the join pushed at
  once, ran in the next cycle. Tests: `TestReplaySonicDX010BreakoutSeries`
  and `TestReplaySonicDX010BreakoutUnderACL`, on the run's own pushes;
  both fail on the old loop.
- *1G on a split cage* (`breakout.go`): a split cage claims 1G (its lanes'
  `supported_speeds`, merged into the claim), but the plan knew only the
  platform modes' speeds and refused it. A speed no mode runs that every
  current lane supports is now a lane speed. That is what `config
  interface speed` checks: portconfig looks at `supported_speeds` alone,
  not the mode, and the FixtureRunner now models that. A speed some mode
  runs keeps the mode path, so 40G on a 2x50G cage is still the join to
  1x40G. Live: the controller refused 1G (`api.err.InvalidInterfaceSpeedDuplex`) until an
  inform had carried the split cage's caps, then the bridge wrote
  `config interface speed Ethernet4..7 1000` (APPL_DB followed, no
  orchagent error), a quiet cycle, and the join from 1G. Tests:
  `TestSplitCageRunsTheLanesOwnSpeeds`, `TestReplaySonicDX010LanesAt1G`.

Port 2's override is back to its pre-test value (all 32 equal to the
record saved before the run), and the restoring push changed 0 ports.
Fixtures: `controller-10.6.109/system_cfg-dx010-brk-*.txt` (the run's
pushes; the rule's RFC 5737 prefixes put back after the scrub, which maps
every address) and `sonic-igmp-202511.20261106-f2e25f54c/` (the switch with
the eth0 row gone, whole and split, with its `config --help` and zone
list). **Still hardware-only:** link at each speed, which needs a 4x25G /
2x50G breakout cable and a far end.

### 11.12 Port isolation, MAC filter, egress limit, LLDP-MED and Voice VLAN (2026-11-07)

Clint, 2026-11-07: build what a UniFi switch does and the DX010 does not.
Each feature was claimed, its push captured from the UI on empty cages
29-31, built against the capture, and verified live on the DX010
(image `igmp-202511.20261106-f2e25f54c`), following what the ECS does
where community SONiC 202511 has the same mechanism.

**What the ECS does (read-only, described, not copied).** Port security
there is `learn_mode cpu_trap` on the port plus a software-learning daemon
of Ubiquiti's (a `PORT_SECURITY` table in CONFIG_DB, kernel bridge rules
admitting the listed MACs). LLDP-MED and the voice VLAN are lldpd custom
TLVs under the TIA OUI 00-12-BB, set per port by Ubiquiti's lldp manager
(`lldpcli configure ports <p> lldp custom-tlv ...`), because lldpd's own
`-M` would need a restart. The ECS carries no isolation or shaping state
while both are off.

**The claims.** `feature_caps` gains PORT_ISOLATION (4), LLDP_MED (16),
EGRESS_RATE_LIMIT (32) and PORT_SECURITY (512) once the collector reads the
new sections (LLDP-MED once lldpcli answers). With them the controller
pushes `switch.port.N.lldpmed.opmode` (and `topology_notify` while on) and
`port-security` for every port.

**The captured keys** (`docs/fixtures/controller-10.6.109/system_cfg-dx010-portfeat-*.txt`):

| UI | Key | Off |
|---|---|---|
| Port Isolation | `switch.port.N.isolation=enabled` | absent |
| MAC Address Filter | `switch.port.N.port-security=enabled`, `switch.port.N.port-security.mac.K=<mac>` | `port-security=disabled` |
| Egress Rate Limit | `switch.port.N.egress_rate_limit_kbps=<kbps>` (the UI's Mb/s x 1000) | absent |
| LLDP-MED | `switch.port.N.lldpmed.opmode=enabled\|disabled`, `lldpmed.topology_notify=disabled` | `opmode=disabled` |
| Voice VLAN (needs a tagged VLAN) | `switch.vlan.<slot>.port.N.voice=enabled` beside that VLAN's `mode=tagged` | absent |

The port override's fields (`rest/device` `port_overrides`; no record
fixture): `isolation`,
`port_security_enabled` + `port_security_mac_address`,
`egress_rate_limit_kbps_enabled` + `egress_rate_limit_kbps`,
`lldpmed_enabled` + `lldpmed_notify_enabled`, `voice_networkconf_id`.

**The mechanisms on 202511, probed on cages 30/31 first (§11.3 rules):**

- **Isolation**: one SAI isolation group of type port, `DUI_ISOLATION`,
  through orchagent's `isolationgrouporch`, which reads APPL_DB
  `ISOLATION_GROUP_TABLE` only (no CONFIG_DB table, no YANG, no CLI): the
  isolated ports are both members and bind ports, so a frame entering one
  cannot leave by another, and reaches every other port. A PortChannel
  member is represented by its PortChannel. The Tomahawk-1 SAI programs it
  (ASIC_DB `ISOLATION_GROUP` and `_MEMBER`, `SAI_PORT_ATTR_ISOLATION_GROUP`
  on each bind port).
- **MAC filter**: `learn_mode drop` on the port (or on its PortChannel,
  which teammgrd hands to the LAG) -- the SAI's
  `SAI_BRIDGE_PORT_FDB_LEARNING_MODE_DROP`: an unknown source is not
  learnt and not forwarded -- and one static FDB entry per allowed MAC in
  every VLAN the port carries (APPL_DB `FDB_TABLE`, fdborch;
  `SAI_FDB_ENTRY_TYPE_STATIC`). An empty list blocks everything, as on a
  UniFi switch. Going off is `learn_mode hardware` first: removing the
  field alone never reaches the ASIC (portmgrd passes only the fields that
  remain).
- **Egress limit**: a port shaper, `SCHEDULER|DUI_EGRESS_<kbps>`
  (`meter_type bytes`, `pir` = kbps x 125 bytes/s, `pbs` 10 ms of the rate
  and at least two jumbo frames) named by `PORT_QOS_MAP|<p> scheduler`
  (qosorch, `SAI_PORT_ATTR_QOS_SCHEDULER_PROFILE_ID`; one profile per rate,
  deleted once nothing names it). The DX010's L2 preset has no QoS maps, so
  the row holds only the scheduler. Every lane of a cage gets it.
- **LLDP-MED**: the capabilities TLV (subtype 1, `00,01,04`: LLDP-MED
  capabilities, network connectivity device) per port, on by default as
  UniFi's is; with a Voice VLAN it is `00,03,04` and a network policy TLV
  (subtype 2: application voice, tagged, the VLAN, priority 5, DSCP 0 --
  the ECS's values) follows. `topology_notify` (SNMP traps) is not
  modelled.

**APPL_DB is written through ProducerStateTable.** A plain `sonic-db-cli
APPL_DB hset` never reaches orchagent: its consumers wait on the table's
key set and notification channel (probed: nothing programmed, nothing
logged). The plan collects the APPL_DB writes and runs them as one
`python3` call at the end of the batch (`disunified-appl`, the operations
as a JSON argument; the log shows it by that name).

**What does not survive a restart, and what restores it.** The isolation
group, the static FDB entries and lldpd's TLVs live in APPL_DB and in the
lldp container: a swss or lldp restart drops them, and the loop's
reconcile on the next cycle writes them again. `learn_mode` and the shaper
are CONFIG_DB and kept by `config save`, so a secured port stays closed
across a reboot (its allowed MACs come back with the first reconcile).

**`learn_mode` is in neither `sonic-port.yang` nor
`sonic-portchannel.yang`**, and sonic-yang leaves an entry with an unknown
leaf untranslated (`_xlateList`'s leaf lookup), so a breakout -- which
validates all of CONFIG_DB first (§11.8) -- would be refused while any port
is secured. The breakout's batch takes every `learn_mode` out just before
`config interface breakout` and puts it back right after; the ASIC keeps
dropping meanwhile. Modelled from the source, not from an observed
refusal.

**A provisioned FDB entry is not a station.** STATE_DB lists the allowed
MACs like learnt ones, and on an empty cage UniFi showed them as connected
clients; static entries on a port without link are no longer reported.

**Live, 2026-11-07** (`replay_sonic_portfeat_test.go` replays the five
pushes over the capture taken before the claims, `collect-portfeat.txt`):
the push for port 30 (isolation, two allowed MACs, 100 Mb/s, LLDP-MED off)
and port 31 (isolation) wrote `learn_mode drop`, the shaper, the
unconfigure of port 30's TLV and one ProducerStateTable call (the group,
two FDB entries). Read back: CONFIG_DB, APPL_DB, ASIC_DB (group with two
members bound to both ports, `FDB_LEARNING_MODE_DROP`, two static entries
on port 30's bridge port, the scheduler at 12500000 bytes/s on port 30)
and lldpd (31 of 32 ports sending the TLV); the next cycle wrote nothing;
the two removal pushes took all of it out in order (learning back to
hardware). Port 29 with VLAN 69 tagged as its Voice VLAN: `00,03,04` and
the policy `01,40,8b,40` in lldpd. Ethernet0 stayed up throughout.

**Not proven, needs a second linked port** (Clint's rack day): an
isolated port's frames not reaching another isolated port while reaching
the uplink; a non-listed source dropped on a secured port and a listed one
forwarded; the shaper's measured rate; an LLDP-MED phone or switch reading
the TLVs.

**Multicast Router Port** (IGMP family, found unclaimed while capturing):
the UI's per-port setting arrives as `switch.vlan.<slot>.igmp_mroute_port.K=
<port>` in each snooping VLAN, and becomes `L2MC_MROUTER|Vlan<id>|<iface>
{mrouter_port}`, the row the CLI's `igmp-snooping mrouter add` writes and
l2mcmgrd reads (a PortChannel member as its PortChannel); the controller's
list is the switch's, a row made by hand goes. Written and removed live on
cage 28; l2mcd lists it in APPL_DB only once the port is oper up (its rule
for members, §8.14), so the empty cage stayed out of
`L2MC_MROUTER_TABLE`.

**Fan mode** is §11.15: the controller keeps Fan Mode for no switch, so
the DX010's mode is the device's `fan_mode` setting.

### 11.13 Layer 3 switching: routed networks, static routes, DHCP (2026-11-07)

UniFi's Layer 3 switching, captured on the DX010 and built the same
evening. The ECS claims it (`feature_caps` 1, `max_l3_intf` 64,
`max_static_routes` 256) and the DX010 now does wherever FRR runs
(`FEATURE|bgp` enabled, every community image); the controller then offers
the switch as a network's **Router** and as the device of a **switch**
static route (Routing -> Create New Route -> Static -> Device: Switch).

**The push** (`controller-10.6.109/system_cfg-dx010-l3-{1-dhcp-server,
2-routes,3-relay-isolated,4-none,5-off}.txt`; scrubbed with
`sanitize-controller.py --subnet` so the subnets survive, in 198.18.0.0/15).
`switch.routing_enabled` is in every push (`false` with nothing routed).
Each routed network is a `switch.routing_interface.N.*` group: `vlan`,
`ip`/`mask` (the switch's own address), `default_route`, `opmode`
(`manual` = no DHCP, `dhcp_server`, `dhcp_relay`), `purpose` (`guest` is the
UI's **Isolate Network (ACL)**, with `switch.routing_interface.guest_network
=true`), `dhcp_server.{ipstart,ipend,gateway,leasetime,dns.K}` or
`dhcp_relay.K.{id,ip}`, and `switch.dhcp_server=true` / `switch.dhcp_relay=
true` beside them. **Routing the first network on a switch makes the
controller create a network of its own**, "Inter-VLAN routing" (VLAN 4040,
a /24, routed by the gateway): a transit whose interface on the switch
(`opmode manual`, no `type`) carries the switch's default route as
`switch.routing_interface.1.route.1` = 0.0.0.0/0 via the gateway's address
there; the VLAN joins `switch.vlan.*` but with every port `exclude` -- the
operator must allow it on the uplink. Removing the last routed network
removes the transit too. Switch routes are filed under some interface:
`route.K.{prefix,mask}` with `gateway=<ip>` (next hop), `gateway=Null0`
and `interface=0` (black hole), or `interface=<routing_interface id>` and
no gateway (out of that network's VLAN). The gateway's own routing table
shows none of them; `rest/routing` holds them with `gateway_device` = the
switch.

**On SONiC** (`l3.go`):

- **Interfaces:** `config vlan add` when the VLAN is missing (no port need
  carry a routed network), `config interface ip add Vlan<id> <ip>/<len>`.
  An interface carrying an address the driver did not add -- the switch's
  own management address on `Vlan<native_vlan>` -- is refused with a note.
- **Routes:** STATIC_ROUTE rows written whole through `sonic-db-cli`
  (`nexthop`, `ifname`, `distance`, `blackhole`, `nexthop-vrf` as aligned
  comma lists: bgpcfgd drops a row whose lists differ), which bgpcfgd
  hands to FRR in SONiC's separated routing mode (`ip route <p> <nh>`,
  `... Vlan3999`, `... blackhole`). Several routes to one prefix are one
  row's hops.
- **The default route** shares `STATIC_ROUTE|default|0.0.0.0/0` with the
  management gateway the IP Settings own (§6.4, `address.go`): the transit
  hop goes beside it and the management hop drops to distance 200 (a
  floating fallback, FRR's `distance` per hop -- verified live with a test
  prefix: both hops listed, 200 and 1, the lower selected), and comes back
  to its own distance when the transit goes. `defaultRoute()` skips the
  driver's hop (the managed record's), so the IP Settings apply still
  finds its gateway and changes nothing. **The transit hop is written only
  while the transit VLAN reaches a port with link**: SONiC's `Vlan<id>`
  netdevs are up with no member at all, so FRR would take the hop and
  black-hole the switch's own traffic off the LAN; until the uplink carries
  VLAN 4040 the switch keeps its management gateway and logs why.
- **DHCP relay:** the VLAN's `dhcp_servers` list through `config vlan
  dhcp_relay add|del <vid> <ip>...` (each call restarts the dhcp_relay
  service, so one add and one del per VLAN); dhcprelayd starts `dhcrelay
  -id Vlan<id> -iu <every other Vlan> <servers>`.
- **DHCP server:** community SONiC has none (`config dhcp_server` is not in
  the image); the host's busybox has `udhcpd`. The driver writes
  `/etc/sonic/disunified-dhcpd/Vlan<id>.conf` (range, `max_leases`, subnet,
  router, DNS, lease time; leases in `/var/lib/disunified-dhcpd/`) and runs
  it as a transient unit `disunified-dhcpd-Vlan<id>` (`Restart=always`),
  through one `sudo python3 … disunified-dhcpd '<json>'` call per apply;
  collect.py's `dhcpd` section is each config's sha256 and the unit's
  state, so a reboot (transient units vanish) or a changed config restarts
  it. CoPP already traps DHCP to the CPU (`queue4_group3`).
- **Isolate Network:** rules of `DUI_IP_ACL` (§11.7.6) below the
  controller's own (`ISO_k` from priority 9870 down): a FORWARD of the
  switch's own address in the isolated network (relay replies come to it),
  then a DROP each way between the isolated subnet and every other subnet
  the switch is connected to, the transit excepted (the gateway is the
  network's way out and its DNS server). Written only beside a translated
  IP rule set.
- **Ownership** is the managed record (`sonic-managed.json` `l3`: addresses,
  route prefixes, the transit hop, relay VLANs, DHCP servers): a network
  leaving the push takes only what the driver made for it. A VLAN the
  driver created stays (the driver never deletes a VLAN).

**Order matters on removal** (both found live): `config interface ip
remove` refuses the last address of an interface while FRR still lists a
static route through it ("A static ip route is still bound to the RIF"; it
greps `show ip route vrf all static`), and FRR drops a deleted row only
once bgpcfgd has passed it on. The plan runs route, relay and default-row
removals first, then waits (`sh -c '… vtysh … </dev/null | grep -qw
Vlan<id> …' disunified-rif-wait`, 30 s at most), then the address removals,
then every add. **The CLI's own vtysh call reads stdin**, which in a `bash
-s` batch is the rest of the batch: the first fixed apply removed one
address and silently swallowed the rest and the save. The removal now
gets `</dev/null`, and **every batch ends in an `echo` marker that
`runBatch` checks** -- output without it is an error, so a command that
eats a batch can no longer pass for success (that check is what caught the
CLI).

**Live, 2026-11-07** (`replay_sonic_l3_test.go` replays all five pushes
over the switch as captured after the run, `collect-l3.txt`;
`l3_test.go` covers the transit default and the management VLAN refusal):
the routed network (VLAN 3999) and the transit (VLAN 4040) got their
addresses; the three routes reached CONFIG_DB, FRR, the kernel and APPL_DB,
the interface and black-hole routes the ASIC (the next-hop route waits in
APPL_DB for a neighbour that does not exist); the transit default was held
(VLAN 4040 on no port) with the management default untouched; DHCP Server
mode: udhcpd active, bound to `Vlan3999:67`, and **a client in a network
namespace on VLAN 3999 of the kernel bridge (no front-panel port) leased
an address from it**; DHCP Relay with Isolate Network: `dhcp_servers` set,
dhcrelay started for Vlan3999, a DISCOVER from the namespace relayed from
the switch's address to the server, the three `ISO_*` rules Active; the
UI's removal of the network and both routes, in one push, took everything
out (routes, wait, addresses, save, the DHCP server), `config_db.json`
clean. The test VLANs 3999 and 4040 were then deleted by hand. Ethernet0
stayed up throughout.

**SONiC's monit `routeCheck` flags a route whose next hop does not
resolve**: while the test route via a host that did not exist (198.18.3.10 in the fixtures) stood,
`route_check.py` reported it every 5 minutes as `missed_ROUTE_TABLE_routes`
(in CONFIG_DB and FRR, not in APPL_DB/ASIC until a neighbour answers). That
is SONiC's view of any UniFi switch route to an absent next hop, not a
driver fault; it cleared with the route (`route_check.py` exit 0 after the
run).

**Not proven live:** the transit carrying the default route (needs VLAN
4040 on the uplink, Ethernet0 -- the operator's change on both ends; the
row it writes is fixture-tested and its distance handling proven with the
test prefix); isolation dropping real traffic and a routed client reaching
the gateway (needs a second linked port, as §11.12's checks); a relayed
reply (the gateway has no scope for a network the switch routes).

### 11.14 BGP and OSPF: what the controller offers a switch (2026-11-07)

**Both are offered for a `USWF07D`.** A switch's BGP follows
`SWITCH_CAP_BGP` (`feature_caps` 33554432) and its OSPF bit 4 of a second
field, **`feature_caps2`** (bit names from upstream unifi-emu's
`capability_bits.json`: `LINK_DEBOUNCE` 1, `ROUTES_OSPFV2` 4, `PORT_ROUTING`
16 -- routed ports, §11.16 --; the ECS's own inform claims 128 there and
no OSPF; the names `POE_STAGING` 32, `TRUSTED_PORT` 64 and
`MAC_TABLE_PRESSURE` 128 are not captured). Settings -> Routing -> Create New Route
has an OSPF and a BGP tab whose Device list is "the site gateway or a
BGP-capable (OSPF-capable) switch"; with both bits claimed the DX010
appeared in both lists (live, 2026-11-07). The switch's push, as captured
(`controller-10.6.109/system_cfg-dx010-bgp-*.txt`, `-ospf-*.txt`): BGP is
`switch.bgp.1.enabled` and
`switch.bgp.1.raw_config_base64`; OSPF is `switch.ospf.id`,
`.announce_default_route`, `.redistribute_connected_route`,
`.redistribute_static_route`, `switch.ospf.area.N.number/type` and, per
routed network, `switch.routing_interface.N.ospf.*` (area, cost, hello and
dead intervals, priority, passive, authentication none / plaintext /
md5 with `md5_keys.K.id/password`). Neighbour tables in the UI come from
the gateway's own udapi (`/routes/bgp`, `/routes/ospf`); nothing a switch
reports feeds them.

**BGP is an uploaded FRR file.** The UI's BGP form is a name, a device and
a configuration file (`frr_bgpd_config`); the controller pushes the file
whole, base64-encoded, beside `enabled`/`disabled` (a disabled router keeps
its file), and a deleted router leaves the push. Captured on the DX010
(`controller-10.6.109/system_cfg-dx010-bgp-{1-enabled,2-disabled,
3-changed,4-removed}.txt`; the test file uses documentation addresses and
private ASNs, so its content needed no scrubbing). Ubiquiti's SONiC (the
ECS's `ubntbox`) writes the decoded file into a CONFIG_DB table of its own
(`BGP_FULL_CONFIG|global config_b64`) for its FRR manager; community SONiC
has no such table and runs FRR 10.4.1 in separated mode (`bgpd`, `zebra`,
`staticd`, `mgmtd`; bgpd's configuration rendered from templates at
container start).

**On SONiC** (`bgp.go`, claimed with L3 routing wherever FRR runs):

- The driver loads the file into the bgp container: `docker exec -i bgp sh
  -c 'umask 077 && f=$(mktemp …) && cat > "$f" && vtysh -f "$f"; rc=$?; rm
  -f "$f"; exit $rc'`, the file on stdin. **Never `vtysh -f /dev/stdin`**:
  FRR 10's vtysh forks a child per daemon and each reads the file itself,
  so only the first child got it -- live, the second load exited 0 and
  left bgpd untouched (mgmtd had read the file).
- It owns the top-level objects the file opens -- `router bgp <asn> [vrf
  <name>]`, route-maps, prefix-lists and access-lists, community and as-path
  lists -- recorded in `sonic-managed.json` (`bgp`: the sha256 of what it
  loaded and the objects). An object the switch already runs and the
  driver does not own (SONiC's own `LOCAL_VLAN_IPV4_PREFIX`) is left out of
  the file with a note; so is any stanza that opens none of bgpd's objects
  (an interface, a static route, another daemon, FRR's own `hostname` or
  `password`) -- those belong to the controller's other settings. A note
  names the line's first word only (a second word after `interface`,
  `router`, `ip`, ...), so a password never reaches the log.
- **A changed file replaces the old one whole**: `no` for every owned object
  bgpd still runs (routers first), then the new file, in one load. Its
  sessions restart. The router off or gone removes the owned objects.
- `collect.py`'s `frr_bgp` names bgpd's top-level objects from `show
  running-config bgpd` -- names only, never a body (a neighbour's password
  is in there). **A bgp container restart** (config reload, reboot) loses
  the file; the next reconcile sees an owned object missing and loads the
  same file again, with no removal.
- vtysh applies what it can and exits 2 on a bad line, echoing it; the
  driver logs the line numbers only, keeps the file in the record as
  `failed`, and does not load it again until it changes.

**Live, 2026-11-07** (the test router made through the controller's own
endpoint with the UI's body; `replay_sonic_bgp_test.go` replays the four
pushes over the switch as captured before the run, `collect-bgp.txt`): the
file loaded (`router bgp 65010`, the prefix-list and route-map read back
through `vtysh`); a peer -- FRR's bgpd from the switch's own `docker-fpm-frr`
image in a throwaway `--network none` container (`NET_ADMIN`, `SYS_ADMIN`)
linked to the host by a veth, 198.51.100.0/30, no front-panel port --
reached **Established**; the DX010 accepted the peer's 192.0.2.0/24 and
dropped its 198.18.99.0/24 as the file's route-map says, the learnt route
reached the kernel and APPL_DB, and the peer learnt the file's
`network 203.0.113.0/24`. The reconciles after it wrote nothing. The
objects removed by hand were loaded again by the next reconcile (the
session back in 25 s); the changed file (timers 10 30, a new description)
replaced the old whole (the hold time 30 s read back); the router deleted
in the controller removed all three objects, the learnt route left the
kernel and APPL_DB, and the record emptied. The peer container and veth
were removed after; Ethernet0 stayed up throughout.

**Not proven live:** a real bgp container restart (simulated by removing
the objects; the restart is the same state to the driver), a session over
a front-panel port, a `vrf` router.

**OSPF** (`ospf.go`, claimed with L3 routing wherever FRR runs; built the
same evening with Clint's OK for a test network). The OSPF form needs at
least one area holding a network (the server answers `areas: must not be
empty`), and for a switch the area's networks are the ones it routes: with
nothing routed on the DX010 the form's Create New Area stays disabled. For a
switch the form drops "Redistribute BGP Routes" (gateway only).

*The push* (`controller-10.6.109/system_cfg-dx010-ospf-{1-defaults,2-md5,
3-plaintext,4-passive-stub,5-nssa,6-removed}.txt`; the test network
`dui-ospf-test`, VLAN 3999, routed on the DX010 with its Inter-VLAN routing
transit; scrubbed with `--subnet`, the keys as `<hash>`;
`sanitize-controller.py` now keeps 0.0.0.0/8, where the area ids live):
`switch.ospf.id`, `.announce_default_route` (true/false),
`.redistribute_static_route` and `.redistribute_connected_route` (`e1`,
`e2` or `none`), `switch.ospf.area.N.number/type` (`normal`, `stub`,
`nssa`); per routed network in an area `switch.routing_interface.N.ospf.
{area,passive,cost,priority,hello_interval,dead_interval,
authentication_type}` with `none`, `plaintext` + `ospf.password`, or `md5`
+ `ospf.md5_keys.K.{id,password}`. The router deleted leaves the push.

*On SONiC.* The bgp container ships FRR's `ospfd`, but in SONiC's
separated routing mode supervisord never starts it; frrcfgd's mode
(`DEVICE_METADATA frr_mgmt_framework_config true` -- what the ECS sets)
starts it from CONFIG_DB's `OSPFV2_*` tables, but would also move the
static routes and the management default route (§11.13, §6.4) onto
another config daemon. So the driver runs ospfd itself and refuses with a
note when frrcfgd's mode is on. It renders the router into one FRR file
(`interface Vlan<id>` with `ip ospf area/passive/cost/priority/
hello-interval/dead-interval` and the authentication, then `router ospf`
with the router id, `redistribute connected|static metric-type 1|2`,
`default-information originate`, `area <id> stub|nssa`), writes it as
`/etc/frr/disunified-ospfd.conf` in the container (`/etc/sonic/frr` on the
host: it outlives a container restart; frr's, mode 0600 -- it holds the
keys) and starts `ospfd -d -A 127.0.0.1 -P 0 -f <file>`. `collect.py`'s
`frr_ospf` says whether ospfd runs and the file's sha256 (never its
content). A changed router restarts ospfd (its adjacencies with it); a
bgp container restart leaves the file and no ospfd, and the next reconcile
starts it; the router gone stops ospfd and removes the file.

*Live, 2026-11-07* (`replay_sonic_ospf_test.go` replays the six pushes
over the switch as captured during the run, `collect-ospf.txt`): the
router started ospfd on Vlan3999 (cost 10, priority 2, hello 5 s, dead
20 s, area 0, read back with `show ip ospf interface`). A peer -- zebra,
mgmtd and ospfd from the switch's own FRR image in a throwaway
`--network none` container, its veth on the kernel `Bridge` in VLAN 3999
only, MTU 9100 to match Vlan3999 (OSPF compares MTUs), the same MD5 key --
reached **Full**. The DX010 learnt the peer's 192.0.2.0/24 as an E2 route
into the kernel, APPL_DB and the ASIC; the peer learnt the DX010's default
route (announced) and its connected networks (E2). The cycles after wrote
nothing. ospfd killed by hand was started again by the next reconcile
(Full again within a minute); a cost changed in the controller restarted
it with the new file (cost 20 read back); the router deleted stopped ospfd
and removed the file, and the learnt route left the kernel and APPL_DB.
Then the peer, the test network (its transit went with it) and VLANs 3999
and 4040 were removed (the VLANs by hand: the driver never deletes one);
route_check passes, Ethernet0 stayed up throughout.

*Not proven live:* an adjacency over a front-panel port, a real bgp
container restart (simulated by killing ospfd), a plain-text key longer
than OSPF's 8 characters (FRR's limit; the UI does not cap it), a stub or
NSSA area with a peer.

### 11.15 Fan mode: Default and Quiet as a per-device setting (2026-11-07)

UniFi's Fan Mode (Default / Quiet) is in the UI for a device whose record
holds `sys_fan_mode` with non-zero `caps`. **Network 10.6.109 stores that
block for no switch** (2026-11-07):

- seen live: no switch's record keeps it whatever it sends. The DX010
  reported `sys_fan_mode {caps: 3, mode}` in the gateway's shape and its
  stored record (`stat/device`) never held it; none of the site's 72
  devices' records does, the ECS Aggregation included; no Fan Mode panel
  appeared;
- not captured: that a gateway's (UDM, UXG) record keeps it; that the REST
  API cannot add it (the device's REST fields have `fan_mode_override:
  default|quiet`, and an unknown key is refused with `Invalid key exists in
  Device payload`);
- not captured, since no switch's record has the caps: the rest of the
  chain -- the panel's options enabled by the `caps` bits (DEFAULT 1 |
  QUIET 2), `sys_fan_mode.mode` as the current mode, a save as
  `fan_mode_override`, and a `system.fan.mode=<default|quiet>` push once the
  record has caps and an override. With no caps on the record there is no
  push to capture.

So the mode is the device's `fan_mode: default | quiet` in `config.yaml`
(generic: `devicemodel.FanModeController`, applied by the loop after every
collect as a diff against the read-back; unset = the fans are left alone).
The inform still reports `sys_fan_mode {caps: 3, mode}` in the gateway's
shape, because it is true and a later controller may store it. Driving
the mode from the UI once the controller keeps it for a switch is
[TechBlueprints/disunified#2](https://github.com/TechBlueprints/disunified/issues/2).

**The floors** (`fan.go`), on every one of the ten PWMs of the platform's
fancontrol table, `x86_64-cel_seastone-r0` with F2B airflow only:

| Mode | MINTEMP | MINSTOP / MINPWM | Duty at the floor | Source |
|---|---|---|---|---|
| Default | 26 | 89 | 35 % | the stock 2017 table (the lines patch 0012 removed) |
| Quiet | 35 | 51 | 20 % | patch 0012 (Clint's standing choice; ASIC ~56 °C on the bench) |

MINSTART 89, MAXTEMP 45, MAXPWM 255 and the hysteresis are the same in
both; thermalctld's policies are untouched. A B2F unit offers nothing
(nothing was measured on one) and logs why once; so does any other
platform, and the VS has no table at all.

**Read and write.** `collect.py`'s `fancontrol` section prints the airflow
the boot script recorded (`fan_airflow`) and the floor lines of
`fancontrol`, `fancontrol-<airflow>` and pmon's running `/etc/fancontrol`
(`docker exec pmon cat`); the mode is the one all three match, so a file
out of step (a hand-set 25 % in pmon, a boot file left at stock) reads as
none and the configured mode is written again. The write rewrites
`fancontrol-<airflow>` in place (the file `/etc/init.d/fancontrol.sh`
copies over `fancontrol` at every boot), copies it over `fancontrol` and
into pmon, and restarts only fancontrol there (`docker exec pmon
supervisorctl restart fancontrol`). Patch 0012 stays: an image built from
the kit boots at Quiet, and `fan_mode` moves it from there.

Fixture: `sonic-igmp-202511.20261106-f2e25f54c/collect-fan.txt` (the DX010
at Quiet); `fan_test.go`, and through the loop `TestReplaySonicFanMode`
(a bridge restart included).

**Verified live on the DX010, 2026-11-07 20:48-21:07 MST** (deb
`f40f47f`, image `igmp-202511.20261106-f2e25f54c`, room ~24 °C at the
intake):

- before: Quiet in all three files (patch 0012), fans 20 %, ASIC 58 °C. The
  inform carried `sys_fan_mode: {caps: 3, mode: "quiet"}`; the stored
  record kept no `sys_fan_mode`, as the code predicts -- no panel.
- `fan_mode: default`, bridge restarted: one write (`fan mode quiet ->
  default`, ~12 s with the fancontrol restart), all ten PWMs at 26/89/89 in
  `fancontrol`, `fancontrol-F2B` and pmon's copy, fancontrol RUNNING again
  in pmon; the fans settled at 41-42 % (PWM 102: the stock curve sits above
  its floor even with the intake at 24.5 °C, under MINTEMP 26, as it did
  before 0012), all OK; the ASIC fell to 53.75 °C; the next cycles wrote
  nothing and reported `mode: default`.
- `fan_mode: quiet`, bridge restarted: one write (`default -> quiet`),
  35/51/51 in all three, the fans at 20 %, all OK.
- a bridge restart with no change: no write, `mode: quiet` reported.
- the ASIC at Quiet, sampled each minute: 49.6, 50.9, 51.6, 52.1, 52.6,
  53.3, 53.4, 53.8, 54.1 °C -- climbing toward the bench's 56-58 °C,
  nothing near the 70 °C high threshold. The switch is left on Quiet.

### 11.16 Routed ports and aggregates, DHCP fixed IPs, and VRRP (2026-11-08)

The three Layer 3 items left after §11.14, captured on the DX010 (Clint's
OK for the test networks, 2026-11-08); bit names from upstream unifi-emu's
`capability_bits.json`.

**What the controller offers.**

- **Routed ports** are `SWITCH_CAP_PORT_ROUTING` (`feature_caps2` 16). The
  port editor's Operation gains Routing and Routing + Aggregating ("Routed
  ports do not pass VLANs and other Layer 2 traffic and rely on OSPF or
  static routes"). Saving one creates a network of its own
  (`rest/networkconf`: `gateway_type switch`, `gateway_device` the switch,
  `l3_interface_type port|lag`, `routed_port_idx` or `routed_lag_idx` -- the
  aggregate's first member --, the address as `ip_subnet`, no DHCP) and sets
  the port's override to `op_mode routed|routed_aggregate` with
  `routed_networkconf_id`. For an aggregate the UI keeps only the first
  member's override, listing every member. Leaving the mode deletes the
  network. The first such network makes the Inter-VLAN routing transit
  (§11.13), like any network routed on a switch.
- **DHCP fixed IPs:** a client's Fixed IP on a network a switch routes with
  DHCP Server mode ("Requires a UniFi Gateway or Layer 3 switch").
- **VRRP is not a single switch's.** `SWITCH_CAP_VRRP` (`feature_caps`
  268435456) appears in the UI only as an MC-LAG group's "L3 Automatic
  Failover (VRRP)" between its Top and Bottom switches. Not captured: the
  push, expected as `routing_interface.N.vrrp.{enabled, vrid,vip,priority,
  adv_int}` from that group's context (whether VRRP is on for the group,
  and which peer is master), and that with it on a routed VLAN's DHCP
  Server mode is refused because the range is split between the two
  switches. So
  VRRP comes with MC-LAG, which needs a second switch; the DX010 claims
  neither.

**The push** (`controller-10.6.109/system_cfg-dx010-routed-{1-port,
2-removed,3-aggregate}.txt` and `system_cfg-dx010-fixedip-{1-network,
2-one,3-two}.txt`; scrubbed with `--subnet`, the test subnets in
198.18.3.0/24 and 198.18.4.0/24, the transit in 198.18.253.0/24):

- A routed port is a routing interface with `type=port` and `port=<idx>`,
  plus `ip`/`mask`, `opmode manual`, `default_route` and `purpose`. **The
  port's own keys do not change**: it is still `opmode=switch` and an
  untagged member of its native VLAN.
- A routed aggregate is `type=lag` and `lag=<first member>`. That member is
  `opmode=aggregate`, `lag=1`, with no VLAN keys. **The other members are in
  no key at all**: the controller's port table shows them `aggregated_by` the
  first, but the push leaves them out, with or without an override of their
  own (the cfgversion was the same both ways). Any device, a UniFi one too,
  gets a one-member aggregate from that push.
- A fixed IP is `dhcp_server.static_ip.K.{id,mac,ip}` under the network's
  routing interface, inside the DHCP range or outside it.

**On SONiC.**

- The loop marks a routed port, and each member of a routed aggregate the
  push lists, `PortDesired.Routed`: a port in no VLAN. The port apply
  removes every membership (the static L2MC rows first, as for any removal),
  so the port leaves the kernel `Bridge` and mstpd drops it. The L3 apply
  then adds `config interface ip add Ethernet<n>|PortChannel<n> <addr>`.
  The device-wide apply runs before the ports, so the port apply re-runs the
  L3 apply after its batch, and a routed port is done in one cycle.
- **202511's CLI** (`config/main.py`, `config/vlan.py`): `ip add` refuses a
  VLAN member and a PortChannel member. On a port whose `PORT` `mode` is
  trunk or access it prints a line and adds nothing, with exit 0. `vlan
  member add` refuses a router interface ("is a router interface!"), and
  removing an interface's last address deletes its `INTERFACE` row. So the
  address waits, with a note, while the port still has VLANs. A port
  returning to switching gets no VLAN while it still carries an address,
  again with a note. In practice the device-wide apply removes the address
  first in the same cycle. The DX010's ports have no `mode`.
- A routed port on a split cage is refused with a note (no one interface).
  DHCP relay on a routed port is refused, because SONiC's relay CLI
  configures VLANs only. A DHCP server on one runs like any other, and a
  route out of one uses its interface name.
- A fixed IP is udhcpd's `static_lease <mac> <ip>`. busybox never leases a
  static address to another client, inside the range too. One outside the
  network, or on the switch's own, the network or the broadcast address,
  is left out with a note.

**Live, 2026-11-08** (deb b092e26; `replay_sonic_routed_test.go` replays
the six pushes over `collect-l3.txt`):

- **Fixed IPs:** the next cycle restarted udhcpd with both static leases. A
  client in a throwaway namespace on the kernel `Bridge`, in VLAN 3999 only
  (no front-panel port), leased the fixed address for each test MAC, one
  outside the range and one inside it. A third MAC got a dynamic address,
  never the reserved one. Fixed IP off removed the leases.
- **Routed port** (cage 31, Ethernet120, empty): in one cycle the port left
  VLAN 4094 and got its address. It showed in CONFIG_DB `INTERFACE`, APPL_DB
  `INTF_TABLE`, the kernel (out of the `Bridge`), and ASIC_DB as a
  `SAI_ROUTER_INTERFACE_TYPE_PORT` router interface at MTU 1500 with the
  /32 to the CPU. mstpd no longer listed it, and the cycles after wrote
  nothing. Removing it took the address out first. The port then rejoined
  VLAN 4094 untagged and the `Bridge`, with its seven reserved-group rows.
- **Routed aggregate** (cage 30 as pushed): PortChannel1 at fast rate,
  min-links 1 and MTU 1500, with Ethernet116, no VLAN, the address in
  `PORTCHANNEL_INTERFACE`, `INTF_TABLE` and the kernel, and a router
  interface on the LAG. Removing it took the address out, then the member,
  then the PortChannel, and the port went back to VLAN 4094.
- **Cleanup:** the test networks, their transit and the two test clients
  were removed in the controller. VLANs 3999 and 4040 were deleted by hand.
  The DX010's port overrides equal their pre-test copy, and
  `config_db.json` holds no trace. Ethernet0 stayed up throughout.

**Not proven live:** traffic over a routed port, since the test cages have
no link (rack day). The subnet's connected route, which SONiC installs when
the port comes up. An OSPF adjacency or a static route over a routed port.
A routed aggregate of more than one member: the controller pushes only the
first.

### 11.17 DHCP packet rate (anomaly 32): counted by disunified-l2d (2026-11-14)

**What UniFi expects.** Bit 32 of a port's `anomalies` raises the alert
`PORT_DHCP_STORM`, "Incoming DHCP packets exceed 300/sec on <device> Port
N." (the controller's system log, seen live 2026-11-13 when the bridge sent
the bit by mistake; root `CLAUDE.md` §5). The 300 is the alert's own text;
it is not a value the inform or the push carries. No captured push, the ECS's
or the DX010's (`docs/fixtures/controller-*`), has a DHCP rate key: the
switch's DHCP keys are `switch.dhcp_snoop.status` and `switch.dhcp_guard.N.*`,
which are Rogue DHCP Server Detection (§11.7); the ECS's own push, read on
the switch on 2026-11-14, has none either. So the limit is the switch's
own, not a setting. **The bit is a detection, not a port shutdown**: in the
UI's own wording only loop protection and BPDU guard take a port down, and
the DHCP alert's recommended action is to look for misconfigured devices and
rogue servers (§11.20, the model for every bit). For a driver that is a count
that grows while the condition lasts, `Port.Health.DHCPRateDrops`
(cumulative; `internal/device` sets the bit when it grows between informs),
not a `Port.Fault`, which names a fault keeping the port down.

**What the 202511 image has** (fixture
`sonic-igmp-202511.20261106-f2e25f54c/dhcp-rate-20261114.txt`, read-only):

- A field and a CLI, no enforcement. `PORT|<port> dhcp_rate_limit`
  (`sonic-port.yang`: "DHCP DOS Mitigation Rate with default value 300"),
  `config interface dhcp-mitigation-rate add|del` and `show interfaces
  dhcp-mitigation-rate` come from sonic-utilities#3301 (merged 2025-02-26; in 202505 and 202511 alike).
  The half that would apply it, sonic-swss#3130 (portmgrd: a `tc` ingress
  qdisc per port and a u32 filter on UDP destination port 67, `police rate
  <limit x 406>bps ... conform-exceed drop`), has been open since 2024-05-02
  and is in neither 202505 nor 202511: the kit's sonic-swss has no `dhcp` in
  `cfgmgr/`. On the DX010 every front-panel netdev has only its root
  `fq_codel` qdisc and no ingress filter (32 of 32, read from the swss
  container). There is no per-port DHCP drop counter anywhere to read.
- The field sits on 31 of the 32 ports, at 300, in CONFIG_DB and APPL_DB.
  It was on all 32 in the DX010's first capture
  (`sonic-202505.1238526-ff3c0c9f6/collect.txt`, 2026-10-31); Ethernet4 has
  had none since the breakout-and-join of 2026-11-01 (`collect-20261101.txt`),
  because a breakout re-creates the cage's ports and nothing writes the field
  but the `dhcp-mitigation-rate` command. The 300s fit db_migrator's
  `version_3_0_0` step, which adds 300 to every port of a config with no
  `VERSIONS` row, as a freshly generated L2 preset is. **§11.8's "every PORT
  write adds `dhcp_rate_limit: 300`" was wrong**: the field was there before
  those writes.
- What does limit DHCP is CoPP, switch-wide. With the `dhcp_relay` feature
  enabled, the `dhcp,dhcpv6` traps are in `queue4_group3` together with LLDP
  and UDLD: `trap_action trap` (the ASIC hands the frame to the CPU instead
  of forwarding it), policed `sr_tcm` at a CIR and CBS of 100 packets, red
  dropped (`APPL_DB COPP_TABLE:queue4_group3`, from the image's
  `copp_cfg.json`). One policer serves the whole switch, so its drops cannot
  be attributed to a port. Not tested: a flood above 100 packets/s on one port
  should starve DHCP and LLDP on every port. **Measured 2026-11-14 (below):
the CPU sees exactly 100 DHCP frames a second from a 400/s sender.**

**What the ECS shows** (Ubiquiti SONiC 4.0.1, read-only, 2026-11-14 about
09:00 MST; the facts are stated so a reader with an ECS can check them):

- DHCP reaches its CPU through the SAI DHCP trap, action trap, in a CoPP
  group of its own: APPL_DB `COPP_TABLE:queue3_group1` `trap_ids dhcp`,
  queue 3, `sr_tcm` at a CIR and CBS of 256 packets, red dropped (ASIC_DB:
  the DHCP `HOSTIF_TRAP` in a trap group on queue 3 whose policer is 256/256).
  LLDP and UDLD are in another group. One policer serves the whole switch.
- Nothing counts DHCP per port: ASIC_DB holds one ACL entry (a MAC rule)
  and six policers, every one a CoPP group's; no process holds a packet
  socket open for DHCP (`ss -0 -p`: l2mcd, lldpd, mstpd, teamd, syncd, lbpd
  and macsnoopingd), and there is no `tc` filter on the ports' ingress.
- `dhcp-rate-limit` is a cause in its error-disable table (CONFIG_DB and
  APPL_DB `ERROR_DISABLE|dhcp-rate-limit`, `admin_status enabled`,
  `auto_recovery_interval 0`; `show errdisable recovery`), as every other
  anomaly cause is, and `show errdisable status` is empty. A tripped port
  would be STATE_DB `ERROR_DISABLE|<cause>|<port>`; none exists.
- Its inform (`mca-dump`) has no `anomalies` key on any of its 54 ports.

So on 4.0.1 the ECS registers the cause and polices DHCP to its CPU, and no
detector can be seen; its CoPP rate (256) is itself below the 300 the alert
names, so a count taken at its CPU could never pass it. What a UniFi switch
counts to raise the alert stays not captured.

**The DX010, as built.** disunified-l2d counts DHCP the only place this
switch can see it per port, at the CPU (`l2d.py`, class `DhcpRate`):

- **What is counted.** An `AF_PACKET` socket with a classic BPF filter
  (IPv4 UDP to port 67, untagged or 802.1Q, no fragments; the hostif hands
  trapped frames over tagged, VLAN 4094 here) receives what the ASIC traps;
  `recvfrom`'s interface is the ingress port, outgoing frames and anything
  that is not a front-panel `Ethernet` netdev are skipped. A frame counts
  if it is a client's own request: BOOTREQUEST (op 1) with `giaddr` 0. So
  a DHCP server's port is never counted for its replies (BOOTREPLY, and to
  port 68 besides), nor a relay's for the requests it forwards (they carry
  its `giaddr`). DHCPv6 is not counted: the alert says DHCP, and the ECS
  traps only DHCPv4 in its group.
- **The threshold.** 300 requests in one wall-clock second on one port,
  the alert's figure; a second over it adds the excess to the port's count.
  STATE_DB `DUI_DHCP_RATE_TABLE|<port>` holds `over` (cumulative), `peak`
  and `last` (the last second over); l2d reads them back when it restarts,
  a reboot starts from 0 (a decrease sets nothing). `collect.py`'s
  `l2_dhcp_rate` reads the table, and the driver sums a cage's lanes into
  `DHCPRateDrops`. The name is the model's; on the DX010 these frames are
  not dropped by a per-port limiter (the image has none), they are the
  requests over UniFi's rate.
- **CoPP, the one write.** With the image's group the count can never pass
  100 a second for the whole switch. While the detector runs, l2d moves the
  image's `dhcp_relay` trap into a group of its own, as the ECS keeps DHCP:
  CONFIG_DB `COPP_GROUP|queue3_group1` (queue 3, `sr_tcm`, 600/600 packets,
  red drop, trap priority 3) first -- the trap's `trap_group` is a leafref
  sonic-yang checks on every breakout (§11.8) -- then `COPP_TRAP|dhcp_relay`
  with the image's `trap_ids` and that group. **The rate departs from the
  ECS's 256 for a code reason**: below 300 the alert's threshold could never
  be seen. 600 fits one port at 300 with room for the rest; LLDP and UDLD
  keep the image's group at 100, so a DHCP flood no longer starves them.
  Stopping (the config's `dhcp_rate.enabled` false, `--teardown`) deletes
  the trap row, coppmgr restores the image's from `copp_cfg.json`, and the
  group goes two seconds later. A trap row left behind by an l2d that died
  is removed the same way on the next start with the detector off. With
  `FEATURE|dhcp_relay` disabled the trap is not installed at all (DHCP is
  forwarded in the ASIC, never seen): l2d logs that once and counts nothing.
  coppmgr moves the trap but does not rewrite the image's group row, whose
  APPL_DB `trap_ids` keeps naming `dhcp,dhcpv6` while the ASIC's DHCP trap
  is in the new group (read live); harmless, and gone once the trap is back.
- **Nothing is acted on.** No port is shut, so no exemption is needed for
  the uplink, the STP root port or the gateway's port; the uplink is counted
  like any other port (its alert is a real one). The operator's recovery is
  UniFi's: find the device; the count stops growing and the anomaly ends.
- **Where it runs.** Wherever disunified-l2d runs (STP or loop protection
  in the push, `l2d` not off): the driver writes `dhcp_rate {enabled true,
  threshold 300, rate 600}` into `l2.json` with the rest; it is not a
  controller setting. A switch with `FEATURE|stp` (the PVST path) has no
  l2d and no count.

**Live check on the DX010 (2026-11-14 16:16-16:17 UTC, image
igmp-202511.20261106-f2e25f54c, under the shared test lock).** A harness
loaded only `DhcpRate` from the branch's `l2d.py` (the installed l2d,
mstpd and the bridge untouched) and counted while a LAN host sent
BOOTREQUESTs to the switch's own address on the uplink:

| Phase | Sent | The CPU saw on Ethernet0 |
|---|---|---|
| the image's CoPP (`queue4_group3`, 100/s) | 400/s for 5 s | 136 (the burst), then 101, 100, 100, 100 a second: the switch-wide policer, nothing past it |
| DHCP in `queue3_group1` (600/s) | 400/s for 5 s | 402, 390, 403, 402 a second; `over` 397, peak 403; one log line |
| the same | 150/s for 3 s | 150, 150 a second; nothing counted |
| teardown | -- | the trap back in the image's group (ASIC_DB: DHCP and LLDP in the same trap group, queue 4, policer 100), `queue3_group1` gone from APPL_DB and STATE_DB, CONFIG_DB as before |

The baseline before it: one DHCP frame in 30 s arrives on the uplink. The
capture taken with the count in place is
`sonic-igmp-202511.20261106-f2e25f54c/collect-dhcprate.txt`
(`TestDHCPRateCountFromTheLiveCapture`); two of the uplink's real requests,
scrubbed, are `dhcp-frames-20261114.txt`, on which
`testdata/l2d_dhcp_rate.py` runs the filter through a classic-BPF
interpreter and the classifier, the accounting and the CoPP writes
(`TestL2dDHCPRate`). Afterwards the switch was as before: no
`DUI_DHCP_RATE_TABLE` key, the installed `disunified-l2d` unchanged (same
sha256 and pid), `config_db.json`'s CoPP rows `stp` / `queue4_group1` only.

**Not exercised:** a flood arriving on any port but the uplink (no other
cage is cabled; a loopback cage would loop the VLAN), the alert raised in
the controller (the deployed bridge does not read the table yet: it needs
this branch deployed), and l2d running the detector as a service (the
harness ran the class alone; the service path is `sync` -> `configure`
every 2 s and the main loop's `select`, fixture-tested only).

### 11.18 Switch-detected anomalies the DX010 cannot report yet: PHY failure, MAC locking, 802.1X, DoS (2026-11-14)

A port's `anomalies` field is what the switch itself detected, and every
bit it carries raises an alert (`docs/feature-map.md` §2). The storm bits
(1, 2, 4), the DHCP rate (32) and rogue DHCP (64) are other sessions' work;
loop protection (512) is §11.5; the DX010 has no PoE (4096). This section
is the other four: for each, what the controller means by it, what the
DX010 offers that would mean the same, and whether a bit can be set that
is true. The rule is the one in `internal/device/caps.go`: a bit only for
the condition itself, detected by the switch on that port; a signal with a
similar name is not enough.

**What the controller means.** The four are the controller's own labels
for bits of the field (128 802.1X authentication failed, 256 MAC locking,
1024 DoS, 2048 PHY failure; root `CLAUDE.md` §5), and its UI words them:
2048 is "Port Failure Detected", the port being unable to link up because
of a physical failure; 256 is "Unauthorized MAC Detected", a MAC seen on
the port that is not on its allowed list; 128 is a failed 802.1X
authentication attempt on the port; 1024 is a denial of service attack
detected on the port. None of the four has been seen raised, so none of
these meanings is exercised.

**What the ECS does.** Nothing: the ECS Aggregation's inform, printed on
the switch (`controller-10.6.109/inform-ecs-aggregation-uswf066.json`), has
no `anomalies` key on any of its 54 ports, so the family the DX010 is
claimed as reports none of these bits. (The ECS's `admin` login refused
the site key on 2026-11-14, so it was not read again.)

**The evidence**, read on the DX010 on 2026-11-14 (image
`igmp-202511.20261106-f2e25f54c`, read-only):
`docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/anomaly-probes.txt`.

| Bit | What the DX010 can do | Built |
|---|---|---|
| 2048 PHY failure | no PHY between the ASIC and the cages; the per-port fault it does report is a link-down reason seen on the healthy uplink (§11.18.1) | nothing |
| 256 MAC locking | enforces the MAC filter (§11.12) but counts no dropped source per port; counting needs the ingress-port match in the kit's `L2` ACL type, an image change (§11.18.2) | nothing (enforcement as before) |
| 128 802.1X authentication failed | no authenticator on the image; 202511 holds Broadcom's Port Access Control sources, unbuilt and without an orchagent consumer (§11.18.3) | nothing |
| 1024 DoS | nothing the controller configures or describes beyond the name; the switch's CoPP drops are per CPU queue, not per port, and its malformed-frame drop reasons are not an attack (§11.18.4) | nothing |

#### 11.18.1 PHY failure (2048)

**The DX010 has no PHY to fail.** Its cages are wired to the Tomahawk's
SerDes: the SKU's directory holds `hwsku.json`, `port_config.ini` and
`sai.profile` and no `gearbox_config.json`, the file SONiC describes an
external PHY with (capture, `device_dir`). A UniFi switch's PHY is a chip
of its own that can stop answering; here the nearest parts are the ASIC's
port and the module in the cage.

**The ASIC's per-port fault is a link-down reason, not a broken port.**
With every oper-down notification the Broadcom SAI passes the port's error
status, and orchagent keeps it in STATE_DB `PORT_OPERR_TABLE|<port>`
(`oper_error_status` as a bitmap, and `mac_local_fault_count`,
`mac_remote_fault_count` and the other reasons' counts with their times;
202511's `PortsOrch::updatePortErrorStatus`). On the DX010 the only port
with a record is Ethernet0, the live uplink to the core:
`mac_local_fault_count 2`, `mac_remote_fault_count 2` since swss last
started, and its log shows every fault followed by `oper state set from
down to up` within 0.5 to 95 s (capture, `port_operr`,
`orchagent_port_errors`). A local fault is the receiver seeing no valid
signal, which is what a far end rebooting or a cable being pulled gives; a
remote fault is the far end saying the same. Mapping either to 2048 would
raise "Port Failure Detected" for an uplink that is working.

**The module's faults are already sent, as their own fields.** xcvrd's
`TRANSCEIVER_STATUS_SW` carries an `error` field (`N/A` on every cage) and
`TRANSCEIVER_STATUS` the module's flags; the bridge sends a module's
faults as `sfp_rxfault`/`sfp_txfault`, from which the controller derives
its own SFP alerts (`custom_anomalies` 1/2/4, `docs/feature-map.md` §2).
A module fault is not this bit.

**Built: nothing.** What a PR needs: an inform from a real UniFi switch
reporting 2048 (a support-bundle `last.inform` or `mca-dump` on the
switch) and what had failed on it, so the condition is known; then a
signal on the DX010 for a port the ASIC cannot bring up whatever the far
end does. None was found in STATE_DB, APPL_DB or the SAI's port
notifications.

#### 11.18.2 MAC locking (256)

**The condition is the MAC Address Filter's.** "Unauthorized MAC Detected"
is a source MAC on the port that is not on the port's allowed list: UniFi's
port security, pushed as `switch.port.N.port-security=enabled` and
`port-security.mac.K` (§11.12).

**Enforcement is built** (§11.12, written and read back in ASIC_DB on
2026-11-07): `learn_mode drop` on the port or its PortChannel, and a static
FDB entry per allowed MAC in each of the port's VLANs. A frame from any
other source is not learnt and not forwarded. It is dropped without a
trace: nothing on the running image counts it per port. What was checked:

1. **The drop counters.** The SAI offers one `PORT_INGRESS_DROPS` debug
   counter in all, and none of its reasons is a learning drop
   (`MPLS_MISS`, `FDB_AND_BLACKHOLE_DISCARDS`, `IP_HEADER_ERROR`,
   `L3_EGRESS_LINK_DOWN`, `EXCEEDS_L3_MTU`, `DIP_LINK_LOCAL`,
   `SIP_LINK_LOCAL`, `ACL_ANY`, `SMAC_EQUALS_DMAC`; capture,
   `dropcounters_capabilities`). `FDB_AND_BLACKHOLE_DISCARDS` and
   `ACL_ANY` each count other causes as well. The port's own
   `IF_IN_DISCARDS` counts every ingress drop.
2. **An ACL that counts.** Per secured port, one FORWARD rule per
   allowed source and a catch-all DROP whose counter would be the count of
   unlisted sources. SONiC matches SRC_MAC only in the image kit's `L2`
   table type (sonic-swss 0003; `sonic-acl.yang` accepts SRC_MAC on a rule
   only when its table's type is `L2`, so a custom `ACL_TABLE_TYPE` with
   it would fail the breakout's whole-config validation, as `learn_mode`
   would without the hold, §11.12). That type matches SRC_MAC, DST_MAC,
   the ether type and the outer VLAN, and not the ingress port. A rule
   counter is the sum over every port the table is bound to, so a catch-all
   in `DUI_MAC_ACL` cannot say which port saw the source. A table per
   secured port is no answer: the ASIC reports three more ingress PORT
   tables available beside the one in use (CRM, capture `crm_acl_group`),
   shared with `DUI_DHCP_GUARD`, `DUI_IP_ACL` and `DUI_MAC_ACL`. And ACL
   counter polling is off on the image (`counterpoll`: `ACL disable`), so
   no ACL counter reaches COUNTERS_DB.
3. **The ECS's route.** `learn_mode cpu_trap` (portsorch has it) sends an
   unknown source to the CPU instead, where software can count it. On the
   DX010 those frames enter the kernel's `Bridge`, whose FORWARD chain
   accepts everything but link-local, ARP and multicast (capture,
   `ebtables_forward`): unicast from an unlisted source would be forwarded
   in software unless the bridge adds an ebtables rule set per secured
   port, which is also where it would count. `learn_mode` is CONFIG_DB and
   is kept across a reboot; ebtables rules are not, so between boot and the
   bridge's first reconcile a secured port would pass what today it drops
   from boot. The frames would reach the CPU at whatever rate the host
   sends them, policed only by the default trap group, and that could not
   be measured: no second linked port. A weaker filter for a counter: not
   done.

**Built: nothing; enforcement only.** The bit is never set.

**What it takes to report it, as a build assessment** (all three, in this
order; each is ordinary work in this repository):

1. **The kit's `L2` table type gains the ingress-port match**: one
   `.withMatch(SAI_ACL_TABLE_ATTR_FIELD_IN_PORTS)` in 0003's builder and a
   case in its mock test. `sonic-acl.yang` already allows `IN_PORTS` on any
   rule. It is an image change: build, install, and the driver's `Start`
   probe for it (as it probes 0003 itself).
2. **The driver writes the filter into `DUI_MAC_ACL` as well**: per secured
   port (or its PortChannel), FORWARD rules for its allowed sources and one
   counted DROP with `IN_PORTS` set, below UniFi's own MAC ACL rules, beside
   `learn_mode drop`, which stays the enforcement from boot. That is the
   allowed MACs plus one rule per secured port against
   `max_custom_mac_acls`. On Broadcom an ACL drop does not cancel a
   to-CPU trap's copy (§11.7.8), which changes nothing here: the learning
   drop already applies.
3. **ACL counter polling on** (`FLEX_COUNTER_TABLE|ACL`), the DROP rule's
   packets read each poll into a neutral cumulative counter in
   `Port.Health`, and its growth set as 256 in `internal/device`, as the
   storm and DHCP counters are. The read is the same one the rogue-DHCP
   report needs for `DUI_DHCP_GUARD`'s drops; build it once.

Verification needs a source on a linked port that is not on its list: a
host on rack day, or a second DAC.

#### 11.18.3 802.1X authentication failed (128)

**The condition** is a failed 802.1X authentication attempt on the port:
the switch is the authenticator, relays a host's EAPOL to a RADIUS server,
and the server refuses it.

**The running image has no authenticator.** Nothing on the host is one
(no `hostapd`, `authmgr`, `pacmgr` or `mabmgr`), and the one container
that speaks EAPOL, `docker-macsec` (the `macsec` feature, disabled),
carries `wpa_supplicant`, `wpa_cli`, `wpa_action` and `wpa_passphrase`
only: MACsec's key agreement on the supplicant side (capture,
`authenticator_host`, `authenticator_macsec_container`). The EAPOL trap
exists (`copp_cfg.json`'s `macsec` group, trap `eapol`; capture
`copp_cfg_traps`). By the task's rule, the bit is not implemented.

**What 202511 holds, unbuilt.** `src/sonic-pac` is Broadcom's Port Access
Control, contributed upstream in 27 commits between 2024-10-26 and
2025-07-02: `authmgr` (the authentication manager), `pacmgr`,
`hostapdmgr` (which drives hostapd), `mab` and `mabmgr` (MAC
authentication bypass), `paccfg`, `pacoper`, and their OS layer
`fpinfra`. sonic-swss-common names its tables (CONFIG_DB
`PAC_PORT_CONFIG_TABLE`, `PAC_GLOBAL_CONFIG_TABLE`,
`HOSTAPD_GLOBAL_CONFIG_TABLE`; APPL_DB `PAC_PORT_TABLE`; STATE_DB
`PAC_GLOBAL_OPER_TABLE`, `PAC_PORT_OPER_TABLE`,
`PAC_AUTHENTICATED_CLIENT_OPER_TABLE`). Nothing else is there: no
`rules/*.mk` builds it and no container packages it; nothing in the
202511 sonic-swss consumes `PAC_PORT_TABLE`, so no code turns an
authorised or unauthorised port into ASIC state; no `config`/`show`
command in sonic-utilities 202511 names it. hostapd's source is in the
tree (`src/wpasupplicant` is the hostap project), and `rules/wpasupplicant.mk`
builds the supplicant from it only.

**What building it takes** (the real assessment; nothing here is
impossible on the DX010, and all of it is larger than any feature built
so far):

1. **The image**: build rules and a container for the eight PAC
   components, and hostapd built from the same hostap tree with the wired
   driver, as `hostapdmgr` expects; then make it run (the code has never
   been built in a community image, so expect the first build to be
   porting work against 202511's libraries).
2. **The data plane**, written for sonic-swss: an orchagent consumer of
   `PAC_PORT_TABLE` that holds an unauthorised port closed and admits each
   authenticated client. The mechanism exists and is verified on this ASIC:
   §11.12's MAC filter is exactly `learn_mode drop` plus a static FDB entry
   per admitted MAC. The EAPOL trap goes with it.
3. **The controller side**: claim 802.1X, capture the push (RADIUS
   profile, per-port control mode, the fallback VLAN), write it to the PAC
   tables, a RADIUS server the switch can reach.
4. **The signal**: failed attempts per port from `PAC_PORT_OPER_TABLE`
   or hostapd's EAP-failure events, as a cumulative counter in
   `Port.Health`, its growth set as 128.

Verification needs an 802.1X supplicant on a linked port and a RADIUS
server: rack day at the earliest.

#### 11.18.4 DoS (1024)

**The condition** is a denial of service attack detected on the port; the
UI's advice for it is to look for a misconfigured device sending excessive
traffic, or an attacker. Nothing more about it is observable: no captured
push (58 in `docs/fixtures/controller-*`, the ECS's among them) carries a
key that configures a DoS detector, the UI has no setting for one, and the
ECS reports no `anomalies` at all. What a UniFi switch counts as an attack
is not known.

**The DX010 has no per-port detector to match.** What it has is near the
name only:

- **CoPP** polices traffic to the CPU per trap group (`copp_cfg.json`:
  ARP, LACP, LLDP, DHCP, BGP, IGMP, `ip2me` and the rest; capture
  `copp_cfg_traps`). Its drops are per CPU queue, switch-wide: a policer
  drop names no port.
- **The drop counter reasons** include `SMAC_EQUALS_DMAC` and
  `IP_HEADER_ERROR`, malformed-frame checks the ASIC makes anyway (one
  counter in all, §11.18.2). A frame with its own address as both ends or
  a bad IP header is as likely a broken host as an attack; calling it a DoS
  is a reinterpretation.
- **Storm control** drops are bits 1, 2 and 4 (another session), and the
  DHCP rate limit is 32: each already has its own bit.

**Built: nothing.** What a PR needs: an inform from a real UniFi switch
reporting 1024 and what traffic set it, so the condition is defined by
something observable; then a per-port detector for that condition on the
DX010.
### 11.19 Storm-control drops: no count to report (2026-11-14)

UniFi's broadcast, multicast and unicast storm anomalies (bits 1/2/4) are
set by `internal/device` when a port's `StormDropsBroadcast` /
`StormDropsMulticast` / `StormDropsUnicast` grow between informs. The Arista
fills them from `show storm-control` (`drivers/arista-eos.md` §4). **The
SONiC driver fills none of them: on the DX010's image nothing counts the
frames storm control drops, per port and type.** What was looked at, on
`igmp-202511.20261106-f2e25f54c`, with storm control put on empty cage 31
(`Ethernet120`) by hand and removed again (`PORT_STORM_CONTROL` empty, the
port's three policer attributes back to `oid:0x0`):

- **The type mapping** (orchagent `policerorch.cpp`, and ASIC_DB live):
  `broadcast` → `SAI_PORT_ATTR_BROADCAST_STORM_CONTROL_POLICER_ID`,
  `unknown-multicast` → `SAI_PORT_ATTR_MULTICAST_STORM_CONTROL_POLICER_ID`,
  `unknown-unicast` → `SAI_PORT_ATTR_FLOOD_STORM_CONTROL_POLICER_ID`; each is
  its own policer named `_<port>_<type>` (`SAI_POLICER_MODE_STORM_CONTROL`,
  `METER_TYPE_BYTES`, red action drop, CIR in bytes/s: 100000 kbps →
  `12500000`). The driver writes UniFi's Multicast as `unknown-multicast` and
  Unicast as `unknown-unicast` (§5), so a per-policer count would map 1:1.
- **COUNTERS_DB / FLEX_COUNTER_DB:** no key for any of the three policer
  oids, and no `POLICER` flex counter group (the 24 groups present are port,
  queue, PG, buffer, RIF, ACL, tunnel, MACsec, trap, route, SRv6, switch and
  debug counters). orchagent creates storm policers and never registers them
  for statistics: in the sonic-swss source (the 202511 work clone and master
  of April 2026) only the P4 orchestrator calls `get_policer_stats`, for ACL
  meters. syncd's flex counter does know `POLICER_COUNTER_ID_LIST`, so the
  polling half exists; the orchagent half does not.
- **Drop counters (`show dropcounters capabilities`):** one free
  `PORT_INGRESS_DROPS` counter and a list of SAI drop reasons with no storm
  reason in it (SAI has none), so a `DEBUG_COUNTER` cannot be built for it.
- **The chip (BCM56960, Tomahawk; `bcmcmd` in syncd):** the SAI programs
  storm control as the ingress pipeline's `IFP_STORM_CONTROL_METERS` (four
  entries took the three rates, the multicast rate twice), whose fields are
  `REFRESHCOUNT`, `BUCKETSIZE`, `BUCKETCOUNT`, `METER_SPARE`, `EVEN_PARITY`:
  a token bucket, no counter. The legacy `rate` diag (packet-mode
  `bcm_rate_*`) shows every port `FALSE` with the policers bound, so that is
  not the mechanism. The SDK in `libsai.so` names one debug-counter trigger
  for it, `bcmDbgCntRxStormControlDrop`: one reason for all three types.
  The nine receive debug counters (`RDBGC0`-`8`) are the SAI's; `RDBGC8`'s
  select is 0 (unused).

A per-port total of storm drops could be had by pointing a free receive
debug counter at that trigger, but it cannot say which type dropped, and a
multicast storm reported as a broadcast one is worse than none. **What a
contributor needs to add one:** a per-type count the platform really keeps
-- either a SAI whose `get_policer_stats` answers for a storm-control policer
(with orchagent registering the three policers in a `POLICER` flex counter
group and the driver reading their `SAI_POLICER_STAT_RED_PACKETS` from
COUNTERS_DB in the one poll exec), or a chip whose storm meters count; then
a capture of the count growing under a storm (a cabled traffic source, or a
MAC-looped cage in a VLAN of its own), scrubbed with `scripts/sanitize-sonic.py`,
and the alert seen in the controller.

## 12. The bridge on the switch itself (2026-11-02)

Once the SONiC switch is the LAN's core, a bridge that reaches it over SSH
through the very links it configures is a dependency to remove: the bridge
runs **on** the switch as a systemd service from a `.deb`, executing the
driver's commands locally (`options.transport: local`, `sshrun.Local`) as
the login user, with its config, API key and state in `/host/disunified`.
With nothing to dial, an address move and the first STP enable are proven
by a TCP connection to `options.probe` (the controller's inform host:port
by default), and the deb rides SONiC's own first-boot install across an
image upgrade. The specification, the verification plan and the cutover
are [`sonic-on-switch.md`](sonic-on-switch.md); the install steps are
[`docs/install.md`](../install.md) §7. Over SSH nothing changed.

**The VLANs' MTU (found at the cutover, 2026-11-02).** With UniFi's jumbo
off the driver set every port to 1500, but SONiC's VLAN netdevs
(`Vlan<id>`) stayed at 9100: `vlanmgrd` creates the kernel `Bridge` at a
fixed 9100 and every VLAN device under it inherits that. The gateway's LAN
side runs jumbo and advertises MSS 9176, so the switch's own TCP socket to
the controller ran at `mss:9048 pmtu:9100` (`ss -tni`), and every segment
over 1500 bytes was dropped on the 1500-byte uplink: the on-switch
bridge's 60 KB inform never got out (`bytes_retrans` growing, `cwnd 1`),
while a one-byte POST was answered. A bridge reaching the switch over SSH
never saw it -- SSH sessions never sent such segments from the switch. The
VLAN table's own `mtu` field does **not** fix the kernel: in
`cfgmgr/vlanmgr.cpp` (202505 and the 202511 tree alike) `vlanmgrd` passes
it to APPL_DB `VLAN_TABLE`, where orchagent sets the router interface's
MTU, and leaves the netdev with a `TODO` (`setHostVlanMtu` exists and is
never called). The rule: whenever a push carries the ports' MTU, every VLAN
the driver manages (the site's and the native stand-in) gets it twice, in
the same batch before `config save` -- `sonic-db-cli CONFIG_DB hset
'VLAN|Vlan<id>' mtu <mtu>` (absent = 9100) and `ip link set dev Vlan<id>
mtu <mtu>` against the kernel's own value from `ip -j addr`, capped at the
Bridge's MTU (a VLAN device cannot exceed its parent: jumbo on writes 9216
to CONFIG_DB and 9100 to the netdev). A VLAN `EnsureVLANs` makes behind
ports at another MTU is made with theirs in the same batch; its netdev
follows on the next apply. The kernel half does not survive a swss restart
or a reboot (the netdevs are recreated at 9100). After a reboot the
bridge's start-up reconcile (before its first inform) restores it, and so
does the next cycle after a swss restart under a running bridge: since
2026-11-02 the loop reconciles every cycle whether or not the controller
answered the inform (`informloop.Loop.informOnce`; until then it reconciled
only after an answered one, so a bridge whose informs the netdev MTU had
broken could never repair it). What UniFi's own SONiC (the ECS) keeps in
`VLAN|…` `mtu` is not in any capture.

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
