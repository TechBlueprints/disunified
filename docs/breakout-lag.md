# A LAG on the far side of a breakout cable (design note, 2026-11-02)

Status: **two shapes written up, neither chosen, nothing built.** Clint's
question (2026-11-02): when the far end of a breakout cable bundles its
lanes with LACP (say three of a 4x25G cable into one bond), can the bridge
notice, bundle the same lanes on our side, and show UniFi the result --
with the least intrusion and still looking like UniFi?

Two shapes came out of the discussion, documented in full so either can be
picked up later:

- **Shape 1, the summed-speed port (§4):** the cage stays one UniFi port;
  the bridge detects LACP on the lanes itself, bundles them, and reports
  the port at the bundle's speed. Least code, nothing new in the UI.
- **Shape 2, the lane switch (§6):** a split cage is presented as its own
  small UniFi switch (a 5-port profile: the cable as its uplink, a port
  per lane) placed under the cage. The operator marks the aggregate in the
  UI as on any UniFi switch; nothing is detected. More code, more UniFi.

§2 (detection) and §3 (what the controller can show) are shared ground.

## 1. Why UniFi cannot express it

- UniFi has no breakout. A cage is one port; the bridge folds a split
  cage's lanes into that one port (any lane up = up, counters summed,
  `speed` = the lane speed, `docs/drivers/sonic.md` §7.5, the Arista the
  same). The controller never learns there are lanes.
- A UniFi aggregate is two or more **consecutive whole ports**
  (`switch.port.N.opmode=aggregate` + `lag`; the UI refuses one port). A
  sub-set of one port's lanes has no spelling in `system_cfg`.
- So whatever is done, the controller cannot be the source of the
  decision. This is the kind of thing `CLAUDE.md` §3 allows a per-device
  setting for: "what UniFi cannot express (breakout handling ...)".

What the bridge already has that this builds on:

- The SONiC and Arista LAG paths bundle **every lane of a cage** when the
  cage is a member of a UniFi aggregate (`PortDesired.LAG`), and the
  PortChannel carries the cage's VLAN/MTU/storm intent. Only "every lane"
  and "a UniFi id" are missing for this case.
- The collector reads teamd's per-member LACP state (`LACP.Active`,
  `PartnerSystem`, §11.4) for any PortChannel the lane is in.
- The reconcile **dissolves any PortChannel UniFi did not ask for**
  (`lagLeaves`: a lane in a PortChannel with no matching `d.LAG` leaves
  it). A bundle made for this feature must be exempt or it is undone on
  the next cycle.
- The module table (§10.9) already keeps a split made for a 25G-only
  breakout cable on "auto".

## 2. Detecting that the far side is LACP

Three signals, from most to least direct:

1. **LACPDUs on the lane.** A bond in LACP mode sends an LACPDU on each of
   its links every 1 s (fast) or 30 s (slow) whether or not anyone answers.
   The PDU carries the far side's **actor system id and actor key**: lanes
   with the same pair belong to the same bond. On SONiC the default COPP
   traps LACP (`stp,lacp,eapol`) to the CPU on every port, so the PDU
   reaches the lane's host netdev: `tcpdump -i Ethernet5 -c 1 -w - ether
   dst 01:80:c2:00:00:02` for up to ~35 s (slow rate) answers it, one
   lane at a time, only for a lane that is up, split, and in no
   PortChannel. Needs no far-side cooperation beyond LACP itself.
   **To verify on the DX010:** that the stock COPP really delivers the PDU
   to the netdev of a port in no LAG (it should), and that `tcpdump` is on
   the host image (it is in the SONiC host rootfs).
2. **LLDP's IEEE 802.3 link-aggregation TLV** ("Port is aggregated,
   aggregation id N"). lldpd sends it for a bond slave; a Proxmox node or
   any lldpd host says so on every lane. It is **not** in APPL_DB's
   `LLDP_ENTRY_TABLE` (lldp_syncd stores the MIB's remote table only), so
   the SONiC collector would read it from `lldpctl -f json` in its poll
   exec. EOS parses the TLV (`show lldp local-info` reports its own
   `linkAggregationStatus`), but the neighbour-detail capture
   (`eos-4.26.14M/show-lldp-neighbors-detail.json`) carries no aggregation
   field, so on the Arista it would need `show lldp neighbors ... detail`
   text or is not available. Good as corroboration and for the
   aggregation id; not every host sends it (Windows, bare lldpad defaults).
3. **The partner's state once bundled.** After the lanes are in a
   PortChannel, teamd's `partner_lacpdu_info` is the steady-state truth the
   collector already reads: bundled or not, partner system id. This is
   what keeps the bundle honest afterwards; it cannot discover one.

Shape 1 uses 1 for discovery, 3 for steady state, 2 only logged (and
used for the Arista if EOS exposes it). Never discover by "try a
PortChannel and see": a lane put in an LACP PortChannel whose peer is not
LACP never comes up, which breaks a plain link while probing.

## 3. Options for what UniFi sees

| | What the controller gets | Pros | Cons |
|---|---|---|---|
| **A. One port, summed speed** (Clint's proposal) | the folded cage as today, `speed` = the sum of the **bundled** lanes' speed (75000 for 3x25G), `full_duplex`, counters summed as now | nothing new on the wire; the Insights/throughput numbers are right for the link; the port stays a normal UniFi port (VLANs, name, state, mirror apply to the PortChannel) | UniFi shows no aggregate: no LACP badge, no member list, no partner. `75000` is not a speed the UI has a label for -- **must be checked** (the UI may print "75 Gbps", "75000" or nothing; `speed_caps` is for the picker and need not list it). The controller's port record and the UI port drawer show one link; a lane dropping out of the bundle shows only as the speed falling to 50000 |
| **B. A plus the ECS's aggregate fields on the cage** | A, plus `lag_member: true`, `aggregate_num_ports: 1`, `lacp_state: [{active, member_port: <cage idx>, speed: 25000} x3]`, `partner_system_id` -- the keys a real SONiC switch sends on an aggregate's head (`inform-ecs-aggregation-uswf066.json`) | might light the UI's LAG presentation for free | the UI draws aggregates from the controller's own `port_overrides` (`aggregate_num_ports` -> `lag_idx`, `aggregate_members`), not from the inform; `member_port` must be a UniFi port index, which a lane is not. Likely ignored, possibly confusing. Cheap to try (reported keys only, nothing to undo): one capture decides |
| **C. Lanes as UniFi ports** (unfold a split cage into N ports so the operator marks them Aggregate in the UI, the real UniFi way) | a port per lane | UniFi expresses it itself; the controller shows a true aggregate | the profile fixes the port count and the picture; a DX010 would need 128 port slots, there is no such profile, and ports beyond the profile's count have no known behaviour. Port indices would shift with every split. Not viable |
| **D. Config-file override only** (`options.lane_lags`) | as A | explicit; no sniffing; the one knob for a far side that sends no usable LACPDU (none exists) | the operator has to know the lane set and keep it current; against "nearly everything from the controller" -- but this is exactly the exception the rule names |
| **E. The split cage as its own UniFi switch** (§6) | a second adopted device: a 5-port profile with the cable as its uplink port and one port per lane, placed under the cage by the parent's own inform | the operator marks lanes Aggregate in the UI and the existing LAG path builds it; no detection at all; every lane visible (neighbour, VLANs, counters, FEC, STP, LACP partner); the topology shows the hosts under the lanes | a device the LAN does not have (its own MAC, adoption, state, naming); a parent/child concept the code lacks; the cage's own port settings become dead; dynamic appearance on split is a later step |

A and D are not alternatives: D is the manual spelling of the same
bundle A reports. Together with discovery (§2) they are shape 1 (§4). C
is not viable. E is shape 2 (§6), and makes A, B, D and the detection
unnecessary: the aggregate is UniFi's own.

## 4. Shape 1: the summed-speed port

**Auto-detect, report as A, override to pin or forbid.** Smallest change
that is still UniFi-like: the controller keeps one port per cage, every
control it has on that port still applies (to the PortChannel now), and
the thing UniFi could never express is decided by the wire, not by hand.

Driver (SONiC first; the Arista's detection is open, §2):

1. **Discovery**, driver-owned like the module FEC and the §10.9 speed
   rule (so `PlanPorts` plans without it and the first-push hold never
   waits on it): on each collect, for every split cage, each lane that is
   up, in no PortChannel and not seen before is sniffed once for an
   LACPDU (budgeted; a lane with none is re-tried only on link change).
   Lanes sharing (actor system, actor key) form a group. One group per
   cage; a second group on the same cage is logged and left as plain
   lanes.
2. **Bundle**: `config portchannel add PortChannel<1000+cage>` (an id
   range the UniFi path never uses, so `lagLeaves` and the LAG code can
   tell the two apart by name alone; `--fast-rate true --min-links 1` as
   the UniFi path), MTU set from the cage's as the existing code does,
   the lanes' VLAN membership moved onto the PortChannel exactly as a
   UniFi aggregate's is, the DUI ACL tables rebound (the existing
   PortChannel path). Unbundled lanes keep the cage's intent as plain
   ports. The cage's whole intent (VLANs, MTU, storm, FEC, state, name)
   is applied to the PortChannel + the loose lanes by the same plan.
3. **Report**: the folded cage's `speed` = sum of the lanes teamd reports
   bundled (`LACP.Active`), `full_duplex` true; `LACP` on the cage with
   the partner system id (already there for a UniFi aggregate). Logged
   once per change: "cage 2: 3 lanes bundled with <partner>, 75G".
   Option B's keys only if the capture in §5 shows they help.
4. **Undo**: sticky. The bundle stays while the cage stays split; teamd's
   partner going away (a far-side reboot) only drops the reported speed.
   It is dissolved when (a) UniFi joins the cage (a manual 100G or the
   module rule): the auto bundle is removed **before** the breakout
   instead of the breakout being refused "for a cage in an aggregate";
   (b) UniFi puts the cage in its own aggregate: refused with a log (two
   LACP partners on one cage cannot both be right) unless the override
   says otherwise; (c) the override forbids it; (d) a lane has had no
   partner for a long time (an hour) **and** an LACPDU sniff on it finds
   nothing -- the far side was reconfigured to plain ports. The bundle is
   the switch's saved config, so a bridge restart finds it and keeps it
   (by its name).
5. **Override** `options.lane_lags`, per cage: `auto` (default), `off`,
   or an explicit lane list (`[1,2,3]`) that is bundled without sniffing
   and never dissolved by (d). That is Clint's "manual config file
   override", kept as the pin/escape hatch rather than the only path.

Not in scope: a bundle across two cages' lanes (that is a UniFi
aggregate of the two cages, which exists), and MLAG.

## 5. Shape 1: what to verify first, before any code

Each is a capture or a reported-value experiment, nothing written to a
switch:

1. **What the UI makes of a non-standard `speed`.** Report 75000 (and
   50000, which is a real speed) on an idle port of the DX010 for one
   inform; read the port drawer, the Ports list and the device overview.
   If "75" is unprintable, A falls back to reporting the lane speed and
   the bundle lives in the log and the LACP fields -- or 50/100 only.
   (Could also be answered from the UI bundle's speed-label table, as the
   UPS picture was.)
   **Answer (2026-11-02, Network 10.6.109, the stub of §6.7):** the
   controller stores `speed: 75000` as sent, and the Ports list prints
   **"75 GbE"**. Nothing else knows the speed: the text is grey (no legend
   colour), the port's tile in the picture is drawn like a disconnected
   port, the Link Speed filter files the port under **"40 GbE"**, and the
   drawer (auto-negotiation on, as reported) shows no speed or FEC row.
   50000 is a real UI speed (its own legend colour and label). So A can
   print the sum, but the picture and the filters misread it; reporting
   the lane speed, or 50/100 only, is the honest form.
2. **Option B's keys on a non-aggregate port**: one inform with
   `lag_member`, `aggregate_num_ports: 1`, `lacp_state`,
   `partner_system_id` on an idle port; what the controller stores
   (`rest/device`) and whether the UI changes. Decides B.
3. **An LACPDU reaches the netdev of a lane in no PortChannel** on the
   DX010 (COPP), and `tcpdump` is on the host. One command on the switch
   against a cage whose far side is a bond (or a lab host with a bond).
4. **The Arista**: whether `show lldp neighbors detail` (text) prints the
   802.3 aggregation TLV on 4.26.14M, and whether an LACPDU on a lane in
   no channel group can be seen at all (EOS punts them to its own LACP
   agent; `tcpdump -i et50_1` from bash may or may not). If neither, the
   Arista gets the override only.

Then the usual: the sniff and the bundle fixture-tested against the DX010
capture with an LACPDU capture added to `docs/fixtures/sonic-*`, the
replay of a split-then-bundle cycle, and the live run on a free cage with
a breakout DAC to a bonded host.

## 6. Shape 2: the lane switch

A split cage is presented to the controller as a second device: a small
UniFi switch whose uplink is the breakout cable and whose ports are the
lanes. The cage on the real switch carries that switch; the LAG is then
an ordinary UniFi aggregate on the lane switch's ports.

### 6.1 Why the controller will place it

Placement comes from the *upstream UniFi switch* reporting the device's
MAC on a port (`docs/topology-placement.md` §1; for switches the LLDP
chassis id adds to it). The UPS's synthetic MAC was never placed because
no real switch had seen it. Here the upstream switch is **our own
parent inform**: the DX010 lists the lane switch's locally-administered
MAC in the cage's MAC table and as the cage's LLDP neighbour (chassis id
= that MAC), and the controller places the lane switch on the cage. The
hosts behind the lanes are reported by the lane switch on its ports and
by the parent on the cage, as any cascade reports them; the controller
places clients at the deepest switch.

### 6.2 The profile

Five-port profiles in the catalogue (`docs/unifi-models.md`): **USFXG
(USW-Flex-XG, 4 x 10G RJ45 + 1 x 1G uplink)**, USF5P, USMINI, USMINI2,
USWED35 (5 x 1G/2.5G RJ45). The Flex XG is the natural pick: four fast
ports and one uplink is the breakout picture. The device's per-port
`media` and `speed_caps` replace the profile's icons and pickers
(`CLAUDE.md` §5), so the lanes show as SFP28 at 25G (or 50G) and the
uplink as QSFP28 at 100G; the port count, picture and default names come
from the profile. On a 2x50G split the two spare ports are reported empty
(not present), as an unpopulated cage is. The uplink is a **port in the
table marked `is_uplink`**, as a real switch reports its own (the ECS
inform has one), not the Arista's management-interface trick; which
index the Flex XG profile treats as its uplink (port 5 on the real unit)
is checked in §6.6.

### 6.3 Identity and reachability

- MAC: locally administered, derived from the parent's MAC and the cage
  index (bit 0x02 set in the first octet, the cage in the last), stable
  across restarts. Serial and `guid` derived the same way. A capture is
  the proof that the controller does not mind a device at a MAC it has
  seen only on one of its own adopted switches.
- `connect_request_ip`, `netmask`, `gateway_mac`: the parent's. The lane
  switch has no address of its own; whether two devices may report one
  address is open (§6.6). `uplink` is the uplink port's `if_table` name.
- State: `state/<name>/device.json` like every device; one adoption per
  lane switch. Adopting it deletes no client record (the MAC was never a
  client).

### 6.4 Config flow

- **Declared first, automatic later.** First step: the lane switch is a
  device in `config.yaml` (`driver: breakout`, `options: {parent:
  <device name>, cage: N}`), in the same process as its parent. A lane
  switch that appears by itself when a cage splits (and goes offline when
  it joins) is the UniFi-like version and waits for the loop to grow a
  dynamic device list.
- **The parent's half exists (2026-11-02):** a device's `children:` list
  (`[{port: N, mac: M, name: X}]`, `docs/install.md` §3) reports M in
  port N's MAC table (native VLAN) and the switch's, and as port N's LLDP
  neighbour (chassis id M, system name X, port id `eth0`, a bridge) while
  the port has none of its own; a child is never the parent's uplink
  (`Neighbor.Child`), and nothing is written to the device
  (`internal/informloop/children.go`). The lane-switch driver declares
  its cage's child through the same list.
- **Collect:** the child is a view over its parent's latest snapshot. The
  SONiC collector keeps the per-lane ports it folds today
  (`Port.Lanes`/`Port.Interfaces` become real `Port`s on the side); the
  child reports them as ports 1..N with every lane field: state, speed,
  counters, FEC, neighbour, MAC table, STP state, LACP member state. The
  uplink port reports the cable: up if any lane is up, 100G (or the
  cage's native speed), QSFP28, counters summed, the parent as its LLDP
  neighbour.
- **Apply:** the child's `switch.port.N.*` go to the lanes through the
  **parent driver's plan** (same SSH runner, same first-push hold, same
  DUI ACL rebind). `opmode=aggregate` + `lag` builds the PortChannel on
  the named lanes by the existing LAG path, with the PortChannel id
  offset per cage (`PortChannel<100*cage+lag>`) so the child's LAG 1 and
  the parent's LAG 1 never collide; min-links, fast rate, MTU and the VLAN
  move as for a UniFi aggregate. Jumbo, STP port settings, storm control
  and a mirror between two lanes apply to the lanes. Port settings on the
  uplink port: ignored with a log (a real switch's uplink is also mostly
  untouchable in the UI).
- **The child's switch-wide pushes** have nothing to apply to: STP mode
  and priority, IGMP, NTP, syslog, hostname, SSH keys, SNMP, time zone,
  IP Settings, DHCP guard, ACLs are ignored, each logged once (the parent
  applies the site's). `reboot` refused with a log; `set-locate` logged
  (the DX010's lanes have no LED the driver can drive; the parent's
  status LED would be misleading). Firmware upgrades emulated as on every
  device.
- **The cage on the parent** keeps exactly one control: its speed is the
  breakout control (25G or 10G → 4 lanes, 50G → 2, 100G/40G → join, §7.5
  of the SONiC doc; the module rule of §10.9 still applies), and
  disabling the cage shuts every lane. Its other settings (VLANs, STP,
  storm, mirror) are not applied and are logged once: the lanes take
  theirs from the lane switch. The cage reports 100G, QSFP28, the lane
  switch as its neighbour and MAC.
- **Split and join through UniFi:** a split makes the declared child
  start reporting (it reports nothing and does not inform while the cage
  is whole); a join takes it offline, and the controller shows it so
  until forgotten (`cmd/sitemgr delete-device`, which the bridge could do
  itself once the dynamic version exists).
- **The first push** is held as on every device: the child's adoption
  seeds the controller's port overrides from the lanes as they are.

### 6.5 What it costs

- A parent/child concept in the loop: a driver that reads another
  device's snapshot and writes through its plan; a registry lookup by
  name is enough while every device runs in one process (it does, §4 of
  `CLAUDE.md`).
- The SONiC collector keeping per-lane ports beside the fold; the Arista
  later, its fold being the same shape (`parse.go`, `slotAcc`).
- One more adopted device per split cage in the device list and the
  topology -- which is also the honest picture of a breakout.
- No detection code, no sticky-undo rules, no override: the aggregate is
  the operator's, as on a UniFi switch.

Roughly three to four times shape 1's work, and not a dead end: it is the
representation of breakout itself, LAG or not.

### 6.6 Shape 2: what to verify first, before any code

1. **Placement by our own inform.** A device with a synthetic MAC is
   placed on a cage when the parent's inform lists that MAC in the cage's
   MAC table / LLDP table. Cheapest proof: a stub device (the contract
   test's payload with the Flex XG profile) adopted from the Mac while a
   temporary option makes the deployed DX010 bridge report its MAC on an
   idle cage; forgotten afterwards. This decides the shape.
   **Answer (2026-11-02, Network 10.6.109): placed. Shape 2 is live.** The
   stub (`USFXG`, a locally administered MAC with no other trace on the
   LAN) was CONNECTED 10 s after `cmd/devmgr adopt` and placed within
   30 s more: `uplink.uplink_mac` = the parent, `uplink_remote_port` = the
   port its `children:` entry names, `uplink_source: lldp_downlink`, on a
   port that is **down**. The parent's record gained the child in
   `downlink_lldp_macs` (from the LLDP entry) and `downlink_table` (from
   the MAC entry, `{mac, port_idx, speed: 0}`). Which entry places, with
   the parent restarted on one at a time: **either does.** LLDP only: kept,
   source still `lldp_downlink`, `downlink_table` emptied. MAC only: kept,
   and re-derived -- the source went `legacy` then `legacy_only_sta` within
   two cycles. Neither: still placed after four cycles
   (`legacy_only_sta`, `last_uplink` kept), so a placement outlives the
   report, which is what a joined cage (§6.4) will show until the child is
   forgotten. LLDP is what the controller prefers; the feature carries
   both. Records: `docs/fixtures/controller-10.6.109/rest-device-flexxg-placement.json`
   (parent and child per step, reduced to the placement fields).
2. **A shared address.** The stub reports the parent's
   `connect_request_ip`; the controller must adopt and place it and leave
   the parent's record alone.
   **Answer: fine.** The stub's `ip` and `connect_request_ip` were the
   parent's throughout; the parent's `ip`, name, state, `uplink`,
   `gateway_mac`, `connect_request_ip` and hostname were unchanged before
   and after, its only new fields the two downlink lists.
3. **The Flex XG profile under our tables:** which index it treats as
   the uplink, how it draws a 25G SFP28 lane and a 100G QSFP28 uplink on
   its 10G copper picture, and what it does with two empty ports (the
   2x50G case). If the picture fights the data, USMINI2 (five plain
   ports) is the fallback.
   **Answer: the Flex XG works; no fallback needed.** Port 5 is the
   profile's uplink (`Connected Via: Port 5 (Uplink)`, the tile drawn with
   the uplink mark); the picture is the profile's (port 1 apart, 2-4
   grouped, 5) with our media on the tiles (SFP, QSFP) coloured by speed
   (25 GbE, 100 GbE). A lane's drawer: speed picker 25/10 GbE full duplex
   (our `speed_caps`), the FEC box, Operation Switching/Aggregating, and a
   profile note that the SFP28 ports cannot mix speeds in 25G mode. The
   uplink's drawer: 100 GbE only, FEC unticked, no Operation field. 2x50G:
   the two lanes are QSFP tiles at 50 GbE, the two spares "Not Connected".
   One quirk, seen on the lanes the aggregate test had left forced at
   25000: **the stored `speed` follows a forced override, not the
   inform** (50000 sent, 25000 stored; the port without an override
   stored 50000) -- the child must seed its lanes' speeds so overrides
   never disagree with the hardware.
4. **A UniFi aggregate on a cascaded switch's ports**, pushed to the
   stub as `opmode=aggregate` -- that the UI offers it on a Flex XG
   profile (the profile's own capability claims may gate the LAG editor;
   `switch_caps`/`feature_caps` are ours to claim) and what it pushes.
   **Answer: offered and pushed as on any switch.** With the stub
   claiming LACP and `max_aggregate_sessions: 8`, a lane's Operation
   offers Aggregating (behind a "Confirm Aggregation" note), the member
   picker greys out the uplink, and saving ports 2-4 pushes, per member,
   `opmode=aggregate`, `lag=1`, `autoneg=disabled`, `speed=25000`,
   `duplex=enabled`, `fec=cl-91`, `ld_mode=disabled`
   (`docs/fixtures/controller-10.6.109/system_cfg-flexxg-aggregate.txt`).
   The undo ("Remove Aggregation?", then the drawer applies the head's
   settings to all three) pushes `opmode=switch` and leaves the members'
   forced speed. After the aggregate push the record went provisioning,
   then disconnected for about 90 s (`last_seen` frozen while the stub
   informed every 30 s), then connected at the next push, with nothing
   done; seen once, cause not chased.

Then: the child driver fixture-tested against the DX010 capture with its
cage 2 split, the replay of the child's push through the parent's plan
(the PortChannel on lanes 1-3, the fourth lane plain), and the live run
on a free cage with a breakout DAC to a bonded host.


### 6.7 The placement capture, planned (2026-11-02)

One run that answers all four questions of §6.6, plus shape 1's first
question (§5.1) for free. Nothing on any switch is changed; the only
writes are reported values and one adoption that is forgotten afterwards.

**What is already known, which lowers the stakes.** A bridged switch's
*reported* MAC table does place another bridged device: the Podman host
(a bridge) was placed under its Proxmox node's VM port on 2026-09-27, and
that port's MAC table is what the proxmox driver reports from guest tags,
not a hardware FDB (`CLAUDE.md` §1). What is unproven is narrower: a MAC
with no other trace on the LAN (no ARP entry, no lease, no client record),
and an address shared with the parent.

**The parent for the run is whichever bridged switch is cheapest to
redeploy with an idle cage**, since the controller's behaviour is the same
for any parent. The DX010's bridge runs on the switch itself (a `.deb`
rebuild and install per change, `sonic-on-switch.md`); the Arista's runs
in the container, whose redeploy is routine (ships `HEAD`, bounces every
bridged device for about a minute), and the Arista has a down QSFP cage
named as a test port in `local-information/site.md`. Use the Arista. The
knob is generic, so the DX010 gets it without a rebuild for the test.

**Pieces to build** (real code, kept):

1. **A reported child, vendor-neutral**, in the loop, not in a driver: a
   per-device config list `children: [{port: N, mac: M, name: X}]`
   (loaded like `control`). Before the snapshot is rendered, port N's
   `MACs` gains `{M, native VLAN, N, now}` and, when the port has no LLDP
   neighbour of its own, `Port.Neighbor` becomes `{ChassisID: M,
   SystemName: X, PortID: "eth0", IsBridge: true}`; a port that already
   has a neighbour is logged and left alone. One line in the start log
   per child. This is the parent half of shape 2 itself, not a test hook.
2. **A `stub` driver** (`internal/drivers/stub`): read-only, `Collect`
   returns a snapshot described in a file named by `options.snapshot`
   (ports with index, up, speed, media, speed caps, FEC, optic present;
   the device's hostname, serial, version), with the UPS driver's
   `options.mac`, `netmask`, `gateway_mac` for identity and
   reachability. No `Controller` interface, so no push can write
   anything; a pushed `system_cfg` is logged and recorded in the reply
   log as on every device. Documented as the instrument for UI and
   controller experiments (the 75000-speed question of §5.1 is its
   second job); `docs/adding-a-device.md` gets a line.
3. **The Mac-side config** `config.local.stub.yaml` (gitignored by
   pattern): `model: USFXG`, five ports -- 1..4 the lanes (SFP28, 25000,
   caps 10G/25G, FEC rs-fec, up on 1..3, port 4 up with no neighbour),
   5 the uplink (QSFP28, 100000, `is_uplink`); MAC M locally
   administered; `connect_request_ip`, netmask and `gateway_mac` the
   parent's. A second snapshot file for the 2x50G look (two lanes, two
   empty) and a third for §5.1 (one port at 75000).

**Procedure** (site addresses, names, the cage and M live in
`local-information/`; the results come back here scrubbed):

1. Build, `go test ./...`, `scripts/check-site-info.sh --staged`, commit
   on `main` (the container ships `HEAD`).
2. Add `children: [{port: <idle cage>, mac: M, name: <stub name>}]` to
   the Arista's entry in the deployed `config.yaml`; redeploy; confirm
   from the state volume's `payload-last.json` that the cage's
   `mac_table` and `lldp_table` carry M.
3. Start the stub from the Mac (`config.local.stub.yaml`; a Mac
   instance is otherwise forbidden and this one is stopped at the end);
   it goes pending; adopt it by exact MAC with `cmd/devmgr adopt`, never
   Adopt All.
4. Watch the stub's `rest/device` record for `uplink.uplink_mac` and
   `uplink_remote_port` (the UPS was placed 20 s after CONNECTED, the
   UPS 2U Pro in 40 s; give it five inform cycles). Save the stub's and
   the parent's records before and after (scrubbed) -- the parent's must
   be unchanged, and the stub's `ip` is the parent's.
5. Which half places: restart the parent with the LLDP entry only, then
   with the MAC entry only (two more redeploys, about a minute each);
   record whether the placement is kept, lost, or regained.
6. The UI walk, in Clint's Chrome tab: device overview (parent, picture,
   uplink), Ports list, a lane's port drawer (speed picker, FEC, media),
   the uplink's drawer, the two-empty-ports snapshot. Screenshots to
   `local-information/`, findings to §6.6.
7. The aggregate: on the stub's Ports, mark 2-4 Aggregate (the UI
   decides whether a USFXG with our `switch_caps` offers it); the pushed
   `system_cfg` lands in the reply log -> scrubbed into
   `docs/fixtures/controller-<ver>/system_cfg-flexxg-aggregate.txt`;
   undo in the UI. Then the §5.1 snapshot: one inform at 75000, what the
   drawer and list print.
8. Teardown: forget the stub (`cmd/sitemgr delete-device`, exact MAC),
   stop the Mac instance, remove `children:` from the deployed config and
   redeploy, delete the stub's state dir. Write the outcome into §6.6,
   `docs/topology-placement.md` (a new fact either way) and the support
   matrix.

**The run (2026-11-02).** The Arista has been unplugged since
2026-10-31, so the parent was the Podman bridge (the same `UDC48X6`
profile, in the same container, with empty QSFP ports): `children:` on
its idle port 50. To restart the parent on one entry at a time the
knob gained `report: lldp|mac` (unset = both). Everything else ran as
written; the answers are in §6.6 and §5.1, the screenshots in
`local-information/`. Teardown: stub stopped, forgotten (`cmd/sitemgr`),
its state deleted, the deployed config byte-identical to its backup.

**Outcomes.** Placed with both entries: shape 2 is live; step 5 says
which entry the real feature must carry (it will carry both). Placed by
the MAC entry alone or the LLDP entry alone: the same, noted. Not placed
at all, with the fields verified in the record as the UPS's were: shape
2 is dead and shape 1 stands, and the run has still answered §5.1.

**Guards.** The stub cannot write (no `Controller`); the parent's knob
adds reported entries and nothing else; the UI edit in step 7 is on the
stub only; adopting M deletes no client record (M was never a client);
the redeploys bounce every bridged device for about a minute, three
times in all (steps 2, 5, 8 -- step 5 twice). The one real-network risk
is the UI aggregate edit landing on the wrong device: the Ports page must
be the stub's.
