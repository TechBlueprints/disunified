# `breakout` -- a split cage as its own UniFi switch (build spec, 2026-11-02)

Status: **specified, not built.** The design discussion and the capture
that decided it are [`docs/breakout-lag.md`](../breakout-lag.md) (§6, the
lane switch; §6.6-6.7, the controller placing a stub lane switch from our
own bridged switch's report). This document is what to build, written for
the session that builds it; the facts it rests on are cited back to the
note and the code.

## 1. What it is

A split cage of a bridged switch (the DX010's QSFP28 cage broken out to
4x25G or 2x50G) is presented to the controller as a second adopted device:
a five-port UniFi switch (`USFXG`, the Flex XG profile) whose port 5 is the
breakout cable itself, marked as the uplink, and whose ports 1..4 are the
lanes. The controller places it under the cage because the parent's inform
reports its MAC there (`docs/topology-placement.md`, the 2026-11-02 fact).
From then on everything the controller can do to a switch port it does to
the lanes -- state, name, VLANs, speed, FEC, storm control, STP port
settings, a mirror between two lanes, and **an aggregate**, which is the
case that started this: three lanes to one LACP partner become an ordinary
UniFi aggregate on the lane switch, built by the parent driver's existing
LAG path on the lane interfaces. Nothing is detected; the operator decides
in the UI, as on a UniFi switch.

The child has no device of its own. It is a view over its parent's latest
snapshot and a caller into its parent's plan, in the same process
(`CLAUDE.md` §4: every bridged device runs in one container; the DX010's
bridge runs on the switch, and its lane switches run there with it).

Not in this build: a lane switch that appears by itself when a cage splits
(§12), the Arista as a parent (§6 is the contract it will implement), MLAG,
a bundle across two cages (that is a UniFi aggregate of the two cages on
the parent, which exists).

## 2. Config

```yaml
devices:
  - name: core-1
    driver: sonic
    ssh: admin@192.0.2.10
    …
  - name: core-1-cage2
    driver: breakout
    options:
      parent: core-1        # the config name of the device whose cage this is (required)
      cage: 2               # the parent's front-panel index of the cage (required)
      # mac: "02:…"         # optional; default derived from the parent's MAC (§4)
    # model: defaults to USFXG for this driver; url/ssh/auth not used
    control:
      ports: all            # the usual scope; off = report only
```

`internal/config`: `driver: breakout` needs `options.parent` naming another
entry in the same file and `options.cage` a positive integer, and needs no
`url`/`ssh`/`auth` (today an entry without `url`/`ssh` is not opened at
all, `cmd/disunified/main.go` `runOne`: open the driver whenever `driver`
is set). `model` unset becomes `USFXG` for this driver (today `""` becomes
`auto`, which would rank a 4xSFP28+1xQSFP28 layout against the catalogue
and land somewhere else). `children:` on the parent is **not** needed: the
child registers itself (§5.3).

One child per cage; two entries naming the same parent and cage are a
config error. A child whose parent entry is a `breakout` itself is a config
error.

## 3. What the controller sees

The snapshot the child reports each cycle (`Collect`), built from the
parent's last snapshot through the `LaneProvider` contract (§6), never from
a device read of its own:

| Port | What | Fields |
|---|---|---|
| 1..N (N = 4 for a 4-lane mode, 2 for a 2-lane mode) | the lanes, in SONiC interface order | every per-lane field the parent collects: `Up`, `Enabled`, `SpeedMbps`, `FullDuplex`, `MTU`, `Counters`, `Health` (link changes, FEC counters), `FEC`/`FECConfig`, `FECCapable`, `AutoNeg`/`AdminSpeed`, `VLAN`, `STPState`/`STPRole`/`STPPathCost`/`BPDUGuard`/`STPEdge`/`STPOperEdge`, `StormCtrl`, `Neighbor` (the host on that lane), `MACs` (the FDB entries on that lane), `LAG`/`LAGID`/`LACP` (§5.2), `MirrorFrom` (a lane mirrored onto this lane), `Media` SFP28 (QSFP28 for a 2x50G lane), `SpeedCaps` = the lane speeds the current mode runs (`4x25G[10G]`: 10000, 25000; `2x50G`: 50000), `Present` = the cage's module present, `Optic` = nil (the module sits in the cage: reported on port 5), `IfName` = the SONiC interface name (`Ethernet5`), `Interfaces` = that one name, `Lanes` 1 |
| N+1..4 (a 2-lane mode) | empty slots | `Present` false, `Up` false, `Enabled` true, Media SFP28, `SpeedCaps` nil, nothing else. The Flex XG draws them "Not Connected" (the §6.6 capture) |
| 5 | the cable: the cage | `Up` = any lane up, `Enabled` = any lane enabled, `SpeedMbps` = the cage's native speed (the highest speed in the parent cage's `SpeedCaps`: 100000 on the DX010), `FullDuplex` true, `Counters` = the lanes summed, `Health` summed, `Media` QSFP28, `Optic` = the cage's module, `Present` = the module present, `Neighbor` = the parent (§4), `MACs` none, `IfName` = the parent cage's `IfName`, `STPState` "forwarding", no VLAN config, no LAG |

`Snapshot.System`: `Vendor` "disunified", `Model` "Lane switch", `SKU` the
mode (`4x25G[10G]`), `Hostname` = the child's config name, `Serial` = the
MAC without colons (main's default), `Version` = the parent's `Version`
(the lanes run the parent's software), `MAC` (§4), `Addresses` = the
parent's dialled address first (§4), `Netmask`/`GatewayMAC`/`GatewayIP` =
the parent's, `Uptime` = the parent's, `CPU`/`Memory`/temperatures/fans/PSUs
= none (zero, `Has*` false: the profile shows nothing rather than the
parent's twice), `OOBInterfaces` none, `MACTable` = the lanes' entries,
`VLANs` = the parent's. `UplinkHint` 5. `Snapshot.UplinkPort()` finds port 5
by itself: its neighbour is a bridge that is not a `Child` and the port is
up when any lane is.

Names (`devicemodel.Namer`, the driver's own): a lane is named by its SONiC
alias when the parent has one (`Eth2/1`), else its interface name; port 5
by the cage's alias (`Eth2`); the device `"<parent hostname> <cage alias>"`
(`core-1 Eth2`). `DefaultPortNames`/`DefaultDeviceNames` list the earlier
forms so a rename after a convention change still works while an operator's
own name is kept.

Capabilities (`devicemodel.Capable`), derived from the parent's claims:
`LACP`, `FEC`, `Jumbo`, `StormControl`, `BPDUGuard`, `STPAutoEdge`,
`STPPortCost`, `LoopProtection` as the parent claims them (they are per
port); `AggregateSessions` = the parent's; `MirrorSessions` 1 when the
parent has any (a lane onto a lane); `STP` false (switch-wide mode and
priority are the parent's), `IGMPSnooping`, `DHCPSnooping`, `SNMP`,
`LLDPMED`, `PortIsolation` false, `IPACLs`/`MACACLs` 0. The claims must be
true for what §5.1 applies.

When the cage is **not split** (one interface, `Lanes` 1), `Collect`
returns an error (`cage 2 of core-1 is not split`) and the loop treats the
child as a device that does not answer: informs are withheld once the
snapshot is stale (`informloop` `staleSilenced`), the controller shows it
disconnected, and it keeps its placement (a placement outlives the report,
§6.6). `Start` returns the same error; main's start retry (15 s doubling to
5 min) keeps trying, so a child declared before its cage is split, or whose
parent has not started yet (§5.3), comes up when it can. The cage's
`Lanes` changing between modes (4 to 2) changes the port layout: the loop
already logs a layout change; the controller keeps the five-port profile
and the lanes' overrides by index.

## 4. Identity, reachability, placement

- **MAC**: `options.mac` if set; else derived from the parent's system MAC:
  the first octet with the locally-administered bit set (`| 0x02`) and the
  multicast bit clear, the last octet replaced by the cage index. Stable
  across restarts, distinct per cage, never a real OUI's address. Logged
  at start. (The controller keeps the adopted key under this MAC in
  `state/<name>/device.json` as for any device.)
- **Address**: the child reports its parent's address. `System.Addresses`
  holds the parent's dialled address first (main takes `Addresses[0]` when
  there is nothing to dial), and the driver implements
  `devicemodel.AddressReporter` returning the parent's `CurrentAddress()`
  when the parent implements it, so an address move on the parent is
  followed. `Netmask` and `GatewayMAC` are the parent snapshot's. Proven
  fine in the capture (§6.6: the stub's `ip` and `connect_request_ip` were
  the parent's throughout; the parent's record was unchanged).
- **Placement**: the parent's inform reports the child's MAC in the cage's
  MAC table and as the cage's LLDP neighbour (§5.3), the way the
  `children:` knob does today (`internal/informloop/children.go`); either
  entry places, LLDP is the controller's preference, both are sent. The
  parent's cage reports **no neighbour of its own** while a child is
  attached (the hosts' neighbours belong to the lanes, §5.4), so the LLDP
  entry is never blocked by a host's.
- **Port 5's neighbour** is the parent: `ChassisID` = the parent's LLDP
  chassis id as the parent reports it for itself (`System.MAC` is the
  system MAC; the SONiC driver's chassis id is eth0's MAC, `internal/drivers/sonic/CLAUDE.md`
  -- use what the parent's own `lldp_table` would carry for itself, which
  the contract exposes, §6), `SystemName` = the parent's hostname, `PortID`
  = the cage's interface name, `IsBridge` true. The controller then also
  sees the edge from the child's side.

## 5. Control

### 5.1 The child's push goes to the lanes

The child implements `devicemodel.Controller` and `Planner`:

- `ApplyPorts(desired)`: desired ports 1..N are the lanes, in order; the
  call becomes `parent.ApplyLanes(ctx, cage, desired)` with the same
  `PortDesired` values and `Index` = the lane number. A desired entry for
  an empty slot (N+1..4) or for port 5 is dropped; a port-5 entry that
  carries anything but the profile's defaults (a VLAN set, a name, a
  disabled state) is logged once per change: the uplink's settings are not
  applied (a real switch's uplink is mostly untouchable in the UI; the
  capture showed the Flex XG's uplink drawer offers 100 GbE and FEC only).
- `PlanPorts(desired)` the same through `parent.PlanLanes`, so the loop's
  first-push hold and fresh-port withholding work unchanged: a child's
  adoption seeds the controller from the lanes as they are and holds a
  first push that would change one.
- The lane `PortDesired` carries the child's `MTU` (the lane switch's own
  jumbo setting). The driver implements `JumboSwitcher` when the parent
  does. The lanes take the child's MTU; the parent's cage plan no longer
  writes one (§5.4).
- `LAG` in a lane's desired is the child's UniFi aggregate id (the lowest
  member's index on the child). The parent maps it to a PortChannel id of
  its own range (§5.2).
- `MirrorSource` on a lane names another lane (the child's index); the
  parent maps it onto the lane interface. A mirror whose source is not a
  lane of this cage cannot be expressed and is refused with a note.
- `SpeedMbps` on a lane: a lane-speed change on that lane only, within the
  speeds the cage's mode runs (`SpeedCaps`); a speed the mode does not run
  (100000 on a 4x25G lane) is refused with a note -- a lane never splits or
  joins the cage. 0 (auto) = the mode's default speed for that lane.
- The child has **no** `DeviceController`, `AddressController`,
  `SSHKeyInstaller`, `SyslogController`, `Locator`, `Rebooter`,
  `PortCycler` (a `PortCycler` is cheap -- bounce the lane -- and may be
  added), `VLANController` (`EnsureVLANs`: the parent creates VLANs; the
  child's push names the same site VLANs, which `ApplyLanes` passes through
  the parent's `EnsureVLANs` first, §6). The loop logs what it cannot
  apply as it does for any device lacking the interface: STP mode and
  priority, IGMP, NTP, syslog, hostname, SSH keys, SNMP, time zone, IP
  Settings, DHCP guard and ACL rules of the child's push are the parent's
  business and are ignored; `set-locate` is reported only; Restart is
  refused with a log.

### 5.2 PortChannel ids

A lane aggregate is `PortChannel<100*cage + lag>` (`cage` the parent's
index, `lag` the child's UniFi id): cage 2's aggregate headed by its port 2
is `PortChannel202`. The parent's own aggregates are `PortChannel<lag>`
with `lag` a front-panel index, never ≥ 100, so the two never collide and
either side tells the other's by the id alone:

- the lane plan reads a lane in `PortChannel<n>` with `100*cage < n <
  100*cage+100` as `LAGID = n - 100*cage`; a lane in any other PortChannel
  (hand-made, or the parent's) reports `LAG` = its name, `LAGID` 0, and the
  lane plan leaves it alone with a note;
- the parent's cage plan never touches a PortChannel with id ≥ 100
  (`lagLeaves`, `ownOrphan`), and refuses to put a child cage into one of
  its own aggregates (a cage that is a lane switch cannot also be a member
  of the parent's LAG: note, not written).

The create is the existing one (`config portchannel add … --fast-rate true
--min-links 1`, the MTU hset, members out of their VLANs and in, the
PortChannel carrying the members' VLAN intent, the DUI ACL tables unbound
from the joining lanes and the PortChannel bound after -- all of it
`control.go`'s `lagJoins`/`lagLeaves`/`aclUnbind`/`applyACL`, unchanged
but for the id). LACP state per lane comes from `state_lag_member` as
today (`hardware.go`): `lag_member`, and on the aggregate's lowest lane
`aggregate_num_ports`, `lacp_state`, `partner_system_id` -- the ECS's
shape, now on the lane switch, which is exactly the LACP-partner picture
the whole exercise was for.

### 5.3 Attachment: how the child finds its parent and is reported by it

`devicemodel` gains a small in-process registry, vendor-neutral:

```go
// Attach records an opened device under its config name; Opened finds it.
func Attach(name string, d Device)      // main, after openAndStart succeeds; Detach on Close
func Opened(name string) (Device, bool)
```

main calls `Attach(o.name, sw)` once `openAndStart` returns, for every
device. The `breakout` driver's `Open` looks up `options.parent`; not
opened yet → an error main's start retry keeps retrying; opened but not a
`LaneProvider` → a permanent error (the retry still loops; the log says
why). It then calls `parent.AttachLaneSwitch(cage, Attachment{MAC, Name})`
(§6), which does three things on the parent: records the cage as a child
cage for its plan (§5.4), makes its folded cage report the child's MAC and
LLDP entry (the same effect as a `children:` entry, now produced by the
driver rather than by config -- the loop's `withChildren` keeps serving
config entries; the contract's attachment is reported by the parent
driver's own snapshot, so the two paths never disagree about what is a
child), and drops the cage's own neighbour. `Close` detaches.

Both parent and child run their own `informloop.Loop` with their own
inform session, state dir, reply log and provisioning, as any two devices
do today. The child's loop polls `Collect` at its interval and gets the
parent's **last** snapshot's lanes -- no device read; the parent's cycle
is the only SSH exec. A child's snapshot is therefore up to one parent
interval old, which is the same age the parent's own inform has.

### 5.4 The cage on the parent while a child is attached

The parent keeps one control on the cage: **its speed is the breakout
control** (`docs/drivers/sonic.md` §7.5: 25G/10G → `4x25G[10G]`, 50G →
`2x50G`, 100G/40G → join; the module rule of §10.9 too), and
**disabling the cage shuts every lane** (then the lanes' own `Enabled`
from the child is not applied until the cage is enabled again). Every
other setting of the cage's `PortDesired` -- VLANs, name, FEC, MTU, storm
control, STP port settings, mirror, LAG -- is **not applied** to a child
cage and is noted once per push version ("cage 2: a lane switch
(core-1-cage2) sets its lanes; the cage's own settings are not applied").
The parent's `PlanPorts` must count none of them either, or the parent's
first-push hold would wait on the child's lanes.

A join (100G on the cage, or the module rule) while lane aggregates or
lane mirror sessions exist: the plan removes them first in the same batch
(members out, PortChannel deleted, mirror session removed, the ACL tables
rebound), then the breakout. Today a breakout is refused for a cage in an
aggregate or a mirror session (§7.5); for a child cage the child's own
aggregates are the parent's to dissolve, since the join is the operator's
decision on the parent. The child then stops answering (§3) and the
controller shows it disconnected until forgotten (`cmd/sitemgr
delete-device`; automated later, §12).

The parent's folded cage report changes for a child cage: `Neighbor` nil
(the lanes' hosts are on the lanes; the child's LLDP entry takes the
cage's neighbour slot), `SpeedMbps` = the cage's native speed (100000; a
split cage reports its lane speed today), the child's MAC in `MACs`
(beside the lanes' real FDB entries, which stay: a real upstream switch
sees every downstream MAC on the port). Counters, health, `Lanes`, the
module, `SpeedCaps` unchanged.

## 6. The parent contract (`devicemodel`)

```go
// LaneProvider is a device whose broken-out cages can be presented as
// lane switches (the breakout driver). Lane indexes are 1..N in the
// device's lane order; a PortDesired's Index is a lane index.
type LaneProvider interface {
	// Lanes returns the cage's lanes from the last snapshot, each a Port
	// of its own (Index = lane number, Interfaces = its one interface),
	// plus the cage itself as folded. ok is false when the cage does not
	// exist or is not split.
	Lanes(cage int) (lanes []Port, cage Port, ok bool)
	// LaneAlias is the device's faceplate label for a lane ("Eth2/1") and
	// for the cage ("Eth2"); "" when the device has none.
	LaneAlias(cage, lane int) string
	// ApplyLanes makes the cage's lanes match desired (a diff; idempotent;
	// the device's VLANs ensured first). PlanLanes reports which lanes
	// ApplyLanes would change, writing nothing.
	ApplyLanes(ctx context.Context, cage int, desired []PortDesired) (changed int, err error)
	PlanLanes(cage int, desired []PortDesired) []int
	// AttachLaneSwitch declares a child on the cage: the device reports
	// the child's MAC and an LLDP entry for it on the cage, drops the
	// cage's own neighbour, and stops applying the cage's port settings
	// other than speed and admin state (its lanes are the child's).
	// DetachLaneSwitch undoes it.
	AttachLaneSwitch(cage int, child Attachment) error
	DetachLaneSwitch(cage int)
	// LocalChassisID is what this device reports as its own LLDP chassis
	// id (what a neighbour of it sees).
	LocalChassisID() string
}

type Attachment struct {
	MAC  string // lower-case colon form
	Name string // the child's config name, reported as the LLDP system name
}
```

Nothing UniFi-specific crosses it: lane indexes, `Port`s and
`PortDesired`s. `Capable` on the parent gives the child its claims (§3).
`AddressReporter` on the parent gives the child its address (§4).

## 7. SONiC implementation notes (`internal/drivers/sonic`)

- **Per-lane ports beside the fold.** `buildPorts` already makes a full
  `Port` per interface (`lanePort(...)`) before folding it into the cage;
  keep them: `cagePort` gains `lanes []devicemodel.Port` (Index = 1..N in
  `ifNum` order, `Interfaces` = the one name, `Lanes` 1, the lane's own
  alias kept beside it) and the `Collector` stores them with `last`
  (`c.lanes map[int][]devicemodel.Port`, under `mu`). A lane's `LAG`/
  `LAGID`/`LACP` are read per interface as today (`lags[name]`,
  `state_lag_member`), with the id mapping of §5.2 applied for lanes of a
  child cage. `Lanes(cage)` copies from there.
- **The plan takes a scope.** `newPlan` keys `pl.ports` by UniFi index and
  walks `lanesOf(p)`; give it a scope: `{ports map[int]Port, pcBase int,
  laneOnly bool}`. The cage plan is `{snap.Ports, 0, false}` (today's
  behaviour); the lane plan for cage N is `{lanes of N keyed by lane
  index, 100*N, true}`. With `laneOnly`: `speed` writes `config interface
  speed <lane> <Mb/s>` only when the mode runs it (`SpeedCaps`), never
  `breakoutSpeed` or `moduleSpeed`; `pcName(lag)` is `pcName(pcBase+lag)`;
  `mirrorSource` resolves the source lane's interface within the scope;
  everything else (`portState`, `syncVLANs`, `lagLeaves`/`lagJoins`,
  `stormOps`, `stpOps`, `vlanMTUs`, `aclUnbind`/`applyACL`, the
  module FEC) runs unchanged on the lane as the single interface of its
  "port". `applyPorts` and `PlanPorts` take the scope; `ApplyLanes`/
  `PlanLanes` call them with the lane scope after `EnsureVLANs` for the
  VLANs the desired lanes name.
- **One apply at a time.** The parent's loop and each child's loop call
  into one `Collector`; `c.mu` guards `last`/`state` but not a whole
  apply. Add `applyMu` held across `newPlan` → batch → state commit, in
  `applyPorts`, `ApplyLanes`, `applyDevice`, `EnsureVLANs`, the address
  move and the SSH-keys script, so two plans never interleave their
  batches or diff against a state the other is changing. The write budget
  (`ApplyBudget`, 90 s) applies per call.
- **Child cages in the cage plan.** `c.children map[int]Attachment`
  (from `AttachLaneSwitch`). For a child cage `portState` writes only the
  admin state (every lane) and the speed (`breakoutSpeed`/`moduleSpeed`
  as today); `syncVLANs`, description, FEC, MTU (`vlanMTUs` leaves its
  lanes out), storm, STP, mirror and LAG are skipped with the §5.4 note;
  `lagLeaves`/`ownOrphan` ignore PortChannel ids ≥ 100; a cage plan that
  would join a child cage (a cage speed that changes the mode) first
  dissolves the child's PortChannels and mirror sessions in the batch
  (`-f` removes VLAN memberships, not aggregates: do it explicitly) and
  the `breakoutBlocked` refusal for "a cage in an aggregate" does not
  apply to the child's own. `PlanPorts` (controller-only) must leave the
  same things out.
- **The folded cage with a child**: `Neighbor` nil, `MACs` + the child's
  entry (VLAN = the native stand-in's UniFi id 1, `PortIndex` = the cage,
  `LastMove` = the collect time), `SpeedMbps` = `max(SpeedCaps)`.
  `LocalChassisID()` = eth0's MAC as `LLDP_LOC_CHASSIS` carries it
  (`collect.py` already prints it; `MgmtMAC`).
- **l2d (RSTP)**: the lane plan's `stpOps` write per interface as today;
  l2d's config is per port and the lanes are ports to it. Nothing new.
- **Hardware**: FEC codeword counters, link changes and the FDB are per
  interface already (`hardware.go`, `lanePort`); the module (`xcvr` keyed
  by the first interface) goes to port 5.
- `collect.py` needs no change: every table it prints is per interface.

## 8. Loop, main, config, presentation

- `cmd/disunified/main.go`: open a driver when `driver` is set even with
  no `url`/`ssh` (the breakout driver, and `stub` with a file, both have
  nothing to dial); `devicemodel.Attach(o.name, sw)` after
  `openAndStart`, `Detach` on exit; `o.ip` from `Addresses[0]` already
  covers the child; `-list-drivers` shows it.
- `internal/config`: §2's rules (`parent`, `cage`, the `USFXG` default,
  no transport needed, one child per cage), tested.
- `internal/informloop`: no change for the attachment (the parent driver
  reports it); `withChildren` stays for config entries. A `Collect` error
  on a child is the existing "device does not answer" path. The loop's
  "device settings not supported" notes cover the missing interfaces.
- `internal/device` (wire): nothing new -- the stub's inform under the
  same tables was adopted, placed and configured in the capture; port 5's
  `is_uplink` comes from the descriptor's uplink port as today
  (`UplinkPort()` = 5).
- `internal/unifiapi`: seeding the child's lane overrides at adoption
  works as for any device. The capture's quirk (a forced-speed override
  wins over the inform in the stored record): the seed writes a lane's
  speed override only when the lane runs a forced speed (`AdminSpeed`),
  which `SeedPortOverrides` should already express -- check it, test it.
- Docs: this file becomes the driver's write-up as it is built (§6.6 of
  `breakout-lag.md` for the capture, `docs/feature-map.md` and
  `docs/support-matrix.md` rows for the lane switch, `docs/adding-a-device.md`
  one line, `internal/drivers/CLAUDE.md` one bullet: a parent exposes
  `LaneProvider`; a child driver has no device), a `CLAUDE.md` in the
  driver directory with the facts that cost time.

## 9. What a push to the child does not do (and says so once)

Switch-wide keys of the child's `system_cfg` (STP mode/priority, IGMP,
NTP, syslog, hostname, SSH keys, SNMP, time zone, IP Settings, DHCP guard,
ACL rules): nothing to apply to, the parent applies the site's; logged
once per setting per push version. `set-locate`: reported only. Restart:
refused with a log (the parent is what restarts). Firmware upgrade:
emulated as on every device. Port 5's own settings: §5.1.

## 10. Tests (before any live run)

Fixtures, real captures only:

- The DX010's capture with a split cage: the breakout tests already build
  a split cage in the `FixtureRunner`'s CONFIG_DB from
  `dx010-platform.json` (`fixture_breakout.go`); the lane tests start
  from a split cage 2 that way, and from the real
  `collect-20261101.txt`/`collect-acl.txt` with cage 2 split in the
  fixture (the 2026-11-01 split was done live from the UI; its capture is
  the §11.6 run -- if a `collect` with a split cage exists in the
  `sonic-*` fixture dirs, use it, else take one on the next live run and
  add it, §11).
- The controller's push for a lane aggregate:
  `docs/fixtures/controller-10.6.109/system_cfg-flexxg-aggregate.txt`
  (the stub's ports 2-4 aggregated, captured 2026-11-02) and the undo.
- The controller's records from the capture:
  `rest-device-flexxg-placement.json`.

Tests:

- `internal/drivers/breakout`: the snapshot from a split cage (4 lanes
  and 2 lanes: ports, empty slots, port 5, System, names, capabilities,
  the derived MAC, the address following the parent); the not-split
  error; `ApplyPorts` mapping (lanes, dropped port 5 with its note,
  refused speeds and mirrors); a fake `LaneProvider` for the driver's own
  tests.
- `internal/drivers/sonic`: `Lanes(cage)`; the lane scope end to end
  against the fixture CONFIG_DB -- state, name, VLANs (native mapping),
  a lane speed and a refused one, FEC, storm, STP, a lane-to-lane mirror,
  the aggregate of lanes 2-4 (`PortChannel202`, fast rate, min-links,
  MTU hset, VLAN move, ACL unbind/bind) and its dissolution, idempotence
  (a second apply writes nothing); the cage plan with a child attached
  (admin state and speed only, the note, PortChannel ≥ 100 untouched, the
  join dissolving the lane aggregate first, `PlanPorts` counting none of
  the skipped settings); the folded cage's report (no neighbour, the
  child's MAC, native speed); `applyMu` (two applies serialised).
- `internal/informloop`: a replay of the recorded Flex XG aggregate push
  through a child loop onto a parent `sonic` collector with the fixture
  runner -- the expected batch on the parent, the reply log, a zero-change
  second cycle; the first-push hold on a child whose lanes differ from the
  controller's defaults; the child going stale when the cage joins.
- `internal/device`: the child's inform against the contract test's
  checks (the stub's was accepted by the controller; the tables are the
  same code).
- `internal/config`: §2.
- `go vet ./... && go test ./...` and `scripts/check-site-info.sh --staged`
  before every commit; nothing site-specific anywhere (`local-information/`
  holds the real names, cage and addresses).

## 11. Live verification order (the DX010, from the controller)

Prerequisite: a free cage with a 4x25G breakout DAC whose far side is a
host with two or three of the lanes in one LACP bond (Clint cables it; the
cage and host are named in `local-information/site.md`). Never the uplink
cage. The bridge runs on the switch (`sonic-on-switch.md`): the child is
one more entry in `/host/disunified/config.yaml`, the package rebuilt and
installed as that document says.

1. Deploy with the child declared; the cage still whole: the child logs
   "not split" and retries; the parent unchanged. Record the parent's
   zero-change reconcile.
2. Split the cage from the parent's port drawer (25G). The parent's log
   shows the breakout; the child starts, goes pending; adopt it by exact
   MAC (`cmd/devmgr adopt`), never Adopt All. Placed under the cage within
   a minute; the parent's record gains the downlink entries. The
   first-push hold seeds the lanes. Record both devices' records and the
   child's first inform (scrubbed).
3. The UI walk on the child: lanes with their hosts' neighbours, counters
   moving, FEC, the uplink's module on port 5.
4. Mark the bonded lanes Aggregate on the child. The parent's log shows
   `PortChannel2<nn>` created, the lanes joined; `teamdctl` on the switch
   shows the partner; the child's inform carries `lacp_state` and
   `partner_system_id`; the UI shows the aggregate with its members. The
   next cycle writes nothing. Traffic from the host spreads over the
   lanes (the host's bond is up).
5. Remove the aggregate in the UI; the PortChannel dissolved, lanes back
   in their VLANs; zero-change cycle.
6. A lane setting each: name, a tagged VLAN, disable/enable, storm
   control, a forced 10G and back, a lane-to-lane mirror if a host can
   see it. Each restored.
7. Jumbo on the child vs the parent: both ways, the lanes' MTU follows the
   child.
8. Join the cage from the parent (100G) while an aggregate exists: the
   parent dissolves the aggregate, joins; the child goes stale, the
   controller shows it disconnected; forget it (`cmd/sitemgr
   delete-device`). Split again: it comes back pending (new adoption; the
   same MAC, so the old record must be forgotten first -- note in the doc).
9. Restart of the switch with a child adopted: both come back, the child
   after the parent; the placement kept.

Record: the logs, both records at each step, the pushes into fixtures
(scrubbed), §6.6-style answers into this document's "live" section, the
feature map and support matrix rows.

## 12. Later

- **Automatic lane switches**: a `sonic` option (`lane_switches: auto`)
  that creates a child for every split cage and retires it on a join,
  which needs the loop to add and remove devices at run time (today the
  device list is fixed at start, `runConfig`), and the bridge to forget a
  retired child in the controller (`cmd/sitemgr delete-device`, which the
  `unifiapi` client can send). The declared form is the first step
  because it needs neither.
- **The Arista as a parent**: its lanes are distinct interfaces already
  (`Ethernet50/1..4`, `parse.go`'s `slotAcc`); `LaneProvider` is the
  contract to implement, with `channel-group` on lanes for the aggregate.
- A `PortCycler` on the child (bounce one lane).
- `docs/breakout-lag.md` §4-5 (shape 1) stay as the record of the road
  not taken; nothing there is built.

## 13. Open questions for the build (decide, note the answer here)

1. The parent's cage `SpeedMbps` while a child is attached: the native
   speed (this spec) shows the cage as a 100G link to a switch, which is
   the cable's capacity and what the capture drew well; the alternative
   (the lane speed, as today) would read as a 25G link. Native.
2. Whether `AttachLaneSwitch` should also require the cage to be split
   (refuse otherwise) or accept it whole and wait: accept and wait (§3),
   so a child can be declared before the split.
3. Whether a child cage's `Enabled` on the parent should be honoured at
   all, or the cage's state left to the lanes: honoured (a cage disabled
   on the parent is every lane down), because "disable the port" on the
   parent must mean something.
