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
  speed only. `Health.LinkChanges` = `flap_count` (portsorch adds one on
  every oper-status change, and a port starts down, so half of it, rounded
  down, is the drops). **A split cage adds its lanes' drops, not their
  changes** (2026-11-15, as the Arista's e3d5218): `LinkChanges` = twice
  the summed drops, plus one while the cage is up. Summing the lanes'
  `flap_count` counted every up lane as half a drop: on the DX010 (cage 32
  split 4x25G, lanes 1-3 in MAC loopback, lane 1 dropped and brought back
  once, lane 4 down; `flap_count` 3, 1, 1, 0) the cage read 5 changes, 2
  drops, for the one drop there was (`TestSplitCageLinkDrops`,
  `sonic-igmp-202511.20261115-6c8eb1cf7/collect-split-loop.txt`). No PCS/FEC counters on
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
| `EnsureVLANs(ids)` | `config vlan add <id>` for missing ids; a VLAN the push stops naming is deleted at the end of the port apply (§11.29) | `VLAN\|Vlan<id>` | **collision check:** if `ids` contains `native_vlan`, return an error naming it and write nothing; `ApplyPorts` likewise refuses any desired port that lists `native_vlan` as a tagged VLAN. This is how the §5.2a loop rule is met without UniFi knowledge in the driver. |
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

### 6.4.1 The kernel's default route (`kroute.go`, 2026-11-15)

**Found on the DX010, 11 h after a boot:** the kernel had no IPv4 default
route. `ip -4 route` showed only the connected /16 on `Vlan4094`, and
`ping` to any off-subnet host failed with "Network is unreachable". Every
layer above the kernel had it: CONFIG_DB `STATIC_ROUTE|default|0.0.0.0/0`,
APPL_DB `ROUTE_TABLE:0.0.0.0/0`, FRR's `show ip route 0.0.0.0/0` (static,
selected, installed, offloaded, nexthop group 32), and the kernel's own
`ip nexthop show` (id 32 via the gateway, `proto zebra`). The route's
nexthop lacked FRR's `fib` flag (flags 1, active only); a healthy one shows
flags 3. Nothing depended on the route at the time: the controller, DNS,
NTP and syslog were all on the LAN. With no kernel default the snapshot
also had no gateway, so the informs carried no `gateway_mac` for those
11 h.

**The cause, in three parts:**

1. **zebra installs routes through kernel nexthop objects on 202511.**
   `zebra.conf.j2` renders `zebra nexthop kernel enable` unless
   `DEVICE_METADATA|localhost zebra_nexthop` is `disabled`.
   sonic-buildimage #23459 had forced it off on 202505 ("netscan loss on
   production devices"); #23500 made it this metadata field; #25117
   (202511) made enabled the default.
2. **The kernel deletes those objects silently on carrier loss.** In
   kernel 6.12's `net/ipv4/nexthop.c`, `nh_netdev_event` runs
   `nexthop_flush_dev` on `NETDEV_CHANGE` without `IFF_RUNNING`/`LOWER_UP`.
   That calls `remove_nexthop(net, nh, NULL)`: with no `nl_info` there is
   no RTM_DELNEXTHOP, and the routes using the object go with no
   RTM_DELROUTE. A route with inline nexthops only gains `linkdown` and
   stays (`fib_sync_down_dev`). Both were shown live on a test bridge in
   table 100: across a 2 s carrier loss the nhid route and its object
   vanished, no `Deleted` line, while the inline route read `linkdown`
   and came back.
3. **zebra brings back the object, not the route.** On interface up,
   `if_up()` calls `zebra_interface_nhg_reinstall()` (FRR 10.4.1
   `zebra/zebra_nhg.c`), which re-creates the interface's singleton
   nexthop groups. A route comes back only if zebra re-resolved it while
   the interface was down: a new group id, sent again on up. Seen live:
   an idle zebra does this for every flap down to 17 ms. If the down and
   the up are handled before the RIB queue runs, the route resolves as
   before. It keeps its group id and `installed` flag, and is never sent
   again.

   Upstream FRR has this open: FRRouting/frr#20747 ("quick flap of
   interface"; adds a route back-pointer to each nexthop group to
   re-install its routes). The other open PRs cover the group, not the
   routes: #23747, #23022, and #22383, which a maintainer prefers. None is
   merged.

**The trigger on the DX010** was the driver's own disunified-l2d. At
21:35:46.68 (boot) it started spanning tree on the kernel `Bridge`, and
mstpd set every port blocking. The dummy port, an admin edge, was
forwarding again 88 ms later. For that moment no port forwarded, so
`br_port_state_selection` took the Bridge's carrier down, and `Vlan4094`,
stacked on it, followed. zebra logged the link down at 21:35:46.77, 6 s
after it had installed the default (~21:35:40-44). Group 32 and the route
both date from before the flap, so zebra never re-resolved them.

The same blink comes back whenever mstpd starts on a running bridge (an
l2d restart, a deb upgrade that restarts it). It also comes back on any
real loss of the Bridge's carrier.

**Ruled out:**

- **The early DNS step** (da68007, `resolv-config.sh` before the bridge's
  start). It touches resolv.conf only. It ran at 21:33 in the old boot
  and before zebra in the new one.
- **The driver's address and MTU writes.** The IP Settings apply at
  21:36:20 wrote nothing ("no interface carries the bridge's own target",
  Start's snapshot from before swss, §6.4.2). The `ip link set … mtu
  1500` at 21:36:40 is the second "Link up" zebra counted: zebra calls
  `if_up` on any RTM_NEWLINK
  of an operative interface. That came a minute after the route was gone.
- **An address bounce.** The connected route's uptime matches the flap,
  not a later event, and bgpcfgd saw the address once.

The direct link from the 88 ms flap to the lost route was not reproduced
on demand. An idle zebra handles every flap the test could make, so the
boot case needs zebra busy (fpm connecting, orchagent's first sync).
That is where the timeline puts it.

**Reproduced end state** (`collect-route-missing.txt`). `sudo ip nexthop
del id <n>` makes the same deletion. Its RTM_DELNEXTHOP made zebra
re-create the object within 10 ms. The route stayed missing, and FRR
reported it installed with `fib` set. It was still missing a minute later.

**The fix (`kroute.go`, run from `ApplyDevice` every cycle):**

- **The cause.** `sonic-db-cli CONFIG_DB hset 'DEVICE_METADATA|localhost'
  zebra_nexthop disabled` plus `config save -y` sets zebra.conf for the
  bgp container's next start. The same line, `no zebra nexthop kernel
  enable`, goes into the running zebra at once. It is a hidden command in
  FRR 10.4.1 and changes only how later installs are encoded. zebra then
  installs inline nexthops, so a carrier blink can no longer delete the
  route. The YANG model knows the field, and nothing but the template
  reads it.
- **The repair.** If FRR selects and installs 0.0.0.0/0 and the kernel's
  main table has no default through any of its hops on two collects in a
  row, the driver logs it loudly. It then takes out the running config's
  own `ip route 0.0.0.0/0 …` lines and puts them back. It does the same
  at once when the kernel's default still uses a nexthop object, which is
  how every 202511 boot leaves it.

  The repair is one `docker exec bgp vtysh -c 'configure terminal' -c 'no
  <line>' -c '<line>'`, with each `-c` committed on its own. **Not `vtysh
  -f`**: in FRR 10 a file is one mgmtd transaction, so a delete and an add
  of the same route cancel out (live: the route's uptime ran on and the
  kernel stayed empty). Between the two lines the route leaves the chip for
  a few milliseconds.

  Two collects, because zebra's own correct re-install leaves the kernel
  briefly without the route. A route FRR holds through another protocol
  (no static line) is only logged, once.
- `collect.py`'s `frr_route` carries FRR's view, the static lines and the
  zebra setting. It takes no other line from `show running-config`, which
  holds the daemons' passwords. It is "error" when the bgp container does
  not answer. Captures before 2026-11-15 lack it, which writes nothing.

**Verified live (by hand, the driver's exact commands, 2026-11-15):**

- the broken state reproduced;
- the zebra line taken at runtime;
- the `-c` repair putting the default back inline (`proto 196 metric 20`);
- off-subnet ping answering;
- with inline nexthops, a 1 s carrier loss on a test bridge leaving the
  route in the kernel (`linkdown`) until zebra's own delete and re-add.

`DEVICE_METADATA zebra_nexthop` was set for the capture and removed again,
unsaved, so the deployed driver's own write is what proves that half.

**Needs a reboot to prove:** that with the saved field the bgp container
renders `no zebra nexthop kernel enable`, and that the default survives
l2d's spanning-tree start at boot (`ip -4 route show default` with no
`nhid`, right after boot).

Fixtures: `sonic-igmp-202511.20261115-6c8eb1cf7/collect-route-{nhid,
missing,fixed}.txt`.

### 6.4.2 The first snapshot after a boot (`starting.go`, 2026-11-15)

**Found on the DX010's boot of 2026-11-14 (21:34:39 MST):** the IP
Settings apply at 21:36:20 refused with "no interface carries the bridge's
own target". Start had reported "32 ports (0 up), 0 MACs". Nothing was
due to change, so nothing was lost. A push moving the address in that
window would have been refused once and applied a cycle later.

**The journal** (`journalctl -b -u disunified` and `/var/log/syslog`;
the switch's name, its address and the controller's host replaced):

```
21:35:13.40 systemd[1]: Started swss.service              <- the container only
21:35:13.43 systemd[1]: Starting disunified.service
21:35:21.99 swss#supervisord: spawned: 'portsyncd'
21:35:22.23 swss#portsyncd: handlePortConfigFromConfigDB: Getting port configuration from ConfigDB...
21:35:22.70 disunified: disunified ... starting           <- Start's collect, until ~21:35:24.9
21:35:24.95 disunified: sonic : pmon's /etc/fancontrol could not be read (docker exec pmon)
21:35:34.64 swss#supervisord: wait_for_link Waiting until interface Vlan4094 is ready...
21:35:34.75 swss#supervisord: spawned: 'intfmgrd'
21:35:38.16 swss#supervisord: wait_for_link Interface Vlan4094 is ready!
21:35:44.63 bgp#bgpcfgd: Received message : '('Vlan4094|192.0.2.1/16', 'SET', (('state', 'ok'),))'
21:35:46.87 swss#orchagent: :- addIp2MeRoute: Create IP2me route ip:192.0.2.1
21:35:55.77 disunified: sonic: <switch>, Celestica Seastone-DX010, ..., 32 ports (0 up), 0 MACs, uplink hint 0
21:36:05.79 disunified: [dx010] ... informing http://<controller>:8080/inform as USWF07D ...
21:36:20.60 disunified: sonic <switch>: sonic: BGP: bgpd could not be read (vtysh); BGP not written
21:36:20.61 disunified: [dx010] ... apply IP Settings: sonic: no interface carries the bridge's own target 192.0.2.1; not changing an address blind
21:36:20.64 disunified: [dx010] ... reconciled system_cfg ...: 0 of 32 ports changed
21:36:37.92 disunified: sonic apply switch: sudo sonic-db-cli CONFIG_DB hdel 'NTP|global' src_intf ; sudo config save -y
```

**The cause, in two parts:**

1. **Start collected before swss had made anything.** The address is read
   from the kernel (`ip -j addr show`), not from a Redis table, and no
   section failed. At 21:35:23 the kernel had no `Vlan4094` netdev at all:
   vlanmgrd made it at about 21:35:38, intfmgrd gave it its address by
   21:35:44. APPL_DB had no port rows (portsyncd had just started), so no
   port was up, and STATE_DB had no FDB. CONFIG_DB was whole (the database
   and config-setup ran at 21:35:02-07), so Start's own checks passed: a
   SONiC version and a non-empty CONFIG_DB `PORT`. The unit's
   `After=swss.service` is met when the swss container runs (21:35:13),
   not when the ports exist. The premise that `Vlan4094` "already had its
   address" holds for 21:36:20, not for 21:35:23.
2. **That snapshot was acted on 40 s later.** Start's `config --help`
   checks took 30 s (21:35:25-55), the naming step's TLS timeout 10 s, and
   `informloop.Run` reconciles once before its first inform, on the
   session's snapshot: Start's. Every loop collect from 21:36:20 on was
   whole. The NTP step at 21:36:37, on the first loop collect, already
   found the address on `Vlan4094` (`ntpSource`).

**What consumed the empty snapshot, and what it wrote: nothing.**

- **IP Settings:** refused (the line above).
- **Ports:** planned from CONFIG_DB, 0 of 32 changed. The LLDP TLVs at
  21:37:07 came from a later collect, once the lldp container ran.
- **Device apply:** no address, so no NTP source; FRR's section was from
  before the bgp container, so BGP was not written; IGMP only noted.
- **IPv6:** the controller's API did not answer. Had it answered,
  `ntpSource` would have been empty and the apply skipped with a one-time
  warning.
- **Kernel default route** (§6.4.1): not in that build (da68007). With
  `frr_route` "error" before the bgp container, it writes nothing.
- **Naming and seeding:** `provision` got Start's snapshot, but the API did
  not answer; its retry at 21:37:00 took the session's fresh snapshot.
- **ACL hold** (`mgmtL2`): no address reads as "every VLAN may carry the
  management path", the conservative side.
- **The informs:** the first one followed a fresh collect, so Start's
  snapshot never reached the controller.

**The fix:** Start collects again, every 5 s for up to 5 min, until
`startingWhy` has nothing to say:

- APPL_DB has `PORT_TABLE:PortInitDone` (portsyncd has seen every port's
  netdev; disunified-l2d waits for the same marker);
- the kernel carries the address the bridge dials, where CONFIG_DB puts it
  on an in-band interface (`VLAN_INTERFACE`, `INTERFACE`,
  `PORTCHANNEL_INTERFACE`, `LOOPBACK_INTERFACE`).

An address CONFIG_DB does not hold (eth0's own, a lease, a name that does
not resolve) says nothing about starting. After 5 min Start fails and
`main` retries it with its backoff, so no half-started snapshot reaches the
loop, the naming step or the first reconcile. A collect during the run is
unchanged. The kernel was already the earliest true source for the address:
CONFIG_DB holds it from the first second, but it is not yet true there.

**Needs a reboot to prove:** at boot the journal shows "the switch is still
starting: ..." lines, then "the switch has finished starting", before the
"32 ports (N up)" line, and no "no interface carries the bridge's own
target". No collect from a booting switch was ever captured. The tests
(`starting_test.go`) derive the two boot states, no `PortInitDone` and no
`Vlan` netdevs, from
`sonic-igmp-202511.20261115-6c8eb1cf7/collect.txt`, a running switch's
capture.

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
| UniFi glue | `ubnt-lnxcfgd` writes the controller's config straight into CONFIG_DB (corrected 2026-11-01: `ubnt-lnxcfgd` is a helper that runs as interfaces come and go; which of Ubiquiti's processes translates the controller's keys into CONFIG_DB is not established, and §9's attribution of VLAN 1 to `ubnt-lnxcfgd` is the same mistake); there is **no `show ip igmp snooping` CLI** on the box (`show ip` has no `igmp`). A feature file of Ubiquiti's switches those daemons on and off (MAC and DHCP snooping, the STP manager, LLDP, fan control). |

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
  rows directly) -- and, found 2026-11-15, the breakout's knowledge that these rows
  depend on a port: `config interface breakout -f` keeps them, so the driver takes
  them out itself (§11.25.3).

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
removing a VLAN or a member; take a cage's rows out before breaking it out (the
breakout does not, §11.25.3); UniFi's VLAN 1 settings land on `Vlan<native_vlan>`
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

**Later, outside the parsers (2026-11-15): 0011 and 0012.** l2mcd numbered a port
that a breakout deletes and creates again 4096 + its kernel ifindex, which is
another port's index: Ethernet0's for a lane reported before its netdev,
Ethernet<ifindex>'s otherwise. That port lost its receive sockets, its tree and its
group memberships (§11.25.3, from l2mcd's log of a split on the DX010). 0011 gives a
re-created EthernetN its start-up index again; 0012 frees a closed socket's event.
The harness's `port-list-breakout` case reproduces it: its checks fail without 0011, a
LeakSanitizer report remains with 0011 alone, and it is clean with both. (On the trees
before 0011 it stops earlier, at 0013's use-after-free below.) Not in an image yet.

**0013: `mcgrp_stop_phy_port()` read and cleared a membership it had just freed
(2026-11-15).** The function (`src/igmp.c` 616-698 at upstream `651a13b`, shared by
IGMP and MLD) walks a VLAN's groups. For each group it destroys the port's membership with
`mcgrp_destroy_mbrshp_entry()`, which `free()`s it. Then it called
`pims_is_pim_snoop_mbrship()` on the freed membership and cleared
`MLD_OR_IGMP_JOIN_PORT` in its `pims_mbr_flags`. Every way a port's memberships are
stopped runs it, static groups included:
- a member port's link going down (`l2mcd_portstate_update` -> ... -> `mld_vport_state_notify`);
- the port leaving the VLAN (`L2MCD_VLAN_MEM_TABLE_UPDATE` op 0);
- snooping turned off on the VLAN (`mcgrp_stop_vir_port`);
- a version change (`mcgrp_handle_intf_ver_change`).

The review of the Micas PR flagged it as Critical on 2026-02-20, and it was never fixed
([`prior-art.md`](../prior-art.md) §6.1). No patch of the series touched `src/igmp.c`. The
neighbouring callers of `mcgrp_destroy_mbrshp_entry()` (`igmp.c` 1810, 2043, 2358,
`igmp_fsm.c` 1934, `l2mcd_mld_port.c` 2454, 2765, `l2mcd_mld_pim_snoop.c` 263) do not use
the membership after the call. The fix reads the PIM flag into a local before the destroy
and drops the write. Nothing in the daemon sets `PIMS_WG_MBR_PORT` or `PIMS_SG_MBR_PORT`
(they are only tested and cleared), so the flag was always false, and the function's
behaviour is unchanged.

**Why the switch is quiet.** A membership is a 336-byte `calloc` (a 352-byte chunk), and
`pims_mbr_flags` is at byte 328 (gcc 12, amd64). In a freed chunk of that size, glibc's own
fields are the first 16 bytes and the next chunk's `prev_size` at byte 336. Nothing
allocates between the free and the read. The read therefore sees the old flags, and the
write lands on freed bytes nobody uses. It is a latent fault: a different allocator,
`MALLOC_PERTURB_`, a change of layout or a sanitizer build makes it real.

**Harness** (`contrib/sonic-image/l2mcd-fuzz`): `member-link-down` (IGMPv3),
`mld-member-link-down` (MLDv2) and `member-vlan-leave`. In each, Ethernet4 and Ethernet8
join a group, then Ethernet8 loses link or leaves Vlan100, through l2mcd's own functions.
The checks: the group keeps Ethernet4 alone, and one member deletion went out, under
Ethernet8. Results:
- `651a13b`, and the series 0001-0012: heap-use-after-free in `pims_is_pim_snoop_mbrship`.
  The free is in `mcgrp_destroy_mbrshp_entry`, called from `mcgrp_stop_phy_port`.
- With 0013: clean, every check passing.
- The other 30 cases give the same results with and without 0013, on the series and on
  `651a13b`. One exception: on `651a13b` + 0013, `port-list-breakout` gets past the
  use-after-free to 0011's own fault.
- The IGMP and MLD fuzzers ran 30 minutes with 4 workers each on the series with 0013:
  6.9 million IGMP and 19.4 million MLD packets, about 1,090 and 1,150 edges, no crash
  and no sanitizer report.
- gcc 12 `-Wall` gives no warning in `igmp.c` either way.
- `dry-run.sh` passed (2026-11-15).

The same work found the harness's APPL_DB stub counting deletions where l2mcd never sends
them (§11.25.3). It now counts them where l2mcd does.

**On the DX010's image (0001-0010, `igmp-202511.20261115-6c8eb1cf7`), 2026-11-15:
nothing visible.** The run was under the lock, with the bridge running and no config
written. The chip's MAC loopback brought empty cage 5 (Ethernet16) up for 13 s, and then
the loopback was taken off.
- **Up:** l2mcd activated the cage's seven reserved static groups in Vlan4094 (orchagent:
  Ethernet16 at 7 members, the VLAN at 14). It also learned the cage as a dynamic router
  port, from its own looped query.
- **Down:** `mcgrp_stop_phy_port` ran over the seven memberships it frees. l2mcd sent the
  seven `Group-DEL`s and the router-port delete under Ethernet16, and orchagent took the
  VLAN back to its 7 members, the uplink's.
- **After:** the uplink aggregate stayed the VLAN's router port. l2mcd kept its pid and
  uptime; there was no new core and no error in its log or syslog.

This is what the heap layout predicts. The seven groups also left l2mcd's own tree (its
VLAN dump: `ngroups` 7 before, 0 after). The uplink's static rows are in the chip and in
l2mcd's static-group list, but no membership in that tree held the groups, so the cage's
departure emptied them. That is the function's normal path and the same with 0013. Why the
7 groups were in the tree beforehand with no member (another session's looped cage had gone
down at 15:51) was not chased.

**The risk on a switch with real receivers.** Every member's link-down, leave of the VLAN,
snooping turned off and version change takes this path. On this image's glibc it changes
nothing: the read sees the right (always false) PIM flag, and the write lands on unused
freed bytes. No crash or wrong group state follows from it. The fault is latent until the
allocator, its tuning or the struct changes. 0013 removes it.

**What an image install with 0013 has to prove**, besides §11.25.3's list for 0011 and
0012. On an empty cage under the lock: MAC loopback on and then off, as above. Then:
- l2mcd keeps its pid;
- the cage's reserved static members leave ASIC_DB and come back on the next link-up;
- every other port's members and the uplink aggregate's mrouter entry are unchanged.

With no sanitizer on the switch, the install cannot show the fault's absence directly.
That proof is the harness's.

**0014: l2mcd aborted on a report from a port the port db no longer named (2026-11-15).**
At 11:16:47 l2mcd died on an uncaught `std::logic_error` ("basic_string: construction from
null is not valid"; core `/var/core/l2mcd.1794766607.26.core.gz`). That was one second after
Ethernet0's link event had rebuilt its tree (§11.25.3). systemd restarted the container at
11:19:48, so snooping was down for three minutes.

**The cause, from the core.** The core was copied off the switch (read-only) and opened with
gdb in the bookworm build slave. The image's `l2mcd` binary is in the build tree that made
`igmp-202511.20261115-6c8eb1cf7` (`target/debs/bookworm/l2mcd_1.0.0_amd64.deb`, md5 equal to
the running `/usr/bin/l2mcd`), with its `l2mcd-dbgsym` package beside it. The sysroot is the
image's `docker-l2mcd.gz` layers. The build VM's `~/out` holds only the installer images.
- **Backtrace** of the only thread: `std::__throw_logic_error` ← `basic_string(const char *)`
  ← `l2mcd_is_peerlink()` (`l2mcd_sync.cpp:138`) ← `receive_igmp_packet()`
  (`src/igmp_io.c:920` in the image's source, upstream line 835) ← `l2mcd_recv_igmp_msg()`
  (fd 57) ← libevent.
- **The packet:** an IGMPv3 report on Vlan4094, to 224.0.0.22, `rx_phy_port_number` 4096. That
  is the receive socket Ethernet0's link event had opened at index 4096 a second before.
- **The port db in the core:** the index→name table has **no entry for 4096**, and 4131 names
  Ethernet0. Ethernet35's own name had been overwritten at 4131 (its name→index entry still says
  4131). The name→index table also holds a key at 4096 whose string reads empty: that is the
  freed "Ethernet115" (below).

**How 4096 lost its name** (l2mcd's log, `l2mcd-breakout-index-20261115.txt`, read against
`port/l2mcd_portdb.c`). `portdb_add_ifname()` overwrites an index's name when the index already
has one: the hash insert returns 2 and replaces the data. The old name keeps its
name→index entry. `portdb_delete_ifname(name)` looks the
name up, then deletes and frees whatever name the index now holds.
1. At 10:48:27 Ethernet113, 114 and 115 were each added at 4096. Index 4096 ended up naming
   Ethernet115, and all four names (Ethernet0 too) still mapped to 4096.
2. Ethernet113's `delete2` then removed 4096's entry and freed the "Ethernet115" string, which
   was still a key of the name→index table. That freed string is the empty key in the core.
3. From then on no name was added at 4096 (Ethernet114's `delete2` logged rc -1).
4. At 11:16:46 Ethernet0's recovery found its name still mapped to 4096. The `delete2` branch
   built its new tree and sockets there, then deleted the name. The next update numbered
   Ethernet0 4131 (4096 + kif 35), so the socket sat at 4096 with no name. The first IGMPv3
   report on it aborted the daemon.

**Every place with the same fault** (`651a13b` lines; the series through 0013 in brackets).
`l2mcd_is_peerlink()` is the only consumer of a port-db name that cannot take NULL, and six
calls pass it unchecked:
- `receive_igmp_packet()`: the v1/v2 report at 777 [862], the v3 report at 835 [920], the v2
  leave at 850 [935];
- `l2mcd_mld_process_v1_report()` at 391 [469], `l2mcd_mld_process_done()` at 490 [568],
  `l2mcd_mld_process_v2_report()` at 573 [651].

The rest are safe:
- `receive_igmp_packet()`'s own peer-link test (640) and `mcgrp_add_router_port()`
  (`l2mcd_mld_port.c` 2040) check the name first.
- Every other assignment from `portdb_get_ifname_from_portindex()` or the
  `mld_get_if_name_from_*` wrappers checks for NULL before `strcmp`, `strncpy` or a delete
  (lines on the series): `l2mcd_utils.c` 178 and 302, `l2mcd_mld_port.c` 1473, 1699, 1844, 2327
  and 2357, `mld_vdb.c` 803.
- The remaining calls are arguments of log formats. glibc prints `(null)` for a NULL `%s`, and
  the harness evaluating every per-VLAN log argument (`HARNESS_LOGARGS`) reports nothing on
  these cases. They are left as they are.

**The fix, sonic-l2mcd 0014.** `receive_igmp_packet()` and `receive_mld_packet()` drop any
message except a query from a port the port db does not name, before the handlers run. The
drop is counted in `rx_bad_if` and logged with the VLAN's debug log, as a packet from a down
port is; that is the only place it shows with the debug log off. Queries take no name on their
way (`mcgrp_add_router_port()` checks it) and are processed as before. The patch applies to
`651a13b` as it is. gcc 12 `-Wall` gives the same single warning in `mld_io.c` (an unused
`dest_ip`) with and without it, and none in `igmp_io.c`.

**Harness.** The stub of `l2mcd_is_peerlink()` now aborts on NULL, with the caller's stack, as
the real one does. Before, it accepted NULL, which is why no case saw the fault. The six
`port-no-name-*` cases each take Ethernet8's name out of the port db with
`portdb_delete_ifname()`, keeping its tree and sockets as on the switch. Each then sends one
message: an IGMPv2 report, an IGMPv3 report, an IGMPv2 leave, an MLDv1 report, an MLDv2 report
or an MLDv1 done. Results:
- On `651a13b` and on the series 0001-0013, each aborts in `l2mcd_is_peerlink(NULL)` at its own
  line above. On the series, `port-no-name-v3-report` aborts at `igmp_io.c:920`, the core's
  line.
- With 0014, on both bases, each is clean: `rx_bad_if` one higher, nothing forwarded, no
  membership.
- The other 33 cases give the same result with and without 0014, on each base, with the debug
  log's arguments evaluated as well as without.
- The IGMP and MLD fuzzers ran 30 minutes with 4 workers each on the series with 0014: 39.0
  million IGMP and 24.0 million MLD packets, about 1,095 and 1,157 edges, no crash and no
  sanitizer report. They feed a named port, so they show that the parsers are unchanged, not
  the new check; that is the six cases' job.
- `dry-run.sh` passed with 0014 in the series (2026-11-15).

**Is it still reachable with 0011?** Not in any way found:
- 0011 stops `delete2` deleting the name, so a re-created port's tree and its name keep one
  index.
- `l2mcd_port_index_for()` never numbers a port at an index another name holds. The other
  callers of `portdb_add_ifname()` are the start-up loop and `mld_map_port_add_del()`
  (`l2mcd_mld_port.c` 1461 on the series), which names a VLAN at its VLAN id, below 4096. So with
  0011 nothing overwrites a port's name.
- A packet cannot outlive its port's deletion. `l2mcd_recv_igmp_msg()` parses each packet in
  the socket's own callback (frame 11 above), and a port-list delete closes the socket before
  its name goes.

0014 is therefore a guard against a way not yet found, as much as a fix: any such case becomes a
counted drop instead of an abort. The port db's own fault (an overwritten name stays a key, and a
later delete by the old name frees the index's current name while it is still a key) is 0016's,
below.

Upstream as [jasper-hou-micas/sonic-l2mcd-micas#11](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/11)
(posted 2026-11-16, one commit on `651a13b`; [`prior-art.md`](../prior-art.md) §6.1).

**What an image install with 0014 has to prove.** A breakout and a join of an empty cage, as in
§11.25.3, then the uplink's next link event. l2mcd keeps its pid, logs no `rx_bad_if` growth
(`l2mcd_ctl -s <file> -c 2`) and leaves no new core in `/var/core`. With 0011 in the image the
nameless index should never occur, so the install shows only that nothing regressed; the
fault's absence is the harness's proof.

**0016: the port db holds a name and its index once each way (2026-11-16).** The fault under
0014's crash, read with the series through 0014 applied (`port/l2mcd_portdb.c` is the same on
`651a13b`; other files' lines on `651a13b` in brackets).
- **The two maps.** `portdb_portindex_to_ifname_hash` maps an index to a string, and
  `portdb_ifname_to_portindex_hash` maps a string to its index. Both are `hashGrowGeneric`
  tables created without a `key_init`, so the name map's key is the very pointer stored, not a
  copy. `portdb_add_ifname()` (60) mallocs one string and inserts it into both: it is the index
  map's value and the name map's key. `portdb_delete_ifname(name)` (318) looks the name up,
  removes that key without freeing it (`ret_key` NULL), then removes the index's value and frees
  it (333). That is right only while the key and the value are the same string.
- **The fault.** `portdb_add_ifname()` looks at neither map first. `hashGrowGenericInsert()`
  answers a key it already holds by replacing the data and returning 2, which the function
  takes for success. A second name B at index i, named A, replaces i's value with B's string;
  A's key still maps to i. `portdb_delete_ifname(A)` then removes A's key and frees B's string,
  which is still B's key: the next look-up whose probe reaches it compares against freed memory
  (`string_key_compare()`), and i has no name. That is the freed "Ethernet115" key at 4096 in
  0014's core. Two more shapes of the same fault: A added at a second index j moves A's key to
  j and leaves i naming A (a delete of A frees j's string and leaves i's); A added twice at i
  replaces i's string, and the first one is no longer freed by anything (13 bytes for
  "Ethernet900").
- **The callers.** Four: `port_ifname_db_init()` (`l2mcd_system.c` 2284 [2252]), once at start,
  Ethernet0-511 at 4096 + N into an empty db; `l2mcd_port_list_update()` (`l2mcd_utils.c` 328
  [266]), only for a name the db does not hold; `mld_map_port_add_del()` (`l2mcd_mld_port.c`
  1461 [1457]), a VLAN's name at its VLAN id. `mld_if_snoop_set()` adds `VLAN<vid>` there once
  per address family, so a VLAN snooped for IPv4 and IPv6 takes the third shape and loses a
  string each time, and `mld_add_vlan_to_protocol()` (1030) adds the VLAN node's own
  `VLAN%04d` name at the same index when the VLAN has no port-db entry. Deletes come from
  `l2mcd_port_list_update()` by the port's own name and from `mld_map_port_add_del()` by the
  index's current name.
- **Reachable without 0011** (`651a13b`): yes, and it was reached on the DX010.
  `l2mcd_port_list_update()` numbers a port it does not know 4096 + its kernel ifindex, which
  is Ethernet<ifindex>'s index from the start-up loop (4096 itself for a lane reported before
  its netdev), and the add overwrote that port's name.
- **Reachable with 0011:** not on SONiC, and only through one gap. `l2mcd_port_index_for()`
  refuses an index another name holds on its ifindex branch, but its EthernetN branch returns
  4096 + N without asking. That index is free whenever EthernetN's name is absent, except when a
  name that is not a canonical EthernetN (such as "Ethernet01") was numbered 4096 + ifindex with
  ifindex N meanwhile; SONiC's port list carries only canonical names and PortChannels, which
  are numbered 4608 + ifindex, above every EthernetN index. VLAN names never meet a port's (they sit below 4096), and a VLAN's
  delete passes the index's current name, so the VLAN path only loses strings. The harness
  setup, the breakout case and the VLAN-leave case log no refused add with 0016 (checked with
  `HARNESS_DEBUG`); whether `mld_add_vlan_to_protocol()`'s `VLAN%04d` add ever runs after
  `mld_if_snoop_set()`'s `VLAN%d` one is not checked; with 0016 it is refused and logged.

**The fix, sonic-l2mcd 0016:** `portdb_add_ifname()` looks both up first. The name already at
its own index is no change (return 0, nothing allocated). Any other overlap, a named index or
a name at another index, is refused (-1, a notice naming the holder), and both keep what they
had. A failed insert frees its string and takes back the index map's entry. `l2mcd_port_list_update()`
stops on a refused add (-1 for an add, 0 for a delete) instead of building a tree, or on a delete
closing the sockets, at another port's index.

Refusing rather than taking the index over, because of what the callers need. The holder of a
port index is a port with its receive sockets and tree at that index. Taking its name would
leave exactly the nameless index whose report crashed l2mcd (0014), and the holder could no
longer be found by name to delete. No caller wants a name to move: the port path adds only an
unknown name, and a VLAN's name always goes to its VLAN id. On `651a13b` alone, 0016 turns the
DX010's corruption into a re-created port without an index (no snooping on it, logged); 0011
gives that port its own index.

**Harness:** three cases, each run by name, on free indexes 4000 and 4001 with names past
Ethernet511. `portdb-name-overwrite` adds Ethernet900 at 4000, then Ethernet901 at 4000, deletes
Ethernet900, then reads index 4000 and looks Ethernet901 up; with the fix it also adds Ethernet901
at the freed index and deletes it. `portdb-name-moved` adds Ethernet900 at 4000, then at 4001.
`portdb-name-twice` adds Ethernet900 at 4000 twice. After each step every name and index must
agree both ways, and after the deletes the db must be as it was with no byte still allocated
(ASan's allocated-bytes count: LeakSanitizer does not see the lost string, because the hash
leaves a deleted key's pointer in its slot).
- On `651a13b` and on the series 0001-0014: `portdb-name-overwrite` is a heap-use-after-free,
  a read in `string_key_compare()` from `portdb_get_portindex_from_ifname("Ethernet901")` of the
  string `portdb_delete_ifname("Ethernet900")` freed. `portdb-name-moved` fails its checks (4000
  still names Ethernet900 after the delete). `portdb-name-twice` fails with 13 bytes still allocated.
- With 0016, on both bases, all three are clean.
- All 42 cases on the series with 0016 are clean, with the debug log's arguments evaluated as
  well as without. On `651a13b` with 0016 alone, every other case gives the result it gives on
  `651a13b` except `port-list-breakout`: there Ethernet40 now keeps its index, tree, sockets and
  membership (on `651a13b` the case stopped at 0013's use-after-free), and it fails because
  the two lanes get no index, which is 0011's part.
- gcc 12 `-Wall`: no warning in `l2mcd_portdb.c` or `l2mcd_utils.c`, with or without 0016. The
  fuzzers were not run: they feed packets to named ports, and 0016 touches no packet path.
- `dry-run.sh` passed with 0016 in the series (2026-11-16).

**What an image install with 0016 has to prove:** nothing beyond 0011's and 0014's lists
(§11.25.3, above). A refused add logs `portdb_add_ifname port:<name> index:<n> refused`. With
0011 none should appear, so one in the log is a caller this section has not found.

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

### 8.15 A port that leaves its last VLAN, and the bridge port orchagent would not remove (2026-11-14)

**What was seen.** On the DX010 (`igmp-202511.20261106-f2e25f54c`, snooping
on in Vlan4094 with the reserved groups' static members on every port), a
native-network change in the UI on a test cage (Ethernet32: Default to a
VLAN-only network and back) left the port dead. orchagent's syslog, from
16:21:32 UTC: `removeVlanMember` took Ethernet32 out of Vlan4094, then
`removeBridgePort` logged "still has references to a Layer 2 multicast group
member" about twice a second from then on; the add to the new VLAN reused the
bridge port, and ASIC_DB held it with `SAI_BRIDGE_PORT_ATTR_ADMIN_STATE
false` (the chip's learn column `D`). Moving the port back did not clear it.
An hour later CONFIG_DB, APPL_DB and STATE_DB had Ethernet32 untagged in
Vlan4094, while ASIC_DB had **no** VLAN member on its bridge port (at 16:32:15
a re-add was undone 12 ms later by the delete orchagent kept retrying) and
**no** L2MC group member either: the six L2MC group members left were all the
uplink's. The capture is
`sonic-igmp-202511.20261106-f2e25f54c/collect-stranded.txt` (its
`asic_bridge_port` section, the new collector section below).

**The cause is the kit's L2MC orch (sonic-swss 0001, the Micas series), not
the driver's order.** `removeBridgePort` refuses while `Port::m_l2mc_count` is
not zero, and that count had leaked nine minutes before the move, when the
port's link dropped. The same syslog, 16:11-16:12 UTC:

1. 16:11:40, the port up (a test loop): l2mcd added its seven static members
   (`AddL2mcGroupMember ... l2mc_count 1` ... `7`).
2. 16:11:50, l2mcd learned the port as a multicast router port (it heard
   queries over the loop): `AddL2mcMrouterPort` added it to four more groups,
   **without counting them** -- the orch counts only the group-member path.
3. 16:12:01, link down. l2mcd's member deletes arrived while the port was
   still a router port: for five of the static groups `RemoveL2mcGroupMember`
   logged "mrouter port is same as group member port" and kept the SAI member
   for the router port, **returning without decrementing** the count or the
   group's `ref_count`; for 224.0.1.60, a group the port was in only as a
   router port, the same path **decremented** a count it had never raised
   (7 -> 6). Then the router port's delete: `RemoveL2mcMrouterPort` removed
   the five kept SAI members, **decrementing nothing**. Two more member
   deletes after it went the normal way (6 -> 5 -> 4).
4. Result: `m_l2mc_count` 4 with no group member on the port's bridge port,
   and five groups whose `ref_count` never reaches zero (their entries stay in
   the chip, holding only the uplink's router-port members).

The refusal then strands the port because `removeBridgePort` sets the bridge
port admin down and removes its STP ports **before** it checks the count, and
`addBridgePort` returns an existing bridge port as it is: the VLAN added next
gets the admin-down one. The same code strands a port without any leak
whenever l2mcd's member deletes reach orchagent after the VLAN member delete
-- a native move is a delete then an add in one batch, a quarter second apart.

**Who is exposed.** Any port that is a static or dynamic member and a router
port at once, and loses link or stops being a router port while it is both.
With UniFi's reserved groups flooded to every port, that is **the uplink**
(Ethernet0: the router port, beside the gateway's queries, and a static member
of all seven groups). On the unpatched image, anything that takes Ethernet0 out of
its last VLAN -- joining the planned core aggregate first of all -- would
strand the uplink: install an image with the fix (below) before that
(the DX010 has run one since 2026-11-14).

**What shares it** (from the source; only the native move was seen live):
every change that takes a port out of its **last** VLAN goes through
`removeBridgePort` -- a native-VLAN move on an access port, a port that
becomes routed, a port joining an aggregate or becoming a mirror destination
(the driver strips its VLANs first), and a breakout's deleted lanes (whose
static rows the breakout itself leaves; the driver deletes them before it
since 2026-11-15, §11.25.3). A tagged
VLAN taken off a port that keeps another never removes the bridge port: not
affected (its group members in that VLAN only linger until l2mcd deletes
them). On an empty cage none of it shows: l2mcd programs members only on an
oper-up port, so a down port has no members to wait for (the 2026-11-07
breakouts with snooping on were on an empty cage).

**Recovery.** On the unpatched orch none was found short of a swss restart
or a reboot: the count lives in orchagent's memory, and the only path that
decrements it without a matching member is turning snooping off on a VLAN
where the port is a router-port-only member of exactly as many groups as the
count is high -- theoretical, untried, and the pending VLAN-member delete
would still take the port's re-added membership away. On the DX010 the
port stayed stranded until the reboot at about 17:54 UTC.

#### The fix: sonic-swss 0004

`contrib/sonic-image/series/sonic-swss/0004-*`, this project's patch on the
series:

- `m_l2mc_count` is every SAI L2MC group member on the port's bridge port,
  whichever path made it (router-port members included); a group's
  `ref_count` counts the members only. A router port's member that becomes a
  group member, or stops being one while the router port stays, moves the
  `ref_count` and not the SAI member.
- `removeL2mcPortMembers()` drops a port's members (and its place in the
  router-port lists) in the VLAN it leaves, from `removeVlanMember`, and in
  every VLAN from `removeBridgePort` **before** anything of the bridge port
  changes. A group left with no member goes, as on a leave; l2mcd's late
  deletes find nothing. A count still above zero with no member is logged and
  reset, never refused. This is also what the ECS's own orch does in effect:
  its orchagent (Ubiquiti's 202106 lineage, read-only 2026-11-14) records
  which path made a member and converts between the two, and has no
  bridge-port refusal on L2MC references.
- PortsOrch's copies of the port and the VLAN take L2mcOrch's counts before
  they are written back; a router port is listed once per VLAN (a retried
  SET used to push it again).

Tested in the build host's bookworm slave (`tests/mock_tests`, the patch's
`L2mcOrchTest`): the 16:11-16:12 sequence above, a router port leaving before
the member, a port leaving its VLAN with members left (the native move), and
a port leaving one of two VLANs. **All four fail on the orch without 0004**
-- the first with the leaked count, the third with the delete left pending and
the same bridge port reused admin down, the live symptom -- and pass with it,
with the whole mock suite (607 tests in 68 suites). Built into
`igmp-202511.20261114-42cccc87a`, installed on the DX010 on 2026-11-14 and
proven live there (below).

#### The driver: a safe order for an image without it

The driver cannot prevent the leak (it happens on a link event), but it can
keep its own writes from stranding a port that has not leaked:

- `Start` checks the running orchagent for 0004 (`l2mcFixCheckCommand`: the
  notice its `removeL2mcPortMembers` logs) on an image with l2mcd, as it does
  for sonic-swss 0003, and logs when it is missing.
- Without it, and with `FEATURE|l2mcd` enabled, every `config vlan member del`
  that leaves an interface in no VLAN is followed in the batch by
  `bridgePortWait`: a read-only `python3 -c` that waits (up to 10 s) until
  ASIC_DB holds no bridge port for the interface, then lets the batch go on to
  the add, the aggregate join or the address. When l2mcd's deletes are merely
  late, orchagent's retry removes the bridge port and the next step gets a new
  one. When the count has leaked, the wait fails the batch with the reason
  before the add, instead of the add landing on the admin-down port.
- `collect.py`'s `asic_bridge_port` (every port's and PortChannel's bridge
  port, `{"admin": ...}`, mapped through COUNTERS_DB's name maps) shows a
  stranded port: the collector warns once ("held admin down in the chip ...
  a swss restart or a reboot clears it"), and the plan leaves that port's
  VLANs as they are with a note instead of retrying against it.
- The static rows still go before the delete (the CLI's refusal, §8.14).

The FixtureRunner models orchagent's bridge port (`OrchL2MCFix`,
`L2MCLeaked`, `Stranded`): on an image without 0004 a last-VLAN delete with
static rows deleted earlier in the batch (l2mcd's deletes not yet through) or
a leaked count is refused with the bridge port admin down, a VLAN add then
records the port as stranded, and the wait clears the refusal unless the count
leaked. `bridge_port_test.go` runs on the capture above: the move's order
(statics, delete, wait, add), the same commands without the wait stranding the
port, no wait on a patched orchagent, a leaked count failing the wait, and the
stranded cage left alone.

#### Live on the DX010 (2026-11-14, 18:00-18:04 UTC)

The image without 0004, the bridge at 0.0.0+20261114.1c1cd16 (this change),
an empty test cage (Ethernet48), changed through the controller's REST as the
UI saves it. The record is
`sonic-igmp-202511.20261106-f2e25f54c/bridge-port-live-20261114.txt`.

- **The bridge logged the missing patch at start** ("orchagent lacks the image
  kit's sonic-swss 0004 ...").
- **A native move on a down cage, Default to a VLAN-only network and back.**
  The driver's batch was the seven static deletes, `config vlan member del
  4094`, the wait, `config vlan member add -u 4000`, and the save. orchagent
  removed the bridge port 55 ms after the VLAN member. The add made a new one,
  `ADMIN_STATE true` in ASIC_DB. The way back ran the wait too, and
  `applyIGMP` put the seven rows back. No `removeBridgePort` refusal since
  the boot: the only two `removeBridgePort` lines are these two removals.
  A down port has no L2MC members, so this run proves the driver's path, not
  the race.
- **The trigger, reproduced.** `bcmcmd "port ce12 lb=mac"` brought the cage
  up with no cable. RSTP made it a Backup port, discarding, and l2mcd added
  its seven static members (count 7). **4 s later l2mcd learned it as a router
  port anyway**: the trapped queries reach l2mcd whatever the port's STP
  state. With `lb=none` (link down) the sequence of 16:12 repeated:
  - four "mrouter port is same as group member port" with no decrement;
  - 224.0.1.60, a group the port had only as a router port, decremented;
  - the router-port removal took the kept members uncounted;
  - three normal deletes after it.

  orchagent's count stopped at **3** with no L2MC group member on the port's
  bridge port in ASIC_DB: the leak's signature. The bridge port itself stays
  up and the port works. The leak waits for the port's next removal from its
  last VLAN, and that is the one change not made: it would strand the cage
  until a swss restart.
- **What a cable-less test cannot show:**
  - **The race on a port with programmed members that is not a router port.**
    A looped cage becomes a router port in seconds, sooner than any controller
    push reaches it.
  - **The driver failing its wait on a leaked count, live.** Fixture-tested
    only: doing it would strand the cage.

  Both need a host on a cabled port, or the patched image (on it the leak
  cannot happen).
- **Left on the switch:** cage 13's leaked count (3) lives in orchagent's
  memory until the next swss restart or reboot. Do not move cage 13 to
  another VLAN, make it routed or aggregate it before then: the move would
  strand the cage and the driver's wait would fail the batch. Everything
  else was restored and checked: the loop off (`ps ce12` shows no
  loopback), Vlan4094 untagged with its seven static rows, and the
  controller's 32 port overrides equal to the pre-test copy. (The count
  was cleared by the reboot at 18:31 UTC; the switch has run the patched
  image since 22:53 UTC.)

**The image with 0004: `igmp-202511.20261114-42cccc87a` (built
2026-11-14, 144 min).** It is the running `igmp-202511.20261106-f2e25f54c`
plus sonic-swss 0004 and sonic-buildimage 0013 (the Seastone SO-DIMM sensor
limits, set by hand on the switch since 2026-11-07); its bases and the build
scripts are unchanged, and `dry-run.sh` passed on the kit first. The DX010's
`sonic-broadcom-legacy-th.bin` has sha256
`b913217d3268eff364f8a1830afecba44a1124f247ca26d4ac04b0c7050bed0f`. Both
patches were found in the image itself, not only in the series: the `.bin`
unpacked, the orchagent in both of its docker layers (the same file as the
built `swss_1.0.0_amd64.deb`'s) carries 0004's notice string, the one
`l2mcFixCheckCommand` looks for, and the root filesystem's Seastone
`sensors.conf` has 0013's `jc42-*` block. The image is kept with the others
on the deployment host.

#### Installed and proven live (2026-11-14, 22:45-23:03 UTC)

Clint's go; the kit's procedure, step by step as planned
(`sonic-upgrade.sh arm <image.bin> <sha256>`, `sudo reboot`,
`sonic-upgrade.sh confirm`). The record is
`sonic-igmp-202511.20261114-42cccc87a/l2mc-0004-live-20261114.txt`.

- **The install.** Armed at 22:48 (the install deleted the `e53777446`
  slot, as install.sh keeps two), rebooted at 22:49:50, answering on the
  new image at 22:53:22, confirmed at 22:58 with `f2e25f54c` kept as the
  fallback. The bridge's `.deb` came back with the image's first boot;
  l2d reinstalled itself under its dead-man and cancelled it; the first
  reconcile changed 0 of 32 ports, then one apply put back what a reboot
  loses (the VLAN netdevs' MTUs and the LLDP-MED TLVs, reported as 32 of 32
  ports), then nothing. The start log has no "orchagent lacks ... 0004"
  line, and the running orchagent carries 0004's string. 0013: the SO-DIMM
  sensor read 28.8 C against high 85 and crit 95, no alarm.
- **The loop-and-drop on cage 13**, the run that left the count at 3 on the
  old image. With `port ce12 lb=mac` the port took its seven static
  members, and 3 s later became a router port in three more groups (the
  ones it was not a static member of): **those are counted now**, the
  port's count 10 and the VLAN's 20. At `lb=none` the router-port removals
  and l2mcd's member deletes took it down one member at a time to **0**
  (the VLAN's back to 10, the uplink's), with no "reset to 0" line: the
  count matched the chip's members throughout.
- **A native move and back**, through the controller's REST (the port
  overrides PUT with only the native network changed, as the UI saves it).
  The batches: the seven static deletes, `config vlan member del 4094`,
  `config vlan member add -u 4000`, the save -- **no bridge-port wait** --
  then `del 4000`, `add -u 4094` and `applyIGMP`'s seven rows. orchagent
  removed the bridge port and added a new one each time (about 1.1 s
  apart), a new oid with `ADMIN_STATE true` each time, and no "still has
  references" line since the boot. Restored afterwards: Ethernet48
  untagged in Vlan4094 with its seven static rows, and the controller's 32
  port overrides equal to the pre-test copy.
- **A move while the cage is looped** (step C, 2026-11-15, on
  `igmp-202511.20261115-6c8eb1cf7`, the bridge stopped; the driver's plan
  run by hand, since nothing in it depends on where the push came from;
  `sonic-igmp-202511.20261115-6c8eb1cf7/l2mc-0004-stepc-20261115.txt`).
  Looped in Vlan4094, cage 13 took its seven static members and became a
  router port in four more groups: **count 11**. The static rows out took
  it to 5. Then `config vlan member del 4094` set the race going: the
  router-port members were still programmed when the VLAN member went, and
  orchagent logged `removeL2mcPortMembers: ... l2mc group member
  Ethernet48 removed with its port` for each of the four, the count going
  to **0**. The bridge port was removed 50 ms after, and `add -u 4000`
  made a new one (oid changed, `ADMIN_STATE true`), with no "still has
  references" line. With the loop off, the move back gave another new
  bridge port, admin true. The bridge's start-up reconcile put the seven
  rows back, and CONFIG_DB equalled the snapshot taken before, saved and
  running. The race with members but no router port needs a cabled host.
  The wait failing on a leaked count cannot happen on this image and stays
  fixture-tested.

### 8.16 A LAG's group members, and the entries the orch could not remove (2026-11-15)

**What was seen.** On the DX010 (`igmp-202511.20261115-6c8eb1cf7`, the
uplink an aggregate since that afternoon: PortChannel1 = Ethernet0 +
Ethernet4, untagged in Vlan4094 with the seven reserved static groups),
orchagent logged `RemoveL2mcEntry: Failed to remove l2mc entry (0.0.0.0,
<group>, 4094) rv:-7` for the seven reserved groups 159 times in two hours:
seven at a time, at every PortChannel or VLAN removal on the test cages.
ASIC_DB held 7 `L2MC_GROUP`, 7 `L2MC_GROUP_MEMBER` (each output
PortChannel1's bridge port) and no `L2MC_ENTRY`; the chip had no IPMC entry
and no multicast group for them (`bcmcmd "ipmc table show"`, `"mc show"`).
-7 is `SAI_STATUS_ITEM_NOT_FOUND`. The record is
`sonic-igmp-202511.20261115-6c8eb1cf7/l2mc-lag-egress-20261115.txt`.

**The removals were a symptom.** The cause is one event, at 14:14:46:

1. 13:40:58, PortChannel1 came up with only Ethernet0 distributing. l2mcd
   added PortChannel1's seven reserved-group members then. Ethernet4 started
   distributing at 13:48:04.
2. 14:14:46, a looped test cage (Ethernet116, a static member and a router
   port) went down. Its member deletes took each reserved group's
   `ref_count` to 0. By then PortChannel1 was in those groups only as a
   router port (l2mcd had dropped its static membership, below).
3. `RemoveL2mcEntry` (sonic-swss 0001) removes the **entry first**, then the
   members it kept for router ports. Ethernet116's member went. syncd
   refused PortChannel1's: `brcm_sai_remove_l2mc_group_member:634 mcast
   egress remove failed with error Entry not found`. The orch returned
   there, half-way, seven times.
4. That left seven groups recorded with no entry in the chip. Every later
   removal that reached them (a test cage's PortChannel or VLAN leaving, in
   `removeL2mcPortMembers`) failed at `remove_l2mc_entry` with -7. And
   because `hasL2mcGroup()` stays true, a member joining those groups later
   never makes the entry again. **The seven reserved groups are out of the
   chip until a swss restart or a reboot.** Meanwhile they flood as unknown
   multicast (`forward_unknown_multicast` is on, §8.14), so nothing that
   wanted them lost them.

**Why the SAI refused only those members**, measured on a test LAG
(16:39-16:43). Cages 25 and 26 (Ethernet96 = ce24, Ethernet100 = ce25,
empty) were taken out of Vlan4094 and made PortChannel8, untagged in
Vlan3998 with snooping on. A LACP aggregate of looped cages never
distributes, so the members' APPL_DB `LAG_MEMBER_TABLE` status and one
`L2MC_MEMBER_TABLE` row (239.1.2.3, PortChannel8) were written through
ProducerStateTable. This was done under the lock, with the parent session's
OK for the two cages' rows, and restored after.

| Run | Chip group (`mc show`) | Removal |
|---|---|---|
| A: made with ce24 distributing, then ce25 enabled | ce24 only, before and after | refused ITEM_NOT_FOUND, retried ~5/s |
| A, ce25 disabled again (the create-time set) | empty: the first attempt had taken ce24 out | still refused |
| A, ce24 disabled too (none distributing) | | went through: entry and group gone |
| B: made with both distributing | ce24, ce25 | clean |
| C: made with both, ce25 then disabled | ce24, ce25 (ce25 kept) | clean; the group went with ce25 in it |

So the legacy-TH SAI programs a LAG's L2MC member as one egress per LAG
member distributing when the member is made, and never follows a later
change. Its removal takes one egress per member distributing **at that
time**, and fails at the first port it never added. What this means for a
LAG:

- **Delivery.** A group made while one member distributes sends only through
  that member's port. One made with both keeps sending through a member that
  has stopped.
- **Removal.** Once a port has started distributing since the member was
  made, the member cannot be removed. The SAI's own object stays in
  sairedis after the refusal, so tolerating ITEM_NOT_FOUND in the orch would
  not help: the group could then never be removed either.

PortChannel1's members made after 13:48 (the learned groups, from 14:08)
were removed cleanly all afternoon, after their entry as well. This is the
first time the uplink has been an aggregate: no earlier syslog on the switch
has one `mcast egress remove failed`.

**Does anything leak?** Not per removal: the 159 failures are the same seven
groups each time, and the ASIC_DB and chip counts did not grow. The seven
stranded groups and their members stay in ASIC_DB until a swss restart or a
reboot. The test rig left nothing: CONFIG_DB was identical in all 39
tables, and APPL_DB's and ASIC_DB's L2MC keys were the same before and
after.

**The uplink's own snooping state.** PortChannel1 is still the router port
(`show igmp-snooping vlan 4094`, l2mcd's `mrtportr PortChannel1`), and it
kept that role through the test. Its reserved-group membership is gone, for
a separate reason found on the way:

- l2mcd held PortChannel1's reserved groups as learned (`Group-ADD ...
  static:0`) beside the static rows, since the LAN reports them.
- When the learned membership aged out (14:08:13, 14:13:10, `l2mcd.log`), it
  sent `Group-DEL ... static:0` and did not put the static back.
- Now `l2mcd_ctl -v 4094` shows `ngroups : 0`, and its static list names 5
  of the 31 configured ports. `show igmp-snooping groups vlan 4094` shows 0
  entries.

That is l2mcd's, not the orch's. It is filed as its own item in the register.

#### The fix: sonic-swss 0005

`contrib/sonic-image/series/sonic-swss/0005-*`:

- **A LAG's members are made again around every change of its members.**
  `PortsOrch::doLagMemberTask` takes the LAG's L2MC group members out of the
  chip before a member is added, changes status or is removed
  (`L2mcOrch::detachLagMembers`, while the distributing set is still the
  one they were made with). It makes them again when that entry is done
  (`attachLagMembers`). Records, counts and `ref_count`s stay; the new
  members take the new set, so delivery follows the LAG and every removal
  matches. A member the SAI refuses to take out is kept and logged; one it
  refuses to make again is dropped with its counts.
- **`RemoveL2mcEntry` removes the members before the entry**, as
  `RemoveL2mcEntrys` already did. A refused member then leaves the entry,
  the group and the record together for a later removal, instead of a
  record of an entry the chip no longer has.

`L2mcOrchTest`'s mock SAI now holds a LAG member's egress as the table
above measured it. Three new cases: a LAG gaining a distributing member,
one losing a member and being taken off and back, and a refused member
leaving its entry whole. They fail on the orch without 0005 and pass with
it, with the whole mock suite (the slave container on the build VM).

**The driver.** Nothing in its order causes this: a LAG's distributing set
changes with LACP, not with a push. It needs no change. On an image without
0005, any LACP change on an aggregate with snooped groups can strand those
groups. The uplink's next member flap is the likely one.

**What the next image's install must prove** (the register's install check):

- orchagent carries 0005: `docker exec swss grep -c -a -F "made again for
  its LAG members" /usr/bin/orchagent` is 1.
- After the reboot, ASIC_DB holds an `L2MC_ENTRY` for every reserved group
  and `bcmcmd "ipmc table show"` lists them. With l2mcd's static loss still
  open, that holds only while the LAN keeps reporting the groups.
- `bcmcmd "mc show"` lists ce0 and ce1 in each group PortChannel1 is in.
- A test LAG as above (one member distributing, the member made, the second
  enabled) shows the group on both ports, removes cleanly, and syslog has no
  `mcast egress remove failed` or `rv:-7` line.

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
| Storm control | **none** (corrected 2026-11-14, §11.20): the ECS does not claim storm control (`feature_caps` bit 8 clear), its push carries no `stormctrl` key, and it has no storm table or command; its `ERROR_DISABLE` rows name storm causes, but those rows exist for every cause and show no detector | `PORT_STORM_CONTROL` rate limits in kbps from the UI's percentages, and since 2026-11-14 the storm anomalies from the port's own counters (§11.20) | ours does what the UI says Storm Control does (restrict, and alert past the threshold). keep. |
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

### 10.10 The optic's fault flags: `sfp_rxfault` and `sfp_txfault` (2026-11-14)

An audit on 2026-11-14 found that the driver never filled
`Port.Health.OpticRxAlarm` / `OpticTxAlarm`, which `internal/device` sends
as `sfp_rxfault` / `sfp_txfault` on a cage with a module seated. So the
controller could never raise its SFP alert for a DX010 port. It now reads
the module's own flags.

**What a UniFi switch means by the flags.** The flag is the module's loss
of receive signal. Three sources agree:

- **The informs.** In the ECS Aggregation's informs
  (`controller-10.6.106/` and `controller-10.6.109/
  inform-ecs-aggregation-uswf066.json`), `sfp_rxfault` is true only on
  down optical ports whose `sfp_rxpower` is `"0.00"` (no reading). A down
  port that receives light (`-1.45` dBm, port 16) has no fault. So do the
  in-range up ports, the DACs (49-54) and an optic without readings (port
  7). The USW Aggregation's inform (`controller-11.0.99/
  inform-usw-aggregation-usl8a.json`, UniFi's own firmware) shows the same
  on its port 7. `sfp_txfault` is false on every port of every capture.
- **The ECS's own STATE_DB** (read-only, 2026-11-14 23:48 UTC, firmware
  4.0.3). Its 202106-era xcvrd keeps `TRANSCEIVER_STATUS|<port>`
  `rx_los` / `tx_fault` / `status` per cage. The ports with a module
  (`status` 1) and `rx_los` 1 were exactly Ethernet10, 11, 14 and 26.
  Those are UniFi ports 11, 12, 15 and 27, the four ports whose
  `sfp_rxfault` the controller held as true at the same moment
  (`stat/device`). Ethernet15 (port 16: down, light received) had `rx_los`
  0. The ECS keeps no `TRANSCEIVER_DOM_THRESHOLD` table, and its alarm
  thresholds sit in `TRANSCEIVER_DOM_SENSOR` beside the readings, so it
  could not be comparing readings to thresholds for the flag. Its empty
  cages read `status` 0 with `rx_los` 1 and `tx_fault` 1, yet the inform
  sends false for them: the flags count only with a module present.
- **The controller's alerts**
  (`controller-11.0.99/system-log-device-alert-sfp-rx.json`, the v2
  `system-log/device-alert` reduced to the SFP alerts of the last 30 days,
  scrubbed). Every one is `SFP_RECEIVED_FAULT`, "Loss of receive signal
  detected on SFP inserted into {DEVICE_WITH_PORT}." (title "Receive SFP
  Signal Loss", severity HIGH). They fired on the ECS's ports 11, 12, 15
  and 27, the USW Aggregation's 5 and 7 and the US-16-XG's 1, 2 and 5. A
  standing fault is raised again about once a day. No transmit-fault alert
  is in the log, so its key and text are **not captured**.

Neither the link nor the readings decide the flag. A down port with light
has none. Whether a reading below the module's low alarm threshold with
LOS clear would raise one is not captured: every faulted port in the
captures had both. The Arista driver reads the module's own RX_LOS and
TX_FAULT too since 2026-11-15 (EOS prints them in `phy detail`;
[`arista-eos.md`](arista-eos.md) §4), keeping its old threshold rule only
for the SSH transport.

**What the DX010 keeps** (read-only, 2026-11-14, `igmp-202511.20261114-42cccc87a`;
`docs/fixtures/sonic-igmp-202511.20261114-42cccc87a/collect-optic.txt`).
xcvrd (sonic-platform-daemons `d74bc9b`, May 2025, in both 202505 and
202511) splits a module's state over several STATE_DB tables, all keyed by
the cage's first interface:

| Table | What it holds on the uplink's 100G CWDM4 |
|---|---|
| `TRANSCEIVER_STATUS_FLAG` | `rx1los`..`rx4los`, `tx1fault`..`tx4fault`, each `"False"`; `last_update_time` |
| `TRANSCEIVER_STATUS_FLAG_CHANGE_COUNT` / `_SET_TIME` / `_CLEAR_TIME` | per flag: `0` / `never` / `never` |
| `TRANSCEIVER_STATUS` | `tx<k>disable`, `tx_disabled_channel` |
| `TRANSCEIVER_STATUS_SW` | `cmis_state` (`READY`; `REMOVED` on the 31 empty cages), `status`, `error` `N/A` |
| `TRANSCEIVER_DOM_SENSOR` | temperature, voltage, per-channel bias and powers (rx -3.35 .. -2.65 dBm, alarm floor -10.0) |
| `TRANSCEIVER_DOM_THRESHOLD` | the module's alarm and warning thresholds |

There is no `TRANSCEIVER_DOM_FLAG` (the threshold-crossing flags) for this
module. That table is written only from an API's `get_transceiver_dom_flags`,
and the SFF-8636 API in sonic-platform-common 202511 does not implement it.
The flags come from `get_transceiver_status_flags`
(`sonic_xcvr/api/public/sff8636.py`, `sff8472.py`, `cmis.py`). On SFF-8636
these are the module's latched indicators: page 0 byte 3 (Rx LOS, a bit
per channel) and byte 4 (Tx fault). Each is written as the string `"True"`
or `"False"`. It is `"N/A"` where the module does not implement the flag
(`get_rx_los_support` / `get_tx_fault_support`). xcvrd's DOM thread
(`dom/dom_mgr.py`, `DOM_INFO_UPDATE_PERIOD_SECS = 60`) re-reads them every
60 s, and again shortly after a link change. Reading clears the latch. A
fault that lasts shows on every read. One that cleared inside a minute
shows once. The capture's `last_update_time` was 21 s old. The image's `config --help` texts were captured beside it (`config-help.txt`, the same 28 commands as the 20261106 build's; only click's option order differs), since the contract tests start the driver on every capture.

**The read.** `collect.py` prints `xcvr_flag` (STATE_DB
`TRANSCEIVER_STATUS_FLAG|*`), and `optic_fault.go` maps it:

- A cage's `OpticRxAlarm` is true when any channel's `rx<k>los` is
  `"True"`, and `OpticTxAlarm` when any `tx<k>fault` is. `"False"`, `"N/A"`
  and a missing channel are no fault.
- Only a cage with a module seated reads its flags (`TRANSCEIVER_INFO`, the
  same test as `sfp_found`), so an empty cage never carries a fault.
- Nothing is inferred from the link or from the readings.
- **A split cage's lanes take the channels they run on, in order.** With
  four channels, a 4x25G lane gets one each and a 2x50G lane two. The
  cage itself (the parent's port) gets any channel's fault. xcvrd keys the
  flags by the first subport only. The lane order is the SFF breakout
  convention (subport k on the module's host lane k). It is not verified
  on a split module, since the DX010 has no breakout cable. A module with
  fewer channels than lanes gives every lane all its channels.

Tests: `optic_fault_test.go`. The healthy state (no fault anywhere, the
CWDM4 present) is the capture as taken. No capture holds a fault: the one
module cannot be made to fault, and upstream's own test data (sonic-utilities'
`tests/mock_tables/state_db.json`, sonic-platform-common's
`test_sff8636.py`) holds only `False`. So the fault cases are the capture's
own entry with one field set to `"True"` (LOS on a channel, a transmit
fault, both), every field `"N/A"`, the entry copied onto an empty cage,
and the 4x25G capture's split cage given the uplink module's entries with
one channel's flags set. The wire side (`sfp_rxfault` / `sfp_txfault` only
with a module seated) is `internal/device/anomalies_test.go` and the
contract tests. Live: a `-collect-once` from the branch against the DX010
(2026-11-14 23:58 UTC) read port 1 present (`100G CWDM4`) with no fault
and no fault on any other port. A real fault, the controller's alert for
a DX010 port and a split module are **untested**.

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
| STP, RSTP (`feature_caps` 64; `stp_caps` BPDU_GUARD 1, AUTO_EDGE 2) | yes, `stp_caps` 3 | yes since 2026-11-01 through disunified-l2d (§11.5), `stp_caps` 11 | `switch.stp.status/version/priority`; `switch.port.N.stp.port_mode/edge/bpdu_guard` |
| Loop protection (`KEEPALIVE_LOOP_PROTECTION` 1048576) | yes | yes since 2026-11-01 (§11.5) | `switch.port.N.ld_mode`, `switch.port.N.keepalive` |
| IGMP snooping (256; `IGMP_SNOOPING_ADVANCED` 67108864; `igmp_snoop_caps` 31) | yes | with `options.igmp: on` on the l2mcd image (§8.14), off until H4 | `switch.vlan.N.igmp_*` (snooping, querier, fast leave, membership, max response, mrouter expiry, suppression), `switch.vlan.N.forward_unknown_multicast`, `switch.multicast.*` |
| DHCP snooping (128) | yes | yes since the 2026-11-01 build (§11.7.6) | `switch.dhcp_snoop.status`, and with it on `switch.dhcp_guard.N.server/vlan/status` (captured 2026-11-01, §11.7) |
| Port isolation (4), port security (512) | yes | yes since 2026-11-07 (§11.12) | `switch.port.N.port-security` (isolation keys only when set) |
| LLDP-MED (16) | yes | yes since 2026-11-07 (§11.12) | `switch.port.N.lldpmed.opmode/topology_notify` |
| Egress rate limit (32) | yes | yes since 2026-11-07 (§11.12) | none while unset |
| L3 routing (1), BGP (33554432), `max_l3_intf` 64, `max_static_routes` 256 | yes | L3 routing, 64/256, and BGP since 2026-11-07 (§11.13, §11.14) | `switch.routing_enabled`, and `switch.routing_interface.N.*` once a network is routed on the switch (captured 2026-11-07, §11.13); `switch.bgp.1.*` once a BGP router names the switch (§11.14) |
| OSPF (`feature_caps2` 4) | no (`feature_caps2` 128) | yes since 2026-11-07 (§11.14) | `switch.ospf.*`, and `switch.routing_interface.N.ospf.*` for each routed network in an area (captured 2026-11-07, §11.14) |
| Custom ACLs (`max_custom_ip_acls`/`mac_acls`/`global_acls` 512; no feature bit) | yes | IP 128 since the 2026-11-01 build (§11.7.6); MAC 128 since 2026-11-06 on the image with the kit's sonic-swss 0003 (§11.7.8) | none while no rule exists; `switch.acl.mac.N.*`, `switch.acl.ip.N.*` once one does (captured 2026-11-01, §11.7) |
| Routed ports and aggregates (`feature_caps2` 16, `PORT_ROUTING`) | no | yes since 2026-11-08 (§11.16) | `switch.routing_interface.N.type=port` / `=lag` with `port` / `lag` (captured 2026-11-08, §11.16) |
| VRRP (268435456) | no | no: offered only as an MC-LAG group's L3 Automatic Failover (§11.16) | `switch.routing_interface.N.vrrp.*` from the MC-LAG group's context |
| 802.1X (`feature_caps` 2) | no | yes since 2026-11-15 where l2d can run (§11.26) | the 802.1X keys captured on 11.0.99 (§11.26) |
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
SLAAC and link-local; the DX010's Vlan4094 had only its link-local until
§11.22 gave it SLAAC).
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
  (RSTP) port states; a port-state change is one write per VLAN. But a
  state StpOrch takes after the VLAN went makes the instance again, with no
  VLAN, and only a VLAN leaving an instance removes one: a gone VLAN's port
  keys go out through the producer and l2d removes an instance no VLAN uses
  (§11.35).
- A port-state key naming a port orchagent does not know makes
  `doStpPortStateTask` `return` without erasing it: that table's queue stops
  for good. l2d writes only ports present in APPL_DB `PORT_TABLE`/`LAG_TABLE`
  and ready in STATE_DB, never `dummy`, and drops the keys of a vanished port
  from APPL_DB directly (a plain `Table` delete, which orchagent never sees:
  it already removed the objects with the bridge port). A vanished VLAN's
  keys go through the producer since §11.35.
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
| `VLAN_ID` on the native stand-in | UDP 67 → switch:68 from a host that is not the gateway; control 66 → 68 | The 67s never reached the CPU, the 66s did: **`VLAN_ID 4094` matches untagged frames** of the stand-in (the field processor qualifies after VLAN assignment), and the guard drops an untrusted server's replies. (On 202505's libsaibcm `13.2.1.36`, with no 67 sent without the guard. On 202511's `13.2.1.100` the CPU gets a copy of a dropped frame to the switch, and the chip's counter is the evidence: §11.7.8, 2026-11-15.) |
| Guard drop vs ACL forward | Forged 192.0.2.10:67 → switch:68 with the guard alone, then with a `DUI_IP_ACL` Allow (UDP from 192.0.2.0/24 to any port 68) Active; control from port 66 | Dropped both times; the 66s arrived both times: **the guard's DROP wins** over a FORWARD in the other table, as expected. A UniFi Allow cannot open a hole in DHCP guarding. (Read at the CPU, on the 202505 SAI: see the note in the row above.) |
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
  MAC's included, through the to-CPU traps' copy (traced below). The
  broadcast frames reached the CPU from neither MAC, so they show nothing
  either way.
- **Not observed: a dropped switched frame.** That is the case UniFi's MAC
  rules are for ("traffic between devices on the same VLAN"), and seeing
  it needs a second port with a link. Every other port of the DX010 is
  down, so the check stops at the hit counter.
- **Afterwards.** The timer removed the table (no `MACTEST` key in any DB,
  no ACL table or entry in ASIC_DB). Ethernet0 stayed up and the bridge
  informed every minute throughout.

**Why a drop does not keep a frame from the CPU** (traced 2026-11-15 on
`igmp-202511.20261115-6c8eb1cf7`, libsaibcm `13.2.1.100` legacy-TH).
§11.7.7 had seen the guard's L3 DROP keep forged replies from the CPU,
and this check saw the L2 DROP not do so. The trace shows the two tables
land in the chip the same way. On this SAI no ACL drop keeps a frame
addressed to the switch from its CPU. The §11.7.7 result came from the
older SAI, with no baseline.

- **The traps** are one ingress field-processor group, GID 1, at group
  priority -1. That group's TCAM resolves to one entry per packet, the
  highest-priority match. Its entries for traffic to the switch itself:

  | Entry | Match | Actions | Prio | Stat |
  |---|---|---|---|---|
  | ip2me (EID 0x0f) | `MyStationHit`, `DstClassL3` 1 | `RpDrop`, `GpCopyToCpu`, CPU queue 1 | 1 | 9 |
  | DHCP to the switch (EIDs 0x12, 0x13) | the same plus UDP, `L4DstPort` 67 or 68 | `RpDrop`, `GpCopyToCpu`, CPU queue 3 | 3 | 10 (all six DHCP entries) |
  | DHCP broadcast (EIDs 0x10, 0x11) | broadcast `DstMac`, UDP, `L4DstPort` 67 or 68 | `UnmodifiedPacketRedirectPort` to the CPU, `RpDrop`, `GpCopyToCpu`, CPU queue 3 | 3 | 10 |
  | ARP (EIDs 0x1c-0x1f) | ARP to broadcast or to the switch | `GpCopyToCpu`, `YpCopyToCpu`, queue 4 | 4 | 12 |

  `GpCopyToCpu` copies green packets to the CPU. `RpDrop` drops only what
  the CoPP policer has marked red. CoPP (`copp_cfg.json` plus
  CONFIG_DB) gives `ip2me` and `dhcp,dhcpv6` the action `trap`. On this
  SAI, `trap` for a unicast frame to the switch is that copy-only entry.
  The redirect appears only on the broadcast entries.
- **The ACL tables.** Each orchagent table is its own ingress group. Every
  table's member priority is 100 (`portsorch.cpp`
  `SAI_ACL_TABLE_GROUP_MEMBER_ATTR_PRIORITY`), and the chip shows
  `group_priority= 100`. Two throwaway tables were bound to Ethernet0 at
  once: `TRACEL3`, type `L3`, and `TRACEL2`, type `L2`.
  - Each rule's entry has the single action `Drop`, all colours.
  - `TRACEL3` landed in GID 4 on slice 3, `TRACEL2` in GID 5 on slice 2.
  - The MACTEST table above landed the same way: GID 4, slice 2,
    priority 100, `Drop`.
  - So the stage, the group priority and the drop encoding are the same
    for both table types.
- **The cross** (2026-11-15 18:16-18:23 UTC, under the test lock with a
  300 s timed removal). Frames from a LAN host to the switch's MAC and
  address, three of each:

  | Frames | `TRACEL3` R1 (DROP UDP from 192.0.2.10, port 67) | `TRACEL2` R1 (DROP src 02:00:00:00:00:01, VLAN 4094) | Trap path | On Ethernet0 and Vlan4094 |
  |---|---|---|---|---|
  | 192.0.2.10:67 -> :68 | hit | -- | DHCP | **3 of 3** |
  | 192.0.2.10:67 -> :9 | hit | -- | ip2me | **3 of 3** |
  | from 02:..:01, :66 -> :68 | -- | hit | DHCP | **3 of 3** |
  | from 02:..:01, :66 -> :9 | -- | hit | ip2me | **3 of 3** |
  | controls, :66 -> :68 and -> :9 | -- | -- | both | 6 of 6 |

  - The two drop entries counted 6 each, exactly their frames.
  - The DHCP traps' stat 10 counted the dropped DHCP frames too. One
    entry per group hits, so the trap fired on frames the ACL group
    dropped.
  - A repeat with the guard's own shape gave the same result. That rule
    was a DROP at 8000 with `VLAN_ID 4094`, `IP_PROTOCOL 17`,
    `L4_SRC_PORT 67`, `L4_DST_PORT 68` and the forged source. Its entry
    counted 3, and 3 of 3 reached the CPU.
  - The guard's `VLAN_ID` on the stand-in's untagged frames is therefore
    confirmed by the chip's own counter on this image.
- **How the chip resolves it.** A drop in a group at priority 100 removes
  the forwarding copy (the switched or routed packet, or the redirect).
  It leaves the trap group's `GpCopyToCpu` alone, whichever table type
  made the drop. Counters agree: what the drop entry counts, the trap's
  stat counts too, and the CPU sees it.
- **§11.7.7 again.** That run was on 202505.1238526 with libsaibcm
  `13.2.1.36`. A port-67 reply to the switch did not reach the CPU under
  the guard, while port 66 did. No one sent a port-67 reply without the
  guard, and that SAI's field processor was never read. So it is open
  whether `13.2.1.36` encoded the drop or the trap differently, or
  whether something else dropped those frames. That image is no longer
  installed (only 202511 images are, `sonic-installer list`).
  - Every guard statement since then rests on the chip's counters:
    VLAN_ID, the per-port count (§11.7.9) and today's repeat.
  - The ARP and DHCP traps' policers (`RpDrop`) are unrelated: three
    frames stay green.
- **The guard still guards.** A rogue reply switched to a client is dropped
  in the chip. Its CPU copy dies at the kernel bridge: `ebtables` FORWARD
  drops `-d Multicast` (broadcast included) and ARP, so nothing re-floods
  a trapped frame in software. The switch's own DHCP use is untouched:
  - its address is static, so it runs no DHCP client;
  - the relay and udhcpd listen on 67, and the guard matches only 67 -> 68.
- **What follows for the driver.**
  - **No chip ACL rule protects the switch's own address on this SAI**, a
    UniFi IP Block included. A Block to the switch's address or MAC drops
    the routed or switched copy, but the ip2me copy still reaches the CPU.
  - **The ECS's rules do protect its own address**, because that address
    is a switched host behind an internal port, not a trap (§11.32.1). So
    since 2026-11-15 the driver drops the CPU's copy in the kernel: its L3
    tables (`DUI_DHCP_GUARD`, `DUI_IP_ACL`) are mirrored into iptables'
    `raw` table for packets arriving on a front-panel interface (§11.32.3).
  - The management guards (`cutsManagementMAC` and its IP twin) are what
    keeps a Block from cutting the bridge off, now that the mirror makes
    such a Block bite.
  - **SAI `DENY`** (drop plus copy cancel) is the chip-side alternative.
    `DENY` is not in orchagent's ACL action lookup, which has only FORWARD,
    DROP and COPY (`aclPacketActionLookup`), and STATE_DB's
    `ACL_ACTION|PACKET_ACTION COPY,DROP,FORWARD` lists only those. Whether
    `13.2.1.100` would take `DENY` on an ACL entry is not known: an image
    change was not needed (§11.32.2), so it was not probed.
  - **MAC rules are not mirrored.** UniFi's MAC rules are for traffic
    between devices on one VLAN, which the chip drops, and the UI refuses
    the management network for a MAC rule, the only network the ECS and
    the DX010 have an address on (§11.32.1).

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
| 2 | VLANs on cage 2: native kids; Allow All on a non-Default native; Allow All on Default; Block All; Custom kids+visitor; back | `config vlan add 2`, membership moves (`del 4094`, `add -u 2`); Allow All with a non-Default native logged as the stand-in's limit (VLAN 1 tagged cannot be expressed) and left the port; Allow All on Default created the nine other site VLANs (`config vlan add -m ...`) and tagged them; Block All removed them; Custom `add -m 2,69`; each 3-7 s. l2d gave each new VLAN its own STP instance | pass (the ten site VLANs stay on the switch; the driver never deleted a VLAN until §11.29) |
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
- **A routed network's VLAN takes the ports' MTU** (2026-11-15), as every
  VLAN the driver manages does (§12): made at it (`config vlan add`, then
  `hset 'VLAN|Vlan<id>' mtu`, as `EnsureVLANs` makes a pushed VLAN) and
  kept there by the port plan's `vlanMTUs` (the CONFIG_DB field and the
  netdev). Until then `vlanMTUs` covered the push's `switch.vlan.*` list and
  the native stand-in only, and a routed network no port carries is in
  `switch.routing_interface.N.vlan` alone (§11.29.1). So its VLAN stayed at
  SONiC's 9100 while every port and every other VLAN followed the site's
  jumbo setting. The deployed driver's own state of the 2026-11-07 OSPF run
  shows it (`collect-ospf.txt`, jumbo off): `VLAN|Vlan3999` without `mtu`
  and its netdev at 9100, beside `VLAN|Vlan4040` (the transit, which the
  list names) at 1500 in both, every port at 1500. That is why the run's
  OSPF peer had to run at 9100 (§11.14): ospfd sends the interface MTU in
  its database descriptions and a peer at another MTU stalls in ExStart.
  While no port carries the VLAN, no frame on a front-panel port is
  affected (a port that carries it puts it in the list): what saw 9100 was
  the switch's own stack on the interface and whatever reached it past the
  list, as the runs' test hosts on the kernel `Bridge` did. The fault was never shown with a fresh routed network; the
  fixed plan is replayed over that capture (`TestReplaySonicDX010OSPF`'s
  first push writes both halves for Vlan3999, the cycle after nothing) and
  over the L3 run's creation (`TestReplaySonicDX010L3`: the hset right after
  `config vlan add 3999`, once). Its commands are the ones already run live
  on memberless VLANs (§11.29.6's Vlan4010, §12's Vlan4094).
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
  leaving the push takes only what the driver made for it. The VLAN goes
  once no push names it (§11.29; until 2026-11-15 the driver never
  deleted a VLAN).

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
ECS) keeps the decoded file in a CONFIG_DB table of its own
(`BGP_FULL_CONFIG|global config_b64`); community SONiC
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
only, MTU 9100 to match Vlan3999 (OSPF compares MTUs; Vlan3999 was at
SONiC's 9100 behind 1500-byte ports, fixed 2026-11-15, §11.13), the same MD5 key --
reached **Full**. The DX010 learnt the peer's 192.0.2.0/24 as an E2 route
into the kernel, APPL_DB and the ASIC; the peer learnt the DX010's default
route (announced) and its connected networks (E2). The cycles after wrote
nothing. ospfd killed by hand was started again by the next reconcile
(Full again within a minute); a cost changed in the controller restarted
it with the new file (cost 20 read back); the router deleted stopped ospfd
and removed the file, and the learnt route left the kernel and APPL_DB.
Then the peer, the test network (its transit went with it) and VLANs 3999
and 4040 were removed (the VLANs by hand: the driver deleted none before
§11.29);
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
- **Network 11.0.99, re-captured 2026-11-15** (Routing + Aggregating on
  empty cages 15 and 16 from the port editor, then removed;
  `controller-11.0.99/system_cfg-dx010-routed-agg-{1-aggregate,2-removed}.txt`,
  the push before and after being `system_cfg-dx010-dot1x-8-off.txt`):
  **unchanged.** Against 10.6.109's, key by key, the routing interface is
  the same nine keys (`id`, `name`, `type=lag`, `opmode manual`, `ip`,
  `mask`, `default_route`, `purpose`, `lag=<first member>`), the first
  member is still `opmode=aggregate`, `lag=1`, with no VLAN, the second
  member is still in no key, and no DHCP key comes with it. The controller
  record's override lists both (`aggregate_members [15,16]`, `lag_idx 1`),
  and its port table shows port 16 `aggregated_by` 15. The removal renames
  the second member to the controller's default (`QSFP28 16`) until the
  operator's name is put back. The deployed bridge (f3d3572) built
  PortChannel1 with Ethernet56 alone, as replayed in
  `replay_sonic_routed_net11_test.go`, and took it down again in the same
  order as on 10.6.109. The last push wrote nothing.
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

**The same run again through the deployed bridge (2026-11-15 17:43-17:47
UTC, dx010-live-runs, under the test lock)** showed the same push and the
same writes (replayed in `TestReplaySonicDX010Network11`), and two things
beside them. **Vlan4040 stays behind** when the controller removes the
transit with the last routed network: `EnsureVLANs` only creates, so the
VLAN, with no member and no address left, was deleted by hand as after the
2026-11-07 and 2026-11-08 runs (`config vlan del 4040`). And leaving
Routing + Aggregating, the port editor edits both former members at once
with the first's settings (Tagged VLAN Management back to Allow All); both
overrides were put back on their pre-test copy through REST. (Since §11.29
the driver deletes Vlan4040 itself once the push no longer names it.)

**Not proven live:** traffic over a routed port, since the test cages have
no link (rack day). The subnet's connected route, which SONiC installs when
the port comes up. An OSPF adjacency or a static route over a routed port.
A routed aggregate of more than one member: the controller pushes only the
first (10.6.109 and 11.0.99).

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

**Clint's decision (2026-11-14):** the bit stays a detection, with no port
hold, and the two numbers stay: 300 is the alert's own text, and 600 is the
CoPP cap that lets a count pass it. A flood test on a port other than the
uplink, and a CPU reading of l2d at the full 600/s, are not planned (no
other cage is cabled; a loopback cage would loop the VLAN).

**Through the deployed build (2026-11-15 17:33 UTC, main f3d3572 on
igmp-202511.20261115-6c8eb1cf7, Network 11.0.99, under the test lock).**
The same burst as above, 400 BOOTREQUESTs a second for 5 s from a LAN host
to the switch's own address, with the installed l2d running the detector as
a service (its `queue3_group1` and `dhcp_relay` CoPP rows in CONFIG_DB
before the burst): l2d logged `dhcp rate: Ethernet0 received 401 DHCP
requests in one second (threshold 300)`, STATE_DB
`DUI_DHCP_RATE_TABLE|Ethernet0` read `over 397`, `peak 401`, and the first
inform after it carried bit 32 on port 1. The controller raised
`PORT_DHCP_STORM` at that inform's time, "Incoming DHCP packets exceed
300/sec on <switch> Port 1." (severity HIGH, title "Excessive DHCP Packets
Detected"; read from `system-log/device-alert`), and port 1's stats drawer
showed Broadcasts & Discovery "Degraded -- Excessive DHCP Packets Detected"
with the alert in its activity log. The next inform, with the count no
longer growing, carried no bit. Nothing to restore: the CoPP rows are l2d's
standing ones. One finding on the way: the record dir's `payload-last.json`
showed `anomalies 0` for the very inform that raised the alert. The loop
built the payload a second time for that file after `EncodeInform` had
moved the per-port history forward, so the copy never showed a growth bit
(or a byte rate); it is written from the encoded payload now
(`Session.LastPayload`, `TestLastPayloadKeepsTheSentAnomalies`). The wire
was right throughout.

**Not exercised:** a flood on a port other than the uplink (Clint's limit
above).

### 11.18 Switch-detected anomalies: PHY failure, MAC locking, 802.1X, DoS (2026-11-14)

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

**What the ECS does.** It reports none of them. The ECS Aggregation's
inform, printed on the switch (`controller-10.6.109/inform-ecs-aggregation-uswf066.json`),
has no `anomalies` key on any of its 54 ports, and none either when it was
printed again with `mca-dump` on 2026-11-14 (firmware 4.0.1.995, read-only,
once the switch was back on 4.0.1; on 4.0.3 its `admin` login had refused
the site key earlier that day). Its error-disable table names a cause for
most of the bits (§11.20); what the ECS shows for PHY failure and for DoS
is in §11.18.1 and §11.18.4.

**The evidence**, read on the DX010 on 2026-11-14 (image
`igmp-202511.20261106-f2e25f54c`, read-only):
`docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/anomaly-probes.txt`.

| Bit | What the DX010 can do | Built |
|---|---|---|
| 2048 PHY failure | no PHY between the ASIC and the cages, as on the ECS, whose SONiC describes none either; the per-port fault the DX010 does report is a link-down reason seen on the healthy uplink (§11.18.1) | nothing |
| 256 MAC locking | enforces the MAC filter (§11.12); since 2026-11-14 a secured port's unknown sources go to the CPU (`cpu_trap`, as on the ECS), where disunified-l2d counts them per port; the port stays up (§11.18.2) | built: `Health.MACLockingDrops`, verified by hand on an empty cage |
| 128 802.1X authentication failed | no authenticator on the image; 202511 holds Broadcom's Port Access Control sources, unbuilt and without an orchagent consumer (§11.18.3) | built 2026-11-15: hostapd under l2d, `Health.Dot1XFailures` (§11.26) |
| 1024 DoS | the ECS's `denial-of-service` cause is a configuration row with nothing observable behind it (no detector in any database, the chip or the CPU path, no push key, no UI setting, no bit reported), so there is no condition to copy; the Tomahawk's own DoS header checks could be counted per port, but outside SAI and only as a condition of this project's choosing (§11.18.4) | nothing |

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

**The module's faults are their own fields.** xcvrd's
`TRANSCEIVER_STATUS_SW` carries an `error` field (`N/A` on every cage) and
`TRANSCEIVER_STATUS_FLAG` the module's loss-of-signal and transmit-fault
flags. The bridge sends those as `sfp_rxfault`/`sfp_txfault`, and the
controller derives its own SFP alerts from them (`custom_anomalies` 1/2/4,
`docs/feature-map.md` §2). This paragraph first said they were already
sent; the driver never filled them until §10.10 (2026-11-14). A module
fault is not this bit.

**The ECS gives the bit no observable meaning either** (read-only,
2026-11-14, firmware 4.0.1.995). Its error-disable table has no cause for a
physical failure (§11.20.2). Its SONiC describes no external PHY: the SKU
directory holds the Marvell board, SAI and profile files and no
`gearbox_config.json`, and APPL_DB's only gearbox key is the done marker
(`_GEARBOX_TABLE:GearboxConfigDone`). Its 202106 orchagent keeps no
`PORT_OPERR_TABLE`. STATE_DB `PORT_TABLE|<port>` carries `state`,
`netdev_oper_status`, `linkdown_count`, `supported_speeds` and
`switching_mode`, and `state` is `ok` on all 56 ports, up or down (it marks
the port created, not healthy). The inform has no `anomalies` key, and a
down cage is reported only through `up`, `link_down_count` and the
module's `sfp_found`/`sfp_rxfault`/`sfp_txfault` (4 of its 41 down ports
carry `sfp_rxfault` true), from which the controller derives its own SFP alerts.
So on the family the DX010 is claimed as, "cannot link up due to a physical
failure" is not a state anything reports; the bit most likely belongs to
switches with PHY chips of their own (not captured: no switch on the site
reports it).

**Built: nothing.** What a PR needs: an inform from a real UniFi switch
reporting 2048 (a support-bundle `last.inform` or `mca-dump` on the
switch) and what had failed on it, so the condition is known; then a
signal on the DX010 for a port the ASIC cannot bring up whatever the far
end does. None was found in STATE_DB, APPL_DB or the SAI's port
notifications.

#### 11.18.2 MAC locking (256): built (2026-11-14)

**The condition is the MAC Address Filter's.** "Unauthorized MAC Detected"
is a source MAC on the port that is not on the port's allowed list: UniFi's
port security, pushed as `switch.port.N.port-security=enabled` and
`port-security.mac.K` (§11.12; the push for a test port is captured in
`controller-10.6.109/system_cfg-dx010-portfeat-*.txt`).

**What the ECS does** (the ECS Aggregation on SONiC 4.0.1, read-only on
2026-11-14; no port there is secured, so no violation could be watched
without changing a production switch):

- Its error-disable configuration lists the cause:
  `ERROR_DISABLE|mac-locking` in CONFIG_DB and APPL_DB, `admin_status
  enabled`, `auto_recovery_interval 0`, beside bcast-storm, mcast-storm,
  ucast-storm, dhcp-rate-limit, denial-of-service, dot1x, keepalive,
  bpduguard, bpdustorm and mclag-delay-restore; `show errdisable recovery`
  prints the same list, `show errdisable status` an empty table (the model
  of those rows: §11.20). No port has been seen disabled for this cause.
- A secured port there is `learn_mode cpu_trap` and Ubiquiti's software
  learning (§11.12): unknown sources go to the CPU, only the listed ones
  are learnt, the kernel bridge drops the rest. `systemctl` lists
  `port-securityd.service` ("Port Security daemon"), the swss container runs
  `swlearnd`, and `macsnoopingd` holds a packet socket on the kernel `Bridge`
  (`/proc/net/packet`, protocol `ETH_P_ALL`).
- The ECS's inform printed on the switch has no `anomalies` key
  (`controller-10.6.109/inform-ecs-aggregation-uswf066.json`). The rows are
  written for every cause, including ones the ECS does not even claim
  (storm control, §11.20), so they do not show a detector; what a violation
  does there is not observed.

**What the DX010 does now.** The driver still secures a port with
`learn_mode drop` and one static FDB entry per allowed MAC (§11.12): the
chip forwards the listed sources and drops the rest unseen, from boot. With
the port in l2d's config (`l2.json` `mac_lock`: bridge port -> allowed
MACs), `disunified-l2d`'s `MacLockGuard`:

1. writes the kernel bridge's chain `DUI_MACLOCK` (jumped to from INPUT and
   FORWARD): per secured port, `-i <port> -s <mac> -j RETURN` for each
   allowed MAC, then `-i <port> -j DROP`; every write is one
   `ebtables-restore --noflush` transaction (§11.26.12);
2. only then sets the port's learning to `cpu_trap` in APPL_DB
   (`PORT_TABLE`, a PortChannel's `LAG_TABLE`, through ProducerStateTable),
   the ECS's mode: an unknown source is still neither learnt nor forwarded
   by the chip, but goes to the CPU and arrives on the port's netdev, where
   the chain drops it;
3. reads the port's netdev (each member's, for a PortChannel) on a packet
   socket, which sees a frame before the bridge does, and counts every
   inbound frame whose source is unicast, not the switch's own MAC and not
   on the list: STATE_DB `DUI_MACLOCK_TABLE|<interface>` `drops` (since
   boot; carried over an l2d restart) and `last_mac`;
4. leaves the port up.

`collect.py` reads the table (`l2_maclock`) and the driver sums a cage's
lanes into `Port.Health.MACLockingDrops`; the device layer sets bit 256
when that count grew since the previous inform, as it does for the storm
and DHCP counts.

CONFIG_DB keeps `drop`: a reboot, a swss restart (APPL_DB rebuilt from
CONFIG_DB) or a stopped l2d leaves the port as closed as before, only
uncounted, and l2d re-asserts `cpu_trap` within one 2 s pass. A port leaving
the config gets its CONFIG_DB learn mode back first, and the chain goes
after; `--teardown` does the same for every port l2d trapped. The counts
stay until the switch reboots.

**Why the port stays up.** The DX010 does what a UniFi switch does, and a
claim must be true. As the UI words the alert for bit 256, a MAC was
detected on the port that is not on the allowed list, and its recommended
action is to check what is connected to the port; nothing says the port was
disabled. Loop protection's alert does say so ("disabled by Loop
Protection", to be re-enabled by hand). So the DX010 keeps dropping the
source, as the ECS's mode does, and reports the detection. The ECS's own
outcome on a violation is the open point: its `ERROR_DISABLE|mac-locking`
row exists, but whether a violation disables a port there has not been seen
(untested; no port there was secured). The `mac-locking` Fault cause stays
in the device layer for a device that does shut the port.

**What the count costs the CPU, and its bound.** The trapped frames come
through the default CoPP group (`COPP_TABLE:default`: queue 0, `cir 600`,
`cbs 600` packets), one policer for the whole switch, so under a flood l2d
reads at most about 600 frames a second in all, at most 256 per socket per
turn of its loop; the database is written once per 2 s pass, and a log line
for one port and source at most every 10 minutes. The count is then a lower
bound (the rest are dropped by the policer, which names no port), which is
enough: the bit follows growth, not the number.

**Live: the CPU path** (cage 9 in MAC loopback, 2026-11-14,
`sonic-igmp-202511.20261106-f2e25f54c/maclock-live.txt`, `learn_probe`):
under `drop` no frame from an unknown source reached the CPU; under
`cpu_trap` 20 of 20 did; a burst of 20,000 frames at about 22,000 frames/s
delivered 626 to the netdev, and the chip counted 1,549 to CPU queue 0 and
18,471 dropped there. The chip's learn flag (`bcmcmd ps`, `lrn`) followed
APPL_DB on cage 10: `cpu_trap` C, `drop` D, `hardware` FA (`chip_learn`).

**Live: the watch** (2026-11-14, the candidate `MacLockGuard` run by hand
beside the installed l2d, under the test lock; the switch restored and
checked after): cage 10 (Ethernet36), empty, in MAC loopback in a hand-made
tagged VLAN 3997 beside its Vlan4094, the installed l2d's main process
paused so its VLAN mapping did not block the looped port. The chain and
`cpu_trap` were written (chip C); frames from the listed source were not
counted; frames from an unlisted one were, and the port stayed up
(`admin_status up`); unwatched, the port went back to `hardware` and the
chain was removed (`frames_ethernet36` is what the netdev delivered). The
collector's capture during the second run is `collect-maclock.txt`
(`l2_maclock`: 22 on Ethernet36; `TestMacLockingDropsFromCapture`). The
loopback also brought back the LAN's IGMP frames, which snooping's static
groups flood to every port of Vlan4094: under `cpu_trap` they were trapped,
counted and dropped like any unlisted source, and the FDB afterwards held
every LAN address on Ethernet0. While a looped port in Vlan4094 learns in
hardware, the same frames would move LAN addresses onto it: never loop a
port that is not trapping or dropping.

An earlier build of this section held the port down 150 s on a violation
(945d0b4); that was replaced the same day for the reason above.

Not exercised: a host on a cabled port, a secured PortChannel, the
controller's 256 alert (the deployed bridge predates this build).

**Found while testing: a VLAN move can leave a port's bridge port dead.**
Cage 9 was first set in UniFi to a VLAN-only network (VLAN 4000) with the
filter on. The deployed driver moved it (`config vlan member del 4094
Ethernet32 ; config vlan member add -u 4000 Ethernet32`, after deleting the
port's `L2MC_STATIC_MEMBER` rows). orchagent's `removeBridgePort` set the
bridge port admin down, then refused to remove it ("port Ethernet32 still has
references to a Layer 2 multicast group member": IGMP snooping's static
groups; ASIC_DB held no L2MC group member on it by then), and the add reused
the admin-down bridge port: ASIC_DB `SAI_BRIDGE_PORT_ATTR_ADMIN_STATE
false`, the chip's learn flag D whatever learn mode was set. Moving the port
back did not clear it, and orchagent then kept retrying the removal (about
two log lines a second). A swss restart or a reboot clears it. Any native
VLAN change on a port with snooping's static groups is likely to do the
same; the driver's VLAN plan (or the kit's l2mc reference counting) needs a
fix, outside this work. Testing therefore keeps a port's native VLAN and
adds a hand-made tagged one.

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

**Built since (2026-11-15): §11.26**, on the second of two designs
(hostapd's wired driver under disunified-l2d, not `src/sonic-pac`), with no
image change.

#### 11.18.4 DoS (1024)

**The condition.** The UI words the bit as a denial of service attack
detected on the port, and advises checking for misconfigured devices that
send excessive traffic and for attackers. It is a detection: no UI text says
the port is disabled (§11.20.1). The controller configures nothing for it:
no captured push (the 58 in `docs/fixtures/controller-*`, the ECS's among
them, and the ECS's own 818-line `system.cfg` on firmware 4.0.3 in a state
dump of 2026-11-13) carries a key for a DoS detector, and the UI has no
setting for one, only the alert's texts.

**The ECS has a cause by that name and nothing observable behind it**
(read-only, 2026-11-14, firmware 4.0.1.995 live, and 4.0.3 from a state
dump taken on the switch on 2026-11-13; nothing read off the ECS is kept in
the tree, §11.4, so this is stated for a reader with an ECS to check):

- CONFIG_DB `ERROR_DISABLE|denial-of-service` and APPL_DB
  `ERROR_DISABLE:denial-of-service`: `admin_status enabled`,
  `auto_recovery_interval 0`, as every other cause (§11.20.2). The
  error-disable service runs in the `swss` container (`errdisablemgrd`,
  `errdisabled`; Redis channels `ERROR_DISABLE_CHANNEL`,
  `errdisablemgrd_CHANNEL`, `errdisabled_CHANNEL`). No port has been
  disabled (`show errdisable status` empty; no STATE_DB `ERROR_DISABLE`
  row).
- **Nothing configures a detector.** No other key or value in CONFIG_DB,
  APPL_DB, STATE_DB or ASIC_DB names DoS (the 4.0.3 dump's four databases
  searched whole; on 4.0.1 the tables below read one by one). The chip is
  given no DoS check: ASIC_DB's port objects carry only the standard
  attributes plus Ubiquiti's cut-through pair, and the switch object only
  cut-through, pipeline-bypass and the usual notification attributes. The
  CPU path has nine CoPP groups (IGMP/PIM, ip2me, DHCP, BGP/LLDP/OSPF/UDLD,
  ARP/ND, VRRP, LACP/STP, a user-defined group and the default), each a
  switch-wide policer whose drops name no port, and no ACL beyond the
  controller's own MAC and IP rules.
- **Nothing reports it.** The inform the switch builds (`mca-dump`, 4.0.1)
  has no `anomalies` key on any of its 54 ports, as in the committed
  fixture (§11.18).

So what the ECS's `denial-of-service` cause detects cannot be pinned down
from anything the switch, its push or the controller shows: the row names a
cause, and a cause row is not a detector (the ECS's storm rows name a
feature it does not even claim, §11.20.2). There is no ECS behaviour to
copy.

**What the DX010 can measure** (read-only on 2026-11-14,
`docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/dos-probes.txt`):

- **The Tomahawk's own DoS checks.** The Broadcom SDK's switch controls
  (`bcmcmd sc` in `syncd`) include `DosAttackSipEqualDip`,
  `DosAttackTcpFlagsSF`, `DosAttackTcpFlagsFUP`, `DosAttackFlagZeroSeqZero`,
  `DosAttackSynFrag`, `DosAttackV4FirstFrag`, `DosAttackTcpHdrPartial`,
  `DosAttackTcpPortsEqual`, `DosAttackUdpPortsEqual`, `DosAttackIcmp*`,
  `DosAttackIcmpFragments` and others; every one is off except
  `DosAttackMACSAEqualMACDA` (1, the drop SONiC calls `SMAC_EQUALS_DMAC`).
  They are header sanity checks applied to the whole switch: a frame with
  its source and destination address equal, a TCP segment with SYN and FIN
  both set, a first fragment too small to hold a TCP header. One such frame
  trips a check; a flood of well-formed traffic trips none.
- **A per-port count of them, outside SAI.** The SDK names receive
  debug-counter triggers for those drops (`bcmDbgCntDSL3HE`, `DSL4HE`,
  `DSICMP`, `DSFRAG`, `RxDSL2HE`, `MACSAEQUALMACDA`, in the `syncd`
  container's `libsai.so.1`). The receive debug counters are per-port
  registers (`listreg RDBGC8`: a port register), and `RDBGC8_SELECT` is 0,
  unused. With SONiC's one `PORT_INGRESS_DROPS` debug counter installed
  for 17 s (`SMAC_EQUALS_DMAC`), all nine select registers read as before,
  so which hardware counter the SAI uses for it was not found; whether the
  two would collide is open.
- **Through SAI, only two of the checks.** SONiC's drop-counter reasons
  include `SMAC_EQUALS_DMAC` and `IP_HEADER_ERROR` (one counter in all,
  §11.18.2); SAI has no attribute that turns the other checks on.
- **CoPP** polices per trap group, switch-wide, as on the ECS: no port.

None of this is the ECS's condition, because the ECS shows none. The chip's
checks carry the name "DoS attack" from the chip vendor and are what many
switch makers mean by DoS prevention; reporting them as bit 1024 would
still be a condition of this project's choosing, not the controller's or the
ECS's.

**Built: nothing.** The driver never reports a `denial-of-service` Fault
and never sets 1024. (`internal/device` maps that cause to 1024 for a
driver that one day has it.)

**What it takes to report it** (the build assessment; none of it is out of
reach of the DX010):

1. **A definition**, from a UniFi switch that reports 1024: a support-bundle
   `last.inform` or a switch's own `mca-dump` with the bit set, and the
   traffic that set it (which header check, or which rate). No capture in
   the tree has the bit set (the real switch's port entries in
   `controller-10.6.106/inform-usw-xg16-usxg.json` carry `anomalies` 0; the ECS
   carries none), so this needs a switch whose firmware sets it; until then,
   which checks to turn on is a guess.
2. **If the definition is header checks, the DX010 half is a small driver
   feature:** turn the chosen `DosAttack*` controls on with `bcmcmd sc`
   (outside SAI and orchagent, so lost whenever `syncd` restarts: disunified-l2d
   or the driver re-applies them after every `syncd` start, or an image
   patch exposes them as SAI switch attributes, which upstream SAI does not
   define), point the free receive debug counter at the matching triggers
   (one switch-wide select, a count per port), read it per poll in the
   driver's one exec into a neutral cumulative counter in `Port.Health`, and
   set 1024 on its growth in `internal/device`, as the storm and DHCP
   counters are (a detection, not a port held down, §11.20.1). The checks
   apply to every port, the uplink included, and drop what they match, so
   each one turned on is a change to the switch's forwarding, made only with
   the definition in hand. The debug counter must first be shown not to
   collide with SONiC's own `PORT_INGRESS_DROPS` counter.
3. **If the definition is a rate** (excessive traffic of some kind), a
   per-port rate detector: the port's own counters in `COUNTERS_DB`, read
   by disunified-l2d as the storms detector reads them (§11.21), against the
   definition's threshold.
4. **Verification:** a cabled host on a DX010 test cage sending the frames
   the definition names, the count growing in a capture scrubbed with
   `scripts/sanitize-sonic.py`, and the alert seen in the controller.
### 11.19 Storm-control drops: no count to report (2026-11-14)

(Since the same day the storm bits 1 and 2 come from disunified-l2d's
detection instead, from the port's receive counters against its limit:
§11.21; and bit 4 from the port's sFlow samples, §11.24. This section
stays the record of why the policers cannot tell. Corrected the same day,
§11.24.1: the SAI does answer `get_policer_stats` for a storm-control
policer, with zeros that do not move while the chip's meter for the port
is in the red, so a `POLICER` flex group would poll zeros.)

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

### 11.20 Which protections disable a port: UniFi's model, the ECS and the DX010 (2026-11-14)

The `anomalies` bits (root `CLAUDE.md` §5) are each a condition the switch
detected on a port. This section answers a question the bits do not: **does
the switch disable the port for it, or only detect and report it?** The other
anomaly sections (§11.5 loop protection, §11.17-§11.19, §11.21 storms) build
on it.

**Sources.** (1) The UniFi UI's own wording, as an operator reads it in
Alerts, the System Log and the port panel on Network 11.0.97: its log
messages, recommended actions and tooltips. (2) The ECS
Aggregation (`USWF066`, Ubiquiti SONiC `4.0.1.995`), read-only on
2026-11-14 at about 09:00 MST, after it was back on 4.0.1 (4.0.3 had refused
the site key earlier that day, §11.18): CONFIG_DB/APPL_DB/STATE_DB rows, the
CLI's output (`show errdisable recovery`, `show errdisable status`,
`show loop-protect status`, `show mac-move-guard configuration` and
`capabilities`), and the inform it prints (`mca-dump`). (3) The committed ECS
inform and push, `controller-10.6.109/inform-ecs-aggregation-uswf066.json`
and `system_cfg-ecs-aggregation.txt`. Nothing read off the ECS is kept in the
tree beyond those two (Clint, 2026-11-01); the facts below are stated so a
reader with an ECS can check them.

#### 11.20.1 The model

| Bit | What the UI says happened | Port disabled? | What the operator does |
|---|---|---|---|
| 512 loop protection (keepalive) | the port "was disabled by Loop Protection to prevent a network loop"; the tooltip of the port setting says it disables the port when a loop is detected | **yes** | re-enable the port by hand: the tooltip says disabled ports must be re-enabled manually, and the port panel's message offers a "re-enable" link (what that link sends is not captured) |
| -- BPDU guard (no `anomalies` bit; its own alert) | UniFi disabled the port because it detected a loop; fix the loop, then re-enable the port | **yes** | re-enable by hand (the recommended action says so) |
| 1 / 2 / 4 storms | incoming broadcast / multicast / unknown-unicast packets on the port **exceed the Storm Control threshold**; Storm Control's tooltip says it *restricts* excessive traffic of those types | no | check the Storm Control setting against the port's traffic, look for loops or a misbehaving device (the recommended action) |
| 32 DHCP rate | incoming DHCP packets exceed 300/s on the port | no | check for misconfigured devices and rogue DHCP servers |
| 64 rogue DHCP | a rogue DHCP server detected on the port | no | -- |
| 128 802.1X | a failed 802.1X authentication attempt on the port | no | -- |
| 256 MAC locking | a MAC on the port that is not on its allowed list | no | -- |
| 1024 DoS | a denial-of-service attack detected on the port | no | -- |

So in UniFi's model **only loop protection and BPDU guard take a port down**;
every other bit is a detection the switch reports while the port keeps
running (and, for storms, keeps limiting the traffic). The UI also has
"Anomaly Started" / "Anomaly Ended": a bit is a condition with a duration,
which the bridge produces by setting it in each inform in which the
condition was seen (`internal/device`, `portAnomalies`).

**For a driver this means:** a port held down by the switch's own protection
is a `Port.Fault` naming the cause (`internal/device` maps the cause to its
bit, `protectionHold`); a detection is a count that grows while the condition
lasts (`Port.Health.StormDrops*`, `DHCPRateDrops`, `RogueDHCPDrops`, set
on growth between informs). Taking a port down for a detection-only cause
would do more than the controller's setting promises, and the operator would
have no UniFi control written for bringing it back.

#### 11.20.2 What the ECS shows

- **An error-disable table with every cause.** CONFIG_DB `ERROR_DISABLE|<cause>`
  (APPL_DB `ERROR_DISABLE:<cause>` the same) for `bcast-storm`, `mcast-storm`,
  `ucast-storm`, `dhcp-rate-limit`, `denial-of-service`, `dot1x`,
  `mac-locking`, `keepalive`, `bpduguard`, `bpdustorm` and
  `mclag-delay-restore`, each `admin_status enabled`,
  `auto_recovery_interval 0` (`mclag-delay-restore` also `action
  acl-block`). `show errdisable recovery` lists the same; `show errdisable
  status` (reason, port, last disable time) is empty; the CLI has `config
  errdisable port-recovery <interface>` and per-cause `auto-recovery-status`
  / `auto-recovery-interval`. What an interval of 0 does is not established:
  no port has tripped, and the ECS is a production switch.
- **The table is not evidence of a detector.** The ECS does not claim storm
  control at all: its inform's `switch_caps.feature_caps` is `123317237`
  (live and in the fixture), bit 8 (storm control) clear, no
  `storm_control_caps`; its push has no `stormctrl` key; CONFIG_DB has no
  storm-control table and the CLI no storm-control command. Its storm rows
  name a feature the switch does not offer. It does claim DHCP snooping
  (128), port security (512), loop protection (1048576) and
  `feature_caps2` 128.
- **No port reports anomalies.** No port of the ECS's inform has an
  `anomalies` key (all 54, in the fixture and in today's `mca-dump`).
- **Two loop mechanisms.** `MAC_MOVE_GUARD|GLOBAL` (enabled, threshold 10,
  detect interval 5 s, action interval 150 s, action `DISABLE_PORT`;
  `show mac-move-guard capabilities`: disable-port and disable-learn) with
  `MAC_MOVE_GUARD_INTERFACE|<port> guard enabled` per port, and a separate
  loop protect (`show loop-protect status`: per port status, loop detected,
  last detect time, rx action, transmit interval, auto-recovery interval;
  disabled on every port here, as the push's `switch.port.N.keepalive` is
  `disabled` everywhere). §11.5's l2d implements the first for the
  controller's Loop Protection.
- STATE_DB `PORT_TABLE|<port>` carries `state ok` and `linkdown_count`;
  ubnt-stpmgrd logs an `errdisabled` flag per interface with each STP state
  event (all `false`).

#### 11.20.3 What the DX010 does with it

| Bit | DX010 | Where |
|---|---|---|
| 512 | the port is taken down for 150 s and reported as `Fault "errdisabled: loop-protect (...)"`; recovery is automatic after the hold (the ECS's `MAC_MOVE_GUARD` action interval) or at the operator's Re-enable, never the uplink, the STP root port or the gateway's port | §11.5, §11.28 |
| -- BPDU guard | mstpd shuts the netdev and disunified-l2d holds the chip's port down until the operator's Re-enable (the controller's `port-recovery`), the guard or STP going off for the port; no timer; reported as `Fault "errdisabled: bpduguard"` | §11.28 |
| 1 / 2 | detected by disunified-l2d from the port's own receive counters against the port's storm-control threshold; the port keeps running under its storm-control limit; reported as growth of `StormDropsBroadcast` / `StormDropsMulticast` | §11.21 |
| 4 | detected by disunified-l2d from the port's sFlow samples of the unicast it floods (no counter separates them), on an image with sonic-buildimage 0014; the port keeps running; growth of `StormDropsUnicast` | §11.24 |
| 32, 64, 128, 256, 1024 | §11.17, §11.7.9, §11.18 | -- |

### 11.21 Storm detection on the DX010: anomalies 1 and 2 from the port's own counters (2026-11-14)

**What UniFi asks for** (§11.20): Storm Control *restricts* broadcast,
multicast and unknown-unicast traffic on a port to the configured level, and
the storm anomaly says that incoming traffic of the type **exceeded the
threshold**. No port is taken down. The ECS, the family the DX010 is
claimed as, does not offer storm control at all (§11.20.2), so it gives no
pattern to copy beyond UniFi's own words. The DX010 already restricts
(`PORT_STORM_CONTROL`, kbps from the UI's percent, §5); what it lacked is
the detection, because the policers count nothing (§11.19).

**Decision.** disunified-l2d detects; the port keeps running under its
storm-control limit; the driver reports the detection as the port's storm
drops, which `internal/device` turns into bits 1 and 2 on growth. Not the
error-disable shut-down this work was first scoped as: UniFi's model and
its UI text have no shut-down for a storm and no control to bring such a
port back (§11.20.1), and the ECS has no detector to copy. So none of
loop protection's never-shut-down rule (uplink, STP root port, gateway's
port) is needed here: nothing is shut.

**What the switch can measure.** Per port, COUNTERS_DB (flex counter, 1 s
poll) holds the MAC's receive counters, which count every frame the port
took in, before the ingress pipeline's policers and filters:

| UniFi type | SONiC storm type | Counter | What it tells |
|---|---|---|---|
| Broadcast | `broadcast` | `SAI_PORT_STAT_IF_IN_BROADCAST_PKTS` | exactly the broadcast frames received |
| Multicast | `unknown-multicast` | `SAI_PORT_STAT_IF_IN_MULTICAST_PKTS` | every multicast frame, known groups too: an upper bound on unknown multicast (UniFi's text speaks of incoming multicast packets, which this is) |
| Unknown unicast | `unknown-unicast` | none | `IF_IN_UCAST_PKTS` counts all unicast; nothing separates a frame the switch floods (unknown destination) from one it forwards. Not detected from counters; since the same day detected from the port's sFlow samples instead (§11.24) |

The threshold is bytes per second (kbps × 125, as orchagent programs the
policer's CIR, §11.19), the counters frames. A frame is at least 64 bytes,
so more than limit / 64 frames a second is over the threshold whatever the
frames' size; l2d compares against that. A storm of large frames above the
byte limit but under limit / 64 frames a second is missed: a lower bound,
never a false alert.

**As built (`l2d.py` `StormWatch`, one class, one registration in `L2d`).**
- Every second, for each port with a `broadcast` or `unknown-multicast` row
  in CONFIG_DB `PORT_STORM_CONTROL` (the driver writes it from the push; a
  hand-made row is watched too), read the two counters. Whatever owns
  storm control, l2d watches it: the limits are the switch's own.
- The rate is taken over the last 5 samples (5 s). A window's ends can each
  be up to one 1 s poll stale, so a storm starts when the rate is above
  1.25 × limit / 64 and ends when it is back under limit / 64. Start and end
  are logged (`storm control: Ethernet16 broadcast storm, 3189 frames/s over
  5 s, limit 1000 kbps (1953 frames/s at 64 bytes)`).
- While a port is over, each second's frames above limit / 64 are added to
  its count: a lower bound on what storm control dropped. STATE_DB
  `DUI_STORM_TABLE|<port>` publishes `broadcast` and `multicast` (monotonic;
  adopted again when l2d restarts; the row goes when the port's storm
  control does). A counter that went down (cleared) restarts the window.
- `collect.py`'s `l2_storm` section reads the table; `stormPorts` sums a
  cage's interfaces into `Health.StormDropsBroadcast` / `StormDropsMulticast`.
  `Fault` is not touched.
- The driver installs and runs l2d for storm control alone (`l2Want.storm`):
  a push with broadcast or multicast storm control on any port is enough,
  with spanning tree and loop protection off. Unknown unicast alone is
  too, on an image that can sample a port (§11.24).
- Beside the rate limits: the policers keep dropping in the ASIC exactly as
  before; l2d only reads. Rate mode (packets/s) stays refused with its note
  (§11.8): SONiC's policer takes kbps only, and detecting a limit the
  switch does not enforce would report a storm control that is not there.

**The uplink, observed only** (Ethernet0, 60 s at 1 s, 2026-11-14, before
the live run): broadcast mean 77 frames/s (largest second 152), multicast mean 95
(largest 451). UniFi's lowest level, 1 % of a 100G port, is 1 000 000 kbps,
about 1.95 million frames/s at 64 bytes; even the test's 1000 kbps limit
(1953 frames/s) is four times the uplink's busiest second. The replay test
runs the uplink's samples from the live run through `StormWatch` at that
limit: no storm.

**Live run on the DX010** (2026-11-14 09:18-09:21 MST, image
`igmp-202511.20261106-f2e25f54c`, under the shared l2d test lock; the
bridge stopped for the run and the installed l2d replaced by this build):
cage 5 (`Ethernet16`, `ce4`, empty) brought up by MAC loopback (`bcmcmd
"port ce4 lb=mac"`), storm control written by hand at 1000 kbps for
`broadcast` and `unknown-multicast`, and frames sent out of the port from
the CPU (64-byte frames, a raw socket on `Ethernet16`), counters sampled
every 0.5 s with the uplink's beside them
(`sonic-igmp-202511.20261106-f2e25f54c/storm-counters.ndjson`):

| Sent from the CPU | Received (counter) | l2d |
|---|---|---|
| broadcast, 74 100 frames over 15 s (4940/s) | +74 100 | storm at 3189 frames/s over 5 s; over after the flood; 39 839 frames above the limit |
| broadcast, 22 290 frames over 15 s (1486/s, under 1953) | +22 290 | nothing |
| multicast `01:00:5e:7f:ff:16`, 74 150 frames over 15 s | +74 338 (the 74 150 and 188 control frames the switch itself sent out of the looped port over the 95 s) | storm at 2955 frames/s; 41 585 frames above the limit |

The collector's capture with the row is `collect-storm.txt` (scrubbed); the
worktree build's `-collect-once` on the switch reported port 5 with
`StormDropsBroadcast` 39839 and `StormDropsMulticast` 41585, no fault,
`StormDropsUnicast` 0. `TestWireContractSonicStorm` sets exactly bits 1
and 2 on port 5 from that capture after the one before, and clears them on
an inform with no growth. `TestL2dStormWatchOverLiveCounters` replays the
counters (38 854 and 40 416 frames above: the replay samples every 0.5 s
where the live daemon read its own seconds).

Facts the run found:
- **A port named by IGMP snooping's static groups cannot leave its VLAN**:
  `config vlan member del 4094 Ethernet16` was refused ("remove all
  associated static Layer 2 multicast group entries first": the reserved
  groups the driver floods to every port, §8.10). So the looped port stayed
  an untagged member of the management VLAN. RSTP held it: l2d logged
  `Ethernet16: disabled -> blocking` at link-up and nothing else until the
  loopback was removed, the port's `IF_IN_DISCARDS` grew by 170 540
  (exactly the frames sent: 74 100 + 22 290 + 74 150), and the uplink's
  broadcast-out counter moved by 12 over the whole period: nothing reached
  the LAN. A later test on a looped cage
  should first take it out of the static groups, or rely on a blocked port
  only with spanning tree on.
- The MAC receive counters count a MAC-looped port's frames exactly
  (broadcast +96 390 = 74 100 + 22 290 sent).
- Restored and confirmed: loopback off, both storm-control rows deleted,
  `Ethernet16` untagged in Vlan4094, the installed l2d back to main's
  (sha256 `8c186c56…`), `DUI_STORM_TABLE` gone, l2d and the bridge active;
  the bridge's reconcile read "0 of 32 ports changed" and its inform HTTP
  200. Nothing was saved by the test (`config_db.json` holds no
  `PORT_STORM_CONTROL`).

**Not exercised:** a storm from a cabled host, a storm on a port that
forwards (the looped port was STP-blocked, so the policer itself never
dropped), storm control pushed from the UI with the detection running (the
UI's lowest level, 1 % of 100G, is far above what the CPU can send), the
alert in the controller (the build is not deployed), a broken-out cage's
summed lanes, and a PortChannel member. **What a PR needs** for the rest: a
cabled traffic source on a test port in a VLAN of its own, storm control
from the UI, the "Broadcast Storm Detected" / "Multicast Storm Detected"
alert read from the controller's device alerts. Unicast: §11.24.

### 11.22 IPv6 on the management interface: SLAAC while the controller's network runs IPv6 (2026-11-14)

Clint asked why the DX010 had no IPv6 address, then asked for one to come
and go with the controller's IPv6 setting.

**Why it had none.** The gateway's router advertisements reach Vlan4094
(tcpdump on the switch: one every 10-16 s, a /64 with the autonomous flag,
RDNSS, an MTU option of 9216). SONiC turns forwarding on for every router
interface (`net.ipv6.conf.all.forwarding=1`). On a forwarding interface
Linux takes an RA only with `accept_ra` 2, and SONiC leaves it at 1.
SONiC's own RA set-up (`/etc/sysctl.d/90-dhcp6-systcl.conf`, and
`forwarding=0` on eth0) is for eth0, which the DX010 is not managed
through. So Vlan4094 kept only its link-local, and the inform's `ipv6`
said so (§11.4). The ECS has its management address on a kernel bridge of
its own (`br0`, outside SONiC's router interfaces), with a SLAAC address;
its `Vlan<id>` interfaces carry no address at all. Whether `br0` takes RAs
through `forwarding` 0 or `accept_ra` 2 is not captured.

**What the controller says.** No push to a switch carries an IPv6 key (no
recorded push, 10.6.106 to 11.0.99). The network's setting is in
`rest/networkconf`:

- `ipv6_interface_type` is the network editor's IPv6 Interface Type
  (`pd`, `static`, `none`).
- The gateway serves IPv6 on exactly the `pd` networks: the gateway's own
  DNS name resolves to one address per such network.
- `ipv6_enabled` is no guide. On 11.0.99 the guest network has `pd` and no
  `ipv6_enabled`, and is served all the same.
- `switch.managementvlan` names the management network: 1, the untagged
  Default network, in every recorded push.

Fixture: `controller-11.0.99/rest-networkconf.json`, reduced to the fields
read, names and ids replaced.

**Measured by hand on the DX010** (202511 f2e25f54c; documentation values
here):

- **`accept_ra` 2 on Vlan4094.** Within one RA the interface had its SLAAC
  address (`dynamic mngtmpaddr proto kernel_ra`), the RA's default route
  (`proto ra`, metric 1024) and the on-link /64 (with an expiry). FRR passed
  `::/0` and the /64 through fpmsyncd to APPL_DB `ROUTE_TABLE` and the chip,
  as it does IPv4's default. The switch pinged its gateway and opened TCP to
  it over IPv6, and a LAN host reached the address. The RA's MTU option
  (9216) is above the link's 1500, and Linux ignores an RA MTU above the
  link's: `net.ipv6.conf.Vlan4094.mtu` stayed 1500, so §12's MTU trap does
  not come back this way. Every other VLAN kept 1.
- **No ip2me route.** orchagent programs an interface address's host route
  to the CPU only from an APPL_DB `INTF_TABLE:<iface>:<prefix>` row, and
  intfmgrd writes those for CONFIG_DB addresses only. On 202511 intfsorch's
  `setIntf` adds only that host route for a row (and a directed broadcast
  for IPv4); the subnet route comes from FRR, and routeorch skips a
  full-mask route the interface owns. Without a row, IPv6 to the SLAAC
  address reached the CPU through the default trap, at the default CoPP
  group's 600 packets/s: an SSH upload to the switch ran at 0.9 MB/s. A
  row (`scope global`, `family IPv6`, through ProducerStateTable) brought
  `addIp2MeRoute` in orchagent's log and the /128 in ASIC_DB. The upload
  then ran as fast as over IPv4: ~8.8 MB/s for both, back to back, which is
  the ip2me group's 6000 packets/s (`COPP_TABLE:queue1_group1`; the first
  run of a series, 11-14 MB/s for either family, is the token bucket's
  burst).
- **Teardown.** Deleting the row removes the host route. Then `accept_ra`
  1. `ip -6 addr flush dev Vlan4094 scope global dynamic` removes the
  address, and FRR withdraws the /64. `ip -6 route flush dev Vlan4094 proto
  ra` removes the default. The RA's on-link /64 (`proto kernel`, an expiry)
  outlives all of that until `ip -6 route del`. IPv4 was untouched
  throughout.

**As built** (`ipv6.go`, `devicemodel.ManagementIPv6Controller`,
`informloop/ipv6.go`, `unifiapi.Client.ManagementIPv6`):

- **The loop asks only a driver that implements the interface**, with the
  push's `switch.managementvlan` (1 when absent). `main` answers from
  `rest/networkconf`, cached for a minute: on means the network is enabled
  and its interface type is not `none`. An error, a VLAN no network has, or
  no API key leaves the switch as it is. The setting and each error are
  logged when they change.
- **collect.py** prints `ipv6_ra` (every in-band netdev's `accept_ra`, and
  the routes learned from RAs: `proto ra` or an expiry) and `appl_intf`
  (APPL_DB `INTF_TABLE`). The SLAAC addresses are `ip -j addr`'s dynamic
  global ones.
- **On**, for the interface the bridge manages the switch at (`ntpSource`,
  as for NTP): `accept_ra` 2, a row for each SLAAC address, and the row of
  an address the kernel no longer has goes. The row comes one cycle after
  `accept_ra`: the address waits for the next RA.
- **Off**: the rows, then `accept_ra` 1, the address flush, the `proto ra`
  flush, and each learned prefix route deleted.
- **What is never touched**: an address CONFIG_DB carries, its row, and its
  prefix route (which has no expiry). Nothing goes to CONFIG_DB, so there
  is no `config save`.
- **Lost on a reboot (the sysctl) and on a swss restart (the rows)**: the
  reconcile writes them again each cycle.
- **The sanitiser** now rewrites an IPv6 address in an `INTF_TABLE` key:
  its pattern refused the colon before it, as the MAC pattern once did in
  FDB keys.

**Verified live through the driver** (deb 99e1fdd on the DX010,
2026-11-14 11:19 MST, from SONiC's stock state with Vlan4094 at `accept_ra`
1): the start-up reconcile logged the controller's answer (IPv6 on VLAN 1)
and wrote `accept_ra` 2. The next apply, five seconds later, found the
SLAAC address and wrote its row. The chip had the /128, and the next
inform's `ipv6` led with the global address, which the controller's
record shows. Later cycles wrote nothing, and every inform was answered.
Off has run by hand only (the steps above): turning IPv6 off on the
management network is a change to the whole LAN.

**Not built:** DHCPv6 on the management network (no `slaac` in
`ipv6_client_address_assignment`; the switch runs no DHCPv6 client
in-band), a static IPv6 address in a switch's IP Settings (not captured),
and the RA's RDNSS (`/etc/resolv.conf` comes from CONFIG_DB's
`DNS_NAMESERVER`, which the push's IPv4 DNS fills).

Fixtures: `sonic-igmp-202511.20261106-f2e25f54c/collect-ipv6-off.txt`,
`-off-leftover-route.txt`, `-slaac.txt`, `-on.txt` (the four states
above), tested in `ipv6_test.go`.

### 11.23 The controller's flap, BPDU-guard and loop-protection alerts on a bridged port (2026-11-15)

Clint asked whether the controller's link-flap and STP-flap alerts fire for
a bridged port and at what rate, and what makes it raise its BPDU-guard
alert. Until this day none of the three had been seen for a bridged port.

**Sources.** Live tests on Network 11.0.99, all on the DX010's empty cage 17
(Ethernet64) and on a stub switch run from the Mac for the test
(`docs/drivers/stub.md`, adopted under a locally administered MAC as a
separate device and forgotten afterwards): the bridge's own payloads
(`payload-last.json`, read every 15 s), the switch's logs, and the
controller's alert log (v2 `system-log/device-alert`). The alerts are
`fixtures/controller-11.0.99/system-log-device-alert-bridged.json`, the
counts each inform carried `fixtures/controller-11.0.99/alert-rates-20261115.json`
(`internal/device/alert_rates_test.go`). Real switches' informs on the site
were read only (`mca-dump`); no real switch's port was touched.

#### 11.23.1 Link flap: the growth of `link_down_count` between two informs

The cage was brought up and down with the chip's MAC loopback (`bcmcmd
"port ce16 lb=mac"` / `lb=none`) in a loop on the switch, at three periods.
RSTP keeps the looped port Backup/discarding (it hears its own BPDUs), so
the loop never reached the LAN. `link_down_count` (SONiC's `flap_count` /
2) grew per inform interval (61-89 s here) by:

| period | growth | alert |
|---|---|---|
| 5 s | 0.14-0.15 drops/s | none |
| 2.5 s | 0.23-0.24 drops/s | none |
| 0.8 s | 0.33-0.39 drops/s | **"<device> Port 17 is experiencing frequent link state changes"** at the first inform above (00:19:14 UTC, `COUNT` 123 = that inform's `link_down_count`) |

So the controller raises the alert from the bridge's raw count alone, and
the rule is a rate between consecutive informs: a growth above some value
between 0.24 and 0.32 drops per second (the test holds the capture to 0.3).
A cumulative level is not it: the real USW Aggregation on the site counts
185 000 drops on one port and no link-flap alert for it is among the alerts
the controller has kept (from 2026-11-12), while its STP-flap alert comes
nightly -- consistent with drops that never arrive faster than the rate. One
alert per condition: the two faster intervals after it raised nothing new.
A UniFi switch on 7.6.11 also sends `link_flap_count` on a few ports (1 on
each, sparse, beside a `link_down_count` of 0 to 7); the alert above came
without it. It does not count drops (it counts the switch's own link-flap
detector), and the bridge does not send it
(`docs/feature-map.md` §2, below the table).

**The bridge's count was right; nothing changed for it.** Every toggle was
counted: SONiC's `flap_count` grew by two per cycle.

#### 11.23.2 STP flap: the growth of `stp_state_change_count` between two informs

The looped cage produced no STP changes at all: mstpd's
`num_transition_fwd`/`num_transition_blk` (the driver's count until this
day) stayed at 0/1 through 368 kernel state changes, because a Backup port
is discarding whether its link is up or down. disunified-l2d's own journal of
the run (`fixtures/sonic-igmp-202511.20261114-42cccc87a/l2d-linkflap-20261115.txt`)
shows the kernel bridge port going disabled -> blocking at every link-up and
blocking -> disabled at every drop. A UniFi switch counts those: the real USW
Aggregation's flapping ports count exactly 2 changes per drop
(`fixtures/controller-11.0.99/`, `stp_changes_test.go`), and a never-up port
0 (mstpd counts 1 there, its initial entry into blocking).

**Fix (l2d; deployed 2026-11-15 as 812503a):** disunified-l2d counts every change of each
port's kernel state (disabled, blocking, learning, forwarding) from the
netlink events it already mirrors into StpOrch and from its 2 s poll, and
publishes it as `state_changes` in `DUI_STP_PORT_TABLE`; the driver reports
it as `Health.STPChanges`, falling back to mstpd's counters for an l2d
without the field. The count is since boot (it carries over an l2d restart
through STATE_DB; STP off or a reboot clears it). Tested with the journal
(`l2d_state_changes_test.go`): 2 per drop, none for the poll, 0 for a port
never up. With it deployed the same looped-cage flap counts twice its
drops, so it raises the STP-flap alert too: seen live after the deploy (§11.23.4).

The rate, measured on the stub (its port 3's `stp_changes` grown by the
test's file every 10 s):

| growth | alert |
|---|---|
| 0.05-0.10 changes/s (three intervals) | none |
| 0.19 changes/s | none |
| 0.27 changes/s | **"Port 3 is experiencing frequent STP role state changes"** at that inform (00:27:02 UTC, `COUNT` 63 = the inform's `change_count`) |

A rate between 0.19 and 0.27 changes per second between informs (the test
holds the capture to 0.2). That agrees with the real switches: the USL8A's
nightly alerts, and the ECS's alert of 2026-11-14 on two uplink ports with
`COUNT` 29 -- a low total that crossed a rate, not a level.

#### 11.23.3 BPDU guard and loop protection: `error_disabled` and its reason

**The real case on the DX010.** BPDU guard on port 17 through the
controller (`stp_bpdu_guard_enabled` in its port override, pushed as
`switch.port.17.stp.bpdu_guard=enabled`; the override restored afterwards),
then the cage looped: mstpd logged "Received BPDU on BPDU Guarded Port - Port
Down" within a second and shut the netdev (`bpdu guard error yes`); the
capture is `fixtures/sonic-igmp-202511.20261114-42cccc87a/collect-bpduguard.txt`.
The bridge deployed then reported the port with its fault and no anomalies bit,
and **the controller raised nothing** (three informs).

**The signal: `port_table[].error_disabled` true with
`error_disabled_reason`.** No real switch's inform on the site carries the
fields (none has a port shut by either protection), so they were tested on
the stub, one port at a time:

| the stub's port reports | the controller raised |
|---|---|
| `error_disabled: true`, `error_disabled_reason: 2` (port down, enabled) | **"UniFi disabled Port 4 because it detected a network loop. Fix the loop, then re-enable the port."** (`BPDU_GUARD_TRIGGERED`, title "Loop Protection Disabled a Port") at the first inform (00:28:16 UTC) |
| `error_disabled: true`, `error_disabled_reason: 1`, `anomalies: 512` | **"Port 5 was disabled by Loop Protection to prevent a network loop."** (`NETWORK_LOOP_DETECTED_BY_KEEPALIVE`, "Loop Protection Triggered") at the third inform carrying it (00:31:56 UTC) |
| `anomalies: 512` alone (the same build with the two fields left out, as the bridge sent a hold until this day) | nothing in six informs (6.5 min) |

**Fix (internal/device; deployed 2026-11-15 as 812503a, raising the alert on the DX010 itself, §11.23.4):** a port whose `Port.Fault` names
BPDU guard (`bpduguard`) or loop protection (`loop-protect`) is sent with
`error_disabled: true` and the reason bit (2 or 1); every driver that
reports those faults (`sonic`, `arista-eos`, `proxmox`) gets it. The
loop-protection hold keeps its anomalies bit 512 beside it (`tables.go`,
`caps.go`; `anomalies_test.go`, `contract_sonic_stp_test.go`).

**What differed from a UniFi switch until §11.28:** mstpd shuts the
port's netdev and StpOrch blocks it in the chip, but the chip's port stayed
up, so a cabled peer kept its link. Since 2026-11-15 disunified-l2d holds a
tripped port down in the chip until the operator's Re-enable (§11.28).

**A side effect seen, and its cause: the controller's auto edge.** 90 s
after the link-flap alert the controller's push changed port 17's
`stp.edge` from `auto` to `disabled` with no change to its port override
(l2d: "Ethernet64 auto edge off"). The flap did not cause it. The
controller (Network 11.0.99) runs its own edge evaluation per switch, which
logs to `server.log` (read 2026-11-15, placeholders for the identifiers):

```
<stp-edge-eval:aa:bb:cc:dd:ee:ff> INFO ... - stp-edge-auto written site=... mac=aa:bb:cc:dd:ee:ff port_idx=17 unit_id=null value=disabled
<stp-edge-eval:aa:bb:cc:dd:ee:ff> INFO ... - stp-edge-auto skipped reason=already_matches ... port_idx=1 unit_id=null value=disabled
```

- **What it does.** About every 5-6 minutes for each switch, it takes each
  port that is up and whose edge is left to the controller (no Edge choice
  in the override), and records the switch's own `stp_edge_port`: `true`
  is recorded as `enabled`, `false` as `disabled`. From then on the push
  carries that value as `switch.port.N.stp.edge`. The match holds without
  exception: 109 ports on the site's switches were evaluated in a
  ten-minute window, and on each one the recorded value equals the
  `stp_edge_port` the controller stores for it (`stat/device`). It writes
  both ways on real switches (`enabled` on access ports, `disabled` on a
  USXG's switch-to-switch port). The recorded value is not in the
  override: `port_overrides` and `port_table` in the API show none.
- **Why port 17.** The write came at 00:19:14.308 UTC, the same inform as
  the link-flap alert. The 5-minute sample happened to land on that inform,
  and port 17 was up in it (the looped cage) and reported
  `stp_edge_port: false`. That value is correct: mstpd's `oper_edge_port`
  is `no` on a port that hears BPDUs, and a looped port hears its own. The
  push 89 s later was the next reply that carried a config. Every DX010
  test port that was up in one of these samples was pinned the same way,
  flapping or not: ports 9, 10 and 13 on 2026-11-14 (other tests), 18 at
  01:21:21 UTC (the STP-flap run, below the link-flap rate), and 31 on
  2026-11-15 15:53 UTC. Only port 1, the uplink, is evaluated at every
  sample (`disabled`, matching).
- **A UniFi switch does the same.** The ECS's push already carried
  `stp.edge=disabled` on 2026-10-31 (Network 10.6) on 8 of its up
  ports, and each of them reported `stp_edge_port: false` in its own inform
  that evening. Its firmware applies `disabled` exactly as l2d does: each
  pinned port has `UBNT_STP|interface|EthernetN` `intf_autoedge disabled`
  and mstpd's `auto edge port no`, while the others keep `auto edge port
  yes` (read only, 2026-11-15). So the bridge reports this as a UniFi
  switch would, and the driver does not change.
- **It does not go back by itself.** Down ports are not evaluated, so
  port 17 is still pushed `disabled` 17 hours later. Once a port is
  pinned, auto edge is off on both switches, so `oper_edge_port` cannot
  become `yes` again and the evaluator keeps finding `disabled`. A host
  cabled to such a port later is a non-edge RSTP port: it reaches
  forwarding through the forward delay (about 30 s), not the 3 s of auto
  edge, and every link-up is a topology change. A `disabled` value lasts
  through the controller's nightly restart (02:29 local; a USXG port pinned
  2026-11-12 was found `already_matches` after two restarts). An `enabled`
  value does not: two access ports on a US8 and a USL16LPB were written
  `enabled` again a few minutes after each restart. Whether choosing Edge
  explicitly in the port's settings clears a pinned value was not tested
here; §11.28's run on port 17 tested it (below).
- **The setting behind it** is the site's `rest/setting`
  `global_switch.auto_stp_edge_detection_enabled` (true here), the UI's
  "Auto STP Edge": it "automatically sets ports connected to end devices as
  Edge ... Not recommended when using third-party switches or bridges."
- **No flap needed (2026-11-15, §11.28's run).** While cage 17 sat looped
  and steady as a Backup port with BPDU guard off (18:37-18:42 UTC), the
  controller's 18:38:34 UTC evaluation found the port up and its value
  `disabled` already there (`already_matches`).
- **How an operator un-pins a port** (tested on port 17 the same day).
  Choose Edge in the port's settings and apply: the UI writes the whole
  override with `stp_edge_state: enabled` (and, beside it, switched BPDU
  guard on), shows "Port mode is manually overridden. Auto STP Edge is
  disabled for this port. Restore Auto STP Edge", and the push says
  `stp.edge=enabled`. Then click "Restore Auto STP Edge", which applies at
  once: the override says `stp_edge_state: auto`, `server.log` logs
  `stp-edge-auto reset no-op reason=nothing_to_clear`, and the push says
  `stp.edge=auto` again. The next evaluation logged `skipped
  reason=effective_state_not_auto` while the override still said
  `enabled`. Port 17's override was then put back to its exact pre-test
  object through the REST API, and its push stayed `auto`.
- **The one-step un-pin: `reset-auto-edge` (captured 2026-11-15, ports 9,
  10, 13, 18 and 31, all back to `auto`).** "Restore Auto STP Edge" is a
  call of its own, not part of the override save:
  `POST /proxy/network/v2/api/site/<site>/ports/<device mac>/reset-auto-edge`
  with a list of ports as the body, `[{"port_idx":10}]` (captured from the
  UI), answered `204`. It clears the controller's recorded value and leaves
  the port override alone. The UI offers the link as soon as Edge is
  chosen, before Apply. Clicking it then sends only this call: no override
  is saved, so there is no BPDU-guard side effect. That is how port 10 was
  done, and port 9 at the first try. The API key takes the call with no
  CSRF token (unlike the routing API's writes), and one call with
  `[{"port_idx":13},{"port_idx":18},{"port_idx":31}]` un-pinned three ports.
  The next push carried `stp.edge=auto` for exactly those ports and nothing
  else. The bridge applied it with "0 of 32 ports changed", l2d logged
  "EthernetN auto edge on", and `port_overrides` in `stat/device` stayed
  byte-identical. A reset that clears a value logs nothing in `server.log`.
  Only one with nothing to clear is logged: `stp-edge-auto reset no-op
  reason=nothing_to_clear ... ports=[...]`. The override save logs one too,
  because saving an explicit Edge choice already drops the recorded value.
  So port 17's `nothing_to_clear` came from its Restore after the save, and
  it does not show that the call does nothing. The two-save path above
  (Edge applied, then Restore) also works, but it rewrites the whole
  override with the editor's defaults: on port 9 those were BPDU guard on,
  `autoneg`, `dot1x_ctrl`, `stp_port_mode` and a dozen more. The override
  then has to be put back by REST.
  **After a test that left a cage up as a non-edge port for more than five
  minutes**, make that one call for the cage. Check the next push for
  `switch.port.N.stp.edge=auto`.

**Alerts these tests raised** (each reaches Clint as a notification; all
on test ports or the stub): link flap on the DX010's port 17 (00:19:14 UTC 2026-11-15), STP flap
on the stub's port 3 (00:27:02), BPDU guard on the stub's port 4 (00:28:16),
loop protection on the stub's port 5 (00:31:56); after the deploy, STP flap
on the DX010's port 18 (01:17:42) and BPDU guard on its port 17 (01:20:00).

#### 11.23.4 On the DX010 itself, after the deploy (2026-11-15, build 812503a)

With the fixes running on the switch (`disunified -build-version`
`0.0.0+20261114.812503a`, the container and the DX010 both):

- **STP flap.** Cage 18 (Ethernet68, never flapped before) looped and
  unlooped every ~6.5 s: between two informs (81 s) `link_down_count` grew
  0 -> 12 (0.15 drops/s, under the link-flap rate) and `change_count`
  1 -> 25 (0.30 changes/s, l2d's count: two per drop). The controller raised
  **"<device> Port 18 is experiencing frequent STP role state changes"** at
  that inform (01:17:42 UTC, `COUNT` 25) and no link-flap alert. mstpd's own
  counters stayed at 0/1 through the run; l2d's `state_changes` read 33 at
  its end (`fixtures/sonic-igmp-202511.20261114-42cccc87a/l2d-stpflap-20261115.txt`,
  replayed in `l2d_state_changes_test.go`; the replay sees 32: one change the
  2 s poll caught between two logged events).
- **BPDU guard.** BPDU guard on port 17 through the controller, the cage
  looped: mstpd shut the port at once; the next inform carried port 17 with
  `error_disabled: true`, `error_disabled_reason: 2`, `anomalies: 0`, `up:
  true` (§11.23.3), and the controller raised **"UniFi disabled <device> Port
  17 because it detected a network loop. Fix the loop, then re-enable the
  port."** at that inform (01:20:00 UTC). Restored: the cage unlooped, the
  override back to its value before the test, `ip link set dev Ethernet64
  up` and one link-up cleared `bpdu guard error`.
- **Loop protection was not exercised.** disunified-l2d's loop protection
  holds a port when a source address moves between it and another port (10
  moves in 5 s, §11.5). A MAC-looped cage that is not forwarding learns
  nothing, so it needs two looped cages forwarding in a VLAN of their own
  with spanning tree off on both. The cages are untagged in Vlan4094, the
  LAN's VLAN, and cannot leave it while the bridge runs: the driver puts
  them back as the controller says, and a VLAN-only network for them would
  be a new site network. Stopping the bridge to do it by hand takes the DX010
  offline, so no inform would carry the hold. And forwarding looped cages
  repeat every broadcast in their VLAN without end. The loop-protection
  alert's field is the stub's result above (reason 1); the DX010's hold sets
  it from the same `Port.Fault` as before (`contract_sonic_stp_test.go`).

The alerts are `fixtures/controller-11.0.99/system-log-device-alert-dx010-deployed.json`.
### 11.24 Unknown-unicast storms on the DX010: anomaly 4 from sFlow samples (2026-11-14)

UniFi's unicast storm anomaly (bit 4) is the alert "Incoming unknown
unicast packets on <device> Port N exceed the Storm Control threshold"
(§11.20.1). §11.21 left it unset on the DX010: the port's receive counter
counts all unicast, and nothing that §11.19 and §11.21 looked at separates
a frame the switch floods because its destination is unknown. Clint asked
whether there was really no way. Four routes were tried, each on the
switch itself (image `igmp-202511.20261114-42cccc87a`, 2026-11-14
16:49-17:05 MST, empty cage 13 = `Ethernet48` = `ce12`; the probe scripts
and their risks were written down first; the switch was checked and
restored after each). The detection built on the fourth was proven on
2026-11-14 20:20-20:24 MST (§11.24.4).

#### 11.24.1 The routes

| Route | What it needs | Verdict | Evidence |
|---|---|---|---|
| A counted ACL rule matching "destination not in the FDB" on unicast, per ingress port | the SAI's `SAI_ACL_TABLE_ATTR_FIELD_FDB_NPU_META_DST_HIT` (and the entry's `..._ENTRY_ATTR_...`), an aclorch patch to name it | **refused by the SAI** | below |
| The storm-control policer's own statistics | `get_policer_stats` answering for a `SAI_POLICER_MODE_STORM_CONTROL` policer, then orchagent registering the policers for polling | **answered with zeros**: the chip's storm meters count nothing | below |
| Another per-port count the chip keeps for flooded unicast | a register or a debug-counter trigger | **none**: no `DLF` register on the BCM56960, no "destination lookup miss" trigger | below |
| The ingress sFlow sampler, each sample classified by its destination | sFlow on the port (the SAI's samplepacket), the samples in software | **works** once the image loads `linux-bcm-genl` (sonic-buildimage 0014) | below; built (§11.24.3) |

**The ACL field.** `libsai.so` (libsaibcm 13.2.1.100) carries the names
`SAI_ACL_TABLE_ATTR_FIELD_FDB_NPU_META_DST_HIT` and its entry twin, and
the SDK in it the field processor's `L2DestHit` qualifier, so it was
probed: ACL tables pushed by hand through ASIC_DB's operation queue
(§11.9's method, op `Screate`/`Dremove`, every object id allocated by
`INCR VIDCOUNTER`, the encodings copied from orchagent's own lines in
`sairedis.rec`). One difference from §11.9: **orchagent was stopped
(`SIGSTOP`) for each push and continued after it, with a 60 s timer to
continue it whatever happened**, because sairedis' `RedisChannel::wait`
takes any `getresponse` as the answer to its own pending operation (it
checks only the op name), so a reply left in the queue could be read as
the answer to orchagent's next call. Results, `SAI_ACL_STAGE_INGRESS`,
bind point `PORT`:

| Fields | Status |
|---|---|
| `IN_PORTS` | `SAI_STATUS_SUCCESS` (removed again) |
| `FDB_NPU_META_DST_HIT` | -65535 |
| `IN_PORTS`, `FDB_NPU_META_DST_HIT` | -65534 |
| `DST_MAC`, `IN_PORTS`, `FDB_NPU_META_DST_HIT` | -65533 |
| `ROUTE_NPU_META_DST_HIT`; `NEIGHBOR_NPU_META_DST_HIT` | -65535 each |
| `FDB_DST_USER_META` | `SAI_STATUS_SUCCESS` (removed again) |

The status is `SAI_STATUS_INVALID_ATTRIBUTE_0` (-0x10000) plus the
attribute's index in the list (the bind point list is index 0), and it
moved with the field: this SAI refuses every `*_NPU_META_DST_HIT` field.
syncd logged each as `api SAI_COMMON_API_CREATE failed in syncd mode` and
carried on (synchronous mode answers a failure; it throws only when that
mode is off, `Syncd.cpp`). `FDB_DST_USER_META` is accepted, but it matches
an FDB entry's `SAI_FDB_ENTRY_ATTR_META_DATA`, which hardware-learned
entries do not carry, so it cannot tell a miss from a learned
destination. No upstream change adds the field to aclorch (sonic-swss
knows it only in `p4orch` and in swss-common's `saiaclschema.cpp`, as a
comment), and it would not help on this SAI.

**The policer statistics.** With `config interface storm-control add
Ethernet48 unknown-unicast 1000` the port's
`SAI_PORT_ATTR_FLOOD_STORM_CONTROL_POLICER_ID` named a policer (`MODE
STORM_CONTROL`, `CIR 125000`). A hand-pushed `get_stats` of its
`SAI_POLICER_STAT_PACKETS`, `GREEN_PACKETS` and `RED_PACKETS` answered
`SAI_STATUS_SUCCESS`, all `0`. Then, with the cage alone in a VLAN of its
own and MAC-looped (below), 110 000 unknown-unicast frames came in at 4000
to 5000 a second, two to two and a half times the limit. The chip's meter
for the port took them: `IFP_STORM_CONTROL_METERS.ipipe0[339]` went from
`BUCKETCOUNT=0x10000` to `0x3fffff28` (below zero, red) and `0x194c` during
the flood. The SAI still answered 0, 0, 0. So the SAI answers the call and
counts nothing, which fits §11.19's reading of the meter's fields (a token
bucket, no counter). sonic-net/sonic-swss#3491 ("[FC] Policer counter",
open) would make orchagent poll policer statistics; on this SAI it would
poll zeros.

**Other counters.** `bcmcmd "listreg dlf"` / `"listmem dlf"` find no
register or table on the BCM56960. The SDK's receive debug-counter
triggers (`bcmDbgCnt*` in `libsai.so`, 300 of them) have
`RxL2DstDiscardDrop` and `RxStormControlDrop` (§11.19) but nothing for a
destination lookup that missed and was flooded.

**The ECS.** Nothing to copy: it does not claim or offer storm control at
all (§11.20.2).

**sFlow.** The chip's ingress sampler samples received frames, and the SAI
can deliver each sample to the CPU with the frame's first bytes, its
ingress port and where the switch sent it. So the switch can say, a frame
in N at a time, which of a port's frames it flooded. SONiC's sFlow
container (`docker-sflow`, feature `sflow` disabled) and the kernel's
`psample` module are in the image. Turned on for `Ethernet48` alone at 1:256,
the SAI refused the port's `SAI_PORT_ATTR_INGRESS_SAMPLEPACKET_ENABLE`:

```
SAI_API_PORT:_brcm_sai_port_proc_samplerate_set:18439 cannot open knet link sample proc
sflowAddPort: Failed to set session f000000000822 on port 1000000000015
```

orchagent logged `handleSaiFailure` and kept running. The proc file the
SAI opens is `/proc/bcm/genl/psample/rate` (its name is in `libsai.so`),
and only `linux-bcm-genl` creates it. **The legacy-TH SDK builds that
module and its package neither installs nor loads it**:
`saibcm-modules-legacy-th/systems/linux/user/common/Makefile` sets
`BUILD_BCM_GENL=1` and strips it beside `linux-knet-cb.ko`, but
`debian/opennsl-modules.install` and `.init` name only `linux-knet-cb`
(the same on sonic-buildimage master today). Upstream fixed the same gap
for the XGS package's sdklt module only (sonic-net/sonic-buildimage#26360,
in 202511). Others have hit the legacy-TH/classic half: a SONiC fork's
tracker has the identical `cannot open knet link sample proc` on a
Trident 3 and a fix that ships and loads the module. That one was found
by search and not read beyond its title and summary.

The module from the build tree that produced the installed modules (its
`linux-bcm-knet.ko` is byte-identical to the switch's; vermagic
`6.12.41+deb13-sonic-amd64`, depends `linux-bcm-knet,psample`) was loaded
by hand (`kernel.panic` 10 and `panic_on_oops` 1 were already set, so an
oops would have rebooted, not hung). Its proc files appeared, the same
session then answered `SAI_STATUS_SUCCESS`, and the port delivered
**168 samples for 40 000 received unknown-unicast frames at 1:256**
(156 expected). That was with the frames over the limit, so about half of
them were dropped by storm control: **the sampler sees the frames before
storm control drops them.** Loaded after syncd had made its netifs, the
module did not know them (`Failed setting psample rate on unknown network
interface: 'Ethernet48'`). Its samples carried no ingress ifindex and a
rate of 1, so it has to be loaded at boot: sonic-buildimage 0014 (§11.24.2).
Afterwards `rmmod`, `sflow` back to disabled, and nothing left in ASIC_DB.

#### 11.24.2 The image: sonic-buildimage 0014

`contrib/sonic-image/series/sonic-buildimage/0014`: the legacy-TH package
installs `linux-bcm-genl.ko` and loads it after `linux-knet-cb` (so
`linux-bcm-knet` already has its parameters), and unloads it before
`psample`, which it uses. It is #26360 done for the classic-XGS modules.
`dry-run.sh` checks that every module the init loads is installed, that
the SDK builds genl, and both orders.

#### 11.24.3 As built

**Which frames count.** A sample is flooded unknown unicast when its
destination MAC is unicast, and when linux-bcm-genl reports it sent to
no front-panel port and not to the CPU (`PSAMPLE_ATTR_OIFINDEX` 0xffff;
bcm-genl-psample.c sets that for a multicast destination, which a flood
is, and for a frame dropped in forwarding; it sets the port's ifindex for
a known unicast destination and leaves it out for the CPU). It must also
be neither the switch's own MAC (`DEVICE_METADATA|localhost mac`) nor in
the FDB for its VLAN: the tag's VLAN if the frame has one, else the port's
untagged VLAN (or its PortChannel's). The FDB check is l2d's own copy,
fed from STATE_DB `FDB_TABLE` for loop protection. The module's
multicast flag would say "flooded" directly, but it does not reach
userspace. The switch's own MAC is excluded even where
the chip floods it: live, frames to it in a VLAN without an interface
went out as a flood (OIFINDEX 0xffff), but in a VLAN with one a routed or
CPU-bound frame that is dropped reads the same, and would be a false
storm. That leaves an undercount in the first case.

**Sessions.** `UcastSampler` in `l2d.py` (a detector beside MAC locking)
samples each front-panel port that has a `PORT_STORM_CONTROL|<port>|unknown-unicast`
row, through SONiC's own sFlow configuration, in CONFIG_DB as `config
sflow` writes it:
1. `SFLOW_SESSION|all admin_state down`, first, so no other port is sampled;
2. `SFLOW_SESSION|<port>` (`admin_state up`, `sample_rate`);
3. `SFLOW|global admin_state up`;
4. `FEATURE|sflow state enabled`.

sflowmgrd turns the rows into sfloworch's APPL_DB rows, and coppmgrd
installs the `sample_packet` trap with its group. No collector is
configured, so hsflowd sends nothing.

A feature l2d turned on is marked (`/etc/sonic/disunified/l2d-sflow`).
With the last watched port gone, l2d deletes its rows and turns the
feature off only once sflowmgrd has emptied APPL_DB, or after 10 s. If
the operator turned the feature on, l2d writes nothing, says so once, and
reads whatever is sampled on the storm-controlled ports.

**Why not APPL_DB alone** (the first build, proven wrong live). Rows
written straight into APPL_DB `SFLOW_TABLE`/`SFLOW_SESSION_TABLE` with the
feature off made sfloworch bind the port. The SAI then sampled it, but
not one sample reached the CPU: coppmgrd installs the trap only through
the feature. Its `always_enabled` path (`CoppMgr::addTrap`) writes the
group's `trap_ids` and nothing else when the group has no other enabled
trap, and the group's meter and genetlink fields come only with the
feature's path (`setFeatureTrapIdsStatus`). Tried by hand with
`COPP_TRAP|sflow always_enabled true`, copporch failed the trap
(`SAI_HOSTIF_TRAP_ATTR_PACKET_ACTION ... mandatory but not passed`,
`SAI_STATUS_MANDATORY_ATTRIBUTE_MISSING`; refused by sairedis' metadata
check, so syncd was untouched). l2d's STP trap is spared only because
its group already carries BGP and LACP.

That failure sets orchagent's sticky `gOrchUnhealthy`, which re-logs the
error at ERR every select loop until orchagent restarts. It reaches the
remote log host, and nothing else reads the flag (system health stayed
`OK`). The first boot's SAI refusal of the sFlow binding (§11.24.1) set
it the same way. **Never test a CoPP row by hand on this switch.**

**Breakout.** `SFLOW_SESSION.port` is a leafref to `PORT`
(sonic-sflow.yang), so `config interface breakout -f`, which the driver
uses, clears a sampled lane's session with the other dependents. l2d does
not write it again: the lane has no storm control then. Not exercised
live.

**The rate** is `kbps × 125 / 64 / 25`, at least 256 (sonic-sflow.yang's
range is 256 to 8388608): 25 samples a second at the limit when the
frames are 64 bytes, one a second when they are 1518 bytes, whatever the
limit (1:78125 at UniFi's lowest level on a 100G port, 1 %). Under
about 13 Mbps it is the floor, 1:256 (the test's 1000 kbps: 7.6 samples a
second at the limit). The CPU sees samples in proportion to
the traffic, and CoPP caps them: the sflow trap has its own queue (2) and
meter (`queue2_group1`, 1000 packets a second), shared with nothing else.
A flood large enough to hit the cap is reported low: a lower bound,
never a false alert.

**The threshold** (`StormWatch.usample`). Each sample stands for `rate`
frames of its size (`PSAMPLE_ATTR_ORIGSIZE`, the frame with its FCS), so
the flooded bytes a second are known up to the sampling's noise and are
compared with the policer's own byte limit (`kbps × 125`). §11.21's
types have to bound the frames at 64 bytes instead. The rate is taken
over the shortest of 5, 10, 20 and 30 s that holds 30 samples (else 30
s). A storm starts when the rate less three standard errors (`3/√n` of
it) is over the limit, with at least 10 samples, and ends when the rate
plus them is under it. While a port is over, each second adds its
flooded frames' share above the limit, `frames × (1 − limit / rate)`,
to STATE_DB `DUI_STORM_TABLE|<port>` `unicast`. That is the same sense as
`broadcast` and `multicast`: frames above the limit, monotonic, adopted
when l2d restarts. The port is never touched.

**The driver.** `collect.py` prints `genl_psample` (`yes` when
`/proc/bcm/genl/psample/rate` exists). With it, unknown-unicast storm
control alone makes the driver install and run l2d (`stormWatched`), and
`stormPorts` adds `unicast` into `Health.StormDropsUnicast`;
`internal/device` sets bit 4 on growth, as for 1 and 2. On an image
without it nothing changes from before: no session, `StormDropsUnicast`
0, and one log line, from the driver ("this image cannot detect an
unknown-unicast storm ...") and from l2d.

#### 11.24.4 The image and the proof (2026-11-14/15)

**The image: `igmp-202511.20261115-6c8eb1cf7`.** It is
`igmp-202511.20261114-42cccc87a` plus sonic-buildimage 0014, and its
`sonic-broadcom-legacy-th.bin` has sha256
`fa7ae7fffaa2628f13ecd6402f4ff7a3eb0ba1d525c4434307cf7fd409f120f3`.
- **How it was built.** The 42cccc87a work tree was resumed with 0014
  `git am`'d onto both of its trees (`RESUME=1 ./build.sh broadcom`); a
  fresh tree did not fit on the build host.
  - Two things make such a resume a no-op. slave.mk's deb rules depend on
    `.platform` and their dependencies' installs, not on a package's
    files. The top-level Makefile calls an existing `target/*.bin` up to
    date without entering the slave.
  - So the module deb and the three `.bin`s were moved aside first.
  - A `<deb>-clean` target run outside build.sh walked the whole bookworm
    phase and rebuilt libswsscommon. That was stopped, and its dependents
    were rebuilt by the image stage (125 min).
- **0014 is in the image itself:** the `.bin`'s root filesystem has
  `usr/lib/modules/6.12.41+deb13-sonic-amd64/extra/linux-bcm-genl.ko`
  beside `linux-knet-cb.ko`, and its `/etc/init.d/opennsl-modules` loads
  and unloads it. `build_version` is `igmp-202511.20261115-6c8eb1cf7`.
- **The install** (`sonic-upgrade.sh arm`, reboot, `confirm`; the
  `f2e25f54c` slot went, `42cccc87a` is the fallback):
  - rebooted at 02:59:08 UTC and confirmed at 03:07:29;
  - `linux_bcm_genl` was loaded at boot, its `/proc/bcm/genl/netif` named
    all 32 front-panel netifs, and the bridge's deb came back.

**The proof, live, 2026-11-15 03:20-03:24 UTC** (20:20-20:24 MST), under
the shared switch lock.
- **The setup.** Cage 15 (`Ethernet56` = `ce14`, empty) was taken out of
  Vlan4094 with its seven reserved static groups (§11.21's lesson) and
  put alone, untagged, into a test Vlan3999, with spanning tree off on the
  port (bpdu filter). Then MAC loopback was turned on.
  - The bridge was stopped for the run and l2d replaced by this branch's.
  - Storm control was `unknown-unicast 1000` kbps.
  - 64-byte frames were sent from the CPU (record:
    `sonic-igmp-202511.20261115-6c8eb1cf7/ucast-samples.ndjson`, the psample
    messages as l2d reads them, scrubbed).
- **Sampling started** within 1 s of the storm-control row: `sampling
  Ethernet56 at 1:256`, `sflow feature on`.

| Sent | Samples | l2d |
|---|---|---|
| A: 60 000 frames to an unknown MAC over 15 s (4000/s, 2x the limit) | flooded (OIFINDEX 0xffff), the share of A and B: 273 samples together | **storm 5 s in**: `219546 bytes/s of flooded unicast over 5 s (67 samples), limit 1000 kbps (125000 bytes/s)`; over after the flood: **15 745 frames above the limit** |
| B: 22 000 to it over 15 s (1450/s, 0.74x) | flooded | nothing |
| C: 60 000 to a MAC in the FDB on the port itself (static) | 241, OIFINDEX = the port's ifindex (forwarded, then dropped as the ingress port) | nothing |
| D: 60 000 to the switch's own MAC | 254, flooded (no interface in Vlan3999) | nothing (own MAC) |
| E: 20 000 broadcast | 75 | nothing (group address) |

A sent 60 000 - 15 × 1953 = 30 703 frames above the limit; the count is
half of that, the window starting it late: a lower bound, as designed.

- **The driver's read.** The worktree build's `-collect-once` on the
  switch reported port 15 `StormDropsUnicast` 15745 and every other port
  0 (`collect-ucast-storm.txt`, scrubbed; `collect.txt` before the run).
  `TestWireContractSonicUnicastStorm` sets exactly bit 4 on port 15 from
  that pair and clears it with no further growth.
  `TestL2dUcastStormOverLiveSamples` replays the samples: the same start
  line, 16 132 frames (live: 15 745; the replay ticks on the capture's
  seconds), the sFlow rows written in order and taken out, the feature
  last.
- **Restored.** The storm control went first. l2d then deleted its sFlow
  rows and turned the feature off 8 s later (`unknown-unicast sampling
  stopped, sflow feature off`): no SFLOW key in CONFIG_DB or APPL_DB, the
  container stopped, no `queue2_group1` in APPL_DB.
  - Loopback off, Vlan3999 and the static FDB entry gone, `Ethernet56`
    untagged in Vlan4094 with its seven groups back.
  - Main's l2d back (sha256 unchanged), `l2.json` unchanged.
  - The bridge's first reconcile changed 0 of 32 ports.
- **The controller** recorded `DEVICE_UNREACHABLE` for the DX010 during
  each of the two runs with the bridge stopped (03:11:51, 03:23:51 UTC),
  and `DEVICE_RECONNECTED` after the reboot (03:03:06).
- **A first run** at 03:09 sampled nothing (the APPL_DB-only design above).
  The CoPP row tried by hand then is what set orchagent's error flag on
  this boot (§11.24.3).

#### 11.24.5 Not exercised, and what a deploy has to show

Not exercised:
- a cabled traffic source;
- a forwarding port with other VLAN members (the flood went nowhere);
- frame sizes other than 64 bytes;
- a rate high enough to reach the CoPP cap;
- storm control pushed from the UI with the detection running (done
  through the deployed build 2026-11-15, below; no storm);
- the operator's own sFlow;
- a breakout of a sampled cage;
- split cages and PortChannel members;
- **the controller's "Incoming unknown unicast packets ... exceed the
  Storm Control threshold" alert**: the build is deployed, the alert
  needs a storm (below).

**Storm control from the UI through the deployed build (2026-11-15
17:40-17:42 UTC, main f3d3572, Network 11.0.99, under the test lock).**
Port 16 (Ethernet60, an empty cage) in the port editor: Advanced Manual,
Storm Control, Unknown Unicast 1 %. The push carried
`switch.port.16.stormctrl.{status=enabled,type=level,ucast=1}` and nothing
else new; the bridge ran `sudo config interface storm-control add
Ethernet60 unknown-unicast 1000000`, and 3 s later l2d logged `sampling
Ethernet60 at 1:78125 for unknown unicast`, `sflow feature on for
unknown-unicast sampling` and `reading psample samples`: `FEATURE|sflow`
`state enabled`, CONFIG_DB `SFLOW|global`, `SFLOW_SESSION|all` and
`SFLOW_SESSION|Ethernet60`, and `/etc/sonic/disunified/l2d-sflow` present.
With no traffic, no storm. Removing the level (Storm Control off, Advanced
back to Auto) pushed the three keys away; the bridge ran `storm-control del
Ethernet60 unknown-unicast`, l2d logged `unknown-unicast sampling stopped,
sflow feature off`, and `FEATURE|sflow` was `disabled` again with no
`SFLOW*` row, no `PORT_STORM_CONTROL` row and no marker. The UI left the
port's override expanded with every default; it was put back on its
pre-test copy through REST. Both pushes are replayed in
`TestReplaySonicDX010Network11`.

Still not exercised: a storm against the level. A storm needs a source
above the level: UniFi's lowest, 1 % of 100G, is
far above what the CPU can send. A PR for the alert needs a cabled
source on a test port in a VLAN of its own and the alert read from the
controller's device alerts.

### 11.25 A lane aggregate's STP settings, and deleting a PortChannel an ACL table binds (2026-11-15)

Two leftovers of the lane switch (`docs/drivers/breakout.md` §14 items 4
and 5), closed on branch `claude/lane-stp-ea4897`, checked by hand on the
DX010 (`igmp-202511.20261115-6c8eb1cf7`, sonic-swss 0004) on empty cage 29
with the deployed bridge stopped for five minutes. The record is
`docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/portchannel-del-acl-20261115.txt`.

**11.25.1 Every bridge port a cage makes is named to l2d.** `l2SetPorts`
took the PortChannel of a cage's *first* interface only. A lane aggregate
that leaves lane 1 out (the controller's Flex XG push puts lanes 2-4 in
`lag=1`: `PortChannel<100*cage+1>`) is a kernel bridge port of its own, and
l2d was never told about it: mstpd ran it at its defaults, and the lanes in
it were named instead (harmless, they are not bridge ports). Now each
interface maps to its own bridge port -- itself, or its PortChannel -- and
every one takes the cage's settings. **The settings are the cage's, from
the parent's push**: the lane switch claims no STP (`breakout.md` §0), so
the controller offers none on it and the parent's push is the only one
carrying them; an operator's BPDU guard or loop protection on the cage
covers every lane, in an aggregate or not. `ApplyLanes` derives l2d's
per-port intent again from the parent's last push after a lane batch
(`l2Refresh`), so an aggregate the lane switch makes or dissolves gets its
settings in the same apply. Live, through a probe built from the branch
(the real `l2SetPorts`/`applyL2` on the switch, every other port as
`l2.json` had it, cage 29 with edge enabled, BPDU guard and loop
protection): before it, mstpd ran `PortChannel2901` at `admin edge port no`,
`bpdu guard port no`; after, `l2.json` named `Ethernet112` and
`PortChannel2901` with the cage's settings and both under loop protection,
l2d logged `settings: Ethernet112 admin edge on; Ethernet112 bpdu guard on;
PortChannel2901 admin edge on; PortChannel2901 bpdu guard on`, and mstpd and
STATE_DB `DUI_STP_PORT_TABLE|PortChannel2901` read admin edge, oper edge and
BPDU guard yes. Test: `TestLaneAggregateTakesTheCageSTP`.

**11.25.2 `config portchannel del` of a bound PortChannel.** The CLI's
`remove_portchannel` checks VLAN memberships and members, never ACL_TABLE
(202505 and 202511 source), and the driver's tables bind every PortChannel.
Live, with `DUI_IP_ACL` bound to `Ethernet112,PortChannel2901` and the
join's `dissolveLanes` as built (members out, the VLAN out, the delete):
- the delete returns 0; CONFIG_DB `PORTCHANNEL`, APPL_DB `LAG_TABLE` and
  STATE_DB `LAG_TABLE` lose the PortChannel and teamd removes the netdev;
- **the table keeps the name** (`ports@` still `Ethernet112,PortChannel2901`,
  status Active) and **orchagent keeps the LAG**: binding a table raises the
  port's ref count (`bindAclTable`), and `removeLag` refuses while it is
  above zero -- `Failed to remove ref count 1 LAG PortChannel2901`, retried
  about seven times a second (254 times in 37 s), the ASIC_DB LAG object
  still there;
- **the join's breakout is refused**: sonic-yang loads the whole CONFIG_DB
  first and `ports` is a leafref to PORT or PORTCHANNEL -- `libyang[0]:
  Invalid value "PortChannel2901" in "ports" element ... Failed to break out
  Port. Error: Failed to load the config. Error: ConfigMgmtDPB Class creation
  failed`, exit 1. Under the batch's `set -e` the join would have failed on
  every cycle.

Rebinding the table without the PortChannel (`config acl add table
DUI_IP_ACL L3 -p Ethernet112 -s ingress`) let orchagent's next retry remove
the LAG (`unbind: ... port is unbound from DUI_IP_ACL`, then `Remove LAG`).
**The fixed order rebinds before the delete**: members out, the VLAN out,
`plan.aclUnbind` of the PortChannels about to go, `config portchannel del`,
the breakout. Re-run from scratch on the same cage: every step exit 0, no
`removeLag` error, no LAG in ASIC_DB, the cage `1x100G[40G]`, and `-f`
taking the old lane out of the table. Both paths that delete a PortChannel
do it: `dissolveLanes` (the join of an owned cage) and `lagJoins`' emptied
or orphaned PortChannel (the parent's own aggregate taken apart, where
nothing was refused but orchagent held the LAG until the post-batch
`applyACL`). The plan's model of a breakout now also takes the old ports
out of every ACL table, as `-f` does, so `applyACL` after the batch binds
the new port in the same apply. The FixtureRunner models all three facts:
the delete passes and leaves the table naming it (`HeldLAGs` until no
table binds it), and the breakout refuses a dangling `ports@` entry with
the live message. Tests: `TestLaneJoinUnbindsTheACLTables` (fails with the
live refusal without the fix), `TestACLTablesFollowThePorts` (the parent's
own aggregate).

Not covered here: a table that is not the driver's binding a PortChannel
the plan deletes (§11.25.4 since), and a driver table bound to nothing but
the PortChannel (noted: it cannot be rebound to nothing; it does not occur
while the driver binds every port).

**11.25.3 After the test.** The deployed bridge's start-up reconcile restored
cage 29's MTU, VLAN and snooping static groups. The only CONFIG_DB
difference against the snapshot was `PORT|Ethernet112 dhcp_rate_limit 300`,
which the breakout drops (§11.17: nothing enforces it); it was written back
by hand and saved. Running and saved config then matched the snapshot,
and `l2.json` was byte-identical. Side observation: after the split, lane 1
could not leave Vlan4094 by hand ("remove all associated static Layer 2
multicast group entries first") though nothing had written its static groups
since the breakout, and the start-up reconcile's IGMP apply logged `7 rows,
0 members set, 7 removed`, then (after the port apply) `7 members set`.
Chased the same day, below.

**A breakout keeps the cage's multicast rows (live 2026-11-15).** On empty
cage 29 again, by hand with the bridge stopped for 90 s, recorded in
`docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/breakout-l2mc-20261115.txt`
(CONFIG_DB, APPL_DB, STATE_DB and ASIC_DB for the cage at each step,
syslog):

- **The split** (`config interface breakout -f -y Ethernet112 '4x25G[10G]'`):
  ConfigMgmt deleted `VLAN_MEMBER|Vlan4094|Ethernet112` and the port, and
  nothing else. All seven `L2MC_STATIC_MEMBER|Vlan4094|<group>|Ethernet112`
  rows stayed, and every `L2MC_STATIC_GROUP` row still listed `Ethernet112`
  in `static-members@`. Lane 1 has the parent's name, so the rows now named
  the new 25G port. With lane 1 put back in Vlan4094 (as the driver's plan
  does), `config vlan member del 4094 Ethernet112` was refused: the
  observation above.
- **The join** with lanes 2-4 in Vlan4094 and their 21 rows (as the
  driver's IGMP apply leaves a split cage): ConfigMgmt deleted the four
  VLAN memberships and the four ports; the 21 rows of `Ethernet113`-`115`
  stayed, naming ports that no longer exist, and the joined `Ethernet112`
  inherited lane 1's seven, so it too was refused leaving the VLAN.

**The cause is the image kit, not upstream's model.** #24327's
`sonic-igmp-snooping.yang` makes `out-intf`, `port`, `mrouter-intf` and
`static-members` leafrefs to PORT or PORTCHANNEL, and with them
`config_mgmt`'s `_deletePorts` (`find_data_dependencies`, the schema
backlinks of `PORT_LIST/name`) would delete these rows with the port, as it
does an ACL table's `ports`. The kit's sonic-buildimage 0010 made every one
of them a plain string (§8.6: the leafrefs broke the generic config updater's
tests), and the installed model has none (`grep -c leafref` 0): `sonic_yang`
over the running config finds only the VLAN membership as Ethernet112's
dependency. Putting the leafrefs back would bring back what 0010 fixed, so
the image stays as it is.

**What the rows do.** orchagent never reads them. l2mcmgrd reads
`L2MC_STATIC_MEMBER` (and `L2MC_MROUTER`) on a change, and again from
CONFIG_DB whenever a port's VLAN membership becomes ready while the port is
up (`VLAN_MEMBER_READY`, `LINK_STATUS up`): so a kept row is live
configuration for the new port of the same name the moment it is in the VLAN
and up, written for a port that no longer exists, and a vanished lane's rows
match nothing until a lane of that name comes back. `L2MC_STATIC_GROUP` is
the CLI's bookkeeping only. On the empty cage no L2MC member reached the
chip at any step (members are programmed on oper-up ports only), and with
sonic-swss 0004 a breakout of a live cage drops the old port's members with
its VLAN membership (§8.15) in any case. The driver itself was never
refused: its plan deletes a port's static rows before any VLAN removal
(§8.14), and its IGMP apply after the batch removed the rows of vanished
lanes, because its own model of the breakout kept them too. But the
batch's `config save` wrote them to `config_db.json` first, they stayed for
good if the IGMP apply did not run (`options.igmp` off, an error earlier in
`ApplyPorts`), and the rows of the parent's name were never rewritten for
the new port.

**The fix (the driver; no image change).** `l2mcUnbind` (`igmp.go`), run in
the breakout's batch beside the ACL rules' `dropInPortRules` and before
`config interface breakout`: every `L2MC_STATIC_MEMBER` and `L2MC_MROUTER`
row naming one of the cage's interfaces is deleted and each static group's
list is rewritten without them (a group left empty is deleted), in one
`ConfigDBConnector` call (`python3 -c ... disunified-l2mc '<ops>'`, logged as
`static groups: N rows, ..., M removed`). That is what `-f` would do with
the leafrefs in place, and it is §8.15's order: the statics go before the
breakout takes the port's last VLAN. `applyIGMP` after the batch then writes
the new ports' rows, lane 1's included. The FixtureRunner's breakout keeps
`cfg_l2mc` as the switch does and records every L2MC row a `config save`
writes for an interface without a PORT or PORTCHANNEL row
(`SavedDangling`). `TestReplaySonicDX010BreakoutSeries` (split, lane speeds,
2x50G, join) checks the unbind, that lane 1's rows are written again after
the split, and that no save writes a dangling row; without the fix it fails
on all three. Not covered: hand-made `MLD_L2MC_*` rows (the driver writes
none, and the collector does not read them), and a PortChannel the plan
deletes: its member rows go with its VLAN membership as before, its
`L2MC_MROUTER` row and its place in the groups' lists until the IGMP apply
after the batch.

**Live through the fixed build (2026-11-15, 18:53-19:00 UTC).** A test deb of
the branch (3d2d07e) on the DX010 under the lock, cage 29 changed through
the controller's REST as the UI saves it (same record, section 7):

| # | Push | The batch | Result |
|---|---|---|---|
| 1 | 25G | `disunified-l2mc (static groups: 7 rows, 0 members set, 7 removed)`, then the breakout to `4x25G[10G]`, the lanes' intent; the IGMP apply after it `28 members set, 0 removed` | lane 1's rows written anew (21 before the fix); 28 rows, every lane in each group's list |
| 2 | native DMZ-FutureUse (4000) | per lane: its 7 static rows, `vlan member del 4094`, `add -u 4000` | all four lanes in Vlan4000, new bridge ports admin true, no L2MC row names the cage |
| 3 | native Default | per lane: `del 4000`, `add -u 4094`; IGMP `28 members set` | back as after step 1 |
| 4 | 100G | `disunified-l2mc (... 28 removed)`, then the join; IGMP `7 members set, 0 removed` | nothing left for the IGMP apply to remove: no save wrote a dangling row |
| 5 | the pre-test override (auto) | none | 0 of 32 ports changed |

Then the deployed build went back from `/host/disunified`; the join's lost
`dhcp_rate_limit 300` (§11.17) was written back and saved, and running and
saved CONFIG_DB, `l2.json` and the controller's 32 port overrides matched
the copies taken before.

**The member deletions after the split were another port's (explained
2026-11-15).** Section 5 of the record shows orchagent's
`doL2mcMemberTask ... op DEL` for the new `Ethernet113` in five dynamic
groups, two seconds after the split. They came from l2mcd, and the
memberships were not Ethernet113's: they were those of **Ethernet0**, the
uplink then, and l2mcd took Ethernet0's receive sockets with them. The
evidence is l2mcd's own log of the same run
(`sonic-igmp-202511.20261115-6c8eb1cf7/l2mcd-breakout-index-20261115.txt`,
from `/var/log/l2mcd.log`) read against its source (`port/l2mcd_utils.c`,
`l2mcd_port_list_update`, the kit's sonic-l2mcd 0001-0010):

- l2mcd numbers ports in its port db. At start, every EthernetN is
  4096 + N (`port_ifname_db_init`): Ethernet0 is 4096, Ethernet64 4160 and
  Ethernet112 4208. A port that is deleted loses its name there
  (`portdb delete`). When a port of that name comes back, l2mcd numbered it
  **4096 + its kernel ifindex**, which is the start index of
  Ethernet<ifindex>, another port's.
- At 10:48:27, l2mcmgrd reported Ethernet113-115 before their netdevs existed
  (an earlier join that day had deleted their names). `if_nametoindex` gave
  0, so all three took 4096, Ethernet0's index, and the port db's name for
  4096 became the last of them.
- 0.1 s later, Ethernet113's link event (`kif:62 ... if_tree not found,
  recreate`) found Ethernet0's tree (kif 35) at 4096. The daemon's
  "breakout" branch (a tree at the port's index whose ifindex differs) took
  it for the lane's own old tree: it closed its IGMP and MLD sockets, dropped
  it and made 4096 Ethernet113's. The re-created port's first state update
  then removed every membership 4096 held in Vlan4094 -- five dynamic groups
  and the router-port entry -- and wrote the deletes to APPL_DB as
  `Ethernet113` (`Group-DEL key:Vlan4094:0.0.0.0:224.0.1.60:Ethernet113`
  ..., `Vlan4094:Ethernet113 mrouter entry deleted`). orchagent had no such
  member, so APPL_DB and the chip kept Ethernet0's own rows. Nothing showed;
  the damage was in l2mcd's state.
- Ethernet115's link event (kif 64) numbered it 4160, Ethernet64's index,
  and did the same to Ethernet64's tree (cage 17, in no group then). The join
  at 10:49 deleted the lanes again and took index 4160's name with them.
- Ethernet0 was up the whole time (`portsyncd` logged no down). It had no
  tree and no receive socket from 10:48:27 until its next link event at
  11:16:46, 28 minutes. That recovery lost Ethernet0's own name and numbered
  it 4131 (4096 + 35), so its tree and its name sat at different indexes
  (`Err kif_to_if: if:4131`). One second later, at 11:16:47, l2mcd died
  ("terminate called after throwing an instance of 'std::logic_error'",
  "basic_string: construction from null is not valid", a core in
  `/var/core`), and systemd restarted the container at 11:19:48: the
  "restart" earlier notes took for a hand one (§8.13 has the cause, from its core, and the fix, 0014).
  Ethernet64 had no tree until then.

**So the deletions are not confined to the cage being split.** Any breakout
or join that re-creates a port can give another port's index to a lane:
Ethernet0's (on this switch a member of the uplink aggregate since
2026-11-15) whenever a lane is reported before its netdev, and Ethernet<k>'s
for a lane created as ifindex k. A port that loses its tree this way keeps
its members in the chip as they were (the deletes went out under the lane's
name), but l2mcd no longer tracks them. l2mcd also hears nothing on that port
until its next link event: no report, no leave, no query. A receiver's
leave is never seen, and a router port learned only from queries there is
not refreshed. The first split after l2mcd starts is the least exposed:
lanes 2-4 still have their start-up names, and only lane 1 takes a kernel
number (in the run above, its link event came first: 4157, which no port had
then; had l2mcmgrd's report come first, 4096). Every later split or join
re-creates names that the previous one deleted. Under AddressSanitizer the
harness case below also hits a heap use-after-free on this path; the switch
logged nothing of the kind. That use-after-free is not the breakout's: it is in
`mcgrp_stop_phy_port()` (`src/igmp.c` 667-680 at upstream `651a13b`), which
frees a port's membership with `mcgrp_destroy_mbrshp_entry()` and then reads
and clears its `pims_mbr_flags`. Every link-down of a port that is a group
member runs it, breakout or not: a scratch harness case (a member port's
link-down, nothing else) hits it on upstream, on 0001-0012 and on upstream
with both fixes (2026-11-15). Nothing allocates between the free and the
write, so glibc's heap survives it, which is why the switch is quiet. Fixed
by sonic-l2mcd 0013 (§8.13, with the harness cases and what the running
image does).

**Not the driver's.** The driver runs `config interface breakout` and nothing
of l2mcd; the numbering happens inside l2mcd on any port re-creation, by the
CLI or anyone else. The only driver-side workaround would be to restart l2mcd
after every breakout, which drops every port's learned groups. It is not done.

**The fix: sonic-l2mcd 0011 and 0012 (the image kit).**
`contrib/sonic-image/series/sonic-l2mcd/0011-*`:
- An EthernetN that the port db does not hold gets 4096 + N again, whatever
  its ifindex, so each lane gets its own index back (Ethernet113 4209).
- Any other name (PortChannelN) is numbered by its ifindex as before, but
  only once its netdev exists and only if no other name holds the index.
  Otherwise the update waits for the port's link event.
- The "breakout" branch still closes and drops the re-created port's old
  tree, but the name keeps its index, and a tree of another name at the index
  is left alone with a log line.

`0012-*`: `l2mcd_libevent_destroy` called `event_del` and never freed the
event, so 128 bytes leaked for every port socket closed (two per re-created
port). Both callers drop the tree, the pointer's only other holder, right
after, so it now calls `event_free`. Upstream (`jasper-hou-micas/sonic-l2mcd-micas`
`dev`, still 651a13b on 2026-11-15) has the same code, and both patches
apply to it as they are (`git apply --check` on 651a13b): they are the
upstream draft. Nothing has been posted.

**Tests.** The fuzz harness's new regression case `port-list-breakout`
(`contrib/sonic-image/l2mcd-fuzz/harness.c`) runs the sequence through
l2mcd's own IPC handler and link-event function, with dummy links in the
container. A victim Ethernet40 is up, in Vlan100 and a member of 239.1.2.3
(this namespace's fallback tunnel devices hold the low ifindexes). An earlier
join deletes Ethernet9 and Ethernet10. Then the split: Ethernet9 is reported
before its netdev exists, and Ethernet10 is created as ifindex 40, its link
event first, as on the switch. The case checks that 4096 is still Ethernet0's
name; that Ethernet40 keeps its index, name, tree, kif and packet sockets and
that no group member is deleted; and that each lane has its own index and
tree. (Until 2026-11-15 the harness's APPL_DB stub counted deletions only in
`l2mcsync_del_l2mc_entry()`, which nothing calls; l2mcd sends them through
`l2mcsync_add_l2mc_entry()` with `op_code` 0, so the deletion check could not
fail. With the stub counting them, the results below are unchanged: no
deletion with 0011, and on upstream with 0013 alone, which gets past the
use-after-free, the case now shows the fault itself: Ethernet10 holds
Ethernet40's index and one member deletion goes out under its name.) On the
build host's bookworm slave:

| Tree | `port-list-breakout` |
|---|---|
| 0001-0010 (the image) | heap-use-after-free in `pims_is_pim_snoop_mbrship`, from `l2mcd_portstate_update` -> `l2mcd_port_state_notify_handler` -> `mcgrp_stop_phy_port` |
| + 0011 | every check passes; LeakSanitizer: 4 libevent events (512 bytes) |
| + 0012 | clean |

A second case, `port-delete` (added the same evening), shows 0012 on its own: a
port created and then deleted from the port list leaves its two events
allocated (LeakSanitizer: 2 libevent events, 256 bytes) without 0012 and is
clean with it, on upstream `651a13b` as on the series. With 0012, all 30
regression cases are clean.

The same cases on upstream alone (`651a13b`, the two upstream branches of
`docs/prior-art.md` §6.1 built by the same harness): `port-list-breakout` is
the use-after-free above on `651a13b` and with 0012 alone, the 4-event leak
with 0011 alone, and clean with both; `port-delete` leaks on `651a13b` and
with 0011 alone and is clean with 0012 alone. No other case changes result
between `651a13b` and either branch (the cases the six earlier PRs fix still
fail there, as they should). The IGMP and MLD fuzzers ran
240 s with 4 workers each on the patched tree (876 k and 718 k runs, no
crash). gcc `-Wall` with the daemon's flags gives no new warning in the two
changed files. The kit's `dry-run.sh` passed every check, run with private
work branches made from the series: the Mac's shared work clones lag it,
sonic-l2mcd at 0008 and sonic-swss without 0004.

**Not reproduced live on the current image, on purpose.** The 10:48 log is
the reproduction, on the same image. A second split now would again take some
other port's tree. If l2mcmgrd's report wins the race against the netdev,
that port is Ethernet0, the uplink aggregate's member; otherwise it is likely
an empty cage's (new ifindexes start at 52 after the 13:17 boot: Ethernet52,
Ethernet56). Only an l2mcd restart or that port's own link event repairs it.

**What an image install with 0011 and 0012 has to prove** (on the DX010,
under the lock, on an empty cage, the bridge stopped):
1. Before: save `docker exec l2mcd l2mcd_ctl -s /tmp/dui-l2mcd-c2.txt -c 2`
   (the port tree: interface, aggregate, index, kif, oper, RxPkts) and
   `-c 11`.
2. Split, join, split, join (the second cycle re-creates lanes whose names
   the first deleted, the 10:48 case).
3. `/var/log/l2mcd.log` for that window: each re-created EthernetN shows
   `portdb add port:EthernetN, index:<4096+N>`, and no `delete2`, `Err
   kif_to_if`, Group-DEL or `mrouter entry deleted` names a lane in a group
   it never joined.
4. The port tree again: every port outside the cage has the index, kif and
   event pointer it had before; Ethernet0's and Ethernet4's RxPkts keep
   counting (queries from the uplink); the lanes are 4096 + N.
5. CONFIG_DB compared before and after, as always.

**11.25.4 A table that is not the driver's (2026-11-15).** Until then the
plan noted such a table and left it, so a PortChannel it bound kept its
name there after `config portchannel del`: orchagent held the LAG and the
next breakout's YANG check refused (§11.25.2), and a joining lane it bound
refused `config portchannel member add`. collect.py read only the `DUI_`
tables, so an operator's own table was not even seen.

**The decision follows Clint's rule** (UniFi is the source of truth for
what it configures; a hand-made table is not UniFi's): the aggregate is the
controller's and wins, and the table is the operator's and keeps
everything but the names the aggregate takes away.
- collect.py's `cfg_acl_table` reads every `ACL_TABLE` entry (type, stage,
  ports, description; ACL_TABLE holds no secret). `ACL_RULE` and STATE_DB
  stay `DUI_` only, and only the driver's tables are checked for `Active`.
- **Only `ports@` is written**: `sonic-db-cli CONFIG_DB hset
  'ACL_TABLE|<t>' 'ports@' '<list>'`, never `config acl add table`, which
  replaces the whole entry (its description, any field the CLI does not
  know). orchagent diffs the new port set against the old and unbinds
  what left (`updateAclTablePorts`).
- **What takes a name's place**: a joining lane's PortChannel (orchagent
  keeps a port it does not have yet pending, so the write can precede
  `config portchannel add`), and for a PortChannel about to be deleted the
  members this batch took out of it. The operator's rule stays on the same
  traffic.
- **A table left with no port loses the field** (`hdel ... 'ports@'`): the
  state SONiC's own `config interface breakout -f` leaves when it removes
  a table's last port (config_mgmt deletes the leaf-list value).
- Each rewrite is noted: `ACL table <t> (<type> <stage>) is not the
  driver's: <names>, which <why>, taken out of its ports (now <list>)`.
- A lane switch's join (`dissolveLanes`) passes no replacement: the lanes
  go with the breakout that follows, which takes them out of every table.

**Live on the DX010** (2026-11-15 11:22-11:23 MST, empty cage 32 split
4x25G, lanes in MAC loopback, the bridge stopped; the plan's commands run
by hand on a hand-made `OPTEST` L3 table with a description of its own;
`sonic-igmp-202511.20261115-6c8eb1cf7/foreign-acl-live-20261115.txt`):
- **Dissolve.** `OPTEST` bound `PortChannel99` (one lane a member;
  `config acl add table` refuses a PortChannel without members) and
  another lane. The plan's order: member out, the `ports@` hset with the
  former member in the PortChannel's place, `config portchannel del`.
  orchagent: `Deleting port PortChannel99 from ACL list OPTEST`, `port is
  unbound`, `Adding port Ethernet126`, `Successfully updated existing ACL
  table OPTEST`, then teamd's stop and `Remove LAG PortChannel99`, with no
  `ref count` refusal. The description stayed, and the table stayed Active.
- **No port left.** The `hdel` of `ports@` unbound both lanes, and the
  table stayed Active.
- **Join.** `ports@` naming `PortChannel98` before it existed: orchagent
  unbound the lane, and once the PortChannel was made, logged `Bound port
  PortChannel98 to ACL table OPTEST`. `config portchannel member add` of
  the lane was not refused.
- **Afterwards.** The test table, both PortChannels and the split were
  gone, and no LAG or ACL table was left in ASIC_DB. The bridge's start-up
  reconcile restored the cage. CONFIG_DB matched the snapshot taken
  before, except `PORT|Ethernet124 dhcp_rate_limit 300`, which the
  breakout drops (§11.25.3); it was written back and saved.

Test: `TestForeignACLTableFollowsTheAggregate` (an operator's table made
through the FixtureRunner's model of `config acl add table` on the DX010
capture: the join, the split, only `ports@` changed, the no-port case, no
held LAG).

### 11.26 802.1X port authentication: hostapd under disunified-l2d (2026-11-15)

Clint, 2026-11-15: "Build it." This is §11.18.3's assessment carried out:
an authenticator, a data plane that holds an unauthorised port closed and
admits each authenticated host, the controller's claim and push, and the
failed attempts as anomaly 128.

#### 11.26.1 What the ECS does

Nothing to copy. Read-only on 2026-11-15:

- its controller record does not claim DOT1X (`switch_caps.feature_caps`
  bit 2 clear) and its `port_table` carries no `dot1x_*` key, while the
  site's UniFi switches that claim it report `dot1x_mode` and
  `dot1x_status` per port (the controller's device list);
- nothing on the switch is an authenticator: no `hostapd`, `authmgr`,
  `pacmgr` or `mabmgr` on the host or in any container, no PAC table in
  CONFIG_DB, APPL_DB or STATE_DB;
- its `ERROR_DISABLE|dot1x` row is configuration only, as every row of
  that table is (§11.20), and its informs report the off values.

So the departure from the ECS's pattern has a plain reason: the ECS has
no 802.1X.

#### 11.26.2 The choice: hostapd (wired) under l2d, not `src/sonic-pac`

Two designs were open (§11.18.3):

1. **Broadcom's Port Access Control** (`src/sonic-pac` in 202511):
   authmgr, pacmgr, hostapdmgr, mab, mabmgr, paccfg, pacoper and their OS
   layer fpinfra, 149 C/C++ files and about 66,000 lines, with no build
   rule, no container, and no consumer in sonic-swss (nothing reads
   `PAC_PORT_TABLE`, so no code turns its verdict into ASIC state).
   Building it means porting it to 202511's libraries, writing the
   orchagent consumer, and installing a new image.
2. **hostapd's wired driver run by l2d**, the way mstpd runs: upstream's
   own authenticator (the one sonic-pac's hostapdmgr drives too), built on
   the switch, with l2d turning its verdicts into the mechanism the MAC
   filter already uses and §11.12 verified on this ASIC (`learn_mode drop`
   and a static FDB entry per admitted MAC).

The second is built. Reasons:

- what PAC adds over hostapd is a port state machine and MAB, and both
  map onto pieces this driver already runs and has verified: the learn
  mode and static FDB entries, and l2d's packet sockets on a port's netdev;
- it needs no new orchagent code: every write is to a table orchagent
  already consumes (`PORT_TABLE` learn mode, `FDB_TABLE`, CoPP);
- it runs on the installed image, with no reboot (Clint asked for none on
  2026-11-14), built on the switch from the upstream release as mstpd is,
  so the bridge redistributes no binary;
- **the image kit needs no patch for it.** The one thing the image lacked
  was the EAPOL trap, and that is a CoPP row (below), as STP's is. So no
  image was built for 802.1X: there is nothing to put in one.

#### 11.26.3 As built

**hostapd** (`build-hostapd.sh`): 2.11 from `w1.fi/releases`, pinned by the
sha512 Alpine's aports pin for the same file, statically linked against
musl in the pinned alpine container mstpd is built in (42 s on the DX010,
616 KB). Configured for this job only: the wired driver, a RADIUS client,
the control interface; no nl80211, no TLS library (the EAP methods run on
the RADIUS server; the switch relays them), no WPS or DPP. EAP-MD5 and the
integrated RADIUS server are built in so the switch can run a RADIUS
server of its own for a test. A tarball placed beside the script is used
when the switch cannot fetch it (the hash decides either way). The driver
starts the build in a transient unit (`disunified-hostapd-build`, retried
every 10 minutes after a failure) the first time a push controls a port,
and installs `/usr/local/sbin/hostapd` and `hostapd_cli`.

**The driver** (`l2.go`, `portfeat.go`):

- claims 802.1X (`Capabilities.Dot1X`, `feature_caps` 2) where l2d can run
  and hostapd is installed or docker can build it, and the collector reads
  the port features' sections (the learn mode is theirs);
- holds every port in a closed mode (`auto`, `multi_host`, `mac_based`,
  `force_unauthorized`) closed in CONFIG_DB with the MAC filter's
  `learn_mode drop`, from the first push, before any authenticator runs,
  and across a reboot; a force-authorised port stays open;
- leaves an aggregate's members open and out of l2d's config (hostapd
  authenticates one netdev; no push for 802.1X on an aggregate is
  captured);
- writes l2d's `dot1x` section (each controlled interface and its mode,
  the RADIUS servers' addresses and ports, the switch's management
  address as the NAS address) and the servers' secrets, one per line, to
  `/etc/sonic/disunified/dot1x-radius` (root, 0600). l2.json is read by
  the login user (collect.py), so it holds no secret; the secrets file is
  written whenever this bridge has not written that content since it
  started, so nothing the bridge can read on the switch derives from a
  secret, and no log line carries one;
- keeps the static FDB entries l2d wrote for the hosts it admitted (the
  MAC filter's plan deletes every static entry it does not want);
- reads each port's mode from l2d's config, its admitted hosts and its
  failed attempts from STATE_DB `DUI_DOT1X_TABLE` (collect.py `l2_dot1x`)
  into `Port.Dot1X`, `Port.Dot1XAuthorized` and `Health.Dot1XFailures`.

**l2d** (`Dot1xGuard`, `l2d.py`), per controlled interface in a mode that
authenticates:

- the kernel chain `DUI_DOT1X`, jumped to first from INPUT and FORWARD
  (ahead of MAC locking's): the port's EAPOL accepted, each admitted
  source RETURNed, the rest of the port's frames dropped, so an
  unauthorised host cannot reach the switch's own CPU through a trapped
  frame (ARP, DHCP, IGMP) either; every write is one `ebtables-restore
  --noflush` transaction, and a chain the kernel lost is written again
  at the next sync (§11.26.12);
- the EAPOL trap `COPP_TRAP|dui_eapol` (`trap_ids eapol`, `queue4_group1`,
  `always_enabled true`) while any port authenticates;
- a hostapd of its own (`-B -P /run/disunified-dot1x/<port>.pid`, config
  0600 there: `driver=wired`, `use_pae_group_addr=1`, `eap_server=0`,
  `eap_reauth_period=3600`, the servers and secrets), events read from its
  control socket (`ATTACH`): `AP-STA-CONNECTED` admits the host (a static
  FDB entry in every VLAN the port carries, through ProducerStateTable as
  the MAC filter's, and a RETURN in the chain) -- on an `auto`
  (Credentials) port the first one opens the port to every host until the
  last one is gone (§11.26.9) --, `AP-STA-DISCONNECTED`
  removes it, and
  `CTRL-EVENT-EAP-FAILURE2` counts a failed attempt
  (`DUI_DOT1X_TABLE|<port>` `failures`, since boot, carried over a
  restart, and `last_failed_mac`);
- a port losing its link, or leaving the config, loses its admissions;
- hostapd outlives an l2d restart (the unit's `KillMode=process`), and the
  next l2d adopts it by its pid file, attaches, and reads its station list
  (`STA-FIRST`/`STA-NEXT`, `flags=[AUTHORIZED]`); an earlier l2d's
  admissions keep their FDB entries, their RETURN and an open Credentials
  port until that list (or, for a MAC-authenticated host, its idle timer)
  says otherwise; nothing an earlier l2d wrote is written again
  (§11.26.10);
- `--teardown` stops every authenticator and removes every admission, the
  chain and the trap; the ports stay closed (CONFIG_DB keeps the drop).

**The device layer** claims `feature_caps` bit 2 with `Capabilities.Dot1X`
and sets anomaly 128 when a port's `Health.Dot1XFailures` grew since the
previous inform, as it does for the storm, DHCP and MAC-locking counts. The
port reports `dot1x_mode`/`dot1x_status` `force_auth`/`authorized` for a
force-authorised port (the values the site's own switches report), and
since the evening of 2026-11-15 the other modes as §11.26.9 gives them
(not captured): `force_unauth`/`unauthorized`, `auto` for
Credentials and Multiple Credentials, `mac_based`, each `authorized` while
a host is admitted and `unauthorized` otherwise; `unknown`/`disabled` with
802.1X off.

#### 11.26.4 The controller's push (Network 11.0.99, captured 2026-11-15)

With the DX010's bridge claiming DOT1X (a test deb from this work,
`0.0.0+20261115.dot1x`, installed for the capture and replaced by the
deployed build after it), the controller offered 802.1X in two places,
and every setting below was restored afterwards:

- **the device** (Settings -> Advanced, with "Global Switch Settings"
  unticked so the device takes its own): "802.1X Control", with "RADIUS
  Server" UniFi Integrated or External (a RADIUS profile) and
  "Unauthorized VLAN" (a network). The site-wide switch is the
  `global_switch` setting's `dot1x_portctrl_enabled`, which was not
  touched. On the record: `dot1x_portctrl_enabled`, `radiusprofile_id`,
  `dot1x_fallback_networkconf_id`. The REST API refuses a direct change of
  the first (`api.err.Dot1xControlChangeNotAllowed`); the UI's steps undo
  it. `radiusprofile_id` stays set (to the Default profile) once written;
  it does nothing with 802.1X off;
- **each port** (Port Manager -> port -> Advanced: Manual -> Port Security
  -> "Client Authentication (802.1X)"), with a Method. The UI's own text:
  it "authenticates either a single wired device or each device
  individually using 802.1X credentials. Devices that do not support
  802.1X can be authenticated using their MAC address." On the port
  override: `dot1x_ctrl`, `dot1x_idle_timeout` (300).

The pushes (`controller-11.0.99/system_cfg-dx010-dot1x-*.txt`, scrubbed):

| UI | Push |
|---|---|
| 802.1X Control on (UniFi Integrated) | `switch.dot1x.status=enabled`, `switch.dot1x.dynvlan=disabled`, `switch.radius.auth.1.ip` (the gateway), `.port=1812`, `.secret`, and `switch.port.N.dot1x.mode=force_authorized` on every port |
| Method Credentials | `switch.port.N.dot1x.mode=auto` |
| Method Multiple Credentials | `multi_host` |
| Method MAC Address | `mac_based`, with `switch.port.N.dot1x.idle_timeout=300` |
| Method Always Unauthorized | `force_unauthorized` |
| RADIUS Server External (a profile) | the profile's server in the same `switch.radius.auth.1.*` keys |
| Unauthorized VLAN | **no key**: stored on the device record only; no push followed, nor after a `force-provision` |
| 802.1X Control off (Global Switch Settings) | `switch.dot1x.status=disabled` alone |

`unificfg` parses them (`Config.Dot1X`, `Port.Dot1XMode`,
`Dot1XIdleTimeout`), the loop maps them to `DeviceDesired.Dot1X` and each
`PortDesired.Dot1X` while 802.1X is on, and
`TestReplaySonicDX010Dot1X` replays all seven on the capture from the new
collector: force-authorised ports stay open and build nothing, each method
closes its cage and reaches l2d, hostapd is built once, External changes
the server, off opens every port and empties l2d's section, and nothing is
logged as unsupported or carries the secret. The scrubber replaces
`switch.radius.auth.N.secret` and `resolv.host.N.name` since.

What each method does follows §11.26.9 (not captured on this site):
`auto` (Credentials) opens the port to every host once
one has authenticated, `multi_host` (Multiple Credentials) admits each host
that authenticates and no other. (Until 2026-11-15 l2d read the UI's
"a single wired device" as one host per port, turning a second away.)

#### 11.26.5 Live, by hand (2026-11-15)

Image `igmp-202511.20261115-6c8eb1cf7`, empty cage 31 (Ethernet120, `ce30`)
in MAC loopback, the installed l2d paused for each window, under the test
lock; a test RADIUS server (hostapd's integrated one, EAP-MD5, one user, a
test secret) on 127.0.0.1; the supplicant a raw socket on the port's netdev
sending EAPOL from a test MAC and reading the replies the loop brought
back (`sonic-igmp-202511.20261115-6c8eb1cf7/dot1x-live.txt`):

- `COPP_TRAP|dui_eapol` put `SAI_HOSTIF_TRAP_TYPE_EAPOL` in ASIC_DB; an
  EAPOL-Start to the PAE group address came back from the chip to the
  port's netdev, **tagged with the port's VLAN** (4094); frames to a
  unicast or broadcast address from an unknown source did not (6
  `RPORTD`);
- **hostapd saw nothing while ebtables INPUT dropped the port**: the
  bridge hands a link-local frame to the port's protocol sockets only once
  NF_BR_LOCAL_IN has passed it (`ETH_P_ALL` taps see it earlier). Hence
  the chain's EAPOL ACCEPT first, for both the untagged and the 802.1Q
  form;
- the right password: EAP-Request/Identity, the MD5 challenge,
  EAP-Success, `AP-STA-CONNECTED`, `flags=[AUTHORIZED]`; the wrong one:
  EAP-Failure, `CTRL-EVENT-EAP-FAILURE2`, `dot1xAuthBackendAuthFails=1`;
- the candidate l2d's `Dot1xGuard` run alone beside the installed l2d:
  the chain and the trap, hostapd started; the authorised host admitted
  (APPL_DB `FDB_TABLE:Vlan4094:<mac>`, ASIC_DB `SAI_FDB_ENTRY_TYPE_STATIC`,
  the chain's RETURN); the refused host counted (`failures 1`); a second
  guard (an l2d restart) adopted the running hostapd and admitted the host
  again from its station list; an EAPOL-Logoff removed the admission from
  the ASIC; the teardown left no hostapd, chain, trap or entry;
- hostapd also hears its own frames come round the loop and makes a
  station of the switch's MAC; l2d ignores the switch's own MAC (no
  cabled port does this).

`TestL2dDot1X` replays hostapd's captured events and station replies
against l2d; `dot1x_test.go` tests the driver on a capture from the new
collector (`collect-dot1x.txt`).

#### 11.26.6 Not built, and not exercised

- **The Unauthorized VLAN and a RADIUS-assigned VLAN**: the first reaches
  no push on 11.0.99 (above), the second is `dynvlan`, disabled in every
  capture; neither is built.
- **Forwarding**: an admitted host's frames forwarded and an unadmitted
  one's dropped through the chip, with a second linked port (the MAC
  filter's open point too, §11.12).
- **An authentication against the gateway's RADIUS server**: an 802.1X
  accept and reject through the driver-run hostapd, and MAB's (§11.26.8),
  need a RADIUS user on the gateway's server (a site-wide change: Clint's
  call); a cabled supplicant needs a host on a linked port.
- **The controller's alert for 128**: the count has grown only in the hand
  runs, with the bridge stopped, so no inform has carried it yet.

The driver's own path, which this list held until the evening of
2026-11-15, ran live (§11.26.7), and MAB is built (§11.26.8).

#### 11.26.7 The driver's path live, 2026-11-15

Through the deployed build (`0.0.0+20261115.f3d3572`, which has §11.26.3),
from the UI on the DX010's record and cage 31, under the test lock,
17:51-18:06Z:

| Step | What the switch did |
|---|---|
| Global Switch Settings unticked, 802.1X Control on (UniFi Integrated, no Unauthorized VLAN) | l2.json gained `dot1x`: 32 ports `force_authorized`, the gateway as the server, the switch's address as NAS; `/etc/sonic/disunified/dot1x-radius` root 0600; nothing closed, nothing built ("0 of 32 ports changed") |
| Cage 31 to Credentials | `learn_mode drop` (CONFIG_DB and APPL_DB, saved); `disunified-hostapd-build` started and finished in 46 s, fetching from w1.fi over IPv4 (the default route was there); `COPP_TRAP|dui_eapol`; hostapd on Ethernet120 (`-B -P`, config 0600); `DUI_DOT1X` first in INPUT; STATE_DB `mode auto` |
| (cage 31 in MAC loopback, one EAPOL-Start from a test MAC) | the driver-run hostapd answered with EAP-Request/Identity; nothing was sent on, so no request reached the gateway |
| Multiple Credentials | the same hostapd kept (its config does not depend on the mode), `mode multi_host`, still closed |
| MAC Address | `mode mac_based` (the UI's method shows "Idle Timeout 300 sec"; this build had no MAB) |
| Always Unauthorized | hostapd stopped, the EAPOL trap removed, the port still `drop` with the chain's DROP |
| External (the site's existing profile) | l2.json's server became the profile's, the secrets file rewritten (one line) |
| UniFi Integrated, then 802.1X off | `learn_mode hardware` then the field removed, the chain gone, l2.json back to its size before |
| Global Switch Settings on; the cage's override put back by REST | no new push: the controller's config version stayed the 802.1X-off one |

The read-back: the controller stored `dot1x_mode force_auth` /
`dot1x_status authorized` for the force-authorised ports and `unknown` /
`disabled` for the Credentials port, as §11.26.3 reports them. The RADIUS
secret was in no journal or syslog line.

Found and fixed (in the same branch as §11.26.8):

- **the secrets file outlived 802.1X going off** (root 0600, the
  gateway's secret). The driver now removes it whenever no port is
  controlled; collect.py reports whether it exists (never its content), so
  a file left by an earlier bridge goes too. Replayed in
  `TestReplaySonicDX010Dot1X`.
- **STATE_DB kept `mode force_unauthorized` for the cage** after it left
  the config (a port that never admitted a host was never re-published).
  A port leaving the config is now published with an empty mode
  (`TestL2dMAB`'s force-unauthorised port). The read side takes the mode
  from l2.json, so the payload was right.

Found, not changed: **the bridge's reply log keeps every push as it
came**, `switch.radius.auth.N.secret` included (as it keeps the adoption's
authkey and an SNMP community), in `/host/disunified/inform-log/<name>/`
(the directory the login user's alone, the file 0644). It is the capture
source the replay fixtures are scrubbed from. **Changed since (2026-11-15):**
the recorder redacts every pushed secret by key, files 0600 in a 0700
directory, and every log line masks the push's secret values
(`docs/install.md` §8); a log written before that build keeps its values
until it rotates out or is removed.

After the run the switch held only what the driver itself leaves: hostapd
in `/usr/local/sbin` (as mstpd is) and its build directory.

#### 11.26.8 MAC authentication (MAB), built 2026-11-15

**What UniFi's RADIUS server expects.** The controller's own help text,
Settings -> RADIUS Servers -> UniFi Integrated -> Local Credentials, the
(i) beside Username (Network 11.0.99): MAC addresses go in as both the
user name and the password, and "For 802.1X, addresses must be entered
upper case without separators" (WiFi has a format setting of its own). So
a host `02:00:00:00:0a:01` is user `020000000A01` with password
`020000000A01`. What a UniFi switch puts in the rest of its request is not
captured: no switch on the site runs a MAC Address port, and making one do
so is a production change.

**Why not hostapd.** hostapd 2.11's `macaddr_acl=2` (RADIUS MAC ACL) is
consulted only on 802.11 authentication and association frames
(`ieee802_11.c`, `hostapd_allowed_address`). The wired driver turns an
EAPOL frame, or a DHCP broadcast, into a new station
(`driver_wired.c` `handle_data` / `handle_dhcp` -> `EVENT_NEW_STA` ->
`hostapd_event_new_sta`), which only starts 802.1X: a host that sends no
EAPOL is never asked about. So l2d sends the request itself.

**As built** (`l2d.py`, `Dot1xGuard`; the driver passes the push's
`dot1x.idle_timeout` as l2.json's `dot1x.idle_timeout` per interface):

- a `mac_based` port keeps its hostapd (an 802.1X host still
  authenticates there), and l2d sets its learning to `cpu_trap` in APPL_DB
  once the `DUI_DOT1X` chain drops the port's frames in the kernel, as MAC
  locking does (§11.18.2; CONFIG_DB keeps `drop`), and reads the port's
  netdev on a packet socket;
- the first frame from an unknown unicast source (not the switch, not
  EAPOL, not admitted, not waiting on an answer, not quiet) sends an
  Access-Request: User-Name and User-Password the MAC as above (the
  password hidden as RFC 2865 hides it), Service-Type Call-Check (10),
  NAS-IP-Address (the switch's management address), NAS-Identifier
  `disunified` (as hostapd's), NAS-Port-Type Ethernet (15), NAS-Port-Id
  the interface, Calling-Station-Id the host and Called-Station-Id the
  switch in RFC 3580's `AA-BB-CC-DD-EE-FF` form, and a
  Message-Authenticator (RFC 3579), as hostapd sends one for its own MAC
  ACL. Sent again after 3 s, twice per server, then the next server; at
  most 32 in flight;
- an answer counts only when its Response Authenticator and its
  Message-Authenticator both verify with the server's secret for that
  request (an unsigned or forged one is logged and ignored);
- **Access-Accept**: the host is admitted as an 802.1X host is (a static
  FDB entry in every VLAN the port carries, a RETURN in the chain), and the
  STATE_DB row lists it in `mab` too; **Access-Reject**: a failure counted
  (`failures`, `last_failed_mac`: anomaly 128, as an EAP failure is) and
  the source left alone for 60 s (hostapd's quiet period); **no answer**:
  the source is left alone for 60 s, not counted, logged once a minute;
- **the idle timeout**: an admitted host whose frames the CPU has not seen
  for that long loses its admission, and its next frame (unknown again,
  so trapped) authenticates it again. The CPU sees an admitted host's
  frames only where something copies them there: in a VLAN where the
  switch has an address, its ARP requests (ARP's trap); in a VLAN without
  one, nothing, so such a host authenticates again every idle period and
  loses the one frame that starts it. What a UniFi switch counts as idle
  is not captured;
- a port leaving `mac_based` gets its CONFIG_DB learn mode back (unless
  MAC locking traps it) before the chain goes, and loses its
  MAC-authenticated hosts.

**The probe: why the first 802.1X run's loop dropped unknown sources.** The
Vlan4094 loopback of §11.26.5 dropped unicast and broadcast frames from an
unknown source (6 `RPORTD`), while §11.18.2's run in a tagged VLAN delivered
them. Measured on cage 31 under `cpu_trap` with the installed l2d paused
(`mab-live.txt`, `probe`): frames of an ethertype no protocol trap matches
(0x88B5), untagged in Vlan4094, 0 of 20 reached the netdev (`RPORTD` +20);
tagged in a hand-made VLAN 3997, 20 of 20. Ethernet120's state in
Vlan4094's STP instance was `0` (disabled: l2d writes it for a port
without link, and paused it kept it; running, a looped cage goes to
blocking, §11.23), and StpOrch maps disabled, blocking and listening alike
to `SAI_STP_PORT_STATE_BLOCKING` (202511 `orchagent/stporch.cpp`,
`getStpSaiState`). **The chip drops a non-forwarding port's frames at
ingress before the learn decision**, so `cpu_trap` never sees them; EAPOL
(its trap) and ARP requests (theirs) get through anyway, which is why
802.1X itself worked in that loop. VLAN 3997 is in no instance. So MAB
sees a host's first frame once the port forwards in that VLAN: at once on
an edge port, after RSTP's sync otherwise, as on a UniFi switch. A loop
test of MAB with frames no trap takes needs a VLAN no instance maps; a
host that sends ARP requests is seen through ARP's trap on the looped
(blocked) port in Vlan4094 itself, which §11.26.10's runs use.

**By hand, 2026-11-15 18:11-18:15Z** (image
`igmp-202511.20261115-6c8eb1cf7`, under the test lock; the deployed bridge
stopped and the installed l2d paused for the window; cage 31 in MAC
loopback with the hand-made tagged VLAN 3997; hostapd's integrated RADIUS
server on 127.0.0.1:18120 with one MACACL user, `020000000A01`, and a test
secret; the candidate `Dot1xGuard` driving the port, idle timeout 20 s;
the "hosts" ARP requests from test MACs tagged 3997 out of the looped
port; `mab-live.txt`):

- the host that is a user: one request, Access-Accept 70 ms later, the
  host admitted (APPL_DB `FDB_TABLE` in Vlan3997 and Vlan4094, ASIC_DB
  `SAI_FDB_ENTRY_TYPE_STATIC` for both, the chain's RETURN); the test
  server logged the MAC-ACL user found and its Calling-Station-Id;
- the host that is not: Access-Reject ("User-Name not found"), `failures
  1`, and its next frames sent nothing (quiet);
- the idle timeout: the admitted host revoked 20 s after admission (its
  later ARPs in VLAN 3997, which has no address of the switch's, never
  reached the CPU), and its next frame admitted it again in 60 ms;
- the teardown: hostapd stopped, the chain and the trap gone, the learn
  mode back.

The switch was restored and checked after (loop off, VLAN 3997 deleted,
the test server and files removed, l2d resumed, the bridge started).
`TestL2dMAB` replays the run: the requests this l2d builds are the hand
run's attribute for attribute (with the run's request authenticators), the
captured answers admit and refuse, a forged accept is ignored, and the
idle timeout and the teardown behave as above. It caught one bug the hand
run hid: an answer was published to STATE_DB only on the next 2 s pass.

**Through the driver, 2026-11-15 19:04-19:12Z** (a deb of this branch,
`0.0.0+20261115.mab66eb2c0`, installed from `/tmp` under the test lock, the
carried deb in `/host/disunified` untouched and reinstalled after; no
loopback, so no request left the switch): with 802.1X on and cage 31 set
to MAC Address in the UI, the push's `idle_timeout=300` reached l2.json
(`idle_timeout: {Ethernet120: 300}`), the cage was `drop` in CONFIG_DB and
`cpu_trap` in APPL_DB, l2d logged it authenticating by MAC address beside
its hostapd, and two later reconciles left both learn modes alone. With
802.1X off: `rm -f` of the secrets file, the STATE_DB row's mode emptied,
the cage back to `hardware`, hostapd, chain and trap gone (§11.26.7's two
fixes).

Not exercised: the gateway's RADIUS server (§11.26.6), a cabled host.

#### 11.26.9 What a UniFi switch does with each method (not captured, 2026-11-15)

No UniFi switch on the site runs 802.1X on a port, and Clint would rather
not turn it on to find out (2026-11-15): the aim is the best reading of
what would work correctly if he ever does. So nothing in this section is a
capture or a live observation, and by this project's rule for sources it
is marked not captured.

The mapping the driver follows:

| Push | UI | A UniFi switch, as understood | l2d (since 2026-11-15) |
|---|---|---|---|
| `force_authorized` | (802.1X on, no method) | the port is open | open, nothing runs |
| `auto` | Credentials | one authentication opens the port to every host behind it | the first host that authenticates opens the port to every host (the chain RETURNs the port, APPL_DB learning `hardware`); the last one's logoff or the link's loss closes it again (learning back to `drop`, the port's dynamic FDB entries flushed through fdborch's `FLUSHFDBREQUEST` op `PORT`) |
| `multi_host` | Multiple Credentials | each host has a session of its own | each host that authenticates, no other |
| `mac_based` | MAC Address | each host on its own, by 802.1X or by its MAC | each host, by EAP or by its MAC (§11.26.8) |
| `force_unauthorized` | Always Unauthorized | the port is closed | closed, nothing runs |

What it rests on: Ubiquiti's help page "MAC-Based VLAN Assignment Using
802.1x in UniFi Network" (help.ui.com 115004589707) words Credentials and
Multiple Credentials this way, and gives the MAB user name as one group of
twelve upper-case hex digits, as the controller's RADIUS-user help text
does. The UI's own text for Credentials ("a single wired device") reads the
other way, and l2d followed it until 2026-11-15, turning a second host
away. The idle timeout is taken as a silence period: an authorised host
that sends nothing that long becomes unauthorised, as §11.26.8 does.

**What the switch reports.** `dot1x_mode` is taken to be one of
`force_auth`, `force_unauth`, `auto`, `mac_based` and `unknown`, and
`dot1x_status` one of `authorized`, `unauthorized` and `disabled`. Only
`force_auth`/`authorized` and `unknown`/`disabled` are seen in real informs
(802.1X is off on every UniFi switch here). There is no `multi_host` mode
word: a Multiple Credentials port is taken to report `auto`, as a
Credentials one does. The device layer reports these since
(`dot1xReport`, `TestDot1XReport`), a controlled port `authorized` while
l2d has admitted a host. The controller stores each as the DX010 sends it
(§11.26.11); that it accepts them says nothing of what a UniFi switch
sends.

What would settle it: an inform from a real UniFi switch with each method
on a port, and one port exercised per method. Credentials ran on the
DX010's looped cage on 2026-11-15 (§11.26.10): the first host's EAP-MD5
opened the port (learning `hardware`, the chain's RETURN), it stayed open
over an l2d restart, and the logoff closed it; no second host was tried
there (`TestL2dDot1X` replays one).

#### 11.26.10 Over an l2d restart: MAB and Credentials, live (2026-11-15)

Image `igmp-202511.20261115-6c8eb1cf7`, under the test lock, 21:02-21:31
UTC: empty cage 30 (Ethernet116) in MAC loopback, Backup/blocked in
Vlan4094; the deployed bridge stopped and l2.json's `dot1x` section written
by hand (Ethernet116 `mac_based`, later `auto`; idle timeout 40 s); the
RADIUS server hostapd's integrated one on 127.0.0.1 with two MAC users and
an EAP-MD5 user. A host was a raw socket sending an untagged ARP request
from a test MAC every 2 s: the loop brings it back and ARP's trap takes it
to the CPU on the blocked port, so no hand-made VLAN was needed (§11.26.8's
probe used frames no trap takes). The sequence, each step through
`systemctl` on the real unit: two hosts admitted by MAC and a third refused;
one host silent, then `restart`; the refused host again; the silent host
admitted again and silent 20 s, then `stop`, 25 s, `start`; `stop`, the
loop off, `start` (the link lost while no l2d ran), the loop back on; the
port to Credentials, an 802.1X host by EAP-MD5, `restart`, its logoff
(`mab-restart-live.txt`: run1 the deployed l2d, run4 this one).

**The deployed l2d (fef74fe, run1).** The chip was never disturbed: the
admitted hosts' FDB entries stayed in ASIC_DB and no learn mode was
written. But:

- **the kernel chain dropped the admitted hosts at every start.** The new
  l2d wrote the chain before it re-admitted anyone, so for 40-100 ms the
  port had its DROP and no RETURN (24.13-24.22 s for the two MAB hosts;
  the Credentials port closed for every host 196.80-196.87 s): on a
  forwarding port their frames to the switch itself would have been
  dropped in that window (the looped cage is STP-blocked, so none reached
  ebtables at all: §11.26.12);
- every re-admission wrote the host's FDB entry again (an identical APPL_DB
  set; orchagent changed nothing in ASIC_DB);
- **the idle timer started again at the restart**: the silent host went
  about 41 s after the restart, about 51 s after its last frame;
- after the 25 s stop the silent host (45 s idle) was admitted again with a
  fresh timer and went 40 s later;
- with the link lost during the stop the host was admitted again on a port
  without link and removed on the next pass, 2 s later.

**The Credentials port opened by the first host** (the 2026-11-15 change,
until then fixture-tested only) did what §11.26.9 says on the switch: the
EAP-MD5 host opened it (APPL_DB learning `hardware`, the chain's RETURN for
the port), it stayed open over the restart in the chip (no learn-mode
write, no flush, the host's FDB entry untouched in ASIC_DB) with the
kernel gap above, and the logoff closed it (learning `drop`, fdborch's
flush, the chain's DROP). The open port learnt nothing on the looped cage
(blocked by STP; the rig watched STATE_DB's FDB for it).

**The fix (`Dot1xGuard`):**

- an earlier l2d's admissions (the STATE_DB row) are held, in the chain and
  for a Credentials port's learning, until settled, and settled before the
  new l2d first writes the chain: a port without link loses them (the chip
  closed first, then the chain, as a running l2d does), a MAC-authenticated
  host is kept with its idle time or goes if it has been idle longer than
  the timeout, an 802.1X host waits for hostapd's station list (attach),
  which admits no station on a port without link and deauthenticates it;
- **the row's `mab_seen`** (`<mac>=<epoch>,...`): each MAC-authenticated
  host's last frame, written as l2d stops (SIGTERM ends the main loop with
  `SystemExit`; `stopping()` runs in its `finally`, so an exception does it
  too), read and removed (`hdel`) by the next l2d. A SIGKILL leaves none
  (the timer then started at the restart; since §11.26.13 the row's
  `mab_heard`, written as l2d runs, stands in);
- an FDB entry already in APPL_DB as wanted is not written again;
- the learn modes an earlier l2d set (APPL_DB `cpu_trap` or `hardware` over
  CONFIG_DB's `drop`) are the new l2d's from its first sync, so a port it
  must close is closed, and one that stays as it was gets no write.

**This l2d (run4, a copy of `l2d.py` installed for the run, the deployed
one put back after):** at the restart the chain was rewritten with both
RETURNs ahead of the DROP (the rewrite flushes and appends in order, so
for ~20 ms it passes everything, never drops; one transaction since,
§11.26.12), no FDB entry was written,
and `mab_seen` was written as it stopped and removed 0.5 s after; the
silent host went 40.2 s after its last frame ("idle for 40s"); the refused
host was refused again, its count carried over; after the 25 s stop the
silent host's FDB entry went at start ("idle for 40s, over a restart"),
before the first chain write, and the other host was kept with no write;
with the link lost during the stop the host's entry went at start and it
was not admitted, then authenticated again by MAC once the link was back;
the Credentials restart wrote nothing at all (chain, learning, FDB, no
flush) and the logoff closed the port. Two runs in between (not kept)
found the last two orderings: a host dropped over a restart kept its
RETURN until the next chain write (7 s in the first, 80 ms in the
second).

`TestL2dMABRestart` replays run4 (`testdata/l2d_mab_restart.py`): the
captured frames at their times, the RADIUS answers with their requests'
authenticators and identifiers, hostapd's station replies and events, the
restarts, stops, starts and link changes; it fails on any chain write
during a start that drops a held host, and on the idle timer restarting
(both mutations tried).

Seen on the loop, not a fault of the driver: **the LAN's multicast comes
back round the loop** (snooping's static groups flood every port, a blocked
one too), and MAB asks about each source; the server refused them all (65
failures counted in run1, each anomaly 128 on a deployed bridge), and a
restart forgets the quiet periods, so each restart asks again. l2mcd also
listed the looped port as a dynamic member of the LAN's groups while it was
up (gone with the link). A cabled port hears only its host.

#### 11.26.11 The controller's reading of each method, live (2026-11-15)

Deployed build fef74fe, under the test lock, 21:52-22:14 UTC, on Clint's
word to test on disunified rather than a UniFi switch: the DX010's record
with Global Switch Settings unticked and 802.1X Control on (UniFi
Integrated, no Unauthorized VLAN), empty cage 31 (Ethernet120, no link)
set to each method in turn through its override's `dot1x_ctrl`. For each,
the bridge's `payload-last.json`, then the controller's `stat/device`
record after the next inform:

| `dot1x_ctrl` | sent and stored (`dot1x_mode`/`dot1x_status`) | port panel |
|---|---|---|
| `force_authorized` | `force_auth`/`authorized` | Active |
| `force_unauthorized` | `force_unauth`/`unauthorized` | **Disabled** |
| `auto` | `auto`/`unauthorized` | **Disabled** |
| `multi_host` | `auto`/`unauthorized` | **Disabled** |
| `mac_based` | `mac_based`/`unauthorized` | **Disabled**; Active with 5700e19 |

The other ports reported `force_auth`/`authorized` while 802.1X was on (the
push sends `force_authorized` for every port without a method), and cage
31 `unknown`/`disabled` once it was off.

**A closed port read back as a MAC filter (fixed in 5700e19).** In every
row the controller showed as Disabled, the bridge also sent
`port_security_enabled: true`: 802.1X closes a port with the MAC filter's
learn mode (`drop`, §11.26.3), and the read-back took every `drop` for a
MAC filter (`readBackPortFeat`). The port panel then showed Port State
Disabled with its network fields greyed, the Port Manager counted the cage
under Disabled, and a save of that panel would most likely have pushed the
port disabled (not tried). The controller's own rule shows when a MAC
filter is saved with an empty list. A PUT of `port_security_enabled: true`
with `port_security_mac_address: []` on cage 31's override came back
rewritten as `forward: disabled`, with `native_networkconf_id` emptied
(put back straight away). A port 802.1X holds closed now reads back as a
MAC filter only by static entries that are not l2d's admitted hosts
(`TestDot1XApply`, `TestDot1XKeepsAdmittedHosts`, `TestDot1XWithAMACFilter`;
the first two fail on the old read-back). Test build 5700e19 on the DX010,
cage 31 on `mac_based`: `mac_based`/`unauthorized` with
`port_security_enabled: false`, Port State Active. So the Disabled came
from the MAC filter report, not from `unauthorized`.

Put back: cage 31's override as found, 802.1X Control off, then Global
Switch Settings on (two saves: ticking Global Switch Settings hides the box
and does not save its untick), the carried fef74fe reinstalled, l2.json's
`dot1x` section gone, Ethernet120's learning `hardware` again, l2d's
leftover STATE_DB `DUI_DOT1X_TABLE|Ethernet120` row (counts only) removed.
No UniFi switch was touched.

#### 11.26.12 The kernel chains are written in one transaction (2026-11-15)

§11.26.10 saw `DUI_DOT1X` pass every frame for about 20 ms each time it
was rewritten. The cause: `write_chain` flushed the chain (`ebtables -F`)
and appended its rules one `ebtables` call at a time, and the DX010's
`ebtables` is 1.8.11 on **nf_tables** (`ebtables -V`; `/usr/sbin/ebtables`
is the alternatives link to `xtables-nft-multi`, beside an installed
`ebtables-legacy` that nothing uses). Each call is a transaction of its own,
so between the flush and the last append the chain held the EAPOL ACCEPTs
and some RETURNs but not the port's DROP, and its policy RETURN let
everything through. That happened at every admission, revocation, port
change and l2d start. `MacLockGuard` wrote `DUI_MACLOCK` the same way.

**As built.** Both guards hand the whole change to one `ebtables-restore
--noflush` (`eb_restore`, `l2d.py`): the chain's declaration (`:<chain>
RETURN`, which creates it or keeps it), `-F <chain>`, its rules, and the
INPUT and FORWARD jumps that are missing (`-I <hook> 1` for 802.1X, ahead
of MAC locking's `-A`). Removal is one restore too (`-D` the jumps, `-F`,
`-X`). What that guarantees, measured on the switch (`ip netns`, nothing
the switch's own tables see):

- the whole input is one nf_tables transaction: `xtables-monitor -e`
  prints one `NEWGEN` for it, the flush as a `-D` per rule inside it;
- with `--noflush`, a declared chain that exists is emptied (nft); the
  legacy `ebtables-legacy-restore` keeps its rules and appends, hence the
  explicit `-F` (correct on both);
- a refused line (a jump to a chain that does not exist) refuses the
  batch: exit 2, the table as it was.

**A chain the kernel lost is written again.** SONiC's `database.sh`
(`ebtables_config`) loads `/etc/ebtables.filter.cfg` with a plain
`ebtables-restore`, which empties the whole filter table, at every start of
the database container. `MacLockGuard` already compared the kernel's rule
count and jumps with what it wrote at each sync; `Dot1xGuard` returned as
soon as its rules were unchanged, so after such a reload an 802.1X port
passed every frame to the CPU until the next admission. Both now read the
chain's state (`eb_state`) at each sync and write it again when the count
or the jumps differ, with a warning for 802.1X.

**The iptables mirror (`DUI_ACL_CPU`, §11.32) did not share the fault.**
`acl_cpu.go`'s `cpuScript` already wrote every chain in one `iptables-restore
-n` with the chains declared, and adds a PREROUTING hook only after the
chains exist. Measured in a private netns with the driver's input (two
table chains under `DUI_ACL_CPU`, a blocked source sending UDP every
0.2 ms): 200 rewrites in 7 s, 36 139 packets into the hook, 0 past it, and
6 rules after the last write (a declared chain is emptied by `-n`, so
nothing piles up). No change.

l2d writes no other ebtables chain (the DHCP-rate, storm and BPDU-guard
paths use CoPP, counters and APPL_DB).

**Measured on the switch** (`dot1x-atomic-live.txt` in the image's fixture
directory):

| | old: flush + one call per rule | new: one restore |
|---|---|---|
| netns bridge, a never-admitted source every 0.2 ms, 200 rewrites | 46 890 frames past the chain | 0 (27 561 dropped by the last chain alone) |
| DX010, l2d driving cage 30 (MAB hosts admitted, idle-revoked and admitted again, two restarts, a 4 s link loss), `xtables-monitor -e` | 88 transactions; after 71 of them the port had no DROP; the 5 ms poll saw 17 such windows of 25-100 ms (mean 40) | 14 transactions, one per write, each `ebtables-restore`; the DROP after every one |

The DX010's two runs are the same scenario through the deployed l2d
(fef74fe) and this one. This l2d also wrote nothing at either restart (the
kept admissions of §11.26.10); the deployed one rewrote the chain three
times at each, at start and once per host it admitted again.

**What a looped cage cannot show.** No frame from the loop exercised the
DROP on the switch itself: a looped cage is STP-blocked (Backup), and the
kernel bridge drops a blocked port's frames before ebtables PREROUTING,
except link-local ones, which go to NF_BR_LOCAL_IN (ebtables INPUT) in any
port state (BPDUs and LLDP are handed up without the hook). The link-local
frame the chip traps here is EAPOL, which the chain ACCEPTs by design.
Probed with 20 frames each from an unadmitted source:
an untagged 0x88B5 frame to `01:80:c2:00:00:03` and an ARP request to that
address never reached the netdev; a broadcast ARP request reached it (ARP's
trap, tagged 4094) and was dropped by the bridge's blocking state, so none
reached INPUT. The chain's DROP matters on a forwarding port with a host
behind it; the frame counts above come from the netns bridge, the DX010's
from the kernel's own record of every transaction.

**Tests.** The l2d fakes share `testdata/l2d_ebtables.py`: single commands
and `ebtables-restore --noflush` as the switch's nf_tables ebtables runs
them (a restore applied whole or refused whole), and the kernel's walk of a
never-admitted source through INPUT after every transaction. Each guard's
`write_chain` is wrapped: a port that dropped that source before and after
the call but passed it at a transaction in between prints `WINDOW`.
`kernelAtomic` fails `TestL2dDot1X`, `TestL2dMAB`, `TestL2dMABRestart` (the
replay of §11.26.10's run4) and `TestL2dMacLocking` on a `WINDOW` and on any
`ebtables` write outside a restore; with the old `write_chain` the four
print 1, 6, 9 and 2 windows. `TestL2dDot1X` also empties the filter table
between two syncs, as `database.sh` does, and expects the chain and both
jumps back (FORWARD's ahead of the image's own rules) in one restore.

`TestDot1xChainAtomicLive` holds the capture to the table: 14 transactions
by `ebtables-restore` with the DROP after each for the new run, 88 and 71
for the old.

#### 11.26.13 A MAC-authenticated host's idle timer after a SIGKILL (2026-11-15)

§11.26.10's `mab_seen` is written only as l2d stops. A SIGKILL, an
out-of-memory kill or an interpreter crash writes nothing, and systemd starts
l2d again 3 s later (`Restart=always`, `RestartSec=3`; `KillMode=process`,
so hostapd, the chain and the FDB entries stay).

**What happened (the l2d on main before this change, run `base` of
`mab-kill-live.txt`).** The new l2d found the hosts in the row but no last
frame, so each host's idle timer started again at the restart. A silent
host went 40 s after the restart, not 40 s after its last frame. In the
run:

| host C before the kill | base: C went after its last frame | this l2d |
|---|---|---|
| silent 8.4 s, one kill | 52.6 s (restart + 41 s) | 46.3 s |
| silent 13.3 s, two kills 15 s apart | 73.0 s (second restart + 41 s) | 45.3 s |
| one frame after 25 s of silence, killed 0.6 s after it | 45.1 s (restart + 41 s) | 47.2 s |

With the idle timeout at 40 s, each restart gave a silent host up to a
whole timeout more. If l2d crashed in a loop faster than the timeout, the
host was never removed. An operator saw the host still listed in the row's
`authorized` and in the MAC table (its static FDB entry), as a client of
the port, until the timeout after the last restart. The talking host (A,
an ARP every 2 s) was kept through every kill in both runs: its FDB entry
was written once, at its admission, and never again. Nothing in the chip
moved. No host was thrown out early.

**The options.**

- **Re-authenticate every MAC-authenticated host at start.** A RADIUS
  accept says the MAC is still a user, not that the host is talking, so the
  timer would still start at the restart. Revoking every host at start
  instead ("let the next frame re-admit it") would throw out the talking
  hosts, which lose a frame and a RADIUS round trip.
- **Derive the last frame from the kernel or the chip.** The admitted
  host's FDB entry is static, so neither the chip nor the kernel ages it,
  and the chip forwards the host's frames without the CPU. The kernel bridge
  and `DUI_DOT1X`'s RETURN see only what the CPU sees, which is what l2d
  counts already. The RETURN rule's counter is a count, not a time, and
  every chain rewrite resets it. So neither holds a time to start from.
- **Persist the last frame as l2d runs (chosen).** It costs a few small
  writes and the bound follows from the write rule alone.

**As built (`Dot1xGuard`).** The row gains `mab_heard`
(`<mac>=<epoch>,...`, `mab_seen`'s form): each MAC-authenticated host's
last frame, rewritten for the whole port when a frame finds the stored
value `MAB_HEARD_STEP` (5 s) or more behind (a new admission counts too),
and on the 2 s pass when the port's hosts change. Its last host's
revocation removes the field. So the stored value is never later than a
host's last frame and never 5 s or more before it, whenever l2d dies. A
clean stop still writes the exact `mab_seen`, which the next l2d prefers.
After a death without it, the next l2d takes `mab_heard` as each host's
last frame and adds 5 s to its idle timeout until its next frame (in
memory only, so a crash loop does not add it twice). `publish()` now
compares only the fields it writes, so the extra field does not make it
rewrite the row every pass.

**The bound.** After any number of kills, a silent host goes between
`idle_timeout` and `idle_timeout` + 5 s + one pass (2 s plus the sync's own
time; 2.09 s seen) after its last frame. If l2d is down past that, it goes
at the first sync after the restart. A host that talks is never removed by
a kill. A host whose frame came while no l2d ran is not credited with it.
Its next frame starts the timer, as it does over a clean stop. The write
cost: one `HSET` of one field per port every 5-6 s while a host on it
talks (35 in the 180 s `fix` run, one port, a host every 2 s), and none for
a port whose hosts are silent.

**Live, 2026-11-15 22:44-22:52 UTC** (image `igmp-202511.20261115-6c8eb1cf7`,
under the test lock): §11.26.10's rig on empty cage 30 in MAC loopback, the
deployed bridge stopped, l2.json's `dot1x` by hand (`mac_based`, idle
timeout 40 s), hostapd's integrated RADIUS server on lo. Each l2d was
installed as a copy: `base` main's (aa21c73); `fix` this change's, the file
the test deb `0.0.0+20261115.mabkcaeb045` embeds, checked byte for byte on
the switch. No test bridge ran: a build carrying main's VLAN delete
(§11.29) or the kernel ACL mirror would have changed the switch beyond l2d.
In each run, A talks throughout and C goes silent before each `kill -9` of
the unit's main pid. Every row of the table above is a journal line `idle
for 40s` at the first sync after C's bound. The switch was put back after:
the deployed l2d (fef74fe) reinstalled and the bridge started, cage 30
without loop or link, APPL_DB learning `hardware`, l2.json as before, the
lock released. The candidate in that run predates §11.26.12's transaction.
The replay runs the merged code.

`TestL2dMABKill` replays run `fix` (`testdata/l2d_mab_kill.py`, built on
`l2d_mab_restart.py`'s fakes). It uses the captured frames, RADIUS answers
and kills, each restart at the new l2d's first logged sync, and a sync
every 0.5 s. The switch's l2d also syncs on netlink and FDB events, so the
moment of its sync is not in the capture. The test asserts that C goes
within the bound in each of the three cases and within 0.5 s of the
switch's revocation (up to 2.5 s before it, for the switch's 2 s pass), and
that A is never removed or written again. It fails with main's l2d (C is
not removed by the switch's time) and without the grace or the write on a
frame.

### 11.28 BPDU guard takes the chip's port down; the UI's Re-enable (2026-11-15)

§11.23.3 left one difference from a UniFi switch: mstpd's BPDU guard shut
only the port's netdev, so the chip's port stayed up, a cabled peer kept
its link and the port read up. This section takes the chip's port down for
the trip and brings it back the way UniFi does.

**What UniFi asks for** (the console's own UI, Network 11.0.99, read
2026-11-15). BPDU Guard's tooltip: it "shuts down the port and requires
manual re-enable if a switch is detected (via BPDUs)"; its enable dialog
says the port "must be manually re-enabled"; the alert's recommended action
ends "The port must be manually re-enabled." The port panel then reads
"Blocked by BPDU Guard", and the port's settings carry a **Re-enable**
button, offered when the port's anomaly list holds `BPDU_GUARD_TRIGGERED`
or `NETWORK_LOOP_DETECTED_BY_KEEPALIVE` (loop protection) and hidden after
one click until that list changes. No text mentions a recovery timer, and
the ECS's `ERROR_DISABLE|bpduguard` row has `auto_recovery_interval 0`
(§11.20.2). So: no timer; the port stays down until the operator's
Re-enable.

**What the Re-enable sends** (captured 2026-11-15 by clicking it on the
DX010's held port 17). The UI posts `cmd/devmgr` with `{"cmd":
"port-recovery", "mac": ..., "port_idx": 17}` (its bundle's
`PORT_RECOVERY`); the device receives it at its next inform as

```
{"_type": "cmd", "cmd": "port-recovery", "port": 17, "_id": ..., "device_id": ..., "datetime": ..., "time": ...}
```

with the port number as `port`, not `port_idx`
(`fixtures/controller-11.0.99/reply-port-recovery.ndjson`,
`TestPortRecoveryCarriesItsPort`). The session turns it into a
port-recovery effect and the loop calls the driver's
`devicemodel.PortRecoverer`. A driver without it logs the request and
ignores it.

#### 11.28.1 The design

- **The trip.** l2d reads mstpd's report every 2 s, and a port with
  `bpdu-guard-port yes` and `bpdu-guard-error yes` is held: APPL_DB
  `PORT_TABLE` `admin_status down` through ProducerStateTable, as loop
  protection's hold already did (§11.5). CONFIG_DB, the saved config and the
  driver's plan, which reads CONFIG_DB, keep the operator's setting, so the
  plan never fights the hold, and the controller sees the port enabled and
  error-disabled. The hold is STATE_DB `DUI_BPDU_GUARD_TABLE|<port>` (`state
  held`, `since`) and survives an l2d restart. l2d publishes its verdict as
  `bpdu_guard_held` in `DUI_STP_PORT_TABLE`, and the driver reports the
  port's `errdisabled: bpduguard` from it (from mstpd's error only for an
  older l2d without the field).
- **Re-assert.** portmgrd copies CONFIG_DB's `admin_status` into APPL_DB
  and runs `ip link set up` whenever anything writes the port's CONFIG_DB
  row (a description, an MTU), which undid a hold. Every pass, l2d puts a
  held port's APPL_DB row and netdev down again. That applies to the
  loop-protection hold too, which had the same gap.
- **The release.** The driver's `RecoverPort` writes STATE_DB
  `DUI_PORT_RECOVERY|<interface>` for each interface of the port. l2d reads
  and deletes those rows each pass, and releases a BPDU-guard hold or a
  loop-protection hold on that interface or its PortChannel. A release puts
  CONFIG_DB's `admin_status` back and brings the netdev up (mstpd shut it).
  A hold is also released when mstpd no longer runs BPDU guard on the port
  (the guard or STP switched off for it), when the port leaves the bridge,
  and when spanning tree goes off. `--teardown` releases everything.
- **Released, not re-tripped, until the link comes back.** mstpd clears
  its error only when the port's link comes up again, so after a Re-enable
  on a port with no link (the loop unplugged) mstpd still says error. l2d
  marks such a port released (the row's `state released` with the port's
  state-change count). Its error is not a new trip until the port's kernel
  state has changed since (a link-up), whether the guard stays on or is
  switched off and on again. If the loop is still there, the link comes up,
  the next BPDU trips mstpd again and l2d holds the port again within a
  pass: UniFi's "Fix the loop, then re-enable the port."
- **Loop protection's hold** keeps its 150 s automatic release (the ECS's
  `MAC_MOVE_GUARD` action interval, §11.5), and now also ends at the
  operator's Re-enable, when the port's loop protection is switched off
  (it used to wait for the whole guard to go off), and is re-asserted as
  above.

Tests: `l2d_bpdu_guard_test.go` runs l2d's holds against
`fixtures/sonic-igmp-202511.20261114-42cccc87a/collect-bpduguard.txt`: the
trip, portmgrd's rewrite, a restart, the Re-enable, a release with no link,
the link coming back into the loop, the guard off and on again, and the
loop-protection hold's three releases. `TestWireContractSonicBPDUGuardHeld`
checks the driver's read side on the held capture
`fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/collect-bpduguard-held.txt`.
`TestRecoverPortAsksL2d` checks the STATE_DB write.

#### 11.28.2 Live on the DX010 (2026-11-15 18:25-18:52 UTC, test builds fc73a86 to bd61d4c)

On empty cage 17 (Ethernet64), looped with the chip's MAC loopback, BPDU
guard on through the controller's port override, and the switch lock held:

| step | result |
|---|---|
| loop with the guard on | mstpd "Received BPDU on BPDU Guarded Port - Port Down" and l2d "held down in the chip until re-enabled" in the same second; APPL_DB `admin_status down`, ASIC_DB `SAI_PORT_ATTR_ADMIN_STATE false`, `bcmcmd ps` `!ena` (the port Disabled) |
| the next inform | port 17 `enable: true`, `error_disabled: true`, reason 2; the UI's Port Manager "Port 17: Blocked by BPDU Guard", STP "Blocked", and the Re-enable button |
| Re-enable, loop still on | `port-recovery requested for port 17`; l2d "released (re-enabled by the controller)"; the link came up, mstpd tripped again, l2d held the port again 2 s later |
| an l2d restart while held | still held; the chip port `!ena` |
| a CONFIG_DB write of the port (`admin_status up` over itself) | portmgrd set APPL_DB up and the chip port up; l2d "admin_status was rewritten under a hold: down again" within a pass. The first build left the netdev up: mstpd then ran the port as Designated and sent BPDUs into the disabled port. The re-assert takes the netdev down too since. |
| the loop removed, then Re-enable | released: `bcmcmd ps` `down` (enabled, no link), `bpdu_guard_held no`, the next inform without `error_disabled`, the UI's red mark gone |
| BPDU guard switched off while held, loop on | the first build released on the config alone: the link came up while mstpd still guarded the port, mstpd shut the netdev again, and the port was left down with the guard off. The release now waits for mstpd's own report of the guard off: released cleanly, the port linked into the loop as Backup |
| guard off and on again over mstpd's old error (no link since) | not held (the released mark) |

**The peer's link could not be seen.** With the MAC loopback the chip's PHY
reports link even on a disabled port (`*ce16 (Disabled ...`), and APPL_DB
`oper_status` stays up, so the bridge reported port 17 `up: true` while
held. A disabled port is what `config interface shutdown` produces; on a
cabled port the peer loses link and `oper_status` goes down. Seeing that
needs a cable. The ECS was not re-read for this: §11.20.2 has its
`ERROR_DISABLE` rows, and no port there has tripped.

Restored after the run: the cage unlooped, port 17's override back to its
exact pre-test object (the UI's save in §11.23.3's Edge test had added
fields and switched BPDU guard on), mstpd's error cleared by one link-up,
the deployed deb (f3d3572) reinstalled from `/host/disunified`, leftover
`bpdu_guard_held` fields deleted from STATE_DB, and the lock removed.

### 11.29 A VLAN the controller stops pushing (2026-11-15)

Until 2026-11-15 `EnsureVLANs` only created, so a VLAN outlived what had
asked for it: the Inter-VLAN transit's Vlan4040 was deleted by hand after
the 2026-11-07, 11-08 and 11-15 runs (§11.13, §11.16), and ten site VLANs
the 2026-11-02 Allow All test created (§11.8) were still on the DX010 with
no member. The driver now deletes a VLAN no push names, at the end of the
port apply (`vlan_prune.go`).
The `arista-eos` driver follows the same rule since the same day
(`docs/drivers/arista-eos.md` §12).

#### 11.29.1 When the controller stops naming a VLAN

The push's `switch.vlan.*` lists only the VLANs the device uses. The
DX010's pushes, every port native-only, list VLAN 1 alone while the site
has ten other networks (every `system_cfg-dx010-*.txt` of 10.6.109 and
11.0.99). A VLAN appears when a port carries it or the switch routes it:

| Case | Capture |
|---|---|
| A tagged set that adds or drops it: the voice VLAN 69 on one port, 31 `exclude` | `controller-10.6.109/system_cfg-dx010-portfeat-4-voice.txt` (1, 69) then `-5-voice-off.txt` (1) |
| Allow All to Block All and back on the VS: every network, then 1 alone, then every network | `controller-10.6.106/replies-sonic.ndjson` records 2, 95, 101 |
| The transit of the first routed network: VLAN 4040 with every port `exclude`, gone with the last routed network | `controller-10.6.109/system_cfg-dx010-l3-4-none.txt` then `-l3-5-off.txt`; `controller-11.0.99/replies-dx010.ndjson` records 9 and 12 |
| A routed network no port carries (VLAN 3999): in `switch.routing_interface.N.vlan` only, never in `switch.vlan.*` | `system_cfg-dx010-l3-1-dhcp-server.txt` |
| A network deleted from the site | the ECS's own removal of Vlan4040 (next section); no DX010 capture |

So "no port carries it any more", "the network is deleted" and "the
transit went" reach the switch the same way: the id leaves the list.

#### 11.29.2 What the ECS does

Read-only on the ECS (Ubiquiti's SONiC 4.0.1) on 2026-11-15: its CONFIG_DB
`VLAN` table is exactly the site's ten networks plus `Vlan1`, every one
with members (its trunks are Allow All), and its syslog has the transit's
whole life during that day's routed run: `addVlan: Create an empty VLAN
Vlan4040` at 17:43:37 UTC, members added on every port, then at 17:45:25
`removeVlan: Unable to remove non-empty VLAN Vlan4040: ref count 48` and,
three seconds later with the members gone, `Remove VLAN Vlan4040`. The ECS
follows its push: a VLAN the push stops naming leaves its tables, members
first. (A network that stays on the site while no ECS port carries it was
not seen: every ECS port carries every network.)

#### 11.29.3 Which VLANs on the DX010 the driver made

There is no record: `sonic-managed.json` held only `ntp` and `syslog`
(2026-11-15), and the L3 record keeps addresses, not VLANs. The DX010's
CONFIG_DB had `Vlan4094` (the L2 preset's, the `native_vlan` stand-in) and
2, 8, 9, 10, 69, 1000, 2000, 3000, 4000, 4010: the site's ten networks,
which the driver created in §11.8's Allow All test (2026-11-02, "the ten
site VLANs stay on the switch") and no port has carried since. Nothing on
the switch was made by hand.

#### 11.29.4 The rule, and why

**The push's VLAN list is the switch's, as for NTP and syslog.** Once the
port plan, the IGMP rows and a routed network's address have left a VLAN,
a VLAN no push names is deleted (`config vlan del [-m]`, then `config save
-y`). Clint's rule (2026-10-31): "unifi is in control, not some mishmash
of control with what's already on the switches", and the controller's list
replaces the device's. A UniFi switch does the same (the ECS above), and a
VLAN with no member, address or relay carries nothing, so deleting it
changes no traffic.

A record of the VLANs the driver created was considered and not built: it
would leave the ten VLANs above forever (nothing records them), which is
the mishmash the rule rules out, and it would protect only what a record
can name. What it was for, "adoption takes the device as it is", is met
otherwise: the first push after adoption is held while it would change a
port (a VLAN with members is a port change), and every VLAN that still
holds something the driver did not take out of it stays.

**Kept**, whatever the push says:

- the `native_vlan` stand-in (UniFi's VLAN 1, the management VLAN on the
  DX010);
- every VLAN the device's push names, and every VLAN an attached lane
  switch's push names (`devicemodel.LaneVLANController`: the lane switch's
  list is its own since this change; before, it replaced the device's
  list, so the device's Allow All tagged the lane switch's VLANs and the
  lane switch's the device's);
- the VLAN of every network the switch routes (`switch.routing_interface`,
  which `switch.vlan.*` does not list for a network no port carries; its
  MTU follows the ports' like a listed VLAN's since the same day, §11.13);
- a VLAN that still has a member (a port outside `control.ports`, a lane a
  lane switch owns, a PortChannel outside the plan's range), an address
  the driver did not add (the management address on a VLAN other than the
  stand-in, a hand-made one), a `VLAN_INTERFACE` row with fields (bound to
  a VRF or running IPv6 link-local: the CLI keeps such a row after the
  last address), a DHCP relay, or an IGMP snooping row (`options.igmp`
  off: the driver writes none, and the CLI refuses the delete while one
  exists). Each is noted once.

**Never from a list the run has not been given.** The prune runs only after
an `EnsureVLANs` of this run (the device's push), after every attached
lane switch's first push, and, on an image that routes, after the routing
intent (`ApplyDevice` runs before the ports). A held first push never
reaches it. It runs only with ports to write: `control.ports: off` writes
no VLAN either. The 10.6.106 lab replay never deletes: its pushes predate
`switch.routing_enabled`, so the routing intent is never known there.

**Order.** The port batch takes the memberships out, l2d's config and the
IGMP rows follow, the L3 apply (re-run after the ports) has already
removed a leaving network's routes and address, and the delete comes
last, in its own batch. The DHCP guard's rules are per VLAN on the switch
(§11.7.6), so they are derived again and applied after a delete. l2d drops
a deleted VLAN's STP instance itself (`forget_vlan`: orchagent removes the
instance with the VLAN). The CLI (`config/vlan.py` `del_vlan`, 202505 and
202511) checks and deletes one VLAN at a time: an L2MC or MLD_L2MC row, a
`VLAN_INTERFACE` row or address, a member or a VXLAN map refuses it; it
clears the VLAN's DHCP relay and `STP_VLAN` rows itself. The FixtureRunner
models those refusals in that order. An MLD snooping row (collect.py does
not read MLD) would fail the batch with the CLI's message.

**A lane switch's VLANs and their MTU (2026-11-15).** The VLAN list a lane
switch's push names was kept by the prune, but no plan set its MTU.
`vlanMTUs` (§12) ran only in the device's plan, over the native stand-in,
the device's list and its routed VLANs; the lane plan skipped it. A VLAN
only the lane switch listed was made at the ports' MTU in CONFIG_DB
(`EnsureLaneVLANs`), but its `Vlan<id>` netdev stayed at vlanmgrd's 9100,
the §12 trap, and a jumbo flip reached neither half. Shown on the 4x25G
capture with vlanmgrd's netdev modelled (`FixtureRunner.VLANNetdevs`):
`TestLaneOnlyVLANMTU` and `TestReplayLaneSwitchVLANMTU` (the Flex XG's
captured push making Vlan69, then the child's jumbo flipped in that push).
Against the old plan, the next cycle wrote nothing for the netdev, and the
flip moved the lanes but no VLAN.

Each VLAN now takes the lowest MTU of the pushes that list it. Those are
the device's push (the native stand-in, its list, its routed VLANs) and
each attached lane switch's (its list and the native stand-in). A plan
knows its own push's MTU from its ports' desired MTU and the others' from
what their last applied plan set (`Collector.mtuWant`). A push that has not
applied yet counts for nothing. Both plans compute the same value, so
neither undoes the other. The rules:

- **Who writes it.** A lane plan writes only the VLANs the device's push
  does not list. The device's plan writes every VLAN. So a lane switch
  alone never raises the native stand-in, which is the management VLAN.
- **A jumbo flip on the lane switch** moves its own VLANs with its lanes,
  in the same batch.
- **The stand-in** follows the lower of the two pushes at the device's
  next plan.
- **A new lane VLAN** is made at the lane switch's MTU (its last plan's,
  else its lanes' as collected), not at the device's.
- **Its netdev** waits for the first plan after a collect has seen it, as
  the device's do. That is one cycle of either loop.
- **The prune.** A lane VLAN the lane switch stops listing leaves its list.
  The device's plan deletes it (`dropVLAN` takes its MTU entries), and no
  plan sets an MTU on it after.
- **A list not yet pushed.** A lane plan before the lane switch's first
  `EnsureLaneVLANs` sets no VLAN's MTU. Its Allow All then falls back to
  every VLAN on the switch, which is not a list. The device's plan makes
  the same guess before its own first `EnsureVLANs`, and it then leaves a
  VLAN the lane switch lists at the lane switch's MTU
  (`TestLaneVLANMTUBeforeTheDevicesList`; without that rule the two plans
  undid each other every cycle).

The `arista-eos` parent has no such gap. EOS VLANs have no MTU, the driver
writes no MTU at all (jumbo is logged, not applied, `arista-eos.md` §6),
and it makes no SVI for any VLAN. Not run live: no lane switch is adopted
(`breakout.md` §15 step 5 is the check).

#### 11.29.5 Tests

`vlan_prune_test.go` (collect-l3.txt): nothing deleted before a push or
before the routing intent, then the ten in one command and nothing after;
a port outside the written set keeps its VLAN (noted) until it is written
again, and then the member goes before the VLAN; an address the driver did
not add and a routed network's VLAN stay; nothing until an attached lane
switch has applied, then its VLANs stay and its list leaves the device's
alone; the fixture's CLI refusals. The DX010 replays meet the ten VLANs on
their first reconcile (`pruneTen`), the Network 11 replay deletes
Vlan4040 after the transit's address (record 12), and the L3 replay
Vlans 3999 and 4040 after routing off. `TestReplaySonicDX010VLANPrune`
replays the live run below over the switch as captured right after it
(`collect-vlan-prune.txt`, `controller-11.0.99/replies-dx010-vlan-prune.ndjson`).

#### 11.29.6 Live on the DX010 (2026-11-15 21:33-21:49 UTC, test deb 776f052, under the test lock)

- **The ten unpushed VLANs.** The test build's first reconcile under the
  push in force (VLAN 1 only) ran `config vlan del -m 2,8,9,10,69,1000,
  2000,3000,4000,4010` and `config save -y`, then "0 of 32 ports changed".
  orchagent removed each (`removeVlan: Remove VLAN VlanN`), l2d dropped
  each instance (`VlanN is gone: its STP instance keys dropped`); CONFIG_DB,
  APPL_DB `VLAN_TABLE`, the kernel's `Vlan<id>` netdevs and l2d's instances
  kept only 4094, ASIC_DB two VLAN objects (the default and 4094).
  PortChannel1 and both members stayed up; no ERR in syslog.
- **A tagged set that adds and drops a VLAN.** Port 20 (Ethernet76, an
  empty cage) was given a custom tagged set holding DMZ-FutureUse2 (VLAN
  4010) through the controller's REST, as the UI saves an override
  (`forward customize`, `tagged_vlan_mgmt custom`, every other network in
  `excluded_networkconf_ids`): the push listed 4010 with port 20 tagged and
  31 `exclude`; the bridge created Vlan4010 at MTU 1500, tagged Ethernet76,
  and the next cycle set the new netdev's MTU (§12). Port 20 back to its
  pre-test override: the push listed VLAN 1 alone; the bridge ran `config
  vlan member del 4010 Ethernet76`, then `config vlan del 4010`, and l2d
  dropped the instance. orchagent logged two WARNINGs at the member
  removal (`removeMrouterPortFromL2mcEntries: VLAN Vlan4010 not found in
  IPv4/IPv6 mrouter ports map`): the kit's L2MC orch on a VLAN with no
  snooping, not an error.
- **Quiet after.** Ten minutes of reconciles wrote nothing.
- **Restored:** port 20's override identical to its pre-test copy (the
  REST answer compared field by field), the deployed deb (fef74fe)
  reinstalled from `/host/disunified`, the ten VLANs created again by hand
  at MTU 1500 (CONFIG_DB and the netdevs), CONFIG_DB identical to the
  pre-run dump in all 37 tables, the lock released. The deployed build
  will delete those ten again once a build with this change runs.

### 11.32 ACL rules and the switch's own address: the ECS, and the DX010's kernel mirror (2026-11-15)

§11.7.8 found that on the DX010's SAI no ACL drop keeps a frame addressed
to the switch from its CPU. Whether that matters depends on what UniFi's
own switch does, so the ECS Aggregation was read first (read-only, no
test traffic at it), then the DX010 was made to match.

#### 11.32.1 What the ECS does

- **The UI** (Network 11.0.99, the ACL rule editor, read without saving)
  describes both kinds as filters of traffic between devices: "IP ACLs
  filter traffic between devices on different VLANs, not within VLANs",
  and "MAC ACLs filter traffic between devices on the same VLAN, not
  across VLANs". It says nothing either way about traffic to the switch
  itself. Neither does DHCP guarding's text (§11.7.1). The site had no
  ACL rule, so none could be read for its effect on a switch's own address.
- **The tables and the traps are arranged as on the DX010.** Its two
  tables (`global_ip_acl`, type L3, and `global_mac_acl`, type ANY) are
  bound ingress to every front-panel port and PortChannel. A rule is a
  plain `SAI_PACKET_ACTION_DROP` entry in ASIC_DB, not a deny that also
  cancels a CPU copy, and the ECS has its CPU traps (`HOSTIF_TRAP`) as
  every SONiC switch does. Nothing in its kernel mirrors the rules:
  iptables INPUT is caclmgrd's stock set and ebtables INPUT is empty.
- **But its own address is not behind a trap.** The ECS's management
  address sits on `br0`, a kernel bridge whose one member is `eth1`, the
  CPU's own 10G NIC (`ixgbe`). That NIC is cabled inside the box to a
  switch port, `Ethernet56` (10G, one lane), an untagged member of the
  management VLAN, and the management MAC is learned on it like any
  host's (`FDB_TABLE|Vlan1:<mgmt mac>` -> `Ethernet56`, dynamic). The
  ACL tables do not bind `Ethernet56`. SONiC's own `Vlan` interfaces carry
  no address there (`routing_enabled=false`).
- **So a frame to the ECS's address is switched, not trapped.** It enters
  a front-panel port, which the tables bind, and is forwarded to
  `Ethernet56`. An ingress DROP takes it like any switched frame, and no
  copy goes to the CPU by another path. On the ECS a Block whose match
  covers the switch's address keeps that traffic off the switch.
  - A MAC rule cannot name the management network in the UI (§11.7.8),
    and that is the only network the ECS has an address on, so for MAC
    rules the question does not arise.
  - The guard on the ECS is software (§11.7.3): replies are trapped to its
    snooping daemon, which drops an untrusted one before anything reaches
    the switch's own DHCP client on `br0`.
  - With routing on, the ECS's `Vlan` interfaces would be router
    interfaces behind the traps, as on the DX010. Whether its drop then
    beats the trap is not known (routing is off on the site's ECS).
- **One MAC rule the controller did not push as an ACL.** The ECS's
  `global_mac_acl` held one rule, `MB_<mac>`: `DROP`, `PRIORITY 1023`, a
  source MAC and no VLAN, so it covers the management VLAN too and keeps
  that MAC off the ECS's own address. The site has no ACL rule, so it is
  not from the Policy Table. The setparam reply carries a `blocked_sta`
  field (empty in every reply the bridge has logged), which the bridge
  ignores. That the rule is a client Block delivered through
  `blocked_sta` is not captured; a follow-up in the register.

#### 11.32.2 The decision

The DX010 should match: a Block that covers the switch's own address keeps
that traffic off the switch. On the DX010 the address is on `Vlan4094`, a
router interface, so the traffic reaches the CPU through the ip2me and DHCP
traps whatever the tables say (§11.7.8). Two ways to close it:

| Way | What it takes | Verdict |
|---|---|---|
| SAI `DENY` (drop and cancel the CPU copy) on the driver's entries | an orchagent patch in the image kit (`aclPacketActionLookup` has FORWARD, DROP and COPY only), an image build and an install Clint must OK; whether libsaibcm `13.2.1.100` honours `DENY` against another group's `GpCopyToCpu` would need a probe through ASIC_DB | not built: an image change for what the installed image can do in the kernel; not probed |
| A kernel drop of the CPU's copy, mirroring the tables | the driver alone, on the installed image | **built** |

#### 11.32.3 As built: the kernel mirror (`acl_cpu.go`)

- **What is mirrored.** The driver's two L3 tables, `DUI_DHCP_GUARD` and
  `DUI_IP_ACL` (the controller's IP rules and Isolate Network's `ISO_k`),
  rule for rule from their CONFIG_DB hashes, so the kernel drops exactly
  what the chip's entries match. MAC rules are not mirrored (§11.32.1:
  the UI cannot put one on the management network).
- **Where.** iptables' `raw` table, `PREROUTING`, for packets arriving on a
  front-panel interface: `-i Vlan+`, `-i Ethernet+`, `-i PortChannel+`.
  eth0, lo and docker0 are left alone, as the chip's tables do not bind
  them.
  - **Not the filter table.** caclmgrd rewrites it on any `ACL_TABLE`
    event, the driver's own `config acl add table` included: it flushes
    every chain and deletes every chain it did not make. It never touches
    IPv4's `raw` table (it flushes ip6tables' `raw` only), read in
    202511's `caclmgrd`.
  - `raw` runs before conntrack, so a drop leaves no state behind, as the
    chip keeps none.
- **Shape.** A dispatcher chain `DUI_ACL_CPU`, hooked once per interface
  prefix, jumps to one chain per table, named after it. A table's chain
  holds its rules in priority order: `FORWARD` becomes `RETURN` and `DROP`
  becomes `DROP`. The tables are separate lookups in the chip, where a drop
  in either wins (§11.7.7), and a RETURN goes on to the next table's chain,
  so the kernel resolves them the same way.
  - `VLAN_ID <v>` becomes `-i Vlan<v>`; `SRC_IP`/`DST_IP`, `IP_PROTOCOL`,
    `L4_*_PORT` and `L4_*_PORT_RANGE` map to iptables' own matches.
  - The guard's counted rules (`IN_PORTS`, §11.7.9) are left out: each is
    its VLAN's DROP limited to one port, and the DROP below covers it.
  - A rule with any other field or action leaves its table out of the
    mirror, with a note: a FORWARD left out would let a lower DROP catch
    what the chip forwards.
- **Writing it.** One `sudo bash -s -- disunified-acl-cpu` script: the
  chains through `iptables-restore -n` (each declared chain emptied and
  filled in one transaction), the hooks added where missing
  (`iptables -C || -A`), and the mirror's hash written to
  `/run/disunified/acl-cpu`. Removal unhooks, flushes and deletes the
  chains and the marker.
- **Reconcile.** `collect.py` reads the marker (`acl_cpu`, no sudo
  needed); every apply compares it with the hash of the tables as they
  now are and writes the mirror again when they differ. A reboot clears
  the rules and `/run` together, so the next reconcile restores them. The
  apply runs after the chip writes, in `applyACL`, and is not counted as a
  setting.
- **The management guards matter now.** `cutsManagement` and its MAC twin
  (§11.7.6, §11.7.8) were a second line on this SAI. With the mirror, a
  Block covering the controller's replies to the switch would cut the
  on-switch bridge off, and the hold is what prevents it.
- **Tests** (`acl_cpu_test.go`, from the ECS's two captured pushes on the
  DX010's ACL capture): the mirror's exact rules, counted rules left out,
  idempotence through the marker, a lost marker (reboot) rewritten, a
  deleted rule followed, removal, a capture without the section (nothing
  written), the rule shapes and refusals. The FixtureRunner checks each
  script's shape and that the marker is the restore input's hash.

#### 11.32.4 Live on the DX010

Run by hand on 2026-11-15 (21:32-21:33 UTC) under the switch lock, on
`igmp-202511.20261115-6c8eb1cf7` with the deployed bridge running. No cage
and no ACL table was touched: the mirror is kernel-only, so a LAN host's
datagrams to the switch's own address are the test.
- **The setup.** A 300 s timed removal was armed before the first rule. The
  script was the one `cpuScript` generates, for a guard on VLAN 1 trusting
  `192.0.2.1` (`V1_S1` FORWARD, `V1_DROP`) and one IP rule dropping UDP from
  the test host to the switch's address, port 9999. A listener on the
  switch held UDP 68, 9998 and 9999. The test host sent three datagrams
  each from source port 67 and 66 to 68, and to 9999 and 9998.
- **Before:** 12 of 12 reached the listener. `iptables -t raw -S` held
  only the two default policies.
- **Mirrored:** the restore and the three hooks went in as generated, and
  the marker held the hash. 6 of 12 arrived: 66 -> 68 and the 9998s.
  The guard's DROP counted 3 (67 -> 68, an untrusted source) and the IP
  rule's DROP 3 (9999); neither reached a socket.
- **Removed:** the script unhooked and deleted the chains and the marker.
  12 of 12 arrived again, and `iptables -t raw -S` matched the baseline.
- **Throughout**, the bridge on the switch kept informing (an inform reply
  at 21:32:35, mid-test), and the SSH session the test ran over was
  untouched.
- **Not run in this hand test:** the mirror written by the bridge from a
  controller push, and caclmgrd's rewrite of the filter table leaving `raw`
  alone. Both ran the same evening through the driver (§11.32.5).

#### 11.32.5 Through the driver (2026-11-15)

Run 22:28-22:44 UTC under the switch lock, on `igmp-202511.20261115-6c8eb1cf7`
with Network 11.0.99. A build of main (70a5a6d) replaced the deployed bridge
for the run, and the deployed one was put back after. The site's ACL rule
list was empty before and after.
- **The rule.** An IPv4 Block saved in the UI, scoped to the DX010 alone:
  UDP from a documentation address (198.51.100.7) to the switch's own
  address, destination port 9999. The next inform's push carried it as
  `switch.acl.ip.1.*` (both addresses as /32, the source side
  `port_range=1-65535`).
- **What the driver wrote.** The chip half as before: `config acl add
  table DUI_IP_ACL L3` over the free ports and PortChannel1 (not its
  members), `RULE_1` (`Active`), the save. Three seconds later, the mirror:
  `DUI_ACL_CPU` hooked from `PREROUTING` on `Vlan+`, `Ethernet+` and
  `PortChannel+`, jumping to `DUI_IP_ACL`, which held the one DROP. The
  marker held the hash the journal logged. The next two cycles wrote
  nothing.
- **Traffic.** The Podman host sent single datagrams from a raw socket,
  with the rule's documentation address as the source. The switch's
  reverse-path filter is loose (2) on Vlan4094, so they arrive. It sent
  three each to 9999 (matched), 9998 and 9997, and three to 9999 from
  another documentation address. None of the three matched ones reached the
  listener, and the DROP counted 3. The other nine arrived.
- **Edited** to ports 9998 and 9999. The push carried two groups
  (`-1`, `-2`). The driver deleted and rewrote `RULE_1` (9998, priority
  9998) and added `RULE_2` (9999, 9997). The mirror held two DROPs in that
  order under a new marker. Both ports were dropped (2 each counted), and
  9997 arrived.
- **caclmgrd.** The driver's own `config acl add table` set caclmgrd off at
  once, but that rewrite came a second before the first mirror write, so it
  proves nothing. A second `ACL_TABLE` event was made by hand (the table's
  description written again with its own value). caclmgrd then flushed and
  rewrote INPUT, FORWARD and OUTPUT and ip6tables' `raw` within half a
  second. IPv4's `raw` table was byte for byte the same (`iptables -t raw
  -S` unchanged).
- **A bridge restart** wrote nothing: the start-up reconcile found the
  marker equal to the tables' hash.
- **A lost marker** (deleted by hand, as a reboot clears `/run` with the
  rules) was written again on the next cycle, with the same hash and rules
  and no second set of hooks.
- **Removed.** The driver deleted the rules lowest first, removed the table
  and saved. Then it removed the mirror: the hooks, the chains (the
  dispatcher too) and the marker. `iptables -t raw -S` was back to its two
  policies. ASIC_DB held no ACL table afterwards. CRM still counted one
  table until its 300 s poll; that was stale, not a leak.
- **The guard half, without a push.** Rogue DHCP Server Detection stays off
  site-wide (Clint's decision), so the controller sends no guard.
  - **The setup.** The bridge was stopped and the guard's rows written by
    hand: `V1_S1` FORWARD from 192.0.2.1, a counted `V1_IN_Ethernet120` and
    `V1_DROP`, all on VLAN_ID 4094. They were `ACL_RULE` rows with no
    `ACL_TABLE`, so nothing was bound and the chip had no entry (orchagent
    waits for the table).
  - **The reconcile's own step.** A test build ran the driver's own
    `Collect` and then `applyACL` on the switch. With no push the chip half
    writes nothing and the mirror follows CONFIG_DB. The mirror was the
    trusted server's RETURN and the VLAN's DROP, on `-i Vlan4094`, with the
    counted rule left out. A second cycle wrote nothing.
  - **Traffic.** Three replies (67 -> 68) from the trusted address arrived
    (RETURN counted 3). Three from an untrusted one were dropped (DROP 3),
    and three from source port 66 arrived. dhclient holds `0.0.0.0:68` for
    eth0, so the listener bound the switch's own address with
    `SO_REUSEADDR` to take the unicast.
  - **The bridge started again.** Its reconcile, under the site's
    snooping-off push, deleted the three rows (orchagent: "Skip removing
    rule ... Table does not exist") and removed the mirror.
- **What the run showed about the design:**
  - Every restore declares both table chains, so a table with no rules has
    an empty, unreferenced chain. The cost is cosmetic. The gain is that a
    table that loses its last rule is emptied in the same transaction.
  - The marker is the reconcile's only view of the kernel, because
    `collect.py` runs without root. A hand `iptables -t raw -F` that leaves
    the marker is not repaired until the tables change or the marker goes.
    A reboot clears both.
- **Not run:** the guard from a real push (detection stays off), and a
  breakout's rebind as the `ACL_TABLE` event. A description rewrite stood
  in for it: caclmgrd handles both as the same table event.
- **Fixtures:** the collect before the run
  (`sonic-igmp-202511.20261115-6c8eb1cf7/collect-acl-mirror.txt`) and the
  three pushes (`controller-11.0.99/system_cfg-dx010-acl-mirror-{1-add,
  2-edit,3-removed}.txt`), replayed in
  `TestReplaySonicDX010ACLMirror`: each push's chip writes and restore
  input as the journal had them, a lost marker rewritten, and a cycle
  after each that writes nothing.

### 11.33 UniFi's client Block (2026-11-15)

UniFi lets an operator Block a client (the client list's Block, or
`cmd/stamgr block-sta`). The controller delivers it to every switch through
the `setparam` envelope's `blocked_sta` and a `block-sta` cmd, not through
`system_cfg` (captured 2026-11-15 by blocking a made-up MAC beside the site's
one blocked client: `docs/feature-map.md` §3, `CLAUDE.md` §5).

#### 11.33.1 What the ECS does

Read-only, 2026-11-15. The ECS's `global_mac_acl` (type ANY, bound ingress
to every front-panel port and PortChannel) held one rule the Policy Table
did not make: `ACL_RULE|global_mac_acl|MB_<mac>` with `PACKET_ACTION DROP`,
`PRIORITY 1023`, `SRC_MAC <mac>` (no mask) and no `VLAN_ID`. Its MAC is the
one client the controller lists as `blocked: true` (`rest/user`), and the
MAC every bridged switch's `setparam` carried as `blocked_sta`. The rule
sits below the controller's MAC rules (9999 down) and, having no VLAN,
covers the management VLAN too; with the ECS's own address a switched host
(§11.32.1) the drop also keeps the client off the switch itself. The
test client's Block was not read on the ECS (its rule's arrival there was
not watched).

#### 11.33.2 As built (`block.go`)

- **The chip.** One rule per blocked client in the driver's own MAC table,
  `DUI_MAC_ACL|MB_<mac>`, exactly the ECS's fields. The table is shared
  with the controller's MAC rules (`RULE_N`, §11.7.8) but not their limit:
  `max_custom_mac_acls` counts only the controller's rules. Each half is
  kept as the switch has it while the other changes or is held. Like every
  MAC rule it needs the image kit's sonic-swss 0003 (the L2 table type);
  without it the Block is noted, not written.
- **The switch's own address.** On this SAI an ACL drop leaves the traps'
  copy to the CPU alone (§11.7.8), so the chip's rule alone would let a
  blocked client reach the switch. The kernel drops that copy: an ebtables
  chain `DUI_MAC_BLOCK`, jumped to first from the kernel Bridge's INPUT,
  `-s <mac> -j DROP` per `MB_` rule the chip holds. That covers ARP, IPv4
  and IPv6 alike, where the raw-table mirror of the L3 tables (§11.32.3) is
  IPv4 only. A routed front-panel port is no Bridge member, so its frames
  do not pass the chain. l2d's own chains (`DUI_MACLOCK`, `DUI_DOT1X`) are
  separate and l2d touches only its own jumps; l2d inserting `DUI_DOT1X`
  at INPUT's head later would put an EAPOL ACCEPT ahead of the block.
  One `sudo bash -s -- disunified-mac-block` script writes the chain and
  `/run/disunified/mac-block`, the chain's hash, which `collect.py` reads
  (`mac_block`); a reboot clears both and the next apply rewrites them.
  The kernel half follows the chip's rules in `applyACL`, after the chip
  writes, and is not counted as a setting.
- **Never the management path.** A client that is the switch itself, its
  gateway or the host the bridge is reached from (`mgmtL2`) is held with a
  note: the drop would cut the switch off from the controller.
- **Timing.** The session takes `blocked_sta` from every `setparam` and the
  `block-sta` / `unblock-sta` cmds onto a known list; the loop hands the list
  to `ApplyDevice` on every apply and reconcile, so a Block reaches the
  switch within a cycle of whichever arrives first.

#### 11.33.3 Tests

`block_test.go` (from the DX010's own captured replies, two clients):
both rules and the chain, idempotence, a lost marker rewriting the chain
only, one client unblocked, then none (table and chain removed); the
controller's MAC rules from the ECS's push beside the block, each half kept
while the other is not set; the gateway held; an image without the L2 type.
`informloop/replay_sonic_block_test.go` replays the whole capture through
the loop: the rules and the chain on the first `setparam` with both
clients, the test client's rule removed on the Unblock's, the cmds writing
nothing more.

#### 11.33.4 Live on the DX010 (2026-11-15)

- **The deployed driver** (main `ed2d486`, installed 16:08 MST) wrote the
  site's one blocked client on the first forced provision as listed: the
  `DUI_MAC_ACL` table (L2, ingress, 30 ports and PortChannel1) with its one
  `MB_` rule Active in STATE_DB, `DUI_MAC_BLOCK` with one DROP, exactly one
  `-j DUI_MAC_BLOCK` in `ebtables -L INPUT` (the listing prints the bare
  line, which the script's grep matches), the marker in `/run/disunified`,
  and a second cycle that wrote nothing. The chip had the entry (`fp show`:
  SrcMac, the bound ports, action `Drop`). Checked by the main session.
- **The frame test** (23:15-23:17 UTC, under the switch lock, Clint's go).
  A test table `DUI_MBTEST` of type L2, bound to Ethernet84 alone (empty
  cage 22), held one rule with the driver's `MB_` fields for a made-up
  source MAC; a 300 s systemd timer stood ready to undo it. The cage went
  into MAC loopback (`bcmcmd "port ce21 lb=mac"`); l2d reported it
  Backup/discarding within 10 s. The switch sent 20 frames out of the
  cage from the test MAC, then 20 from a control MAC (ethertype 0x88B5,
  which no trap matches).
  - **The rule's entry counted exactly 20 packets** (`fp stat get` on its
    stat id: `0x14`). The control MAC's 20 added nothing; the deployed TV
    entry on the same port stayed at 0.
  - The port's counters: `RUC` +40 (both sets), `RPORTD` +44 (the 40 and
    4 other frames, dropped by the port's STP state), `RDBGC7` +20 (its
    select bit 13; the 20 test-source frames alone).
  - So the entry matches by source on a port whose STP state also drops
    the frame. A switched frame forwarded past the drop was not seen
    (one linked port, the uplink), as in §11.7.8.
  - **Afterwards.** Loopback off, rule and table removed, no `DUI_MBTEST`
    key in CONFIG_DB, STATE_DB or `config_db.json`, no test entry in the
    chip; the deployed rule and chain untouched. The loop lasted about 25 s,
    under the controller's Auto STP Edge pin (§11.23.3).
- **Not seen live:** the kernel chain dropping a frame to the switch's own
  address. Frames on a discarding port never reach the Bridge's INPUT, and
  the one linked port is the uplink.

### 11.34 A PortChannel member's own chip STP ports, and an l2d restart that logged the chip's refusal (2026-11-15)

On 2026-11-15 the DX010's uplink became a two-member LACP aggregate,
PortChannel1 = Ethernet0 + Ethernet4 (the controller's push, 13:38-13:41
local). From then on every start of disunified-l2d made syncd log, once per
VLAN instance, `_brcm_sai_stp_lag_member_check: Lag member 68 exists as STP
PORT 0x0044012e000000NN in STG NN` and `brcm_sai_create_stp_port: STP lag
member check failed`, and orchagent `addStpPort: Failed to add STP port
PortChannel1 instance <vid>`. The uplink stayed up and mstpd made it the root
port; orchagent did not abort. Fixtures:
`sonic-igmp-202511.20261115-6c8eb1cf7/lag-stp-stray-syslog-20261115.txt`
(the syslog of both occurrences and of a later restart) and
`stp-stray-20261115.json` (the STP tables, read-only, 15:15 local).

#### 11.34.1 What happened

- **It began at the join, not at a restart.** The first refusals are at
  13:38:49, as PortChannel1 entered Vlan4094 and l2d wrote its first state
  (`state 0`, disabled). Each later l2d start wrote PortChannel1's states
  afresh and was refused the same way: 44 lines per start (11 instances x
  the four lines), at every restart the other sessions made that afternoon,
  and again at the 15:10 deploy (by then only Vlan4094's instance was left:
  below).
- **Port 68 is Ethernet0** (`port:0x44` in its own FDB events; `ps ce0`
  `ce0( 68)`). Ethernet4 was never named.
- **Ethernet0 got a bridge port back 50 ms after losing it.** The join's
  sequence (syslog, 13:38:43): l2d logs `Ethernet0: forwarding -> disabled`
  as the kernel takes the port out of `Bridge` (`left promiscuous mode`,
  `entered disabled state`), orchagent removes Ethernet0 from Vlan4094 and
  `Remove bridge port Ethernet0` (.556), then **`Add bridge port Ethernet0`**
  (.605), and only after that `Add member Ethernet0 to LAG PortChannel1`
  (.957). Ethernet4 left Vlan4094 down (no cable until 13:49): already
  disabled, so no state change, no write, and no bridge port came back.
- **The tables at 15:15**: ASIC_DB holds a bridge port for Ethernet0, which
  is in no VLAN and not in the kernel `Bridge`, and one STP port of it
  (BLOCKING) in Vlan4094's instance; PortChannel1 has none there. APPL_DB
  holds `STP_PORT_STATE_TABLE:Ethernet0:<vid>` for all eleven VLANs of
  13:38, though l2d had deleted them as Ethernet0 left. In the other ten
  instances PortChannel1 had STP ports: the VLAN-delete test (§11.29.6,
  21:33-21:49 UTC) deleted those ten VLANs and made them again, deleting a
  VLAN removes every STP port of its instance (Ethernet0's too), and the
  re-made instances took PortChannel1's. The 15:10 deploy then pruned them
  for good (§11.29).
- **The chip** (`bcmcmd "stg show"`, read-only): STG 9, Vlan4094's,
  `Disable: ce1` and `Block: ce0,ce2-ce31`, against `Forward: ce0-ce1` in the
  re-made instances. So in the chip **neither uplink member forwarded the
  native VLAN**, while mstpd and the kernel had PortChannel1 forwarding as
  root port. The switch stayed reachable because frames to its own address
  reach the CPU through the traps' copy whatever the STP state (§11.7.8);
  a host behind any other DX010 port in the native VLAN would have been cut
  off from the LAN. None was cabled.
- **A second occurrence** earlier the same day: Ethernet48, a looped empty
  cage, leaving its last VLAN at 12:01:30 (`blocking -> disabled`, bridge
  port re-added 213 ms after its removal). It joined no PortChannel, so the
  chip never refused anything. A scan of the syslog back to 2026-11-07 finds
  no other removal followed by an add within a second.

#### 11.34.2 The cause

StpOrch's `addStpPort` (202505 and 202511 `stporch.cpp`) creates a bridge
port for a port that has none (`gPortsOrch->addBridgePort(port)`) before it
creates the STP port. orchagent drains its consumers in its own order, so
the state l2d wrote at .321 was taken after portsorch had processed the VLAN
member removal and removed the bridge port (.556, `removeStpPorts` with it):
StpOrch then made a new bridge port and an STP port in every instance for a
port about to become a LAG member. When PortChannel1 asked for its own STP
port in an instance, the Broadcom SAI refused it because a member already
had one (`_brcm_sai_stp_lag_member_check`). l2d's own bookkeeping was right:
it forgot Ethernet0 on the bridge's `RTM_DELLINK` and dropped its keys from
APPL_DB, but its write was already in the producer's queue, and orchagent
put the keys back when it took it.

Neither the driver's plan nor l2.json had anything to do with it:
`l2SetPorts` names PortChannel1, never its members, and l2d writes states
only for kernel `Bridge` members. What l2d wrote that it should not have is
the departing port's "disabled". The kernel's own order makes it
recognisable: `del_nbp` removes `Bridge/brif/<port>` before
`br_stp_disable_port` sends the disabled notification, so when l2d reads it
the port is no longer listed (seen live, §11.34.5).

**The ECS** (read-only, 2026-11-15): one instance, `STP_PORT_STATE_TABLE`
rows for every plain port and for PortChannel1, 2 and 6, none for their six
members, and 51 chip STP ports for its 52 rows (`dummy` is no orchagent
port). The aggregate is the only STP port, which is what l2d meant to do.

No kit patch: StpOrch did what it was asked, for a port it knows, and the
bridge port it adds is what upstream relies on when a state arrives before
the VLAN membership. Refusing a port in no VLAN would change that ordering
for upstream's own stpd. The write was l2d's to avoid.

#### 11.34.3 The fix (`l2d.py`)

- **No state for a port leaving `Bridge`.** `on_link` checks `in_bridge`
  (`Bridge/brif/<port>`) before it writes; a port gone from it is forgotten
  instead. `kstate` is None for a port with no `brport` (the 2 s pass read
  a missing state file as 0, disabled, which would have been the same
  write), and the pass forgets such a port.
- **The repair (`repair_strays`).** A state key in APPL_DB for an interface
  outside `Bridge` (the late write leaves one) sends l2d to ASIC_DB, as
  does a timer once a minute: STP ports whose bridge port belongs to an
  interface outside `Bridge` are strays. One seen on two passes is removed
  through StpOrch (a producer DEL for each of its keys, its chip VLANs and
  the current VLANs: a no-op for an instance it holds no STP port in), l2d
  waits up to 2 s for ASIC_DB to lose them, and drops its record of the
  member's PortChannel's states so the same pass writes them again (the
  DEL goes first; orchagent's queue is ordered by key, `Ethernet...` before
  `PortChannel...`). Keys of an outside interface with no chip STP port
  are only deleted from APPL_DB, as before. The DEL is sent only for an
  interface orchagent knows (APPL_DB and STATE_DB), as every l2d write is:
  a key for an unknown port stops StpOrch's queue (§11.5).
- **What stays**: the bridge port StpOrch made. No orchagent path removes a
  bridge port of a port in no VLAN; it is harmless to the LAG (the SAI's
  check is on STP ports) and a reboot clears it. l2d logs it with the
  repair.
- **A PortChannel waits for its member's repair.** While one of its
  members is a stray, `sync_stp` writes nothing for the PortChannel (the
  chip would refuse it), so a restart over a stray logs no refusal: the
  PortChannel's states and its VLAN's mapping follow two seconds later.
- **The narrow race left**: a state written while the port was still a
  member (an mstpd change in the same millisecond) and taken after the
  removal. The repair clears it within two passes.

#### 11.34.4 Tests

`TestL2dLagMemberStrayStpPorts` (`testdata/l2d_strays.py`) replays the
syslog's two departing ports (Ethernet0 and Ethernet48) into `on_link`
gone from `brif` (no write, forgotten) and still in it (a link drop: one
write), then runs a fresh l2d (a restart) over the captured tables under a
model of StpOrch and the SAI's LAG-member check: the first pass writes
nothing for PortChannel1 and is refused nothing; the second removes
Ethernet0's STP ports and writes PortChannel1's (FORWARDING); the third
writes nothing. Run with the l2d.py before this change, the leaving port
writes, the first pass is refused for PortChannel1 in Vlan4094's instance
as it was live, and the refusal never clears.

#### 11.34.5 Live on the DX010 (2026-11-15 22:58-23:10 UTC, under the test lock)

Two empty cages, 25 and 26 (Ethernet96, Ethernet100), made PortChannel9 by
hand in the driver's order (their snooping static rows out, `config vlan
member del 4094`, `config portchannel add PortChannel9 --fast-rate true
--min-links 1`, the PortChannel's MTU to the members' 1500, the members in,
`config vlan member add -u 4094 PortChannel9`), with Ethernet96 in MAC
loopback (`bcmcmd "port ce24 lb=mac"`: up, blocking, so it leaves with a
state change, as Ethernet0 did) and the bridge stopped. A netlink listener
recorded `brif/<port>` at each notification. The uplink was left alone but
for the one restart of the fixed l2d, which a timed revert covered (below).

- **The kernel's order**, seen: `NEW Ethernet96 state=0 master=5
  brif=False`, then `DEL Ethernet96`. The leaving port's disabled arrives
  with the port already gone from `brif`.
- **The deployed l2d (fef74fe) wrote it every time** (`Ethernet96:
  blocking -> disabled`, and APPL_DB kept `Ethernet96:4094` afterwards),
  but in five joins StpOrch took the write before portsorch's removal
  (about 70 ms apart, one VLAN): no bridge port came back and PortChannel9
  got its STP port. On the uplink the same write was one of eleven, with
  the snooping orch busy, and lost the race.
- **Reproduced** by sending that write through StpOrch's producer just after
  the removal, where it landed live: `Add bridge port Ethernet96`, then at
  PortChannel9's first state `Lag member 17 exists as STP PORT
  0x0011012e00000009 in STG 9` and `Failed to add STP port PortChannel9
  instance 4094`; STG 9 showed ce24 `Block` and ce25 `Disable`. A restart
  of the deployed l2d then logged the refusal for both PortChannel1 and
  PortChannel9.
- **The fixed l2d, started over both strays**, with the revert armed
  (`systemd-run` of a script that pinged the gateway every 10 s for 8 min
  and, after three misses, would have put the deployed l2d back and then
  run its `--deadman`): the first start still logged both refusals (that
  build wrote the PortChannels at once), then two seconds later `Ethernet0
  is no bridge port (a member of PortChannel1) but held 1 STP ports in the
  chip: removed, PortChannel1's states written again`, and the same for
  Ethernet96. **STG 9 went to `Forward: ce0-ce1`**: the uplink forwarded the
  native VLAN in the chip for the first time since 13:38. APPL_DB lost
  every key of an interface outside `Bridge`; the gateway answered
  throughout.
- **The hold-back, live**: l2d stopped (mstpd and the chip stay as they
  are), a fresh stray made on Ethernet96 the same way, the build with the
  hold-back started: no syncd or orchagent error at all, the stray removed
  at 2 s, Vlan4094 mapped at 6 s. A plain restart after that: `started` and
  the mapping, nothing else.
- **The other transitions with the fixed l2d**: a join by the CLI alone
  (no injected write) wrote nothing for the departing Ethernet96 and left
  no key behind; the reverse (PortChannel9 out of Vlan4094, members out,
  deleted, members back untagged) gave each member its own STP port in
  Vlan4094's instance; three link drops of the looped member wrote and
  refused nothing (PortChannel9 stayed LACP-down: teamd does not select a
  port that hears its own LACPDUs, so it never forwarded).
- **Not run live**: BPDU guard and loop protection on an aggregate that is
  up (the loopback rig cannot bring a LACP aggregate up; both act on a
  PortChannel through its members as before, §11.28, and this change does
  not touch them), and the lane switch's `PortChannel<100*cage+lag>` (it
  needs a breakout). Neither path names its PortChannel differently in
  l2d: `in_bridge`, the repair and the hold-back go by kernel membership
  and CONFIG_DB `PORTCHANNEL_MEMBER`, whatever the PortChannel's number.
- **Left as found**: PortChannel9 deleted, both cages back untagged in
  Vlan4094 with their 14 snooping rows, loopback off, the deployed l2d
  (fef74fe) back in place and restarted (quiet: `started`, the mapping, no
  ERR), the bridge started (`0 of 32 ports changed`), CONFIG_DB identical
  to the pre-test dump in all 37 tables, `l2.json` unchanged.
- **What stays until a reboot**: the bridge port StpOrch made for Ethernet0
  (ASIC_DB: a bridge port for an interface outside `Bridge`). Ethernet96's
  went when it rejoined Vlan4094 and left again. The check after the next
  reboot: no bridge port in ASIC_DB for a port that is not in
  `Bridge/brif`, and `bcmcmd "stg show"` with every PortChannel member
  forwarding in each STG whose VLAN the PortChannel carries.

### 11.35 STP instances of deleted VLANs left in the chip (2026-11-15)

After the deploy that brought the VLAN prune (§11.29) the DX010's chip held
STP groups that no VLAN used, with STP ports in them. l2d's header said
that deleting a VLAN removes its instance's port states. That is true, but
StpOrch can then make the instance again.

#### 11.35.1 What the switch held

Read-only, 2026-11-15 16:28 local (`stp-orphans-20261115.json` is the same
state after the repro below added one more):

| | Vlan4094 | the others |
|---|---|---|
| `bcmcmd "stg show"` | STG 9, `contains 1 VLAN (4094)`, Forward ce0-ce1 (PortChannel1's members), Block ce2-ce31 | STG 2-7 and 10-12, each `contains 0 VLANs`; Forward ce0-ce1, Block ce23 (Ethernet92), and in STG 2-4 also ce22 (Ethernet88) |
| ASIC_DB `SAI_OBJECT_TYPE_STP` | `0x…63d`, the STP attribute of Vlan4094's VLAN object | nine objects no VLAN object names, plus the switch's default (`0x…22`, no STP port) |
| ASIC_DB `SAI_OBJECT_TYPE_STP_PORT` | 31: every bridge port | 21: PortChannel1 in all nine, Ethernet92 in all nine, Ethernet88 in three |
| APPL_DB `STP_VLAN_INSTANCE_TABLE` / `STP_PORT_STATE_TABLE` | `Vlan4094` / 31 keys `<port>:4094` | none |
| STATE_DB `STP_TABLE\|GLOBAL` | `max_stp_inst 510` | |

So there were nine orphans, not ten. The tenth VLAN's (Vlan4010's)
removal came last and was clean.

#### 11.35.2 When it happened

At the deploy, 15:10 local (`stp-orphan-syslog-20261115.txt`). The deb
install restarted l2d (its sha had changed) at 15:10:11.66. Fifteen
milliseconds later the bridge's first reconcile ran `config vlan del -m
2,8,9,10,69,1000,2000,3000,4000,4010`. The new l2d started at 15:10:12.0,
and its first pass wrote every bridge port's state into every VLAN's
instance (31 ports x 11 VLANs). From 15:10:13.54 to 15:10:14.32 orchagent
removed the VLANs, each as `brcm_sai_remove_stp: STG instance N deleted`
then `removeVlan: Remove VLAN VlanN`. From 15:10:14.33 to 15:10:14.43,
before l2d's next pass had noticed (`VlanN is gone`, from 15:10:14.44),
syncd logged `STG instance N created` nine times. Only Vlan4010's removal
came after that burst, at 15:10:14.69.

Two quiet deletes before then were clean: Vlan3999 on 2026-11-14, twice
(`STG instance 13 deleted`, nothing created). So were the 21:34 UTC deletes
of the §11.29 live run, which had no l2d restart.

#### 11.35.3 The cause, from the 202511 source

Who removes what when a VLAN goes:

1. The CLI deletes the CONFIG_DB row. vlanmgrd sends APPL_DB `VLAN_TABLE`
   DEL.
2. portsorch's `removeVlan` (`portsorch.cpp:7433`): with `m_stp_id` set,
   it calls `removeVlanFromStpInstance(vlan, 0)`. That sets the VLAN back
   to the default STP object and calls `removeStpInstance(vlan.m_stp_id)`,
   which removes every PHY and LAG port's STP port in the instance, then
   the object.
3. l2d sees the VLAN gone from CONFIG_DB and ASIC_DB at its next pass, up
   to 2 s later. It dropped its `<port>:<vid>` keys from APPL_DB directly,
   with no DEL to StpOrch.

Every step works on its own. The leak is a port-state SET that StpOrch
takes after step 2:

- `doStpPortStateTask` → `updateStpPortState` → `addStpPort` creates the
  instance when StpOrch has none of that id
  (`getStpInstanceOid` → `addStpInstance`). Then it creates the port's STP
  port in it. l2d relies on this: states go first, the VLAN mapping after
  (§11.5).
- Nothing removes such an instance again. `removeStpInstance` is reached
  only from `removeVlanFromStpInstance`, which needs a VLAN orchagent has.
  A port-state DEL removes the STP port and leaves the instance.
- **orchagent runs portsorch before StpOrch in each pass**
  (`orchdaemon.cpp` `m_orchList`). So when orchagent is busy, the VLAN's
  DEL overtakes states that l2d queued before it. The restart's burst of
  341 states was exactly that: the states still queued were those of
  Ethernet88, Ethernet92 and PortChannel1. Which of the 341 states are left
  over is not in l2d's control.

The `0` that portsorch passes is a second fault, a smaller one:
`removeVlanFromStpInstance` looks up `m_vlanAliasToStpInstanceMap` by that
argument, so the VLAN's alias never leaves the instance's list. Only
`STP_INST_PORT_FLUSH_TABLE` reads that list, and l2d does not use it.

**The ECS cannot strand an instance this way.** Read-only, the same
evening, its ASIC_DB holds 2 STP objects (the default and instance 0) and
51 STP ports, and its 12 VLAN objects carry no STP attribute of their own.
Every APPL_DB port state is `<port>:0`. Ubiquiti's orchagent keeps every
VLAN in one instance (§11.5), so a network that leaves it takes no instance
with it.

#### 11.35.4 Reproduced on a test VLAN (16:37-16:38 local, under the lock, the bridge stopped)

These steps ran on the empty cage Ethernet112, cage 29, which has no
module and no link:

1. `config vlan add 3001` and `config vlan member add 3001 Ethernet112`
   (tagged). l2d mapped it: `Vlan3001 in STP instance 3001 (31 ports)`,
   STG 8. That state is `stp-vlan3001-20261115.json`.
2. A delete as the prune does it: `config vlan member del`, then `config
   vlan del`. It was clean: `STG instance 8 deleted`, and the counts went
   back to 11 STP objects and 52 STP ports.
3. The same VLAN made again and deleted again. This time one state for
   `Ethernet112:3001` went through StpOrch's producer 35 ms after
   `removeVlan`, which is where the 15:10 writes landed: `STG instance 8
   created`. STG 8 then contained 0 VLANs, with ce28 (Ethernet112) blocked
   in it. l2d logged `Vlan3001 is gone` and left it there. That state is
   `stp-orphans-20261115.json`, with ten orphans.

CONFIG_DB was identical before and after, in every key and in `sonic-cfggen
-d --print-data`.

#### 11.35.5 The fix (`l2d.py`)

The driver's VLAN delete is not the cause, and its order (members, then the
VLAN) is the CLI's. Any VLAN delete can lose this race, a hand delete
included, so the fix is in l2d:

- **A gone VLAN's port keys go out through the producer** (`forget_vlan`).
  The DEL is sent only for a port orchagent knows: a DEL for an unknown port
  stops the table's queue for good. A DEL replaces a SET still queued for
  the same key, both in the producer's key set and in orchagent's
  `m_toSync`. If StpOrch has already made the instance again, the DEL
  removes that port's STP port from it. The VLAN key still goes from APPL_DB
  directly: a producer DEL for a missing VLAN never completes.
- **A port-state change writes only into VLANs CONFIG_DB still has**
  (`on_link`). Until now it wrote into l2d's own list, which was up to a
  pass stale.
- **The id is retired**: `/run/disunified-l2d/stp-retired` records the
  VLAN id and the time. It is in `/run` because a reboot or a swss restart
  empties the chip's instances as well, and an l2d restart must keep the
  list.
- **`repair_orphans`** counts the instances no VLAN uses: ASIC_DB's STP
  objects, less the default one, less one per VLAN that l2d has written
  states for. It runs only once every VLAN has its states. While the count
  is above zero, it removes one retired id per pass, oldest first,
  `RETIRE_SETTLE` (4 s) after its VLAN went. It never takes an id that
  CONFIG_DB, ASIC_DB or APPL_DB `VLAN_TABLE` still has a VLAN for, so it
  never touches an instance in use, Vlan4094's included.
- **`unmap_instance` removes an instance by the one path StpOrch has**:
  1. It makes a VLAN of that id in orchagent alone: APPL_DB `VLAN_TABLE`
     through the producer, with no CONFIG_DB row, no member, no kernel
     netdev and no host interface.
  2. It maps that VLAN into the instance, which `addVlanToStpInstance`
     reuses.
  3. It unmaps it, so `removeStpInstance` removes the instance and every
     STP port in it.
  4. It removes the VLAN.

  A marker file (`stp-repair`) lets the next l2d finish an unmap that a
  crash cut short.
- An id leaves the list once it has been tried, when its VLAN comes back
  (StpOrch then reuses the instance), or 60 s after its VLAN went if the
  chip shows no orphan. Orphans for which l2d has no id are logged once,
  with the count, and left alone. StpOrch keeps the ids in memory only, so
  nothing else can name them.

There is no kit patch. A patch to StpOrch could remove a VLAN-less
instance when its last STP port goes, which would make the transient VLAN
unnecessary. But image builds are on hold, and the l2d fix works on the
stock orchagent through its own paths.

#### 11.35.6 What a switch accumulates without it

At most one instance per VLAN id. A VLAN made again with the same id
reuses its orphan, because StpOrch's id-to-object map still holds it. Each
leak needs orchagent busy as a VLAN goes. Before 2026-11-15 the driver
never deleted a VLAN. Since then, every deploy that changes `l2d.py` and
also prunes a VLAN is the 15:10 case, and so is a hand delete during a
burst of state changes.

The SAI's limit is `SAI_SWITCH_ATTR_MAX_STP_INSTANCE` 511 (StpOrch
publishes 510 in STATE_DB). Once orphans and live VLANs reach it,
`addStpInstance` fails for every new VLAN. Its states are then dropped
(`addStpPort` returns no oid, and `updateStpPortState` erases the task),
`map_vlan` never sees its STP ports, and the VLAN stays in the default
STG. There it forwards on every member whatever mstpd says, so it has no
loop protection in the chip.

The orphans themselves forward nothing, because no VLAN uses them. But
they hold STP ports: PortChannel1's in all nine here.

#### 11.35.7 Tests

`TestL2dRemovesOrphanedStpInstances` and `TestL2dVlanDeleteRace`
(`testdata/l2d_orphans.py`) run a model of portsorch and StpOrch, with
StpOrch's instance ids, over the two captures. The capture cannot say
which id each orphan had: the model gives them the nine ids the 15:10
prune deleted, in oid order, and 3001 to the newest.

- **Cleanup**: l2d's list holds the ten ids. There is one unmap per pass,
  each making and removing one VLAN. At the end the chip has two STP
  objects and 31 STP ports, Vlan4094's states are unchanged and get no write
  but the ones they already had, and no `VLAN_TABLE` key is left behind.
- **Race**: a restart's first pass is queued, then Vlan3001 is deleted as
  the CLI does it. orchagent takes the VLAN's DEL first and then the queued
  states, which make instance 3001 again with 31 STP ports. The next pass
  DELs its ports through the producer, and the unmap removes the instance
  after `RETIRE_SETTLE`. The nine orphans with no id stay, and the count is
  logged.

Run with the l2d.py before this change, instance 3001 keeps its 31 STP
ports, and the ten ids remove nothing.

#### 11.35.8 Live on the DX010 (2026-11-15 16:46-16:48 local, under the lock)

Setup: the bridge was stopped, so that it would not reinstall its own l2d.
A timed revert was armed: a `systemd-run` script that pinged the gateway
every 10 s for 15 minutes and, after three misses in a row, would have put
the deployed l2d back and started the bridge. The fixed l2d was installed
by hand.

1. **The test orphan first.** With the list holding only 3001, l2d was
   restarted: `Create an empty VLAN Vlan3001`, `STG instance 8 deleted`,
   `Remove VLAN Vlan3001`, `STP instance 3001 (its VLAN gone) was still in
   the chip: removed, 9 left`, all within 0.3 s of the start. `stg show`
   differed from before in STG 8's three lines only, and STG 9 was
   unchanged.
2. **The nine, with the ids from the 15:10 log.** l2d was restarted with
   them in the list. It removed one every 2.3 s: instance 2 was STG 7, 8
   STG 4, 9 STG 12, 10 STG 5, 69 STG 11, 1000 STG 6, 2000 STG 2, 3000
   STG 3 and 4000 STG 10. Every id had an orphan, and the log ended at `0
   left`.
3. **After**: `stg show` lists STG 0, STG 1 (VLANs 1 and 4095) and STG 9
   (`contains 1 VLAN (4094)`, Forward ce0-ce1, Block ce2-ce31). ASIC_DB
   holds 2 STP objects and 31 STP ports. APPL_DB holds only Vlan4094's
   `VLAN_TABLE` and `STP_VLAN_INSTANCE_TABLE` keys. The gateway answered
   throughout.
4. **Left as found**: the deployed l2d (`77c8773e…`) was put back and
   restarted, and was quiet (`started`, the mapping). `/run/disunified-l2d`
   was removed, the revert unit stopped and the bridge started (`0 of 32
   ports changed`). CONFIG_DB was identical to the dump before the test.
   The fix reaches the switch with the next deploy.

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
