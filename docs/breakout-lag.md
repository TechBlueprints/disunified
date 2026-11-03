# A LAG on the far side of a breakout cable (design note, 2026-11-02)

Status: **options, nothing built.** Clint's question: when the far end of a
breakout cable bundles its lanes with LACP (say three of a 4x25G cable into
one bond), can the bridge notice, bundle the same lanes on our side, and
show UniFi one link at the bundle's speed (75G) -- with the least intrusion
and still looking like UniFi?

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

Recommendation: 1 for discovery, 3 for steady state, 2 only logged (and
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

A and D are not alternatives: D is the manual spelling of the same
bundle A reports. The question is whether discovery (§2) is worth having
on top of the override. I think yes, because the override is also what a
*wrong* auto-detection needs to be pinned or forbidden with, so the knob
exists either way.

## 4. Recommended shape

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

## 5. What to verify first, before any code

Each is a capture or a reported-value experiment, nothing written to a
switch:

1. **What the UI makes of a non-standard `speed`.** Report 75000 (and
   50000, which is a real speed) on an idle port of the DX010 for one
   inform; read the port drawer, the Ports list and the device overview.
   If "75" is unprintable, A falls back to reporting the lane speed and
   the bundle lives in the log and the LACP fields -- or 50/100 only.
   (Could also be answered from the UI bundle's speed-label table, as the
   UPS picture was.)
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
