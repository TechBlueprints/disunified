# Adding a device (vendor/OS) to disunified

This is the checklist for a new driver. It is written for an AI agent or a
person who has never seen this repo; every step names the file to touch and
the proof that it worked. The Arista EOS driver ([`internal/drivers/arista-eos`](../internal/drivers/arista-eos))
is the reference implementation — copy its shape, not its commands. The
Proxmox driver ([`internal/drivers/proxmox`](../internal/drivers/proxmox), [`docs/drivers/proxmox.md`](drivers/proxmox.md)) shows the
shape for a virtual switch read over SSH with one script per poll, and
for a driver whose "ports" are not fixed hardware.

## 0. What a driver is

A driver turns one vendor's device into the neutral `devicemodel.Snapshot`
(read side) and applies `devicemodel.PortDesired` / `DeviceDesired` to it
(write side). Nothing UniFi-specific lives in a driver; nothing vendor-
specific lives outside `internal/drivers/<name>`. The contract is in
[`internal/devicemodel/driver.go`](../internal/devicemodel/driver.go) and `model.go`.

Read side (mandatory): identity, per-port link state, counters, media, speed
capabilities, LLDP neighbours, MAC table, STP state, optics, FEC, flow
control, LAG membership, temperature/fans/PSUs. Write side (optional, add
incrementally): enable/disable, description, speed, VLAN membership, VLAN
creation, FEC, storm control, BPDU guard, STP mode/priority, IGMP snooping.

## 1. Before writing code: capture the device

1. Find the exact OS version and whether it is frozen (end-of-support
   hardware). Write it down; the driver targets that version.
2. Capture the JSON (or parsed text) output of every command the driver will
   use into `docs/fixtures/<driver>-<version>/`, one file per command, named
   `<command with spaces→- and /→_>.json`. Scrub identifiers with a
   `scripts/sanitize-<driver>.py` modelled on
   [`scripts/sanitize-arista-eos.py`](../scripts/sanitize-arista-eos.py) (MACs, IPs, serials, hostnames,
   descriptions, optic serials), and run
   [`scripts/check-site-info.sh`](../scripts/check-site-info.sh) before committing. The repo is public.
3. Verify each command exists on that version by running it; note the ones
   that do not (see [`docs/drivers/arista-eos.md`](drivers/arista-eos.md) §1 for how 4.26 differs).

### Controller-side captures

The controller side is captured too, and the same scrub rule applies:

- `internal/device/contract_<driver>_test.go` (one per driver, sharing the
  harness in `contract_test.go`) compares the payload your driver
  produces (through the real device layer) with real UniFi switches' informs
  in `docs/fixtures/controller-<version>/inform-*.json`. Those come from the
  UniFi OS console support bundle (`unifi/devices/<type>/<mac>/last.inform`),
  scrubbed with [`scripts/sanitize-controller.py`](../scripts/sanitize-controller.py). Every key a real switch
  sends must be present with the same JSON type or listed with a reason.
- `internal/informloop/replay_<driver>_test.go` (one per driver, sharing
  the harness in `replay_test.go`) replays the controller's recorded
  replies (`docs/fixtures/controller-<version>/replies.ndjson`, cut from the
  bridge's `inform-log/<mac>.ndjson`, scrubbed the same way) through the
  loop with your driver on its fixtures, and asserts the device commands each
  push produces. When you verify a feature live (§4), the reply that carried
  it is in the log: add it to the fixture with the expected commands.

## 2. Write the driver

Create `internal/drivers/<name>/` with:

- `driver.go`: a `Driver` type, `func init() { devicemodel.RegisterDriver(Driver{}) }`,
  `Name()` (the `-driver` value), `Describe()`, and `Open(ctx, cfg)`. Open
  builds the transport from `DriverConfig` (URL+user+password, or SSH
  target) and returns a `Device`. Document which config fields you use.
- A `Transport` if the vendor needs one (`Run` for structured output,
  `Configure` for config commands). Fail the whole batch on any error line.
- A `Collector` implementing `devicemodel.Device`: `Start` runs *every*
  command once and fails loudly on a missing command or bad credential;
  `Collect` builds the Snapshot. Then, as you add writes,
  `devicemodel.Controller` (`ApplyPorts`), `DeviceController` (`ApplyDevice`),
  `VLANController` (`EnsureVLANs`). Add the compile-time interface checks
  from [`arista-eos/driver.go`](../internal/drivers/arista-eos/driver.go).
- Tests against the fixtures (a `fixtureTransport` that serves the files by
  command name), covering: port count and indexes, an up port's counters,
  a down port, breakout folding if the platform has it, LLDP, capabilities,
  and every apply command sequence (including idempotence: applying the
  same desire twice must write nothing the second time).
- A blank import in [`cmd/disunified/main.go`](../cmd/disunified/main.go) next to the Arista one.
- Optionally `devicemodel.Planner` (`PlanPorts`: what ApplyPorts would
  change, without writing). With it the loop holds a freshly adopted
  device's first push while it would change ports, and the bridge seeds the
  controller's port overrides from the device on adoption; without it the
  first push applies as pushed.
- Optionally `devicemodel.Capable`: the capability claims the controller
  gates its UI on (STP, storm control, FEC, LACP, mirroring, isolation,
  IGMP, jumbo…). Without it the Arista set is claimed; a driver that
  honours fewer features must say so or the UI offers controls that do
  nothing. `Snapshot.UplinkHint` names the uplink when LLDP is silent.

Rules that came from live failures, all enforced by the reference driver:

- **Speed capabilities must come from the device** (a hardware/capability
  command), not from a table: the controller validates every speed request
  against `speed_caps`, and a wrong claim either hides a valid speed or
  offers an invalid one. Use a media-based fallback only for empty cages.
- **Diff against live state; write only differences.** The loop reconciles
  after every inform, so anything that is not a pure diff would flap.
- **Breakout cages** (one physical cage, several lanes): fold lanes into
  one port (UniFi has one port per cage) — up if any lane up, counters and
  MACs summed, speed = lane speed, medium = the cage's. Write speed to lane
  1 to split/join, to every lane for a lane-speed change; write everything
  else to every lane; mark the port `LanesDiverge` when lanes differ so the
  next apply rewrites them; claim the *cage's* speeds so the controller lets
  the operator join it back.
- **Never set an optical port to auto speed** unless the platform makes
  that safe; the Arista driver leaves optical ports alone on "auto".
- **FEC is written only when the controller sends the key**; absent means
  leave the device's setting alone.
- **Lead config batches with the platform's privilege escalation** if the
  API starts unprivileged (eAPI starts at privilege 1).

What a first-party-looking device needs beyond ports and counters (all in
`devicemodel.System` / `Port`, all shown in the UI once filled):

- `Port.Health`: link-change and STP-change counts, STP guard state, optic
  alarm flags, FEC codeword and PCS error counters — the Anomaly column and
  the per-port anomaly breakdown come from these. Some of it may only exist
  in text form on your platform (the Arista driver has a `TextRunner`
  transport capability for that); leave a field unset rather than guess.
- `System.Addresses`, `ARP`, `MgmtMAC`, `OOBInterfaces`: the controller
  places a device by its address, netmask, gateway MAC and interfaces, and
  needs the management address in-band (§2c). Report OOB ports so the loop
  can warn.
- `System.LoadAvg`, memory, CPU, temperature, fans, PSUs (with `Present`),
  `MACTableCapacity`: Insights graphs and the overview cards.
- `System.STPRoot`: the topology's root marker.
- The uplink: mark the port `IsUplink` (the loop picks the LLDP neighbour
  that is a bridge/router) and nothing else — the device layer reports
  `uplink` as the management interface *name* plus `if_table`, which is
  what a real switch sends; the controller derives the rest.

## 2b. Naming

The bridge names the device and its ports in the controller through the
REST API — on first provision and whenever the port layout changes — using
`devicemodel.Namer`. `DefaultNamer` gives "<Vendor> <Model>" and the
interface name (or description; "<base>/1-<lanes>" for a split cage). If
your platform's interface names follow another convention, implement
`Namer` on the driver's Device type: `PortName` must be deterministic for
a layout, and `DefaultPortNames` must list every form `PortName` could have
produced for that port so renames on split/join keep working while an
operator's own name is never touched.

## 2c. The management address must be in-band

A UniFi switch's management IP lives behind its uplink, on the switch's own
MAC. The controller leans on that: it locates a device by where its IP and
MAC appear in the network and expects that to agree with the LLDP view.
A device managed through a dedicated out-of-band port (Arista `Management1`,
a "MGMT" RJ45 on many vendors) breaks the assumption — the controller finds
the IP behind one UniFi switch and the LLDP identity behind another, and
leaves the device with no Uplink, no Parent Device and no place in the
topology map (verified on Network 10.6.106, 2026-09-20).

So: put the management address on a VLAN interface that rides the uplink,
point the bridge at that address, and leave the OOB port addressless. The
driver reports OOB interfaces in `System.OOBInterfaces`; the loop warns
loudly (every state change) when the bridge's own device address is on one,
when an OOB port carries any address, or when one is merely cabled. Keep
LLDP off on a cabled OOB port: it would announce the same chassis ID to a
second UniFi switch. On EOS, two lines under the interface do that.

## 3. Choose the UniFi model

Run `disunified -collect-once -device-url ...` (or `-device-ssh`). It
prints the port layout and the suggested model from the catalogue (see
[`docs/unifi-models.md`](unifi-models.md) for how the catalogue is built, how to scan a newer
controller for new models, and what the choice affects). Pass `-model` to
override. The port count must match; front-port media is cosmetic because
the device's per-port media report replaces the profile's icons.

## 4. Prove it live, in this order

1. `-collect-once` shows sane data.
2. Read-only run (no `-control-ports`): the device appears under Pending
   Adoption, adopt it, `stat/device` shows live counters, an attached
   client appears behind the right port.
3. `-control-ports <one unused port>`: disable/enable, rename, set a
   speed, set VLANs in the UI; verify on the device after each. Then
   `-control-ports all` and confirm the reconcile makes zero changes.
4. Record what the controller pushed for each feature as
   `docs/fixtures/controller-<version>/system_cfg-*.txt` (scrubbed) and add a parser
   test in [`internal/unificfg`](../internal/unificfg) if a new key appeared.
5. Update [`docs/feature-map.md`](feature-map.md) status columns and the driver's doc file
   (`docs/drivers/<driver>.md`) with what was verified and on which version.

## 5. Where things live

| Path | Owns |
|---|---|
| [`internal/devicemodel`](../internal/devicemodel) | neutral model, driver contract, registry |
| `internal/drivers/<name>` | one vendor; transport, parsing, apply |
| [`internal/unificfg`](../internal/unificfg) | parsing the controller's `system_cfg` |
| [`internal/device`](../internal/device) | inform session, payload, capability claims, persistence |
| [`internal/informloop`](../internal/informloop) | the loop: collect → inform → apply/reconcile |
| [`internal/unifimodel`](../internal/unifimodel) | which UniFi model to claim |
| [`internal/unifiapi`](../internal/unifiapi) | controller REST API (naming) |
| [`cmd/disunified`](../cmd/disunified) | flags, wiring, no vendor code |
| [`docs/`](.) | protocol notes, per-vendor notes, feature map, fixtures |
