# switch-to-unifi — working notes and rules for Claude Code

Read this first. It is the map of the repo and the rules that came from live
failures. Deeper material is in `docs/`; the rules for the vendor layer are
in `internal/drivers/CLAUDE.md`.

## 1. What this is

A bridge that presents a non-UniFi switch to a UniFi Network controller as an
adopted UniFi switch (read: ports, stats, topology; write: the controller's
port and switch config applied to the vendor switch). First target: Clint's
Arista DCS-7160-48TC6-F on EOS 4.26.14M, claimed as `UDC48X6` ("USW Leaf").
**Phase 0 (read) and phase 1 (control) are done and verified live** on
Network 10.6.106 — see `docs/feature-map.md` for the per-feature status.

## 2. Map

| Path | Owns | Rules |
|---|---|---|
| `cmd/switch-to-unifi` | flags/env, wiring | no vendor code; identity/model default from the switch |
| `internal/switchmodel` | neutral model, `Driver` contract, registry | nothing vendor- or UniFi-specific |
| `internal/drivers/<name>` | one vendor/OS | `internal/drivers/CLAUDE.md`, `docs/adding-a-switch.md` |
| `internal/device` | inform session (fork of unifi-emu), payload tables, capability claims, `State` persistence | wire keys live here and nowhere else |
| `internal/unificfg` | parse `system_cfg` pushes | every key observed has a fixture under `docs/fixtures/controller-*` |
| `internal/informloop` | collect → inform → apply pending → reconcile | apply is a diff; reconcile runs every cycle |
| `internal/unifimodel` | choose the UniFi model from the port layout | `docs/unifi-models.md` |
| `internal/unifiapi` | controller REST API (naming only) | touches only controller-default names |
| `docs/` | protocol notes, vendor notes, feature map, fixtures | scrub fixtures with `scripts/sanitize-fixtures.py` |

## 3. How Clint wants to work

- Local, on his Mac. Commit locally and show him. **Ask before pushing, merging, opening a PR, or deploying.**
  The repo was first pushed 2026-09-20 (history squashed to one commit) and
  its history rewritten with git filter-repo the same day to purge adoption
  authkeys that had been pasted into a test and a state backup: **never commit
  a real `authkey`** (tests use `0123456789abcdef0123456789abcdef`, fixtures
  `00000000000000000000000000000001`). Making it public is a separate,
  pending step.
- **What an agent must never put in a tracked file, a test, a fixture, a
  commit message or a PR** (this repo is public; history was rewritten twice
  on 2026-09-20 to purge exactly these):
  - a real `authkey`, API key, password, token, or any key file (`id_*`,
    `*.pem`, `known_hosts`, `ssh/`); tests use
    `0123456789abcdef0123456789abcdef`, fixtures `00000000000000000000000000000001`;
  - a real IP address, subnet, DNS name or domain (use `192.0.2.0/24`,
    `2001:db8::/32`, `example.net`);
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
- He logs into the controller in Claude's Chrome tab so Claude can drive the
  UI for captures and round-trip tests; Claude never touches the 2FA code.
- UI edits for testing go only on the test ports named in
  `local-information/site.md` (an unused copper port and a down QSFP cage).
  Never touch a port that carries a live link.
- UniFi is the source of truth for port config and VLANs (he OK'd replacing
  the Arista's VLAN config). IGMP snooping too (`-control-igmp`).
- MIT license, same as unifi-emu; credit James Braid.
- **Tests use real captures, never hand-written protocol samples.** Every
  fixture is output captured from a real switch or a real controller, with
  identifiers, serials and secrets replaced by the scrub scripts
  (`scripts/sanitize-fixtures.py` for EOS JSON, `scripts/sanitize-captures.py`
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
- Bypass-permissions mode is on for this session; the bridge is restarted by
  Claude (`pkill -INT -f 'switch-to-unifi -controller'`, then re-run).

## 4. Running instance

Clint's addresses, names, deployment host and update command are in
`local-information/site.md` (gitignored). Generic shape: `config.yaml`
(gitignored) is the real config with secrets in the environment (`.env`,
gitignored, symlinked from outside the repo); every `control` flag is on.
`state/<name>/device.json` holds the adopted key: one instance per switch,
keep the file. Logs: `run.log`, `inform-log/<name>/`. The deployed instance
runs in a container on Clint's Podman host; **the Mac instance is stopped
and must stay stopped** (one bridge per adopted key).

## 5. Protocol facts that cost time (all verified on 10.6.106)

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

## 6. Switch facts (Arista, see `docs/arista-eapi.md`)

- EOS 4.26.14M is the **last train for the 7160**: verify every command
  against that version (fixtures in `docs/fixtures/eos-4.26.14M/`); eAPI
  batches start with `enable`; eAPI does not expand abbreviations; TLS 1.2
  RSA-kex only.
- 10GBASE-T ports: 1G and 10G only. QSFP28 cages: 10/25/40/50/100G,
  RS-FEC on 100G, fire-code only on 25G lanes.
- Breakout: speed on lane 1 splits/joins; a lane-speed change must reach
  every lane or the others errdisable ("speed-misconfigured").
- `write memory` after every config batch.

## 6b. Proxmox driver (branch claude/proxmox-unifi-bridge, 2026-09-19/20)

`internal/drivers/proxmox` + `docs/proxmox.md`. Each node's `vmbr0` is a
USW Leaf (`UDC48X6`, 54 ports) named after the node; guests are ports 1-48
numbered cluster-wide (persisted in `state/<name>/proxmox-ports.json`, keep
it with `device.json`) and named `VM-<id>` (`VM-Open-<port>` when free),
the physical NICs/bond slaves count down from 54 (`bond0-1`, `bond0-2`;
`NIC-Open-<port>` when free). Free slots are reported and seeded disabled;
a guest arriving on one is re-seeded from its own state. Read: one SSH exec of `collect.sh` per
poll (root on the node, key auth). Write: `qm set`/`pct set` for
`link_down`/`tag`/`trunks`. Nodes run lldpd bound to the active uplink NIC
so the upstream aggregation switch sees them. Controller quirks: the ECS
Core record carries `oob_port_config` that must be sent back empty on every
REST update; the model's controller name is "ECS Core". A throwaway,
diskless test guest (named in `local-information/site.md`) is the Proxmox
"port 2".
The node is the switch (device MAC = the bridge MAC, same IP and hostname;
a derived-MAC/host-port variant was tried and dropped 2026-09-20); the
bond is one uplink port (54); guest ports are `VM-<id>`; lldpd config is
written by the driver.
Uplink/Parent verified 2026-09-20 after merging main's `uplink: "eth0"` fix.
**Adoption is safe by construction (2026-09-20):** the first push after
adoption is held while the driver's plan says it would change ports
(`switchmodel.Planner`), and the bridge seeds the controller's port
overrides from the switch's live VLAN state on the handshake (retrying
while held); verified by re-adopting proxmox-1 with FusionHub's tagged
NICs watched: held → seeded 2 ports → new push applied with 0 changes.
Before this, a re-adoption with control on stripped VM 119's tags for 13 s.
Deployed with the Arista in the one container (config.yaml has four
switches; the container mounts a directory with the bridge's own ed25519
key, installed on the nodes' root user, and a known_hosts — paths in
`local-information/site.md`).

## 7. Done / open (2026-09-19 end of day)

Done and verified live: everything in `docs/feature-map.md` marked done,
including aggregation (every lane of a cage joins), mirroring, per-port STP
disable, NTP/syslog ownership, locate, real reboot on request,
controller SSH keys on the switch user, emulated firmware upgrades
(persisted), fault reporting (fan/PSU/overheating claims, errdisabled logged),
first-provision naming with lane-range names on split, config file with
multi-switch support, container packaging (image build untested), install
guide, contributor process. Refused-with-a-log: isolation, 802.1X, egress
rate limit, jumbo-off, LLDP-MED-off.

**Deployed 2026-09-20 09:25 MDT (Arista + the three Proxmox nodes in one
container)** on Clint's Podman host: podman-compose with `build: ./src`,
`config.yaml`, `env` (mode 600), a named volume for `state/`; image built
locally, uid 65532; `restart: always`. Host, path and the one-line update
command are in `local-information/site.md`. Notes that bit: `git archive`
ships only committed, non-ignored files; `--force-recreate` is needed or
the old container keeps running; container logs are UTC; rsync is not
installed on the Mac.

SNMP: Settings → CyberSecure → Traffic Logging (captured 2026-09-19;
`switch.snmp.*`; `control.snmp: true` in the deployed config).

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

Anomaly/Experience (2026-09-19): per-port `anomalies` bits, `satisfaction`
and `satisfaction_reason` are derived in `internal/device/tables.go`
(`portAnomalies`) from `Port.Health` — see `docs/feature-map.md` for the
bit table and the empirical satisfaction rule. FEC codeword counters come
from text output (`TextRunner`), eAPI only.

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

The WebRTC terminal is parked.
