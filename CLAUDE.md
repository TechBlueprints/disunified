# disunified — working notes and rules for Claude Code

Read this first. It is the map of the repo and the rules that came from live
failures. Deeper material is in `docs/`; the rules for the vendor layer are
in `internal/drivers/CLAUDE.md`.

## 1. What this is

A bridge that presents a non-UniFi device to a UniFi Network controller as an
adopted UniFi switch (read: ports, stats, topology; write: the controller's
port and switch config applied to the vendor device). The initial public
release (2026-09-20) shipped two drivers, both verified live on Network
10.6.106: `arista-eos` (Clint's Arista DCS-7160-48TC6-F on EOS 4.26.14M,
claimed as `UDC48X6` "USW Leaf") and `proxmox` (each node of his Proxmox VE
9.1 cluster as a `UDC48X6` "USW Leaf" too; a `USWF07D` "ECS Core" for its
first day only). **Phase 0 (read) and phase 1
(control) are done for both** — see `docs/feature-map.md` for the
per-feature status. A third driver, `apc-pdu`, followed: Clint's APC AP7931
rack PDU (NMC AOS 3.9.2) as a `USPPDUP` power distribution unit, outlets read
and switched (`docs/drivers/apc-pdu.md`). A fourth, `apc-ups`, presents his APC
SMTL1500RM3UC Smart-UPS as a `USPDA2B` "UPS 2U Pro" (first as a `USWDA25`
"UPS 2U"), read over Modbus TCP from the unit's own SmartConnect port — no
card, no cable (`docs/drivers/apc-ups.md`); outlet-group control built and
enabled, never yet exercised on the unit. A fifth, `podman`, presents Clint's
Podman host (a QEMU guest of proxmox-2) as a `UDC48X6` with its containers as
ports, on the proxmox pattern (`docs/drivers/podman.md`); built and adopted
2026-09-27: placed under its node's VM port; its address made static through
the controller's IP Settings (`control.address`, NetworkManager via nmcli) --
the loop's read-only branch now applies address and NUT blocks too. A sixth,
`apc-backups`, presents Clint's APC Back-UPS Pro 500 (BG500, embedded NMC)
through the card's web pages as a `USPDA2B` -- built and adopted 2026-09-27
(`docs/drivers/apc-backups.md`); SNMP tells nothing about that family, the
usp IP Settings push carries no gateway (`options.gateway`). A seventh,
`sonic`, presents a community SONiC switch as a `USWF07D` "ECS Core": built
2026-10-27 on a SONiC virtual-switch guest (build 202505.1238524, hardware
SKU `Seastone-DX010`) ahead of Clint's Celestica DX010, its Redis tables
read through swsscommon in one SSH exec per poll and the controller's
config written through SONiC's `config` CLI; the control features (ports,
VLANs with the `native_vlan` stand-in for VLAN 1, LAG, mirror, storm
control, jumbo, NTP, SSH keys, reboot, a static address move) were
verified live against that build (`docs/drivers/sonic.md` §6.6) except: a forced speed change (the live 100G was already the cage's speed, so no speed command ran), the driver's own aggregate creation (live it stopped at the PortChannel MTU refusal and was finished by hand; the fixed sequence is fixture- and replay-tested), a gateway change and the undo of a move whose new address does not answer (never run live) and SNMP (not pushed with the site's SNMP off); syslog was verified live the same evening once the site's remote logging was switched on;
the lab session's own pushes are replayed in
`internal/informloop/replay_sonic_live_test.go`. Community images have no
STP daemon, so RSTP and loop protection run through the driver's own
disunified-l2d (mstpd built on the switch, its port states mirrored into
orchagent's StpOrch, one STP instance per VLAN; `docs/drivers/sonic.md`
§11.5): built 2026-11-01, verified live on the DX010 by hand and then
through the deployed driver; on by default (`options.l2d: off` opts out);
hardware-only parts wait for the unit, and
time zone, hostname and locate (§7.5) are fixture-tested only; breakout ran every mode live on the DX010's 202511 image on 2026-11-07, once eth0's gateway-less address row was removed (`docs/drivers/sonic.md` §11.11).
UniFi's DHCP guarding (Rogue DHCP Server Detection) and switch IP ACL rules
are the driver's own L3 ACL tables (`DUI_DHCP_GUARD`, `DUI_IP_ACL`; built
2026-11-01 from the ECS's and the DX010's captured pushes and verified live
on the DX010 the same evening, §11.7.6-11.7.7; never `config acl update`;
since 2026-11-14 the guard counts what it drops per port, a counted
`IN_PORTS` rule each in a table type of its own, for the rogue-DHCP
anomaly bit, verified by hand, §11.7.9);
MAC ACL rules are `DUI_MAC_ACL`, an `L2` table that needs the image kit's
sonic-swss 0003 (orchagent's outer-MAC match; claimed only when Start finds
it): written, Active in the chip and removed live on the DX010 2026-11-06;
the chip's entry matched exactly the blocked MAC's frames, a dropped switched
frame is unseen (one linked port), and frames to the switch itself still
reach its CPU through the traps' copy, as they do under any ACL drop on
202511's SAI, the DHCP guard's included (§11.7.8, traced 2026-11-15).
UniFi's 802.1X Control is hostapd (wired driver, built on the switch like
mstpd) under disunified-l2d, the port closed by the MAC filter's learn mode
and each authenticated host a static FDB entry, failed attempts as anomaly
128: built 2026-11-15, verified by hand on an empty cage with a test RADIUS
server, the controller's push captured on 11.0.99 and replayed (MAB and
the Unauthorized VLAN not built), no image change
(`docs/drivers/sonic.md` §11.26).
UniFi's MAC Address Filter violations (anomaly 256) are counted per port by
disunified-l2d since 2026-11-14: a secured port's learning is `cpu_trap` in
APPL_DB behind a kernel-bridge drop, the port stays up (§11.18.2, verified
by hand on an empty cage in MAC loopback).
IGMP snooping runs through l2mcd on the repo's 202511 image: the chip half
verified by hand on the DX010 2026-11-03 (group entries program, optimised
flood is refused by the SAI), the driver's write path built behind
`options.igmp: on` (off by default until the traffic test with cabled
hosts), and on for the DX010 since 2026-11-05: verified live through the
driver; l2mcd queries from 0.0.0.0 and learns no mrouter port
(`docs/drivers/sonic.md` §8.14).
UniFi's Layer 3 switching -- routed networks, the controller's Inter-VLAN
routing transit, switch static routes, DHCP server (busybox udhcpd) and
relay, Isolate Network -- was captured, built and verified live on the
DX010 2026-11-07 (`docs/drivers/sonic.md` §11.13); the transit's default
route waits for VLAN 4040 on the uplink. UniFi's BGP on a switch (an
uploaded FRR file) was captured, built and verified live with a peer the
same evening (§11.14), and OSPF (`feature_caps2` 4; ospfd run by the driver
in the bgp container) with Clint's OK for a test routed network, verified
live with a peer on the kernel bridge.
Routed ports and routed aggregates (`feature_caps2` 16, the port editor's
Routing) and DHCP fixed IPs on a switch-routed network were captured,
built and verified live 2026-11-08 (`docs/drivers/sonic.md` §11.16); the
controller pushes only a routed aggregate's first member, and VRRP is
offered only for an MC-LAG pair ("L3 Automatic Failover"), so a single
switch never gets it.
**Since 2026-11-02 the DX010's bridge runs on the switch itself**
(`options.transport: local`, a `.deb` whose config and state live in
`/host/disunified`; `docs/drivers/sonic-on-switch.md`). The map of the two
deployments and of the image patches by what each enables is
`docs/drivers/sonic-running.md`.
An eighth, `breakout`, presents a split cage of another bridged switch as a
five-port switch of its own (`USFXG`, the "lane switch" of
`docs/breakout-lag.md` §6): built 2026-11-14 with `sonic` as the parent,
and `arista-eos` as the second since 2026-11-15 (its lane groups are
`Port-Channel<1000+10*cage+lag>`: EOS stops at 2000; the lane batches run
by hand on the 7160's empty test cage), read from the parent's snapshot and
written through the parent's plan in the parent's process. Since
2026-11-16 a split cage gets its lane switch by itself (`lane_switches:
auto`, the default; the lanes handed over at the adoption and back on a
Forget), and since 2026-11-17 the bridge asks the controller for that
adoption itself, by the lane switch's exact MAC, once its parent is adopted
(Clint withdrew the operator's-consent rule: "it doesn't need permission
when a breakout dac is inserted"; a Forget is followed by a re-adoption --
Clint: "if you want to really forget it unplug it" -- and
`lane_switches: manual` leaves them all to the operator,
`docs/drivers/breakout.md` §16.12; since 2026-11-18 a cage the parent
reads as one port on four collects in a row over two minutes retires its
automatic lane switch: stopped, detached, its record forgotten by its exact
MAC and its state directory removed, so a breakout put back is adopted
afresh with UniFi's defaults; a manual parent's, one without a controller
API and a declared one are stopped only, §16.13), and the DX010's
cage 3 is the first one adopted live (`docs/drivers/breakout.md` §0, §16;
the Arista parent is fixture-tested only, §7b). It reports a link-local
address of its own: one that shared its parent's made the controller
refuse every save of the parent.
The live runs also produced generic fixes: a write budget separate from
the collect one (`control.apply_timeout`, or the driver's own through
`devicemodel.ApplyBudget`; the collect budget otherwise), `sshrun` offering
agent keys one at a time (a `.pub` selects one), the `_type: reboot`
reply, and the reported address following an applied move
(`devicemodel.AddressReporter`).
**Since 2026-11-17 each Proxmox node runs its own bridge**
(`options.transport: local`, the same `.deb` as a root service, config in
`/etc/disunified`, state in `/var/lib/disunified`;
`docs/drivers/proxmox-on-node.md`, §8 the live run): the three bridges
coordinate guest port numbers only through the guests' tags in pmxcfs, and a
collect of an unmounted `/etc/pve` fails instead of reading "no guests".
The same work gave the loop a baseline for its fresh-port comparison
(`internal/informloop/layout.go`, §5.5 there): `layout.json` in the device's
state directory (layout signature, port identities, fresh ports), or the
startup snapshot when there is none. A port whose occupant changed between
the start and the first collect, or while the bridge was stopped, is fresh,
and a port withheld at the stop stays withheld: a restart no longer releases
one (the controller's config matching does, or
`control.allow_initial_changes`, or removing `layout.json` while stopped).
A driver's `Start` and first `Collect` must report an unchanged port's
`IfName` and `Interfaces` the same way. A layout change inside provision's
10 s spacing is provisioned when the spacing ends (`main.go`
`layoutChanged`), not dropped.

## 2. Map

| Path | Owns | Rules |
|---|---|---|
| `cmd/disunified` | flags/env, wiring | no vendor code; identity/model default from the device |
| `internal/devicemodel` | neutral model, `Driver` contract, registry | nothing vendor- or UniFi-specific |
| `internal/drivers/<name>` | one vendor/OS; its own `CLAUDE.md` holds the facts that cost time | `internal/drivers/CLAUDE.md`, `docs/adding-a-device.md`, `docs/drivers/<name>.md` |
| `internal/device` | inform session (fork of unifi-emu), payload tables, capability claims, `State` persistence | wire keys live here and nowhere else |
| `internal/unificfg` | parse `system_cfg` pushes | every key observed has a fixture under `docs/fixtures/controller-*` |
| `internal/informloop` | collect → inform → apply pending → reconcile | apply is a diff; reconcile runs every cycle |
| `internal/unifimodel` | choose the UniFi model from the port layout | `docs/unifi-models.md` |
| `internal/unifiapi` | controller REST API: naming, seeding port/outlet overrides, `PowerPath` for `usp` records | touches only controller-default names and unconfigured overrides; one exception: when a lane switch is adopted (and when a declared one is attached at start), its cage's port on the parent goes to Auto if it held a forced speed below 40G (`CageToAuto`), because the controller then refuses every port save on the parent (`docs/drivers/breakout.md` §16.7, §0) |
| `internal/sshrun` | the SSH transport the host drivers share (`Runner`, `SSH`, `ShellQuote`), and `Local` for a bridge on the device itself | drivers take a `Runner` so tests serve captures |
| `internal/nutd` | the NUT server behind a UPS's "NUT Server" switch (read-only upsd protocol) | driven only by the controller's `nutserver.*` push |
| `docs/` | protocol notes, `drivers/<name>.md`, feature map, `support-matrix.md` (verified/untested per driver; every gap says what a PR needs), fixtures | scrub fixtures with `scripts/sanitize-<driver>.py` / `sanitize-controller.py` |
| `contrib/sonic-image` | the SONiC image build kit: the patch series (IGMP snooping, port mirroring, the DX010 on 202511), `build.sh`, `dry-run.sh`, `sonic-upgrade.sh`, the l2mcd fuzz harness | Apache-2.0 patches; placeholder e-mails in patch headers; the `src/` work clones stay outside the repo (`~/techblueprints/sonic/igmp-build/`) |

## 3. How Clint wants to work

- Local, on his Mac. Commit locally and show him. **Ask before pushing, merging, opening a PR, or deploying.**
  The repo was first pushed 2026-09-20 (history squashed to one commit) and
  its history rewritten with git filter-repo the same day to purge adoption
  authkeys that had been pasted into a test and a state backup: **never commit
  a real `authkey`** (tests use `0123456789abcdef0123456789abcdef`, fixtures
  `00000000000000000000000000000001`). The repo has been public since
  2026-09-20.
- **What an agent must never put in a tracked file, a test, a fixture, a
  commit message or a PR** (this repo is public; history was rewritten twice
  on 2026-09-20 to purge exactly these):
  - a real `authkey`, API key, password, token, or any key file (`id_*`,
    `*.pem`, `known_hosts`, `ssh/`); tests use
    `0123456789abcdef0123456789abcdef`, fixtures `00000000000000000000000000000001`;
  - a real IP address, subnet, DNS name or domain (use `192.0.2.0/24`,
    `2001:db8::/32`, `example.net`); an IPv6 link-local other than
    `fe80::N` too -- its EUI-64 half is the interface's MAC;
  - a real MAC address or serial number, even as a "sample" in a unit test
    (use `02:00:00:xx:xx:xx` / `aa:bb:cc:dd:ee:ff`, `SSJ00000000`);
  - device, site or host names, the LAN topology (which port goes where,
    what is live), deployment hosts and paths, credential locations.
  All of that lives in `local-information/` (gitignored except its README;
  `site.md` is Clint's, `denylist.txt` feeds the checker). **Read
  `local-information/site.md` first** if it exists before doing anything
  against a real network. Every capture goes through the scrub scripts
  before it is committed, and **`scripts/check-site-info.sh --staged` must
  pass before every commit** (`scripts/check-site-info.sh` scans the whole
  tree). If it flags something, fix the file; never add an exception.
- **The bridge writes no pushed secret** (2026-11-15): the reply log, its
  effect texts and `payload-last.json` are redacted by key, and every log
  line masks the push's secret values (one table,
  `internal/unificfg/secrets.go`; `internal/logredact`; the inventory is
  `docs/install.md` §8). `device.json` keeps the applied push unredacted
  (0600), because a restart applies it again. A new secret key in a
  capture fails `TestSecretTableCoversFixtures` until the table names it.
  Logs written before that build still hold real values: a deployed
  bridge's `inform-log/` is a secret store, never copied off whole.
- He logs into the controller in Claude in Chrome (the extension in his own
  Chrome, not the app's built-in browser pane, which had no session there on
  2026-10-31) so Claude can drive the UI for captures and round-trip tests;
  Claude never touches the 2FA code.
- UI edits for testing go only on the test ports named in
  `local-information/site.md` (an unused copper port and a down QSFP cage).
  Never touch a port that carries a live link.
- UniFi is the source of truth for port config and VLANs (he OK'd replacing
  the Arista's VLAN config). IGMP snooping too, and everything else UniFi
  configures on its own switches: **no per-setting opt-ins** (Clint,
  2026-10-31: "unifi is in control, not some mishmash of control with what's
  already on the switches"; "nearly everything should come from the
  controller"). Whenever a driver can apply it -- ports written or not --
  the controller's address (static IP Settings), STP, IGMP snooping, NTP
  servers, time zone, syslog host, hostname, SSH keys, SNMP and Restart
  apply, and its list replaces the device's: a server set by hand goes.
  `control.ports`/`outlets` only scope port and outlet writes: unset = all
  the driver can write, `off` = none. Adoption takes the device as it is
  (Clint: "a good feature"): the controller is seeded from the device's
  ports and the first push is held while it would change one -- the Arista
  plans pushes too since 2026-10-31. SNMP off (no `switch.snmp.*` in the
  push) means no community; Proxmox comments out chrony.conf's own pool.
  The old `control.address/igmp/ntp/syslog/hostname/reboot/ssh_keys/snmp`
  keys and `-control-*` flags load and are ignored. A per-device setting is
  for what UniFi cannot express (breakout handling, the bridge's own
  credentials and reachability, a driver's identity options).
- MIT license, same as unifi-emu; credit James Braid.
- **Tests use real captures, never hand-written protocol samples.** Every
  fixture is output captured from a real switch or a real controller, with
  identifiers, serials and secrets replaced by the scrub scripts
  (`scripts/sanitize-arista-eos.py` for EOS JSON, `scripts/sanitize-controller.py`
  for device informs and the reply log). Both directions are covered and
  must stay covered: switch → bridge (`docs/fixtures/eos-*`), bridge →
  controller (`internal/device/contract_test.go` against real switches'
  informs in `docs/fixtures/controller-<ver>/inform-*.json`) and controller →
  bridge (`internal/informloop/replay_test.go` against the recorded replies
  in `docs/fixtures/controller-<ver>/replies.ndjson`). A new wire key or
  behaviour gets its capture first, then the code. Where real informs come
  from: the UniFi OS console support bundle (Settings → Control Plane →
  Console → Support File) holds every device's decrypted `last.inform`
  under `unifi/devices/<type>/<mac>/`; the bridge's own log is
  `inform-log/<mac>.ndjson` and `payload-last.json`.
- **No affiliation with Ubiquiti**: the README, LICENSE and any published
  page must say this is an independent fan/home-user project, not endorsed
  by or affiliated with Ubiquiti Inc.; "UniFi"/"Ubiquiti" are their marks.
- The bridges run in the container on the Podman host (§4), except the
  ones that run on their own device (the DX010, and each Proxmox node since
  2026-11-17: the `.deb`, updated with `dpkg -i`); nothing runs on the Mac.
  Redeploy of the container = ship `HEAD` with `git archive`, rebuild,
  recreate.

## 4. Running instance

Clint's addresses, names, deployment host and update command are in
`local-information/site.md` (gitignored). Generic shape: `config.yaml`
(gitignored) is the real config (a `devices:` list; the older `switches:`
key is still accepted) with secrets in the environment (`.env`, gitignored,
symlinked from outside the repo); every `control` flag is on.
`state/<name>/device.json` holds the adopted key: one instance per device,
keep the file. Logs: `run.log`, `inform-log/<name>/`. Since 2026-09-27 every
deployed bridge (Arista, PDU, UPS, the Podman host itself; the DX010's ran
there until 2026-11-02 and now runs on the switch, §1; the three Proxmox
nodes' until 2026-11-17, each now a root service on its own node,
`docs/drivers/proxmox-on-node.md`) runs in **one** container on Clint's Podman host (`/opt/disunified`;
the UPS had its own stack for a day while its driver was new -- merged back,
old dir kept as `/opt/disunified-ups.retired-20260927`, its volume as a
backup). The container is on two networks: the podman bridge (to reach its
own host: a macvlan container cannot) and the `lan` macvlan (its own LAN
address and MAC, a client behind its port on the podman switch, the NUT
server's address). A rebuild bounces all six for about a minute; they
resume from the state volume. **Every Mac instance is stopped and must
stay stopped** (one bridge per adopted key).

## 5. Protocol facts that cost time (all verified on 10.6.106)

**Sources.** A fact about the controller is stated with a basis an outside
reader can check: a capture in `docs/fixtures` (scrubbed), a live test, the
controller's UI as an operator sees it, or its logs and API responses --
captures and live observation first. The other source of protocol
knowledge is the upstream unifi-emu project (James Braid,
`github.com/jamesbraid/unifi-emu`: `PROTOCOL.md`, `capability_bits.json`
and its code), which this project forks and credits. A fact with none of
these is marked "not captured". The devices' and the controller's own
software is never a source: firmware files, binaries and what can be read
out of them are not read, quoted or described in this repository. A UniFi
switch on the site is read as its operator can read it (its CLI, its
tables, its informs), read-only, and nothing copied off it is committed.

- Inform header 40 bytes, version 1; AES-GCM with a **16-byte nonce** and
  the whole header as AAD; both CBC and GCM required; default key
  `MD5("ubnt")`; HTTP 404 is the normal pending reply; `inform_url` must be
  an IP literal; accept `mgmt_cfg.authkey` only while on the default key.
- **Control channel is `setparam.system_cfg`** (the UniFi device config
  file), not `setstate`. Keys per feature are in `docs/feature-map.md` §3.
  Report the pushed `cfgversion` only after applying.
- **Capability claims are stored only when the inform carries
  `udapi_version`** (default `1.0.0`). Then `switch_caps`/`speed_caps`
  gate what the UI offers, and the UI validates requests against
  `speed_caps` — so claims must be true (from the switch's hardware table).
- The device's per-port `media` and `speed_caps` **do** replace the
  profile's icons and speed pickers; port count, display name, PoE and
  default port names come from the profile.
- The controller sets the inform interval (65-80 s here).
- **The controller's log is readable**: `/usr/lib/unifi/logs/server.log`
  on the console (root SSH, the Mac's key works); a rejected REST save is
  logged as `sanitize - Unsupported format exists in Device payload={…}`
  with the body the UI sent. The PoE-supervision warnings there are noise
  (`docs/install.md`, Troubleshooting).
- **A UPS must claim `smart_power_caps` bit 1 or its Settings panel cannot
  be saved.** The UI bundles `nut_server` into every save of a UPS device's
  settings (even a rename); the controller answers `api.err.InvalidPayload`
  for the null it sends when the device has no block and
  `api.err.NutInformationAccessNotSupported` for a real block on a device
  that did not claim the bit (2026-09-27, Back-UPS). Every UPS now claims it;
  the NUT server itself starts only when the switch is on.
- **The power path (`usp`) pushes IP Settings without a gateway.** A
  `usp` device's static push carries `netconf.1.*` and the nameservers but
  no `route.1.gateway` (the switch path's does); a driver writing a static
  address from it alone leaves the device without a gateway
  (`apc-backups`, 2026-09-27: `options.gateway` fills it). Same gap for any
  future `usp` address control.
- **The syslog host is pushed as `syslog.remote.ip` / `syslog.remote.port` /
  `syslog.remote.status`** (Settings -> System -> remote logging, captured
  2026-10-27: `docs/fixtures/controller-10.6.106/system_cfg-syslog-remote.txt`).
  `syslog.status=enabled` and `syslog.level=7` are in every push, remote logging
  on or off, and are the device's own logging, not a host. The parser knew only
  `syslog.ip`/`syslog.port` (unifi-emu's notes, never captured) until that day,
  so no driver had ever received a host. Deployed the same evening: the Arista
  shows `Logging to '<host>' port 514` on the first push after the restart.
  The power path's (`usp`) push carries the same keys (captured from the PDU,
  already on Network **10.6.109**: `docs/fixtures/controller-10.6.109/`), but
  the loop handed them only to switches until 2026-10-30, so no APC card ever
  logged anywhere; the loop now writes them into the card's `config.ini` (`apc-pdu`, `apc-backups`; verified live the
  same day; the Smart-UPS's SmartConnect port has no syslog at all). The
  Back-UPS card shipped with syslog on and no server; the PDU already listed
  the log host and was simply quiet (it syslogs outlet, load, PSU, restart
  and unauthorised-access events, not logins). Both stamp lines in local
  time. Until 2026-11-16 that was a fixed -07:00 with daylight saving off,
  an hour behind from March to November. Since then both cards follow the
  controller's zone and NTP servers (`DSTConfiguration=Traditional US`,
  verified live in the cards' files and clocks, `docs/drivers/apc-pdu.md`
  §12); the first DST change they make, on 14 March 2027, is not yet seen.
- **A serial is at most 12 characters (Network 11, 2026-11-12).** Network
  11 moves the controller's config from MongoDB to PostgreSQL, whose
  `device` table holds `serial` in a `varchar(12)`; one longer record halts
  the migration (the console's `/data/unifi/logs/migration.log`:
  `mongo2pg.device` ... `value too long for type character varying(12)`)
  and UniFi OS rolls the Network app back to 10.6.109. 11.0.97 tried four
  times and failed each time on the Podman bridge's 36-character DMI UUID;
  the DX010's 22-character chassis serial was next. Every real UniFi
  device's serial is its MAC's 12 hex digits, so **every bridged device
  now reports its MAC as its serial** (`device.SerialFor`; Clint: the MAC,
  fleet-wide), never its own; an operator's `-serial` must fit. The
  controller replaces a stored serial from the next inform -- no forget or
  re-adoption (podman went from 36 characters to 12 on the first inform,
  still adopted). The scrub scripts had hidden it: their `SSJ00000000`
  stood in for an empty DMI serial and a 22-character one alike.
  **Confirmed 2026-11-13:** with every stored serial at 12 characters,
  11.0.97 installed itself overnight (`migration.log`: Phase 1, 97 of 97
  steps) and every bridged device stayed adopted.
- **Network 11 replaces the controller's record ids with UUIDs
  (11.0.97, 2026-11-13).** `_id`, `device_id`, `site_id`, the network ids
  (`native_networkconf_id`, `mgmt_network_id`, `connection_network_id`) and
  `portconf_id` are 36-character UUIDs where 10.x had 24 hex digits; port
  overrides kept their content, with only the network id rewritten. The
  bridge treats ids as opaque, and the first push after the upgrade applied
  with nothing changed on every bridge in the container. MongoDB is gone
  from the console (`mongo --port 27117` no longer answers). Network 11
  fixtures (`controller-11.0.99/`, the version the console runs since
  2026-11-14): a USW Aggregation's inform, the controller's alerts, a
  shapes file of the site's 41 real switches (2026-11-15) that the shape
  test holds every payload to beside 10.6.109's, and the Arista bridge's
  reply log since the upgrade, replayed (`replay_arista_net11_test.go`);
  against that shapes file a live `wire-shapes.py audit` no longer reports
  id paths (`docs/wire-shapes.md` §9.1).
- **Values are held to a real device's shape, not only its keys
  (2026-11-12, `docs/wire-shapes.md`).** The audit after the serial found
  optic readings sent as numbers (real: strings), a `version` with letters
  or an image name (real: digits and dots, at most 13 characters) and the
  Podman ports' `ifname` carrying a label. All three are fixed, and
  `internal/device/shapes_test.go` holds every driver's payload to
  `docs/fixtures/controller-10.6.109/shapes-usw.json` (types, longest
  string, character class, number range of 42 real devices; no values).
  A difference needs an excusal with its reason. `scripts/wire-shapes.py
  audit` repeats the comparison against a live controller. Scrub
  placeholders keep a serial's length now; other scrubs still change shapes.
- **Only a device claimed as the USW Leaf reports its own version
  (Clint, 2026-11-13).** The Leaf has no firmware releases, so its Version
  column says what really runs (`4.26.14`, `9.1.6`). Every other model is
  a UniFi product on that product's firmware line: it reports the model's
  catalogue version, then the controller's (emulated) upgrade targets, and
  never the device's own -- a SONiC switch claimed as an ECS was showing
  its image's branch. `-version` / the config file's `firmware:` (dead
  until that day) overrides either and must be digits and dots.
  `hash_id` is 16 hex digits since the same day: with a UUID's dashes the
  controller kept none.
- **Fan Mode is gateway-only on 10.6.109.** A switch's record never keeps
  `sys_fan_mode` (`caps` DEFAULT 1 | QUIET 2, `mode`): the DX010 reported it
  in the gateway's (UDM, UXG) shape and its stored record never held it, no
  device on the site has it, and no switch gets the panel or a
  `system.fan.mode` push (live, 2026-11-07). That a gateway's record keeps
  it, and that a REST write of it is refused, is not captured. A switch's
  mode is the device's `fan_mode:` in
  `config.yaml` (`devicemodel.FanModeController`; `docs/drivers/sonic.md`
  §11.15, `docs/drivers/arista-eos.md` §8).
- **A switch's push carries no IPv6 key (Network 10.6.106 to 11.0.99,
  2026-11-14).** Whether the management network runs IPv6 is
  `rest/networkconf`'s `ipv6_interface_type` (`pd`/`static`/`none`) on the
  network `switch.managementvlan` names. `ipv6_enabled` is no guide: a
  guest network the gateway serves has none. A UniFi switch takes SLAAC on
  its management interface; a SONiC switch needs `accept_ra` 2 there and an
  APPL_DB `INTF_TABLE` row for the address, or inbound IPv6 rides the 600
  pps default trap (`docs/drivers/sonic.md` §11.22).
- **A capability bit's name and value come from upstream unifi-emu's
  `capability_bits.json`; what it unlocks and pushes, from a live claim and
  its capture** (2026-11-07, BGP/OSPF: `feature_caps` 33554432 put the DX010
  in Settings -> Routing -> BGP's Device list, `feature_caps2` 4 in OSPF's,
  and the pushes are `controller-10.6.109/system_cfg-dx010-bgp-*.txt` /
  `-ospf-*.txt`). A port's Routing is pushed as `opmode=switch`, Routing +
  Aggregating as `opmode=aggregate` on the aggregate's first member
  (captured 2026-11-08, `controller-10.6.109/system_cfg-dx010-routed-*.txt`).
- **The v2 routing API** (`/proxy/network/v2/api/site/<site>/bgp/config/<mac>`,
  `/ospf/router[/<id>]`) takes a write only with `X-Csrf-Token`, which any
  GET returns as a response header (403 without it); the OSPF router's
  `authentication_type` is `off`/`plain_text`/`md5` there but
  `none`/`plaintext`/`md5` in the push.
- **Topology parent = the upstream switch seeing the device's MAC on a port**
  -- or one of our bridged switches *reporting* it there (`children:`): its
  LLDP entry or its MAC-table entry places a device on its own, even on a
  down port with a shared address, and the placement outlives the report
  (10.6.109, 2026-11-02, `docs/breakout-lag.md` §6.6).
  Reachability fields (`connect_request_ip`, `netmask`, `gateway_mac` as the
  segment's L2 address, `if_table`, `uplink` as a string) are necessary, not
  sufficient; a synthetic MAC is never placed. `docs/topology-placement.md`.
- **A wired client's connected-since is its `mac_table` entry's `uptime`**
  (Network 11.0.97, 2026-11-13): `assoc_time` = the switch's first inform
  after a controller start minus the `uptime` it reports, and an `uptime`
  near zero moves `latest_assoc_time` every inform. `uptime` is the learn
  time, `age` the last frame, `uptime >= age` (`docs/mac-table-times.md`).
- **`port_table[].anomalies` is what the switch itself detected, and every
  bit raises an alert (Network 10.6 and 11.0; seen live 2026-11-13).** Its
  table: 1/2/4 broadcast/multicast/unicast storm, 32 DHCP packet rate
  exceeded, 64 rogue DHCP server, 128 802.1X failed, 256 MAC locking, 512
  loop protection, 1024 DoS, 2048 PHY failure, 4096 PoE failure (the UI's
  labels; 64 and 32 exercised; 4 and 64 raised by a real detection on the
  Arista, 2026-11-14, `docs/drivers/arista-eos.md` §9.8). `custom_anomalies` is a different table (1/2/4
  SFP rx/tx/EEPROM ... 64 link flap ... 32768 port lock) and the controller's
  own: it computes and stores it (the ECS's inform printed on the switch has
  none; the support-bundle copies' values follow `sfp_rxfault`). The bridge
  had filled `anomalies` from the second table: each time a Proxmox node's
  bond leg bounced twice, a "Rogue DHCP server detected" alert for that port
  followed about a minute later (seven, read from the controller's v2
  `system-log/device-alert`), and the SONiC switch's topology changes raised
  "Incoming DHCP packets exceed 300/sec". Now 512 is set for a
  loop-protection hold, and 1/2/4, 32, 64 and 256 only on growth of the port's
  own storm, DHCP-rate, rogue-DHCP and MAC-locking drop counters
  (`Port.Health`); `custom_anomalies` is not sent; the raw fields
  (`link_down_count`, `sfp_*`) are (`docs/feature-map.md` §2).
  The STP-flap alert ("Port N is experiencing frequent STP role state
  changes") carries the port's `stp_state_change_count[].change_count` as
  its `COUNT` (11.0.99, 2026-11-14: a USW Aggregation's alerts against its
  inform, `controller-11.0.99/`); no inform carries `stp_state_change_sum`,
  and the bridge already sends the count from `Port.Health.STPChanges`.
  **The link-flap and STP-flap alerts are rates between two informs**
  (11.0.99, live 2026-11-15, `docs/drivers/sonic.md` §11.23): link flap
  when `link_down_count` grows faster than about 0.3 drops/s (seen at 0.32,
  not at 0.24, on the DX010's looped cage), STP flap when `change_count`
  grows faster than about 0.2/s (0.27, not 0.19, on a stub); each alert's
  `COUNT` is that inform's count. `link_flap_count`, which UniFi switches on
  7.5.15 and 7.6.11 send on a few ports, is not its basis and is not a count
  of drops (a USL8A port with 190 000 drops has none): it counts the
  switch's own link-flap detector, one per "Link flapping detected" line in
  the switch's log (2026-11-15, `controller-11.0.99/link-flap-count-20261115.json`),
  and `link_down_count` its logged drops since boot. The bridge does not
  send it (`docs/feature-map.md` §2); the Linux drivers count drops as
  `carrier_up_count` less the running up period (`docs/drivers/proxmox.md`
  §2). **BPDU guard and loop protection alert
  from `port_table[].error_disabled: true` with `error_disabled_reason`**
  (2 BPDU guard, 1 loop protection), not from `anomalies`: seen on a stub
  port, then from the DX010's own driver after the deploy (812503a); a held
  port raised nothing without them, and 512 alone raised nothing. On SONiC the STP count is l2d's own count of kernel state
  changes since that day (deployed; the DX010's own STP-flap alert seen):
  mstpd's counters never move for a looped (Backup) port.
  **Only loop protection and BPDU guard take a port down** in UniFi's model
  (the UI's own text, 2026-11-14): every other bit is a detection the port
  keeps running through (`docs/drivers/sonic.md` §11.20, with what the ECS's
  `ERROR_DISABLE` table does and does not show).
- **The controller pins `stp.edge` from the reported `stp_edge_port`
  (11.0.99, read from `server.log` 2026-11-15).** About every 5-6 minutes
  per switch, `stp-edge-auto` takes each up port whose edge is left to the
  controller and records `enabled` or `disabled` from that port's
  `stp_edge_port`. The push then carries it as `stp.edge`, with no change to
  the override. On the ECS and in l2d, `disabled` turns auto edge off, so a
  port that was once up as a non-edge port (a looped test cage, a switch
  link) stays non-edge. A `disabled` value lasts through the controller's
  restarts (`docs/drivers/sonic.md` §11.23.3). The site setting is
  `global_switch.auto_stp_edge_detection_enabled` (the UI's "Auto STP
  Edge"). An operator un-pins a port by choosing Edge in its settings and
  then "Restore Auto STP Edge": the push goes back to `auto` (port 17,
  2026-11-15). The UI's save of the Edge choice also switched BPDU guard on.
- **The UI's Re-enable is `port-recovery`, and the device gets the port as
  `port` (11.0.99, captured 2026-11-15).** A port whose alert is BPDU guard
  or loop protection gets a Re-enable button. The UI posts `cmd/devmgr
  {"cmd":"port-recovery","port_idx":N}`, and the next inform reply carries
  `{"_type":"cmd","cmd":"port-recovery","port":N}`. The session hands it to
  `devicemodel.PortRecoverer`. UniFi's text promises no recovery timer for
  BPDU guard: "requires manual re-enable" (`docs/drivers/sonic.md` §11.28).
- **A save of a device is checked against all of it, and against the
  other devices (11.0.99, live 2026-11-16, `docs/drivers/breakout.md`
  §16.10).** Every stored `port_overrides` entry is validated on every
  save of the device, a name-only one included: one entry that the port's
  reported media no longer allows (a forced 25G with `fec_mode` on a port
  now reporting QSFP28) refuses them all with `api.err.InvalidFecMode` and
  that entry's `port_idx`. And a device with static IP Settings cannot be
  saved while another adopted device reports its address
  (`api.err.StaticIpAlreadyUsedByDevice`, `mac` the other device's): two
  bridged devices never report one address.
- **A Forget of a connected device is `{"_type":"setdefault"}`, sent once
  (11.0.99, captured 2026-11-16, `controller-11.0.99/reply-setdefault.ndjson`).**
  A top-level reply, not a `cmd`. The device's first inform after the
  Forget gets it; every later inform on the old key gets HTTP 400 with an
  empty body. The session then drops the key and the stored push, and the
  loop's `OnUnadopted` runs. The Forget by API (`cmd/sitemgr
  delete-device`) answers with the whole stored record, its `x_authkey`
  included: print only `meta`. A Forget made while the bridge is down still
  reaches it: the first inform after a 44 s stop got the `setdefault`,
  though the record was gone (live, DX010 cage 3,
  `replies-lane-switch-dx010-cage3-missed-forget.ndjson`).
- **An empty-body HTTP 400 on an adopted key has only ever meant "forgotten"**
  (every reply log there is, 2026-09-20 to 11-16, about 500 000 informs on
  adopted keys: `docs/drivers/breakout.md` §16.11). Controller restarts and
  upgrades (10.6.109, the failed and the successful 11.0.97 migrations,
  11.0.99) refuse or reset the connection or answer 503, then 200 on the
  same key (`controller-10.6.109/statuses-*.ndjson`); the default key gets
  400s for 24-100 s right after a Forget, then 404. A lane switch, and
  only a lane switch, reads three adopted-key 400s over at least two
  minutes as the Forget it never saved (a crash, a restored state file;
  verified live with a state file put back, 13 refusals at the 10 s
  start-up cadence); every other device logs a WARNING and keeps its key
  (`internal/informloop/rejectedkey.go`). Not captured: a controller
  restored from an older backup.
- **UniFi's client Block reaches a switch as `blocked_sta`, not
  `system_cfg` (11.0.99, captured 2026-11-15).** Every `setparam` carries
  the field: the blocked clients' MACs, `""` for none, two joined by a
  newline. A Block or Unblock also sends `{"_type":"cmd","cmd":"block-sta"
  | "unblock-sta","mac":...}` at the device's next inform; where no push is
  due that cmd is the only timely signal (the Arista's `setparam` came 85 s
  after its cmd). No `system_cfg` key changes. The ECS holds the client as
  `MB_<mac>` in its MAC ACL table (DROP, priority 1023, no VLAN). The
  bridge hands the list to `DeviceDesired.BlockedClients`
  (`docs/feature-map.md` §3); the capture came from a made-up MAC blocked
  through `cmd/stamgr` and forgotten after (Clint's OK).

## 6. Driver facts

Per driver, next to its code: `internal/drivers/arista-eos/CLAUDE.md`
(EOS 4.26.14M is the last train for the 7160; verify every command against
it), `internal/drivers/proxmox/CLAUDE.md` (the node is the switch, ports
from guest tags, adoption guards) and `internal/drivers/sonic/CLAUDE.md`
(Redis tables, the `config` CLI's refusals and multi-VLAN forms, the native
VLAN stand-in, `promote_secondaries` on an address move; build 202505.1238524).
Rules common to every driver:
`internal/drivers/CLAUDE.md`. Full write-ups: `docs/drivers/<driver>.md`.

## 7. Done / open (2026-09-19 end of day)

Done and verified live: everything in `docs/feature-map.md` marked done,
including aggregation (every lane of a cage joins), mirroring, per-port STP
disable, NTP/syslog ownership, locate, real reboot on request,
controller SSH keys on the switch user, emulated firmware upgrades
(persisted), fault reporting (fan/PSU/overheating claims, errdisabled logged),
first-provision naming with lane-range names on split, config file with
multi-device support, container packaging, install
guide, contributor process. Refused-with-a-log: isolation, 802.1X, egress
rate limit, jumbo-off, LLDP-MED-off.

**Deployed 2026-09-20 09:25 MDT (Arista + the three Proxmox nodes in one
container)** on Clint's Podman host: podman-compose with `build: ./src`,
`config.yaml`, `env` (mode 600), a named volume for `state/`; image built
locally, uid 65532; `restart: always`. Host, path and the one-line update
command are in `local-information/site.md`. Notes that bit: `git archive`
ships only committed, non-ignored files; `--force-recreate` is needed or
the old container keeps running; container logs are UTC; rsync is not
installed on the Mac. The image is distroless: `podman exec … sh` fails, so
read `payload-last.json` and the reply logs from the state volume's
`Mountpoint` (`podman volume inspect`) on the host.

**Renamed 2026-09-21: switch-to-unifi → disunified.** The GitHub repo, the
module path, the command, the image, the container paths (`/etc/disunified`,
`/var/lib/disunified`) and the files the Proxmox driver keeps on a node all
moved; GitHub redirects the old repo name. Two things deliberately did not:
the `STU_` environment prefix (it still reads true for a switch, and renaming
it would invalidate every deployed env file — new work uses `DUI_`), and the
salt in `anonID`, which is the input to an id already reported for every
adopted device. The deployed state volume was copied, not recreated, so no
device needed re-adopting; the old `switch-to-unifi-state` volume is still on
the Podman host as a backup and can be removed once the rename has settled.

**APC rack PDU bridged and adopted 2026-09-21 (branch `apc-pdu`).** The first
bridged device that is not a switch: `internal/drivers/apc-pdu` presents an
AP7931's 16 switched outlets as a `USPPDUP`. Outlets are the power-device
shape beside Ports in `devicemodel`; the payload renders `outlet_table`,
`outlet_enabled` and `hw_caps`. Facts that cost time: SNMP writes need the
community's access type to be **Write+**, not Write (with Write the card drops
SETs silently, no error, no log); `12.3.3` is the outlet CONTROL table (col 4
switches) and `12.3.5` the CONFIG table (col 4 is a power-on delay); `hw_caps`
bit 128 is what makes the controller store an outlet table at all; outlet
names must never be reported back or the whole table is dropped; and the
controller merges its names by index, so the AC outlets are reported at the
USP-PDU-Pro's AC positions **5..20** or they come back named "USB Outlet 1-4".
The controller pushes `relay_state` at the reported indices but **no names**.
Deployed in the same container as the switches (`/opt/disunified`; an
earlier note here claimed a separate `/opt/disunified-pdu` -- it does not
exist). **Verified live through the UniFi UI** (every screen
walked): the outlet editor's Active/Disabled switches the relay (~20 s,
`1 of 16 outlets changed`), its **Power Cycle** button arrives as
`relayctl` with a selection list and runs the card's own immediate-reboot
(relay open 4 s), and a cycle with no change writes nothing. The card meters
the phase only; `0.0 A` means under ~1 A (firmware floors it), and the log's
`IMax 1.4` proves the sensor. Reports `gateway_mac`/`lldp_table: []` like a
real USP-PDU-Pro. The controller pushes **no outlet names**. Clint confirmed
every outlet is safe to toggle; the editor is reached by clicking the outlet
**row**, not its label or icon. **Adopting a device deletes its UniFi client
record and any fixed-IP reservation on it** -- the PDU lost its reserved address the next
morning; it is now manual on the card (config.ini `[NetworkTCP/IP]` needs the
`Override=<MAC>` line or it is ignored). `main.go` retries a device's Start
with capped backoff instead of dropping it. **The controller owns each device's
address** (`control.address`): its IP Settings are pushed as `netconf.1.*` +
`route.1.gateway` + `resolv.nameserver.N.ip`, and a static one is applied --
to the PDU's card via config.ini (`Override=<MAC>` required), to the Arista
inside a config session with `commit timer 00:02:00` that the switch reverts
itself unless the driver, having dialled the new address, confirms with
`configure session dui-address commit`. DHCP is applied only when it replaces a static setting in the previous push (the default is never applied; a write that fails is asked again every cycle until it succeeds or a static setting replaces it, `State.DHCPPending`, 2026-11-16), and declined on the Arista, whose driver cannot confirm a lease it cannot predict.
Both devices' IP Settings in UniFi are static (2026-09-23).

**Hostname (2026-09-24).** The controller stores a bridged device's `hostname`
**once, from the adoption inform**; later informs carrying a new hostname are
ignored (five cycles and a force-provision changed nothing). Real UniFi
devices have no `hostname` field at all. **The gateway publishes no DNS
name for an adopted device** -- not from `hostname`, not from `name`
(corrected 2026-09-29: the podman bridge, hostname and name `podman`, static
IP Settings, resolved nowhere; the switches' names are static DNS entries
Clint made in Settings → Routing → DNS, the PDU's and the Back-UPS's names
are records in his own DNS zone, the container's name is its DHCP lease.
The earlier note here that the PDU resolved through its stored hostname was
wrong). `PUT rest/device/<_id> {"hostname": "..."}` sets the field and it
sticks, but it changes nothing on the network. The
controller also pushes `resolv.host.1.name` (the UniFi name with spaces
removed) in every system_cfg; no driver applies it yet. **That key is how
real devices get a DNS name (2026-10-05):** the device sets it as its
hostname and sends it as its DHCP hostname, and the gateway's dnsmasq names
the lease -- the console's `/run/dnsmasq.lease` shows every real device's
UniFi name there. A bridged device names itself or is not named: the
Smart-UPS's SmartConnect port sends no hostname (`*` in its lease), its
pre-adoption name lived on in the lease file only until the gateway's next
reboot, and the bridge then failed every collect for two days. It now has a
static DNS record in the controller; a bridge-written record for
static-address devices and the hostname for DHCP ones are queued in
`docs/feature-map.md` §3 (`resolv.host.1.name`). The Arista's own
hostname was the factory `localhost` until Clint had it set from the CLI
(`hostname` + `dns domain` -- `ip domain-name` is deprecated on 4.26).

Controller API facts (10.6.106, 2026-09-24): `rest/user?mac=` ignores the
filter and returns every client (filter locally); `rest/dnsrecord` answers
`api.err.InvalidObject` -- static DNS records live at
`/proxy/network/v2/api/site/<site>/static-dns`; `rest/networkconf` carries
each network's DHCP range (`dhcpd_start`/`dhcpd_stop`); and `cmd/devmgr
force-provision` really does push a fresh system_cfg (the bridge logged the
apply), the one devmgr command whose `rc:ok` has been seen to mean
something. (An earlier version of this note said the gateway's DNS
registers every adopted device at the IP its inform reports; it does not --
see the hostname note above, corrected 2026-09-29.) The alert log
(Network 11.0.99, 2026-11-15) is read with a POST that only filters:
`/proxy/network/v2/api/site/<site>/system-log/device-alert`, body
`{"timestampFrom": ms, "timestampTo": ms, "pageSize": N, "pageNumber": 0}`,
answer `data` plus `total_page_count`; scrub it with
`sanitize-controller.py` as it comes.

**`rest/device` has no GET; the device records are read at `stat/device`
(11.0.99, read-only requests with the API key, 2026-11-17).** `GET
rest/device`, with or without an id (the record's UUID, its MAC, a bare
trailing slash), answers HTTP 404 `{"rc":"error","msg":"api.err.NotFound"}`,
and so does `list/device`. `stat/device` (72 records), `stat/device/<mac>`
(1) and `stat/device-basic` answer 200 `rc: ok`, as does the v2
`/proxy/network/v2/api/site/<site>/device`. It is not the key: `self` shows
it as the owner's (`is_owner`, `is_super`, `site_role: admin`), and the same
key lists `rest/networkconf`, `portconf`, `user`, `setting`, `wlanconf`,
`routing`, `firewallrule` and `usergroup` (all 200). It is not an unknown
collection either: one of those (`rest/dnsrecord`, a made-up name) answers
HTTP 400 `api.err.InvalidObject`. So `rest/device` is a route that takes
writes only: `PUT rest/device/<id>` is how the bridge names devices and
seeds overrides and has run on 11.0.99 since the upgrade; without an id it
answers `api.err.IdRequired` (`docs/drivers/breakout.md` §16.11). Not
captured: whether 10.x answered the GET (no note or capture here ever read
`rest/device`; every read has been `stat/device`), and the same GET from a
logged-in browser session instead of the key.

**The UI's Update is `cmd/devmgr {"cmd": "upgrade", "mac"}`, and a bridged
device asks for it itself (11.0.99, live 2026-11-17).** A connected device
the controller has a newer firmware for carries `upgradable: true` and
`upgrade_to_firmware` in its record (`stat/device/<mac>`); after the POST
its next inform is answered with `{"_type": "upgrade", "version", "url",
"md5sum"}`, the record shows state 4 until the new version is reported
(about 90 s) and state 5 for another minute. Every device that is not
claimed as the Leaf (and has no `-version`) reads its record a minute after
a connect and then hourly, and requests what is offered
(`cmd/disunified/upgrade.go`, `unifiapi.OfferedUpgrade`): the update is
emulated, so a badge waiting for a click says nothing true. A lane switch
and its parent are different UniFi products and so report different
versions (`USFXG` 7.6.11.17272 beside `USWF07D` 4.0.1.995).

SNMP: Settings → CyberSecure → Traffic Logging (captured 2026-09-19;
`switch.snmp.*`; follows the controller, no flag since 2026-10-31).

**SSH gateway parked on branch `ssh-gateway` (2026-09-19).** It was built,
deployed (macvlan with its own DHCP reservation, since deleted) and verified
(`ssh admin@<device IP> "show version"` with a controller-pushed key), then
removed from main because the UniFi UI terminal is WebRTC, not SSH (below).
`docs/ssh-gateway-status.md` on that branch says where it got to. Main
deploys with `deploy/compose.yaml` (bridge network) and reports the Arista's
own in-band IP (on Vlan1) as the device IP. `fw_caps` UTERM is deliberately not
claimed, so no Debug entry appears.

STP facts (2026-09-19): the Arista runs `spanning-tree mode rstp`, priority
32768. It was the LAN's STP root (every switch at 32768, lowest MAC) until
Clint set the upstream aggregation switch to 4096 the same evening; EOS now
reports that switch as root with the uplink cage's lane 1 as the root port.
Only the two cabled 100G cages are STP-active (see `local-information/site.md`). `root_switch` is reported
from `show spanning-tree root detail`.

Anomaly/Experience (2026-09-19, corrected 2026-11-14): `satisfaction` and
`satisfaction_reason` are derived in `internal/device/tables.go`
(`portAnomalies`) from counters and `Port.Health` -- see
`docs/feature-map.md` for the empirical satisfaction rule. FEC codeword
counters come from text output (`TextRunner`), eAPI only. The per-port
`anomalies` bits derived here from the same signals were the controller's
`custom_anomalies` table and raised false alerts (§5); `anomalies` now
carries a loop-protection hold (512) and the growth of a port's own
protection drop counters (§5).

**The UI terminal is WebRTC, not SSH (2026-09-19).** When the Debug
terminal is opened the controller sends the device an inform-reply cmd
`build-ssh-session` (session id, STUN/TURN servers, TURN username), which
this bridge logs as UNHANDLED. The device is expected to establish a WebRTC
session with the browser and pipe its shell over the data channel. unifi-emu
does not implement the device side and does not document how a device
answers that command, so Clint parked it (branch `ssh-gateway`).

Uplink/Parent — SOLVED 2026-09-20 13:45 MDT. Three things were needed:
(1) the management address in-band (on Vlan1; Management1
addressless, LLDP off), (2) reachability fields as real switches send them
(connect_request_ip, netmask, gateway_mac, if_table), and (3) the one that
mattered last: **`uplink` is a string** — the name of the management
interface in `if_table` ("eth0") — not an object. With an object the
controller silently ignored it (only its own counters were stored). The
proof came from the UniFi OS console support bundle (Settings → Control
Plane → Console → Support File → Download), which contains every device's
decrypted `last.inform` under `unifi/devices/<type>/<mac>/` — the definitive
reference for what a real switch sends; `unifi/topology.json` has the edge
list. `payload-last.json` in the record dir is what we send. Ruled out along
the way: neighbour keys in `uplink`, "Port N" LLDP port IDs, vendor ifname,
identity fields, force-provision, the USW Leaf model (a re-adopt was never
needed). The loop warns loudly if an OOB port carries an address or is
cabled.

First-party UI audit 2026-09-20 (Chrome, against the real USW aggregation
switch upstream of the Arista): device
overview (PSUs, fans, memory, temperature, uptime, parent, connected devices
per port), Insights (history, CPU/memory graphs), Settings (all sections
except Etherlighting/LCM which are model features, and Generate Support
File/Debug which are deliberately unclaimed), Port Manager list, port
settings drawer (every control), port stats (anomaly breakdown, MAC table,
activity log), SFP tab (optic details), clients page attribution, topology
map. Control round-trip re-verified: port 2 disable/enable from the UI
reaches the switch in ~15 s. Note: `rest/device` PUT of `port_overrides`
with `forward: disabled` does NOT provision; the UI's Port State toggle does.
Other keys real informs carry that we now send: inform_min_interval,
stats_inform_interval, has_eth1, gateway_ip, uptime_str, total_mac_in_used,
stp_topology_change_count, satisfaction_reason, guid, ssh_session_table.
Re-adoption (2026-09-20): forget via `cmd/sitemgr delete-device`, clear
`state/arista/device.json` (backup first) and the reply log, restart; the
controller answers HTTP 400 (empty body) for about a minute after a forget,
then the normal 404, then the device is pending; `cmd/devmgr adopt` completes
the handshake in ~25 s (mgmt_cfg with authkey → ADOPTING → first system_cfg →
CONNECTED) and the uplink resolves on the first cycle. That handshake (the two
400s included) is now the head of `docs/fixtures/controller-10.6.106/replies.ndjson`,
`set-locate`/`unset-locate` its tail, and the replay test starts unadopted
with the default key. The controller's command names are `set-locate`
and `unset-locate` (`locate` is not one). **`cmd/devmgr` answers `rc:ok` to
any unknown command name** (even `no-such-command`), so an ok there proves
nothing; only a recorded cmd reply does. The port power cycle is
`power-cycle` with `port_idx`, and the controller issues it only for a PoE
port that is powering a device: `api.err.InvalidTargetPort` for our ports,
for a free PoE port and for a non-PoE port on a real switch, and the UI's
"Power Cycle" button exists only on such a port (checked on
a real PoE USW). The 7160 has no PoE, so it can never receive the
command and there is no capture of the device-side name. Found and fixed: naming
only ran at startup, so a later adoption left "USW Leaf" — `OnConnected`
now provisions names; the mgmt_cfg log line masks the authkey.

Release pipeline (2026-09-20, added but never run — there is no tag yet):
`.github/workflows/ci.yml` (vet, test, `scripts/check-site-info.sh`, a
container build plus its one-shots) and `.github/workflows/release.yml`
(tag `v*` -> multi-arch image `ghcr.io/techblueprints/disunified`
`:vX.Y.Z`/`:X.Y.Z`/`:X.Y`/`:latest` and a GitHub release with static binaries;
push to main -> `:edge`). Only `GITHUB_TOKEN` is needed, but the **GHCR package
must be made public by hand after the first push** or pulls need a login;
`docs/releasing.md` is the procedure. The image cross-compiles in the build
stage (`--platform=$BUILDPLATFORM`, `GOOS/GOARCH` from `TARGETOS/TARGETARCH`),
so no QEMU. `main.buildVersion` is stamped by `-ldflags -X`, logged on every
start and printed by `-build-version` (not `-version`, which is the reported
firmware). `deploy/compose.yaml` and the Quadlet unit now pull the published
image; compose's `build:` needs `dockerfile: Containerfile` because the file is
not named `Dockerfile`.

The WebRTC terminal is parked.

**APC Smart-UPS bridged 2026-09-26 (branch `apc-ups`).** The SMTL1500RM3UC's
green SmartConnect port, documented as cloud-only, serves **Modbus TCP on 502**
once enabled at the LCD (Advanced → Configuration → Modbus): APC's SMTL
release notes record it at UPS 01.3. `internal/drivers/apc-ups` reads NUT's
four register blocks on one connection and fills a new `devicemodel.Battery`;
`internal/device/tables_ups.go` emits `vbms_table`/`battpool` (spec-derived
from unifi-emu's PROTOCOL.md — no real UniFi UPS inform exists on site).
Facts that cost time: two-register values are big-endian by register; a
socket-per-register scan made the unit refuse connections; USB HID on this
generation carries no load/voltage at all (NUT issue #1868), so the free
Synology NUT feed could never have given the load ring; the Homebrew `nut`
bottle lacks `apc_modbus`. Read-only by decision: **Clint's rule is "don't
turn outlets off without discussing it first"** — a process rule, not a
design mandate; the unit runs a rack at ~80 % load. **Adopted 2026-09-26 from the Mac** (`config.local.ups.yaml`, synthetic MAC so
the real port's DHCP reservation survives): the controller kept `vbms_table`
and the UI shows the live load ring and battery card — the spec-derived
emitter is now controller-verified. **Re-adopted under the real MAC the same day** (Clint's call; the
synthetic MAC gave no topology parent — the controller places by MAC on a
switch port, `docs/topology-placement.md`): placed on its switch port within
20 s; the client record, reservation and client DNS name went with it. The
profile's eight outlet slots are drawn regardless; the update badge is
accepted per the rule (version becomes the controller's, base kept). **Deployed
2026-09-26 as its own stack on the Podman host** (`/opt/disunified-ups`,
image `localhost/disunified-ups`, volume `disunified-ups-state`, env with
only the UniFi vars), dialling the DNS name for the
unit; the Mac instance is stopped for good. Path and update command in
`local-information/site.md`. **Outlet-group control built 2026-09-26, never
exercised on the unit** (Clint's call): the groups are presented as outlet
rows (Main without a relay, Group 1 with one, six placeholders for the UPS
2U's picture), the command word at register 1538 is held byte-exact to
NUT's encoding by tests, the Main group is refused, and the loop holds the
first outlet push of a run that would switch anything. `control.outlets: "2"`
was enabled the same evening (stack rebuilt from 8630fe0); no command has
been sent to the unit yet.
**The UPS 2U picture (2026-09-26, the adopted unit's panel):** two
multi-cells coloured by outlets 1 and 5 alone (seen live: the bottom row went
grey when outlets 3-8 were seeded off), override on top of the table; the
provisioner seeds the placeholder overrides off (`device.PlaceholderOutlets`,
`unifiapi.SeedOutletOverrides`); there is **no outlet editor for a UPS 2U**
in 10.6.106 -- the API is the only relay-push path; NUT Server is claimed off.
**Re-adopted as the UPS 2U Pro (`USPDA2B`, `type usp`) 2026-09-26 21:40 MDT**
because only that model pairs a UDM for Safe Shutdown, and the gateway and
core switches are on this UPS. First `usp` inform ever sent by the bridge:
accepted, placed in 40 s, power tables stored; the record rejects
`port_overrides` (`unifiapi.Client.PowerPath` sends the name only), the
pinned catalogue's `usw` label is corrected on the wire (`wireType`), the
update to `1.6.1.4933` accepted. Its picture is eight single cells (1 and 2
green, 3-8 Not Powered), the panel gains an **Outlets** tab whose row editor
is the PDU's (Active/Disabled, Power Cycle) -- the UI control path for Group 1
exists on this model -- and Safe Shutdown Pairing offers the gateway as
"Click to Pair", which asks for the console owner's credentials (Clint's
step, never Claude's). **Paired 2026-09-27.** Both rack PDUs hang off the
UPS (~960 W important, ~130 W expendable, the expendable one on the switched
Group 1 = top rear row); the driver reads the groups' load-shed policy
(config block widened to 1073) and `options.shed_on_battery_after: 30s`
writes it -- the one config write the bridge makes to the unit. **NUT server
built 2026-09-27**: `control.nut` claims `smart_power_caps` bit 1, the
controller pushes `nutserver.*` (captured), `internal/nutd` serves upsd's
protocol from the snapshot on the pushed port (read-only), logged-in clients
reported as `nut_client_ips`; the UPS stack publishes 3493.
**Clint 2026-09-26: push, merge and redeploy the UPS stack without asking;
re-adopt when needed.** The first outlet command is still his to send.


**Gateways: researched, not attempted (2026-09-20).** Could the bridge present
a non-UniFi router as a UniFi *gateway*, so another location shows up as its
own network? The blocker is a controller constraint, not a protocol one: one
gateway per site (`api.err.NoSecondGateway` — unifi-emu's prior art, unverified
on 10.6.106), so it would need its own site. unifi-emu's gateway payload is a
stub whose `uplink` is an **object**, the opposite of the switch string that
cost a day. The whole picture, including the cheaper alternative (present the
router as a switch, which works today), is `docs/gateways.md`. **2026-10-19:
a third party adopted an emulated gateway** built from our upstream PR #15 on a
UniFi OS Server (10.6.106). Provisioning waited on an answer to the controller's
`wg-genkeypair` cmd, sent back as an inform notification (`inform_as_notif`).
That is their finding, not verified here; the detail is in `docs/gateways.md`
under "Since then".

**SONiC driver merged and deployed 2026-10-27 (branch `sonic`, 30 commits,
fast-forwarded into main).** Built and verified on a SONiC virtual-switch guest
(`docs/drivers/sonic.md`); the guest is **shut down** (its controller record was
forgotten 2026-10-31). **The DX010 arrived 2026-10-31**: C0 stepping (no AVR54
exposure), reinstalled with SONiC 202505.1238526 (the factory image kept in the
second slot), adopted as `USWF07D` and deployed in the one container the same
evening; the arrival-day steps are `docs/drivers/sonic.md` §3, and what the
hardware exposed (SNMP communities re-imported from `snmp.yml`, the jumbo hold,
FEC on a 100G link, eth0 sharing the switch MAC) is the follow-up to that run.
Parity with a UniFi switch is planned and tracked in `docs/drivers/sonic.md`
§11 (measured against the ECS Aggregation's own inform, claims and push).
Merged and deployed 2026-11-01 (a local main since squashed into 7616b6c): the hardware tables and LACP state as the
ECS reports them, speed "auto" following the module, and RSTP with loop
protection through `disunified-l2d` (mstpd plus glue into the stock StpOrch,
on by default). Verified live on the DX010 the same night:
locate, a forced speed, a 4x25G breakout and its join, and the UI's Restart
(§11.6); RSTP and loop protection by hand and then through the deployed driver
(§11.5), with one fix after (a down port's BPDU guard, lost to a reload race).
IGMP snooping needs a custom 202511 image (§8); a build
VM exists for it.

**What has gone upstream, and the fact that none of it has been reviewed, is
`docs/prior-art.md` §6** (issues #2 and #6–#9; PRs #3–#5, the stacked series
#10–#13, #14 and #15; unifi-emu reviewed nothing as of 2026-11-17, though pushed again 10-27 to 11-01; the only response
is that third party's, on #6 and #15). **Never wait on an upstream merge** —
the corrections live in the `internal/device` fork and the dependency stays
pinned at v0.5.5.
