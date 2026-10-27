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

## 4. Open questions to settle during Phase A

- Does the `202505` VS image carry `sonic-stp`, and does the Broadcom
  image? (`show spanning_tree`, `docker ps` for an `stp` container.)
  Decides whether STP is reported or refused.
- Does the VS's hardware SKU ship a `platform.json` with breakout modes,
  so dynamic port breakout can be exercised before the hardware?
- Which Proxmox node hosts the VS guest, and whether the guest stays as a
  permanent lab switch after the DX010 is adopted.
- Whether `sonic-installer` on `20181130.77` accepts a `202505` image, or
  B3 goes straight to ONIE. The VS rehearsal cannot answer this; only the
  unit can.

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
