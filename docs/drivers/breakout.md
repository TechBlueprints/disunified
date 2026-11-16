# `breakout` -- a split cage as its own UniFi switch

Status: **built 2026-11-14 with `sonic` as the parent; fixture- and
replay-tested only, never adopted.** The first live adoption is the next
step (§15). The Arista is the second parent since 2026-11-15 (§7b as
built): fixture- and replay-tested on captures of its empty test cage
split, its lane batches run on that cage by hand, never adopted.
The design discussion and the capture that decided it are
[`docs/breakout-lag.md`](../breakout-lag.md) (§6, the lane switch; §6.6-6.7,
the controller placing a stub lane switch from our own bridged switch's
report; §6.8, what this build decided). §0 is what was built and where it
departs from the spec of 2026-11-02 below it (§1-§13, kept as written, with
"as built" marks where §0 changes them). The facts it rests on are cited
back to the note, the captures and the code.

## 0. As built (2026-11-14)

**Code.** `internal/devicemodel/lanes.go` (the contract, neutral),
`internal/drivers/breakout` (the child), `internal/drivers/sonic/lanes.go`
plus the lane scope in `control.go`, `model.go`, `collector.go` and
`breakout.go` (the parent), `internal/drivers/arista-eos/lanes.go` plus
the scoped `plan()` in `apply.go` (the second parent, §7b),
`internal/config` (the entry's rules),
`cmd/disunified/main.go` (the attachment and the registry).

**Declared in the parent's file, attached by main.** A lane switch is a
config entry with `driver: breakout`, `options.parent` (another entry of
the same file, not a lane switch) and `options.cage` (1..99), no
`url`/`ssh`, `model` defaulting to `USFXG`; at most one per cage, and not
on a port the parent's `children:` already names (`config.Load`). `main`
opens the parent, then -- **before the parent's loop starts** -- calls
`AttachLaneSwitch(cage, {MAC, Name})` for every lane-switch entry naming it,
and registers the opened parent under its config name
(`devicemodel.RegisterOpened`). The spec had the child attach itself from
its own `Open`; that loses a race: the parent's first apply could run
before the child opened and write the cage's intent over the lanes on every
bridge restart. Attached by main, the parent's very first plan already
leaves the lanes alone. The child's `Open` finds the parent by name
(`devicemodel.OpenedDevice`) and fails with a reason until it is there;
main's start retry (15 s doubling) brings it up.

**Identity.** The MAC is `options.mac`, else
`devicemodel.LaneSwitchMAC(parent MAC, cage)`: the parent's MAC with the
locally administered bit set and the multicast bit clear in the first
octet, the cage in the last (DX010 cage 2 from a parent `xx:..:yy` is
`(xx|02):..:02`). main computes it once and the child reads it back from
the parent (`LaneView.Child`), so the parent's report and the child's
inform cannot disagree. The serial is the MAC's digits like every bridged
device's (`device.SerialFor`, `CLAUDE.md` §5). Hostname = the child's config
name; model string "Lane switch", SKU the cage's mode (`4x25G[10G]`);
version the parent's. Address: `AddressReporter` returns the parent's
`CurrentAddress()` (the dial target, which follows an address move), and
the snapshot carries one address on `eth0` with the parent's prefix length,
exactly the shape the stub that was placed reported. **Why the controller
places it:** the parent reports the child's MAC in the cage's MAC table and
the child as the cage's LLDP neighbour (`Child` set, so never the parent's
uplink), the two entries the 2026-11-02 run proved place a device with no
other trace on the LAN, either one alone (`docs/breakout-lag.md` §6.6.1).

**Reading.** The parent keeps each split cage's lanes as ports of their own
beside the fold (`cagePort.lanes`, `buildPorts`): state, speed, counters,
FEC, MTU, VLAN, the host's LLDP neighbour, and -- through the same
interface-keyed helpers the fold uses -- LACP (`setLACP`), l2d's STP state
and faults (`l2Ports`), the storm and DHCP-rate counts, DHCP guard drops,
the FDB entries learned on each lane (`laneMACs`; a lane aggregate's on its
lowest member) and a lane-to-lane mirror. A lane's `SpeedCaps` are its
STATE_DB `supported_speeds` (the DX010's 25G lanes: 1000,10000,25000 --
what portconfig checks), else the mode's speeds; its media is SFP28 for a
lane whose fastest speed is 25G (at 10G too: the controller takes a FEC at
25G only from an SFP28, `docs/breakout-lag.md` §7), QSFP28 at 50G (the
Flex XG drew a 50G lane as a QSFP tile, §6.6.3 of the note). `Lanes(cage)` hands the child those ports, the folded
cage, the parent's `System`, `VLANs`, its LLDP chassis id
(`LLDP_LOC_CHASSIS`), the mode, the cage's admin state and the attachment.
The child's `Collect` is that view, no device read; it errors while the
cage is whole (not split), while nothing is attached, and when the parent's
snapshot is older than `options.max_age` (default 3 min: the parent is not
answering, and its last snapshot would be frozen numbers).

**The child's snapshot** (`breakout.Snapshot`): ports 1..N the lanes; N+1..4
empty slots (a 2x50G cage) with no interface, not present; port 5 the
cable: up when any lane is, at the cage's native speed (the highest of its
`SpeedCaps`, 100G on the DX010) and claiming only that, counters and FEC
counters summed, the cage's module, the parent as its LLDP neighbour
(chassis id the parent's own, system name its hostname, port id the cage's
interface), the uplink hint. Names: lane alias (`Eth2/1`), the cage's alias
for port 5, the device `"<parent hostname> <cage alias>"`. Claims: the
parent's LACP, FEC, jumbo and storm control, `AggregateSessions` at most 2
(four lanes), `MirrorSessions` at most 1; nothing switch-wide.

**Writing.** `ApplyPorts`/`PlanPorts` on the child pass the lanes'
`PortDesired` (Index = lane) to the parent's `ApplyLanes`/`PlanLanes`;
port 5's settings are dropped (logged once when they ask for anything) and
a mirror whose source is not another lane is refused with a note.
`EnsureVLANs` is the parent's, `ApplyTimeout` the parent's budget,
`SwitchesJumbo` the parent's. On SONiC `ApplyLanes` is the parent's own
plan with a **lane scope** (`planFor(cage, ...)`): the ports are the lanes,
the PortChannel of the child's aggregate `lag` is
`PortChannel<100*cage+lag>` and its mirror session `dui<100*cage+lane>`;
state, name, VLANs, FEC (the module FEC where the push says auto), MTU,
storm control, the aggregate (create, MTU, fast rate, min links, the VLAN
move, the ACL unbind) and a lane mirror run through the same routines as a
cage's; the batch is followed by the ACL rebind and the IGMP reserved
groups, as `ApplyPorts`' is. A lane speed is forced on that lane only and
must be one it runs; auto leaves it at what it runs. The plan refuses a
lane plan whose interfaces no longer match CONFIG_DB (the cage changed
since the collect).

**Who writes what on a split cage with a lane switch ("owned").**

| | the lane switch (child) | the cage on the parent |
|---|---|---|
| admin state | each lane | disabled = every lane down; while it is, the child cannot bring a lane up (`LaneView.CageDisabled`) |
| speed | a lane speed on its lane | **the mode only** (split, re-split, join; the module rule of sonic §10.9 too, kept while the module runs that many ports): a cage speed the current mode runs writes nothing, so it never undoes a lane speed |
| name, VLANs, FEC, MTU, storm control | each lane | not applied (one note per push) |
| aggregate, mirror | between its lanes | the cage's own not applied; a join removes the child's first |
| STP port settings (edge, BPDU guard, loop protection), isolation, port security, egress limit, LLDP-MED | -- | **the cage's, on every lane, as before**; for STP on every bridge port the lanes make, a lane aggregate included (§14.4) |
| VLAN creation | -- (`EnsureVLANs` is the parent's; `EnsureLaneVLANs` makes the child's list at the lanes' MTU) | the parent's |
| VLAN MTU | the child's own VLANs (those the parent's push does not list), from the lane plan | every VLAN, from the cage plan; each VLAN at the lowest MTU of the pushes that list it (sonic.md §11.29.4) |

The VLAN MTU row was "the parent's" until 2026-11-15. A VLAN only the
child listed then got its MTU in CONFIG_DB at creation, but its netdev
stayed at 9100 and a jumbo flip never reached it (§7, sonic.md §11.29.4).

The STP-and-port-features row and VLAN creation depart from the spec
(§5.4), for a code reason: those settings are computed switch-wide from the parent's own ports -- l2d's
per-port intent is the parent's cages' in `l2.go` (`l2SetPorts`, rebuilt
from every port at once: each bridge port a cage's interfaces make, a lane
aggregate included, takes the cage's settings), the isolation group and the static FDB entries from
every isolated or secured port -- so a lane scope would have to rebuild the
whole set from two loops' pushes. The child claims none of them, so the UI
offers none on the lane switch. Moving them to the child is open (§14).

**A join, a re-split, a split.** A cage speed that changes the mode of an
owned cage first removes the child's aggregates (`member del` on each lane,
the PortChannel's VLANs, the ACL tables rebound without it, `portchannel
del`: sonic.md §11.25) and lane mirror sessions in the
same batch (`dissolveLanes`), then breaks out; the new lanes (or the joined
cage) take the cage's whole intent from that plan, as on any breakout, and
the next cycle hands them to the child again if they are still split. The
child of a joined cage stops answering, and the controller shows it
disconnected and keeps its placement (a placement outlives the report,
§6.6.1 of the note) until it is forgotten. A split of a cage whose child is
attached gives the lanes the cage's intent; the child then seeds the
controller from them at adoption.

**Aggregates the plan does not own.** Each scope owns its own range: the
parent's plan PortChannels below 100, a lane plan its cage's 100*cage+1..99.
A lane in any other PortChannel (a lane switch's on a cage no longer
attached, the parent's own, one made by hand) is left in it: no member
del, no VLANs, no MTU, with a note. So a lane switch's aggregate survives
its config entry being removed while it has members, and the operator
removes it by hand (or re-adds the entry and removes it from the UI). An
empty one the driver made, in the lane range of a cage with no lane switch
attached, is the parent's plan's to delete (§7c, since 2026-11-15); any
other orphan outside the plan's range is never deleted.

**One write at a time.** The child applies from its own loop, beside the
parent's: `lockApply` (a one-slot semaphore that gives up with the
caller's context) serialises `Collect`, `ApplyPorts`, `ApplyLanes`,
`EnsureVLANs` and `ApplyDevice` on the SONiC collector, so two plans never
interleave their batches and a collect never reads a batch half done.

**Tests (real captures only).** The parent is the DX010 as captured on
2026-11-07 with cage 2 split to 4x25G
(`sonic-igmp-202511.20261106-f2e25f54c/collect-4x25g.txt`); the push is the
controller's own for a Flex XG with ports 2-4 marked Aggregate
(`controller-10.6.109/system_cfg-flexxg-aggregate.txt`, 2026-11-02).
`internal/drivers/sonic/lanes_test.go` (the view, lane settings, the
owned cage, the aggregate and the join, an unowned lane aggregate, the
lock), `internal/drivers/breakout/breakout_test.go` (the snapshot at 4x25G
and, after a real 2x50G re-split through the parent, at 2x50G; stale,
whole and unattached; the push through the parent; options), the
replay `internal/informloop/replay_breakout_test.go` (the aggregate push
through a child loop onto the parent, both loops' next cycle writing
nothing, the join through the parent's loop, the first-push hold), the
child's payload in the wire contract and shape tests
(`internal/device/contract_breakout_test.go`, `TestLaneSwitchPayload`), and
`internal/config/lanes_test.go`.

What the replay showed about the real push: it names the aggregate `lag=1`
(the controller's aggregate number, not the lowest member's index: the
lane PortChannel is `PortChannel201`); it carries no VLAN keys for ports
without an override, so lane 1 and the aggregate are Allow All; and its
`fec=cl-91` on the members is refused with a note, because the DX010's 25G
lanes support `none,fc` only (STATE_DB `supported_fecs` in the capture).

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
snapshot and a caller into its parent's plan, **in the same process as its
parent, wherever that process runs** (Clint, 2026-11-03: it must work for
every switch, in the container and on the box alike). The bridge has two
deployments (`CLAUDE.md` §4, `docs/drivers/sonic-running.md`): the one
container on the Podman host, whose `config.yaml` lists the Arista, the
Proxmox nodes, the PDU, the UPSes and the Podman host; and the DX010's
bridge on the switch itself, whose `/host/disunified/config.yaml` lists
that one device. Both are the same binary reading the same file format
(`runConfig`: one goroutine per entry), so a child is one more entry in
**whichever file holds its parent**: the DX010's lane switches are declared
on the switch, the Arista's in the container. Nothing crosses a process
boundary; a child whose parent is not an entry of its own file fails to
open with a message that says so (§5.3).

Parents in this build: `sonic` (the DX010) and `arista-eos` (the 7160, whose
capture already holds a split cage); each implements the one contract of
§6, and the child driver is the same for both. Not in this build: a lane
switch that appears by itself when a cage splits (§12), MLAG, a bundle
across two cages (that is a UniFi aggregate of the two cages on the parent,
which exists).

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
child registers itself (§5.3). *(As built: main attaches it, §0.)*

One child per cage; two entries naming the same parent and cage are a
config error. A child whose parent entry is a `breakout` itself is a config
error.

## 3. What the controller sees

The snapshot the child reports each cycle (`Collect`), built from the
parent's last snapshot through the `LaneProvider` contract (§6), never from
a device read of its own:

| Port | What | Fields |
|---|---|---|
| 1..N (N = 4 for a 4-lane mode, 2 for a 2-lane mode) | the lanes, in SONiC interface order | every per-lane field the parent collects: `Up`, `Enabled`, `SpeedMbps`, `FullDuplex`, `MTU`, `Counters`, `Health` (link changes, FEC counters), `FEC`/`FECConfig`, `FECCapable`, `AutoNeg`/`AdminSpeed`, `VLAN`, `STPState`/`STPRole`/`STPPathCost`/`BPDUGuard`/`STPEdge`/`STPOperEdge`, `StormCtrl`, `Neighbor` (the host on that lane), `MACs` (the FDB entries on that lane), `LAG`/`LAGID`/`LACP` (§5.2), `MirrorFrom` (a lane mirrored onto this lane), `Media` SFP28 (QSFP28 for a 2x50G lane), `SpeedCaps` = the lane speeds the parent can set **per lane** (SONiC: the mode's speeds, `4x25G[10G]`: 10000, 25000; `2x50G`: 50000; EOS: the one speed the cage's lanes run, §7b -- the UI then offers no other, as the Flex XG's own note says its SFP28 ports cannot mix speeds), `Present` = the cage's module present, `Optic` = nil (the module sits in the cage: reported on port 5), `IfName` = the SONiC interface name (`Ethernet5`), `Interfaces` = that one name, `Lanes` 1 |
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
- `SpeedMbps` on a lane: a lane-speed change on that lane only, within
  the lane's `SpeedCaps`; anything else is refused with a note -- a lane
  never splits or joins the cage, and on a parent whose lanes share one
  speed (EOS, §7b) a lane speed is the cage's business on the parent. 0
  (auto) = what the lane runs.
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
is `PortChannel202`. *(As built: `lag` is the number the push names, `lag=1`
for the first aggregate whatever its members, so the captured push makes
`PortChannel201`, §0.)* The parent's own aggregates are `PortChannel<lag>`
with `lag` a front-panel index, never ≥ 100, so the two never collide and
either side tells the other's by the id alone:

- the lane plan reads a lane in `PortChannel<n>` with `100*cage < n <
  100*cage+100` as `LAGID = n - 100*cage`; a lane in any other PortChannel
  (hand-made, or the parent's) reports `LAG` = its name, `LAGID` 0, and the
  lane plan leaves it alone with a note;
- the parent's cage plan never moves a lane in a PortChannel with id ≥ 100
  (`lagLeaves`), deletes one only when it is empty, the driver's own and in
  the range of a cage with no lane switch attached (§7c), and refuses to put a child cage into one of
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

*(As built: STP port settings and the switch-wide port features stay the
cage's, and a cage speed only ever picks the mode, §0's table.)*

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
(members out, mirror session removed, the ACL tables rebound without the
PortChannel, PortChannel deleted -- as built, the rebind before the delete:
sonic.md §11.25), then the breakout. Today a breakout is refused for a cage in an
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
module, `SpeedCaps` unchanged. Its media is the cage's own (QSFP28), not
its lanes': the join is a forced 100G or 40G on this port, and the
controller takes the FEC the drawer sends with it only from a QSFP28
(`docs/breakout-lag.md` §7, 2026-11-16).

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
  `LastSeen` = the collect time, the learn time the loop's tracker's),
  `SpeedMbps` = `max(SpeedCaps)`.
  `LocalChassisID()` = eth0's MAC as `LLDP_LOC_CHASSIS` carries it
  (`collect.py` already prints it; `MgmtMAC`).
- **l2d (RSTP)**: the lane plan's `stpOps` write per interface as today;
  l2d's config is per port and the lanes are ports to it. Nothing new.
- **Hardware**: FEC codeword counters, link changes and the FDB are per
  interface already (`hardware.go`, `lanePort`); the module (`xcvr` keyed
  by the first interface) goes to port 5.
- `collect.py` needs no change: every table it prints is per interface.
- **The VLANs' MTU (2026-11-15).** The lane plan ran no `vlanMTUs`, and
  the cage plan's list was the device's own. So a VLAN only the child
  listed kept its `Vlan<id>` netdev at vlanmgrd's 9100 (sonic.md §12), and
  neither half followed a jumbo flip. Now each VLAN takes the lowest MTU of
  the pushes that list it: the device's, and each attached child's
  (`Collector.mtuWant`: each plan's last applied port MTU). Both plans
  compute the same value. The lane plan writes only the VLANs the device's
  push does not list, never the native stand-in. A new lane VLAN is made at
  the child's MTU. Tests: `TestLaneOnlyVLANMTU` and
  `TestReplayLaneSwitchVLANMTU`, over the 4x25G capture with vlanmgrd's
  netdev modelled (`FixtureRunner.VLANNetdevs`). The full write-up is
  sonic.md §11.29.4.

## 7b. Arista implementation notes (`internal/drivers/arista-eos`)

**As built (2026-11-15).** `lanes.go` and the scoped `plan()` in
`apply.go`; tests on captures of the 7160's empty QSFP test cage split on
4.26.14M (`docs/fixtures/arista-eos-4.26.14M/split-cage54*/`, below) and
on the cage 50 split of the 2026-09-19 capture. Where it departs from the
spec of 2026-11-02 (kept below, marked):

- **Channel groups and sessions: `1000 + 10*cage + lag`**, not
  `100*cage+lag`. EOS 4.26.14M takes `channel-group <1-2000>` (live
  2026-11-15 in an aborted session: 2000 accepted, 2001 and 5402 "Invalid
  input"), so the spec's numbers for the Arista's QSFP cages 49-54
  (4901..5499) cannot be written. Cage C's lane groups are 1001+10C..
  1009+10C (cage 54: 1541..1549), cage 1..99 (`AttachLaneSwitch` refuses
  the rest), a lane switch's aggregate ids 1..9 (more is refused with a
  note). A lane mirror is `monitor session <1000+10*cage+lane>` (the
  destination lane; EOS names sessions, "1542" was accepted). The
  device's own groups and sessions are numbered by a port index, under
  1000, so either side tells the other's by the number: the fold never
  reports a group of 1000 or more as its own aggregate (`LAGID` 0, the name
  kept) and reads only sessions under 1000 as its own mirrors; the lanes
  only sessions of 1000..1999.
- **Lanes beside the fold, by running the same readings twice.** Every
  per-interface parser takes an interface key (`ifKey`): the fold's is
  `parseEthName` (a port per slot), the lanes' gives each lane of a split
  cage a port of its own (`buildLanes`), renumbered 1..N in EOS lane order.
  So a lane has everything the fold reads per interface -- state, speed,
  counters, link drops and held STP changes, LLDP neighbour, LACP and its
  channel group, VLANs, storm control and its drops, errdisable, FEC and
  its codeword counters (the same `phy detail` replies, no extra command),
  STP state and role, MACs, the guard's rogue-DHCP drops -- and nothing
  is parsed twice in two ways. `Optic` nil (the module is the cage's).
  `SpeedCaps` is the one speed the cage's lanes run (`AdminSpeed`, 25000
  on the captures); media SFP28 below 50G, QSFP28 at 50G.
- **The owned cage keeps its STP port settings**, as on sonic: BPDU guard
  and the edge (`portfast` + `bpdufilter`) stay the cage's on every lane.
  The spec had the cage plan skip them; but the lane switch claims none of
  them (breakout §0), so the controller pushes it none, and skipping them on
  the cage would leave nobody setting them. The lane plan writes neither.
  The cage plan keeps the admin state (every lane) and the speed (split,
  lane speed, join); description, VLANs, FEC, storm control, the device's
  own LAG and mirror are skipped with a note (logged once), and
  `PlanPorts` counts none of them. A disabled cage holds every lane down
  (`LaneView.CageDisabled`; the lane plan never brings one up).
- **The join** (a cage speed of 40G or more on an owned split cage) puts
  `no monitor session` for each lane session the lanes report and `no
  channel-group` on each lane in a lane group ahead of the speed, in the
  same batch, and the cage's lanes are dropped from the view until the next
  collect. The emptied Port-Channels go in the same batch, after their
  members (`no interface Port-Channel<n>`: EOS keeps an emptied one;
  since 2026-11-15, df0f3bd).
- **The lane plan** is `plan()` with `planScope{cage}` over the cage's
  lanes: state, name, VLANs (a lane in an aggregate takes them from the
  Port-Channel's, `Port-Channel<laneBase+lag>`, as the device's own),
  `channel-group <laneBase+lag> mode active` / `no channel-group`, FEC,
  storm control, a lane-to-lane mirror; no speed: a lane speed other than
  the one the lanes run is refused with a note naming the device's port.
  A lane in a channel group the scope does not own (another cage's range,
  the device's own, one by hand) is left in it, aggregate and VLANs, with
  a note; so is a cage in a lane group with no child attached (a lane
  switch's group outlives its config entry while it has members; an empty
  one goes, §7c). After a batch the lanes and
  the Port-Channels written are marked, so a second apply before the next
  collect writes nothing.
- **One write at a time**: `lockApply` (a one-slot semaphore that gives up
  with the caller's context) around `Collect`, `ApplyPorts`, `ApplyLanes`,
  `EnsureVLANs`, `ApplyDevice`, `ApplyAddress`, `InstallSSHKeys`,
  `CyclePort` and `ApplyFanMode`.
- `LocalChassisID()` is `show lldp local-info`'s `chassisId`, now a start-up
  command; `LaneAlias` is `""` (EOS has no faceplate alias). The `Mode` is
  "<lanes>x<lane speed>G" ("4x25G").
- The child's cable port (`breakout/snapshot.go`) now adds the lanes'
  drops rather than their link transitions (each up lane's rise counted
  once more for every lane, the same error as the Arista's own fold had,
  `docs/drivers/arista-eos.md` §4).

**Captures** (2026-11-15, the empty test cage of `local-information/site.md`,
each split in a `commit timer` session that held the deployed bridge off
and was aborted after; the switch left as found): `split-cage54/` (every
start-up and poll command with the cage at 4x25G), `split-cage54-lag/`
(lanes 3-4 in `Port-Channel1541`, session 1542 mirroring lane 1 onto lane
2), `split-cage54-flexxg/` (the batch the Flex XG aggregate push produces,
run on the switch: lanes 2-4 in 1541 with RS, the Port-Channel Allow All),
`split-cage54-diverged/` (the first try, whose `interface Ethernet54/1-4`
range stopped after lane 1 at the "transceiver not present" warning:
lanes 2-4 at 10G, errdisabled). The join's commands were checked in a
session on top of a split with a lane aggregate and a lane mirror (`no
monitor session 1543`, `no channel-group`, `speed forced 100gfull`:
accepted, aborted). **What the test cage could not show:** it has no
module and no link, so no lane came up, no LACP partner, LLDP neighbour or
MAC was seen on a lane, no FEC was negotiated, and no traffic crossed a
lane mirror; whether the 7160 runs a lane mirror session beside the
device's own (it claims one) is not known.

*The spec of 2026-11-02, as written:*

The 7160's capture (`docs/fixtures/arista-eos-4.26.14M/`) has cage 50 split
to 4x25G (`Ethernet50/1..4`), so every lane test runs on real data.

- **Per-lane ports beside the fold.** `portsFromInterfaces` builds a
  `lanePort` per `Ethernet<slot>/<lane>` and folds it into its `slotAcc`;
  keep the lanes (`Index` = the lane number from `parseEthName`,
  `Interfaces` = the one name) on the folded port's side and in the
  `Collector` with `last`. Everything the second wave attaches is per
  interface already and must land on the lanes as well as on the fold:
  LLDP neighbours, LACP, FEC and PCS counters, counters, the MAC table,
  errdisable, STP state. For a child cage the fold carries no neighbour
  and no LAG/LACP of its own (§5.4).
- **Lane speeds are a cage property on EOS**: a lane-speed change must
  reach every lane or the others errdisable "speed-misconfigured". So
  `Lanes()` reports each lane's `SpeedCaps` as the one speed the lanes
  run, `ApplyLanes` refuses any other speed with a note, and the parent's
  cage speed stays the only speed control.
- **The lane plan**: the same per-port routines on the lane; no
  `speedCommands`. LAG: `channel-group <100*slot+lag> mode active` *(as
  built: `1000+10*slot+lag`, above)*; the Port-Channel gets the members'
  VLAN intent; mirror `monitor session <100*slot+lane>` *(as built:
  `1000+10*slot+lane`)*. `write memory` ends the batch; `EnsureVLANs`
  first.
- **Reading a lane's LAG**: the §5.2 rule on the lane's channel group; any
  other group is reported by name with `LAGID` 0 and left alone with a
  note. The cage plan never touches a lane switch's group, refuses
  `lagCommands` on a child cage, refuses a mirror whose source or
  destination is a child cage, and before a join removes the lane groups
  and sessions in the same batch.
- **Child cages in the cage plan**: shutdown and speed only; VLANs,
  description, FEC, storm, BPDU guard, STP edge, LAG and mirror skipped
  *(as built: BPDU guard and STP edge kept, above)*.
- `LocalChassisID()` from `show lldp local-info`; `LaneAlias` `""`;
  `applyMu` across every write, as for SONiC.
- **No VLAN MTU (checked 2026-11-15).** An EOS VLAN has no MTU of its own,
  and the driver writes none at all (jumbo is logged, not applied:
  `arista-eos.md` §6). It makes no SVI either: its one address write is
  the management address move, on the interface it is reached on. So a VLAN only the child lists
  needs nothing here. The sonic parent's gap (§7) has no counterpart on
  EOS.

## 7c. A detached lane switch's aggregates (both parents, 2026-11-15)

A lane switch is *detached* when its config entry is removed and the bridge
restarts: `main` attaches only the entries it reads (`AttachLaneSwitch`
before the parent's loop starts), and nothing else detaches one. What it
leaves on the cage's lane range (`Port-Channel<1000+10*cage+1..9>` on EOS,
`PortChannel<100*cage+1..99>` on sonic) was nobody's: the parent's plan
owned only its own range, and the lane plan runs only while a lane switch is
attached. An aggregate with members stayed, noted, which is right; an empty
one stayed for good, which was not (the case: the lane switch's members
taken out by hand, a lane batch that failed half-way, SONiC's forced
breakout dropping a member silently, or EOS keeping the interface when its
last member leaves).

**The rule.** The contract stands: a parent never touches a PortChannel
outside its own range while a child is attached, and an attached cage's
lanes are the child's. For a cage with **no lane switch attached** (split or
joined), the parent's own plan takes the cage's lane range for one thing,
removal:

- **empty and the driver's own** -- removed, as an orphan of the plan's own
  range is: on EOS a Port-Channel carrying only the driver's lines (its
  VLANs, its own ACL: `driverChannelLine`); on sonic one with the LACP
  settings only the driver's `config portchannel add` writes (`ownOrphan`),
  its VLANs stripped and ACL tables unbound first, as `lagJoins` does for
  its own. It changes no port, so the first-push hold does not wait on it;
- **with members** -- left as it is, with a note naming them ("... no lane
  switch is attached to port N: left as it is while it has members ...").
  It still bundles lanes, maybe toward an LACP partner; the controller's
  push for the cage says nothing about lane aggregates, so the parent has
  no intent to replace it with; and a lane switch attached again takes it
  back as it was. The operator takes the members out (or re-attaches and
  removes the aggregate from the UI), and the next plan removes it;
- **with a line the driver never writes** (EOS: a description, an MTU,
  another ACL; sonic: SONiC's default LACP settings) -- left as made by
  hand, noted, as anywhere.

Why the parent: an empty PortChannel has no traffic and no intent behind
it, and it is in a range only the driver's lane switches write, so
removing it loses nothing, and no other plan ever would. While a lane switch is attached the
range stays the child's even with the cage joined (a join made by the
parent's plan dissolves the child's aggregates itself).

Membership is read from every lane, not the fold: the fold carries one
channel group per cage, and a split cage's lanes can be in two
(`detachedLaneChannels`, `internal/drivers/arista-eos/portchannel.go`;
sonic reads `PORTCHANNEL_MEMBER`, `pl.sim.lags`, `detachedLane` in
`control.go`).

**Tests.** Arista, on captures of the empty test cage (2026-11-15, one
sanitizer run; `docs/fixtures/arista-eos-4.26.14M/lane-po-*`): the cage split
to 4x25G with lanes 3-4 put in `Port-Channel1541` and taken out again
(`lane-po-empty/`: the Port-Channel left with the lanes' inherited
`switchport mode trunk`; its `show-running-config-handmade.json` the same
with a description), after `no interface Port-Channel1541`
(`lane-po-removed/`), and the cage joined back with an empty
`Port-Channel1541` (`lane-po-joined/`). Each state was made with plain
`configure` under a hold session, aborted after; the switch was left as
found (running and startup unchanged, the deployed bridge wrote nothing in
the window). `lanes_detached_test.go`: the removal from a split and from a
joined cage, once, read back as the capture after it; an attached lane
switch keeps its range (its own lane plan removes the Port-Channel); a
hand-made one and one with members stay, noted. Sonic, on the DX010's split
capture: a lane switch makes `PortChannel201`, is detached, the members
are taken out by hand (`config portchannel member del`, the fixture
runner's model of it), and the device's plan removes it once
(`lanes_test.go`).

## 8. Loop, main, config, presentation

- `cmd/disunified/main.go`: open a driver when `driver` is set even with
  no `url`/`ssh` (the breakout driver, and `stub` with a file, both have
  nothing to dial); `devicemodel.Attach(o.name, sw)` after
  `openAndStart`, `Detach` on exit; `o.ip` from `Addresses[0]` already
  covers the child; `-list-drivers` shows it. Nothing here is
  deployment-specific: the container and the on-switch package run the
  same `runConfig` over their own `config.yaml`, and a child is an entry
  in the file that holds its parent (§1). `-collect-once` with a config
  file opens devices one after another, so a child there reports "parent
  not opened" -- document it; it is a probe, not the service.
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
- `internal/drivers/arista-eos`: `Lanes(50)` from the real capture (four
  lanes with their own neighbours, LACP, FEC counters, MACs); the lane
  scope on the fixture transport -- state, name, VLANs, FEC, storm, BPDU
  guard, STP edge, the refused lane speed, a lane-to-lane mirror, the
  aggregate of lanes 2-4 (`channel-group 5002 mode active` on each, the
  `Port-Channel5002` VLAN intent, `write memory`; *as built 1541 on cage
  54, §7b*), its dissolution,
  idempotence; the cage plan with a child attached (shutdown and speed
  only, the note, channel groups ≥ 100 untouched, the join issuing `no
  channel-group` first, `PlanPorts` counting none of the skipped
  settings); the folded cage's report; `LocalChassisID` from the
  local-info fixture.
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
  runner, and the same push onto a parent `arista-eos` collector on the
  fixture transport (cage 50) -- the expected batch on the parent, the reply log, a zero-change
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

## 11b. Live verification on the 7160 (when it is plugged in again)

The same steps as §11 on the Arista's down QSFP test cage
(`local-information/site.md`) with a breakout DAC to a bonded host, the
child declared in the container's `config.yaml` and the container
redeployed (ships `HEAD`; bounces every bridged device for about a minute).
Differences to record: the lane speed refused on the child and set on the
parent instead; `show port-channel summary` and `show lacp neighbor` for
`Port-Channel<1000+10*slot+lag>` (§7b as built; cage 54's first is 1541);
the join issuing `no monitor session` and `no channel-group` first. The
7160 has been plugged in again since 2026-11-12, and the lane batches have
been run on its empty test cage by hand (§7b); what waits is the child
entry, its adoption (Clint's) and a far end. The entry, in the container's
`config.yaml`:

```yaml
  - name: leaf-1-cage54
    driver: breakout
    options:
      parent: leaf-1       # the switch's own entry
      cage: "54"
```

## 12. Later

- **Automatic lane switches**: a `sonic` option (`lane_switches: auto`)
  that creates a child for every split cage and retires it on a join,
  which needs the loop to add and remove devices at run time (today the
  device list is fixed at start, `runConfig`), and the bridge to forget a
  retired child in the controller (`cmd/sitemgr delete-device`, which the
  `unifiapi` client can send). The declared form is the first step
  because it needs neither.
- A `PortCycler` on the child (bounce one lane).
- `docs/breakout-lag.md` §4-5 (shape 1) stay as the record of the road
  not taken; nothing there is built.

## 13. Open questions for the build (decide, note the answer here)

*(Answered 2026-11-14 as below: native speed, accept and wait, honoured.)*

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

## 14. Still open after the build (2026-11-14)

1. **The stored speed follows a forced override** (§6.6.3 of the note:
   50000 sent, 25000 stored on lanes the aggregate push had forced to 25G).
   Not fixed in code: the cause is the controller's own record, and the
   seed (`unifiapi.SeedPortConfig`) writes VLANs and the admin state only.
   Seeding a lane's speed would *create* the forced override the quirk
   comes from, and the override's speed keys have never been captured (no
   `port_overrides` record with a speed in `docs/fixtures`). The lane
   switch reports what each lane runs, takes auto as "what it runs", and
   refuses a forced speed the lane does not run with a note naming it; so
   after a re-split (4x25G to 2x50G) with the aggregate's forced 25G still
   in the overrides, the push asks 25G of a 50G lane, nothing is written,
   the note says why, and the record keeps showing 25G until the operator
   sets the lane to auto. What a fix needs: a capture of the child's
   `rest/device` `port_overrides` after the aggregate (the speed keys), then
   a rule (clear a lane's forced speed when the mode changes under it).
2. **The record disconnected about 90 s after the aggregate push** (seen
   once with the stub). The reply log of that run (kept site-side) shows
   the stub informing every 30 s and every inform answered HTTP 200 through
   the window, while the controller's record says `disconnection_reason:
   MISSED_INFORM, last_seen '<the push's inform>', considered_lost
   '<last_seen + 30 s>'` and reconnected at the next push -- a controller-side
   state after the push, not the bridge's cadence. Not chased further; the
   live run's step 4 watches for it (§15).
3. **A parent that is a real driver**: built and tested for `sonic` against
   the DX010's capture and for `arista-eos` against the 7160's (§7b, the
   lane batches also run on its empty test cage by hand), neither adopted
   live. A parent on an up cage with its own neighbour: on an owned cage the
   child's LLDP entry replaces the hosts' (they are the lanes', on the
   child), by design; `children:` keeps its old rule (a port's own
   neighbour wins) for the experiment knob. On the Arista: no lane has had
   a link (the test cage is empty), and whether the 7160 runs a lane mirror
   beside the device's own session is not known.
4. **STP on a lane aggregate -- closed 2026-11-15** (sonic.md §11.25.1).
   `l2SetPorts` named the PortChannel of a cage's *first* interface only,
   so a lane aggregate that leaves lane 1 out (the captured 2-4) ran at
   mstpd's defaults. It now names every bridge port a cage's interfaces
   make -- each interface, or its PortChannel -- with the cage's settings,
   and `ApplyLanes` re-derives them after a lane batch. **Whose intent:
   the parent's cage**, because the lane switch claims no STP (§0), so its
   push carries none and the UI offers none on it; the cage's BPDU guard or
   loop protection covers every lane, aggregated or not. Verified live on
   the DX010's cage 29 (a probe built from the branch; mstpd and l2d's
   STATE_DB showed `PortChannel2901` with admin edge and BPDU guard, l2d's
   loop guard listed it) and by `TestLaneAggregateTakesTheCageSTP`. Moving
   the settings to the child is still item 6.
5. **`config portchannel del` of a PortChannel an ACL table binds -- closed
   2026-11-15** (sonic.md §11.25.2). Live: the delete succeeds but leaves
   the table naming the PortChannel, orchagent keeps the LAG (`Failed to
   remove ref count 1 LAG`), and **the join's breakout is refused** by
   sonic-yang (`Invalid value "PortChannel2901" in "ports" element`): the
   join as built would have failed every cycle. Fixed: the plan rebinds the
   driver's tables without the PortChannel before deleting it (in
   `dissolveLanes` and in `lagJoins`' emptied-PortChannel path), verified
   live in that order on the same cage. The FixtureRunner models the
   delete and the refusal from the capture
   (`portchannel-del-acl-20261115.txt`); `TestLaneJoinUnbindsTheACLTables`.
   Still unrun: the join through a deployed bridge with a lane switch
   attached (the hand run replayed its command sequence), part of §15.
6. The lane switch claims no STP, isolation, port security, egress limit
   or LLDP-MED (§0's table); moving them to the child needs the switch-wide
   computations to take both loops' intent.
7. Automatic lane switches (§12) and the bridge forgetting a retired child.

## 15. The first live adoption (DX010, cage 2)

Clint's to start; nothing here has been run. The DX010's bridge runs on the
switch (`docs/drivers/sonic-on-switch.md`), so the lane switch is one more
entry in `/host/disunified/config.yaml`, beside the switch's own, and the
package is rebuilt from a commit that has this change.

1. **Build and install** the package from the commit (`docs/install.md` §7;
   the update steps are in `local-information/site.md`). Before the
   restart, add to `/host/disunified/config.yaml` (keep a backup):

   ```yaml
     - name: dx010-cage2
       driver: breakout
       options:
         parent: dx010        # the switch's own entry
         cage: "2"
   ```

   Expect in the journal: `lane switch dx010-cage2 attached to port 2 as
   <MAC>`, then for the child `start attempt N failed ... cage 2 of ... is
   not split` every retry while the cage is whole. The DX010's own next
   reconcile writes nothing (`sonic apply` absent).
2. **Split cage 2 from the DX010's port drawer** (25G). The parent's log
   shows the breakout; within one retry the child logs `breakout: dx010
   cage 2 (4x25G[10G]) as <MAC>` and informs as `USFXG`, PENDING. Adopt it
   by exact MAC (`cmd/devmgr adopt`, never Adopt All). Expect CONNECTED
   within ~10 s and placement under the DX010's port 2 within a minute
   (`uplink_mac` the DX010, `uplink_remote_port` 2); the DX010's record
   gains the child in `downlink_lldp_macs`/`downlink_table`. With an
   `api_url` the switch can reach, the child seeds the controller from the
   lanes and its first push applies with nothing changed; without one the
   first push is held (`HOLDING the first push after adoption ... ports
   [1 2 3 4]`: the controller's default for an unseeded port is Allow All,
   the lanes are native only) until the lanes are set in the UI. Save both records (scrubbed) and the
   child's first inform.
3. **UI walk on the child**: four SFP28 lanes at 25 GbE (no link on the
   empty cage), port 5 QSFP28 100 GbE marked uplink, the module (if any) on
   port 5, names `Eth2/1`-`Eth2/4`.
4. **A lane setting**: rename lane 1, then back: `hset 'PORT|Ethernet4'
   description` in the parent's journal (`sonic apply (cage 2 lanes, ...)`),
   then a zero-change cycle. Watch the child's record across the push for
   the disconnect of §14.2.
5. **A VLAN only the child carries, and its MTU** (sonic.md §11.29.4,
   never run live). On the child, tag lane 1 with a network no DX010 port
   carries.
   - Expect in the parent's journal `sonic: creating VLANs [<id>]` with
     `hset 'VLAN|Vlan<id>' mtu 1500`.
   - Within one cycle of either loop, expect `ip link set dev Vlan<id> mtu
     1500`, then a zero-change cycle.
   - Read both halves on the switch: `sonic-db-cli CONFIG_DB hget
     'VLAN|Vlan<id>' mtu` and `ip -j link show Vlan<id>`.
   - Turn Jumbo Frames on for the child alone, if the controller offers it
     per device. Expect one lane batch with the lanes at 9216, `VLAN|Vlan<id>`
     at 9216 and its netdev at 9100. `Vlan4094` stays at the DX010's MTU.
     If jumbo is the site's only, both pushes flip, and `Vlan4094` moves at
     the DX010's next plan.
   - Turn jumbo back off. Both halves go to 1500.
   - Untag the network. The DX010's next plan deletes `Vlan<id>` (§11.29).
   - Record the journal lines and both reads.
6. **The aggregate**: on the child, mark lanes 2-4 Aggregate. Expect
   `PortChannel201` created on Ethernet5-7 in one batch (the FEC note for
   `cl-91`), the next cycle writing nothing, the child's ports 2-4 reporting
   it. Without a cabled far side the members stay down; with a breakout DAC
   to a bonded host (the real test) `teamdctl PortChannel201 state` shows
   the partner and the child's inform `lacp_state`. Remove the aggregate in
   the UI: the PortChannel goes, the lanes back in their VLANs, zero-change
   cycle.
7. **The join**: set the DX010's port 2 to 100G (or auto with a 100G
   module) with an aggregate present: one batch dissolves `PortChannel201`
   then breaks out to `1x100G[40G]` (check the ACL tables stay `Active`,
   §14.5). The child stops answering (`not split`), the controller shows it
   disconnected.
8. **Undo**: forget the child (`cmd/sitemgr delete-device`, exact MAC),
   remove its entry from `/host/disunified/config.yaml`, restart the
   service; set port 2 back to what it was. A forgotten lane switch that is
   split again comes back pending under the same MAC.

What could go wrong and how to see it: the child never placed (check the
DX010's `payload-last.json` for the MAC in port 2's `mac_table` and
`lldp_table`); a write on the wrong cage (the journal names `cage 2 lanes`
and Ethernet4-7 only); the parent's own ports touched by the child's push
(they are not in the lane scope; the replay asserts it).

