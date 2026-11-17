# Proxmox VE driver (`proxmox`)

A Proxmox VE node's Linux bridge (`vmbr0`) presented to a UniFi Network
controller as a USW Leaf (`UDC48X6`, 54 ports; `model: auto` picks it for
the 54-port layout, and the 32x100G `USWF07D` "ECS Core" works the same
with `ports: "32"`). Guest NICs are the ports, numbered from 1; the node's
own physical NICs and bond members take the top ports, counting down from
54. Written and verified against **Proxmox VE 9.1.6** (Debian 13, kernel
6.17) on Clint's three-node cluster, 2026-09-19/20.
Scrubbed captures of the collector's output are in
[`docs/fixtures/proxmox-9.1.6/`](../fixtures/proxmox-9.1.6).

**Requirements** (§5 has the steps): root SSH with a key, `lldpd` installed,
a VLAN-aware bridge. **Limits to know before you start:** with the default
cluster-wide numbering the guest ports are shared by the whole cluster:
`ports` (54) less the node's physical NICs, **51 on a node with three**, or
`guest_ports` when it is set (§1c); a bigger cluster needs `numbering:
node`, which has not run live. The driver writes
`/etc/lldpd.d/disunified.conf` (unless `manage_lldpd: "false"`) and
`/etc/chrony/sources.d/disunified.sources` (the controller's NTP servers),
and comments out `chrony.conf`'s own `pool`/`server` lines; nothing else on
the node. Only PVE 9.1 has run it: PVE 8, a standalone node, a second bridge,
an LACP bond, `numbering: node` and UI-driven LACP (§3b) are each listed
with what a PR needs in [`docs/support-matrix.md`](../support-matrix.md) --
**PRs welcome**.

## 0. What is and is not the switch

The switch this driver presents is the **virtual switch inside the node**:
the Linux bridge (`vmbr0`) and the guest NICs plugged into it. That is what
the controller reads and configures: guest ports, their state and VLANs,
the bridge's MAC table, IGMP snooping.

The node's own networking underneath the bridge — its physical NICs, the
bond that joins them, how it fails over, its address — is **reported but
not managed**. Each NIC and each bond member is shown as one of the top
ports with its own link, optic and LLDP neighbour, so the controller can
see what the node is cabled to; an active-backup bond's standby member is
shown link-up but blocking, and only an 802.3ad/balance bond is shown as a
LAG (§1b). The active member is the uplink. The node's own address lives
on the bridge, as a switch's management address lives behind its ports.

A UniFi-side change never touches `/etc/network/interfaces`, the bond's
membership, lldpd's package, or anything a node needs to stay in its
cluster. The one exception is deliberate and guarded: a UniFi link
aggregation over a bond's members converts the bond's mode through
Proxmox's own API (§3b, modelled but not verified live). Failover stays the
bond's business (100 ms), below the switch we present, and the driver
never runs spanning tree across the two links for it.

## 1. What the switch looks like

| Ports | What | Media / speed reported |
|---|---|---|
| 1 up to the first physical port | one per guest NIC on the bridge, **cluster-wide**, named `VM-<vmid>` / `CT-<vmid>` (`VM-119 net1` when a guest has several NICs on the bridge); the port number is recorded in the guest's own Proxmox tags (§1c); a free slot is named `Open-<port>` and reported **disabled** (nothing can be cabled into it). Nothing is reserved: every slot below the node's NICs is open to guests | QSFP28; 100G when the guest runs on this node (virtio/vmxnet3 are memory-bound: Clint's call, "the throughput a VM can get across the virtual switch"), 1G for e1000, 100M for rtl8139; down when the guest is stopped or on another node; an empty cage when no guest is assigned |
| `ports` downwards | the node's physical ports: each NIC under the bridge, a bond's slaves as separate ports named `bond0-1`, `bond0-2`… (the first slave at 54); then every other physical NIC on the box (`eno1`), enabled with no link, so UniFi can see it and later bond it | from `ethtool` per NIC: media from the transceiver EEPROM (`ethtool -m`) or port type, speed caps from the supported link modes, optic vendor/part/serial, the module's compliance (ethtool's "Transceiver type" without its `100G Ethernet: `/`Extended: ` prefix, cut at 32 characters as a real switch cuts it: `100G Base-CR4 or 25G Base-CR CA-`), FEC state, LLDP neighbour. An active-backup bond's active slave forwards and is the uplink; the standby shows link-up but **blocking** and carries no MAC table; neither is a LAG. An 802.3ad/balance bond's slaves form a LAG |

### 1b. Host network layouts

| Host layout | Ports | Status |
|---|---|---|
| one NIC in the bridge | port 54 | verified |
| active-backup bond | one port per slave (`bond0-1` at 54 active/forwarding, `bond0-2` at 53 standby/blocking), no LAG; lldpd announces on the active slave only; the bond stays a bond | **verified** (Clint's cluster) |
| LACP (802.3ad) or balance-* bond | one port per member (`bond0-1`, `bond0-2`), every member in the same LAG (UniFi's aggregate), each with its own counters, optic and LLDP; the first member carries the MAC table; lldpd announces on every member with its own port number; a UniFi aggregate/de-aggregate converts the bond (§3b) | **modelled, not verified live** — no host here has one (the two NICs go to different switches). Unit-tested against the captured bonding text with the mode line changed. If you run LACP, check the first run: two aggregated ports at 54 and 53, each with its upstream neighbour |
| several NICs in the bridge, no bond | one port each | modelled |
| a VLAN device on the uplink (`bond0.10` as the bridge port) | the device underneath, looked through | **modelled, not verified live** |
| several bridges (`vmbr1`…) | one `devices:` entry per bridge (`options.bridge`) | verified for `vmbr0` |
| Open vSwitch bridges | not supported (no `bridge`/`ip` view of the ports) | — |

**The node is the switch.** The device identifies itself with the bridge's
MAC, address and hostname, and is named after the node (`proxmox-2`;
`proxmox-2 vmbr1` for a second bridge): `vmbr0` is where the node's own stack sits,
exactly as a switch's management interface sits behind its own ports.
There is no separate "host" port: the node's own traffic is the switch's
management traffic, its address is the switch's address, and its DNS name
is the switch's. (A variant with a derived device MAC and the host as a
client on its own port was tried on 2026-09-20 and dropped: the "switch"
and the "host" were the same thing with the same address and name, split
in two for no gain. Adopting the node does delete its former *client*
record, hence the static DNS records, §6.)

## 1c. Port numbers live in the guest's tags

A NIC's port is recorded in the guest's own Proxmox tags, so the bridge
keeps no state of its own (the container is stateless; nothing else in
Proxmox can carry per-NIC metadata: the NIC option string is a closed
schema and unknown config keys are dropped on the next write). The grammar
is `unifi.p<port>.<c|h.host>[.<bridge>][.net<N>]`; Proxmox tags allow only
`[a-z0-9_][a-z0-9_-+.]*`, so `.` is the separator (node names never
contain one).

| Tag | Meaning |
|---|---|
| `unifi.p25.c` | `net0` is port 25 on every node (`numbering: cluster`) |
| `unifi.p25.h.proxmox-2` | `net0` is port 25 on proxmox-2's switch (`numbering: node`) |
| `unifi.p27.c.net1` | `net1` is port 27 |
| `unifi.p3.c.vmbr1` | `net0` is port 3 on the vmbr1 switch |

- **The owner writes, the others read.** Only the bridge on the node a
  guest lives on writes tags (`qm set`/`pct set --tags`, a write that
  happens even with control off: it is the bridge's bookkeeping). Every
  bridge computes the same port for an untagged NIC from the same tags
  (lowest free port, guests in VMID and NIC order), so an untagged guest
  is shown at the port its owner is about to write, and once the tag
  exists it alone decides.
- **The tag is authoritative.** Edit it by hand to move a guest to another
  port; the next poll follows it and the fresh-port seeding (§3) fills the
  new port's controller config from the guest's own NIC settings.
- **Duplicates.** Clones and restored backups copy tags, so two guests can
  claim one port: the lower VMID keeps it and the other is retagged by its
  owner, once (every node logs the resolution once, naming both guests and
  the port). The rule depends on the tags alone, never on what a bridge
  remembers, so a restarted bridge agrees with the others; two bridges
  that both read before either writes still converge in one round
  (`converge_test.go`, from the two nodes' captures). Malformed,
  out-of-range or wrong-scope `unifi.` tags for this bridge are replaced
  by the owner; the operator's own tags and another bridge's tags are left
  in place, in order. A well-formed tag ignored for its range or its
  numbering is logged once per guest on every node: it means the nodes'
  `ports`, `guest_ports` or `numbering` disagree.
- **How many guest ports.** `ports` less the node's own physical NICs
  (51 of 54 on a node with three), unless `guest_ports` fixes the number;
  the NICs then take the top ports and the slots between are empty.
  Under cluster numbering every node must count the same, so a cluster
  whose nodes have different NIC counts sets `guest_ports` on each, and
  `Start` fails on a node whose NICs do not fit above it.
- **Failed writes.** A tag write that fails (no quorum, a guest under a
  backup or migrate lock) is retried every poll and logged once, then once
  more when it succeeds. A port write that fails in an apply does not stop
  the other ports; the first error is returned, so the push is applied
  again on the next inform and only what failed is written.
- **Templates** are not ports, but a clone inherits a template's tags, so
  the node that owns a template removes this bridge's `unifi.` tags from
  it (`qm set <id> --delete tags` when nothing else is left), keeping the
  operator's and other bridges' tags. No capture holds a template; the
  test makes one of a captured guest. A template with no NIC at all is not
  seen (the parser reads a guest through its NICs).
- **Moves.** Under cluster numbering a migrated guest keeps its port.
  Under node numbering a guest carrying another node's name has migrated:
  the destination gives it a port, seeds it from the guest's config and
  rewrites the tag; the source's port goes free. A custom port name given
  in UniFi does not follow. **Only cluster numbering has been run live.**

## 2. What is read (every inform, one SSH exec)

[`internal/drivers/proxmox/collect.sh`](../../internal/drivers/proxmox/collect.sh) runs on the node and prints tagged
sections; nothing on the node is installed for the read side. Per section:

| Section | Command | Feeds |
|---|---|---|
| `links` | `ip -j -s -d link show` | per-port link state, MTU, counters (a tap's RX is what the guest sent, i.e. the port's RX) |
| `brlink` | `bridge -j -d link show` | STP state / path cost per member |
| `vlan` | `bridge -j -compressvlans vlan show` | live 802.1Q state of each member (`-compressvlans` matters: 4093 VLANs × 15 ports is 450 KB otherwise) |
| `fdb` | `bridge -j -s fdb show br vmbr0 dynamic` | MAC table: `updated` (seconds since the entry's last frame) is the entry's `age`; the FDB has no learn time, so `uptime` is the loop's (`docs/mac-table-times.md`). `dynamic` drops the per-VLAN permanent entries, 4.6 MB otherwise |
| `bridge` | `/sys/class/net/vmbr0/bridge/*`, `bridge-vids` | STP off/on, priority, IGMP snooping, VLAN range |
| `phys`, `bonding`, `ethtool`, `ethtoolm`, `carrier` | sysfs, `/proc/net/bonding`, `ethtool`, `ethtool -m`, `carrier_changes`, `carrier_up_count`, `carrier_down_count`, `carrier` | uplinks: bond membership and active slave, speed caps, FEC support, optic EEPROM/DOM; every port's link drops (below) |
| `hwmon` | `/sys/class/hwmon` | temperature (coretemp/k10temp package sensor; overheating at `temp1_max`), fans (`fanN_input` as % of `fanN_max`; Dell's `dell_smm`), labelled `Fan1`, `Fan2`… as the ECS labels its own, or with the chip's `fanN_label` unchanged when it has one |
| `qemu`, `lxc`, `vmlist` | cluster config files | guests, NIC model/MAC/tag/trunks/link_down/firewall |
| `lldp` | `lldpcli -f json0 show neighbors details` | uplink neighbours (needs lldpd, §4) |
| `nft`, `dhcpguard` | `command -v nft`, `nft -j list table bridge disunified` | whether DHCP guarding can be claimed; the driver's guard table, its rule-set fingerprint and each guest port's rogue-DHCP drop count (§4c) |
| `stat`, `meminfo`, `uptime`, `dmi`, `pveversion`, `chrony` | `/proc`, DMI, `pveversion` | CPU % (delta between polls), memory, uptime, serial (service tag), NTP servers, and the node's PVE version (`9.1.6`, from `pve-manager/9.1.6/…`) — which is the firmware version the controller shows, and the model string `VE 9.1.6` |

**Link drops (`link_down_count`, 2026-11-15).** The kernel counts each
interface's carrier rises (`carrier_up_count`) and drops
(`carrier_down_count`); `carrier_changes` is their sum. Both include what
the NIC driver did between registering the interface and its first link:
a driver that turns the carrier off after registering counts a drop that
never happened, one that does it before counts nothing
(`netif_carrier_off` skips the count while the device is unregistered,
`net/sched/sch_generic.c`). On kernel 6.17.13-1-pve the nodes' mlx5
uplinks count from a rise (13 changes = 7 up + 6 down while up), while
bonds, bridges, the firewall bridges' veths and the e1000e `eno1` count a
drop first (a bond up since boot reads 1 up + 1 down; `eno1`, never
linked, reads 1 down); taps count nothing (tun never moves its carrier).
The Podman host's virtio_net NIC (kernel 6.12, EL10) also counts the
drop first. So `carrier_changes / 2`, the driver's count until that day,
was one too many on such an interface while up; no port of a node showed
it (uplinks are mlx5, guest ports taps, no LXC veths), the Podman uplink
did. Every real drop ends an up period, so the driver now reports
`carrier_up_count` less the up period still running, which is exact on
any driver. The `carrier` section's line is `<name>=<changes> <up> <down>
<carrier>`; an admin-down interface has no readable carrier (`-`, counted
as down) and a kernel without the up count (before 4.16) prints `-`, which
falls back to `carrier_changes / 2`, as does a line from an older script
(`fixtures/proxmox-9.1.6/collect-node1-carrier.txt`,
`internal/drivers/proxmox/carrier_test.go`). An LXC guest's veth is
recreated with the container, so its count starts again then.

**An unmounted `/etc/pve` is a failed poll (2026-11-17,
[`proxmox-on-node.md`](proxmox-on-node.md) §4.1).** With pmxcfs not
mounted the script still runs to the end, with empty `vmlist`, `qemu` and
`lxc` sections. The driver requires all three, and an empty `vmlist` (the
file exists on every mounted pmxcfs, guests or none) fails the poll:
`proxmox: /etc/pve/.vmlist is empty: pmxcfs is not mounted (is pve-cluster
running?); no guest can be read`. `.vmlist` is also held to the config
headers the globs printed: the same ids, of the same kind, under the same
nodes, both ways, or the poll fails naming each guest that differs (`qemu
100 is in .vmlist on proxmox-2 but has no config`). A guest created,
destroyed or migrated between the two reads costs one poll. The node's own
guests are those under `/etc/pve/nodes/<name>` where `<name>` is the first
label of the `hostname` section (the kernel hostname may carry a domain);
the reported hostname is the whole one. A failed poll leaves the last good
snapshot in place, and the loop neither provisions nor applies from it
(`internal/informloop/replay_proxmox_test.go`,
`TestProxmoxFailedCollectAfterGoodOne`).

Firewall-enabled guests (`firewall=1`) hang off `fwbr<id>i<n>`; their
member of `vmbr0` is `fwpr<id>p<n>`, which is where VLANs and learned MACs
live, while counters come from the tap. The driver knows both.

## 3. What is written (`control.ports`)

Only two things exist per bridge port, and both are written through
Proxmox so they persist and hot-apply to a running guest (network hotplug
re-plugs the tap; verified on VM 999, 2026-09-19):

| UniFi | Proxmox (`qm set <id> --net<n> …` / `pct set`) |
|---|---|
| port disabled / enabled | `link_down=1` / option removed |
| native 1, all tagged (the default "All") | no `tag`, no `trunks` |
| native N, nothing tagged ("Block all") | `tag=N` (`tag=1` for native 1) |
| native N, tagged list | `tag=N` (absent for 1) + `trunks=a;b-c` |
| native N, all tagged | `tag=N,trunks=2-4094` |

Every other option on the NIC line (model=MAC, bridge, firewall, queues,
mtu, rate…) is preserved. Writes are diffs against the config as last read;
a guest on another node is left to that node's bridge. Port cycle =
`link_down=1`, 3 s, restore.

### 3b. Link aggregation (LACP) — UNTESTED LIVE, PRs welcome

The controller's aggregate on the node's physical ports
(`switch.port.N.opmode=aggregate` + `lag=<id>`, the keys the Arista driver
honours) is applied through Proxmox's own API (`pvesh`, so Proxmox writes
and applies its network config):

- an aggregate over **every slave of a bond** converts the bond to
  `802.3ad` (`layer2+3` hashing); removing it puts the bond back to
  `active-backup` with its first slave as primary — the bond is never
  removed;
- an aggregate over **plain NICs** joins them into a new bond in the
  bridge's port list;
- refused, with a log line: members whose LLDP neighbours are different
  chassis (a LAG cannot span switches; applying it would take the node off
  the network), a bond only partly covered, a guest port in the aggregate,
  a mix of bond slaves and plain NICs.

An 802.3ad or balance-* bond is reported as a LAG (`op_mode aggregate` on
its member ports). Capability claimed: one aggregate session.

**None of this has run against a real LACP switch port**: this site's
nodes bond to two different switches, which no LAG can span, and there
are no spare LACP-capable ports. The commands are Proxmox's documented API
calls and the unit tests (`lag_test.go`) cover the plan on the captured
node state with the neighbours rewritten to one switch. If you can run it,
please report what happens or send a PR.

**Adoption seeds the controller from the switch.** A freshly adopted
device has no port config in the controller, so its first push says
"every port at its defaults" — which, applied, would strip every guest's
VLAN tag (it did, for 13 seconds, on 2026-09-20). Two guards, both on by
default:

- when the handshake completes the bridge writes each configured port's
  live state into the controller as its port override (native VLAN,
  tagged set, disabled), through the REST API (`api_url`), so the first
  push already matches; ports on VLANs the site does not have are logged
  and left at the default (`control.no_seed: true` to skip);
- the first push after adoption is **held** while it would change any
  port (the driver plans the diff without writing; the device keeps
  reporting the old cfgversion and the controller keeps re-sending) until
  a push changes nothing or the operator has set the ports;
  `control.allow_initial_changes: true` overrides.

**A guest created later is covered the same way.** Its port appears at the
next poll; the bridge seeds that port's live state into the controller and,
until the controller's config for it matches the switch, the port is
withheld from every write (logged once). A guest that migrates between
nodes keeps its port number and name (the port is in the guest's tag) and its
destination was seeded with its config along with every other node; the
arrived port is still treated as fresh there, because port overrides are
per device in UniFi — if the destination's override for that port was
edited to something else, the guest keeps its own config and the log says
so until the two agree. The comparison has a baseline from the first
collect of a run on (2026-11-17): the previous run's record
(`layout.json` in the state directory), or the startup snapshot when there
is none. So a guest that arrives between the bridge's start and its first
collect, or while the bridge is stopped, is fresh too, and a port withheld
when the bridge stops is still withheld when it starts again
(`docs/drivers/proxmox-on-node.md` §5.5). What is *not* covered: a tag changed by
hand on a node after adoption — UniFi is the source of truth from then on
and the reconcile puts the controller's value back (change it in UniFi).

Switch-wide, from the controller whether or not ports are written (no
per-setting opt-in since 2026-10-31):
IGMP snooping toggles the bridge's `multicast_snooping` (bridge-wide: on
when UniFi enables it on VLAN 1, else on any managed VLAN); NTP writes
`/etc/chrony/sources.d/disunified.sources` and comments out the node's own
`pool`/`server` lines in `chrony.conf` (marked `#disunified:`; chrony is
restarted, since a reload keeps a `chrony.conf` pool), so UniFi's list is
the node's (2026-10-31). The next chrony package upgrade sees a modified
conffile and asks; keep the local version. STP requests are logged once (a
Proxmox bridge runs `bridge-stp off` by design), syslog is logged (journald
has no remote target), reboot is never real (`Rebooter` is not
implemented: the controller's Restart is emulated), SSH keys are not
installed.

Capabilities claimed (`switch_caps`): IGMP snooping and link aggregation
(one aggregate session, for §3b), plus STP, BPDU guard and port cost only
on a node whose bridge runs under mstpd (§4b), and DHCP snooping (Rogue DHCP
Server Detection) only on a node with `nft` and a VLAN-aware bridge (§4c). The UI therefore hides storm
control, FEC, mirroring and isolation for these switches, and STP options
unless mstpd is present; speed pickers offer the one speed each port has.

### 3c. Two faults found on the SONiC driver, checked here (2026-11-15)

On 2026-11-15 the `sonic` driver learned to delete a VLAN the controller
stops pushing ([`sonic.md`](sonic.md) §11.29), and its STP daemon was
found making STP ports for an aggregate's members after a restart. Neither
fault exists in this driver, for the reasons below.

**A VLAN the push stops naming: nothing is left behind.** The driver
writes VLANs only as a guest NIC's `tag` and `trunks` (§3). A VLAN-aware
Linux bridge has no switch-wide VLAN table to fill or prune. It has VLAN
rows per port, plus the bridge's own (`self`) rows for the node's stack.
The uplink's rows come from `bridge-vids` in `/etc/network/interfaces`,
which is the node's own config. `EnsureVLANs` only checks that range
(a site VLAN outside it is logged) and never writes it. When the push
stops naming a VLAN for a port (a tagged set that drops it, Block All, or
the network deleted from the site), the next apply removes it from that
NIC's options, and Proxmox rebuilds the interface's rows from them. On a
running guest, `qm set` with a changed `tag`/`trunks` unplugs the tap and
plugs it again (qemu-server's `vmconfig_update_net`, PVE 9.1.6). Plugging
first deletes every VLAN row on the interface (`bridge vlan del ... vid
1-4094`), then adds the PVID and the trunks (`PVE::Network`
`bridge_add_interface`). `pct set` does the same for a container's veth.
So §11.29's prune has nothing to act on here. What stays on the bridge
besides the guests' rows is the operator's: `bridge-vids`, the node's own
VLAN interfaces, SDN. On a host those are left alone, under the rule that
a hand-made thing UniFi cannot express stays. **Read on all three nodes
on 2026-11-15:**

- every running tap's and `fwpr` port's rows equal its NIC's options (no
  tag gives 1 untagged + 2-4094; `tag=8` gives 8 alone);
- the bridge's own rows are VLAN 1 alone, and the bond's are 1 + 2-4094
  from `bridge-vids`;
- the deployed bridge logged no VLAN write for these nodes in 72 hours.

One difference from a UniFi switch is by design. Allow All on native 1 is
a NIC with no `tag` and no `trunks`, and Proxmox plugs that as 2-4094
(neither `bridge-vids` nor the site's list). So a VLAN id the site has
deleted, or never had, still passes between two Allow All guests on one
node. The upstream UniFi switch carries no such VLAN, so nothing reaches
the LAN. Writing the site's list as `trunks` instead was considered and
not done, for two reasons. It would re-plug every Allow All tap on every
site network change. And it would break the adoption seeding: a guest with
no options is seeded as Allow All, and a plan that always wants a write
holds the first push for good. A bond the driver made from plain NICs
(§3b) also stays a bond, in active-backup, when its aggregate goes: two
NICs to one switch, both in a bridge with STP off, would loop. That path
is modelled, never run.

**STP and an aggregate: one STP port per bond, and nothing written to a
leg.** On a node a bond is one bridge port. Its legs are enslaved to the
bond and are not bridge ports (`bridge link` on every node lists `bond0`,
never its legs), so mstpd, which runs on the bridge's ports, has one STP
port for the bond in any bond mode, LACP included. The driver writes
mstpd four things:

- `settreeprio` and `setforcevers` for the bridge;
- `setportadminedge` and `setportbpduguard` for guest members only (taps,
  veths, `fwpr`), each looked up in that poll's `mstpctl showportdetail`.

A BPDU guard or edge pushed for an uplink port (a bond leg's port, 54 or
53) maps to no guest and writes nothing, and both legs report the bond's
STP port. mstpd keeps its settings in memory only. After an mstpd or node
restart, the next poll reads mstpd's defaults and writes the edge and
priority again, and the loop's reconcile writes the BPDU guard again from
the same report. The driver keeps no per-port STP state of its own across
polls, so there is nothing stale to replay. **No node runs mstpd today**
(`stp_state` 0 and no `mstpctl` on all three, 2026-11-15), so this path is
fixture-tested only. `vlan_stp_test.go` holds both answers:

- `TestDroppedVLANLeavesThePort`: a tagged VLAN dropped, then Block All,
  then nothing;
- `TestSTPNeverWritesTheUplink`: the captured mstpd port copied as the
  bond, active-backup and 802.3ad, BPDU guard pushed for every port plus
  an aggregate over the legs, two cycles; no command names the bond, a
  leg or `eno1`.

## 4. Topology: lldpd on the node, configured by the driver

The controller places a switch by the LLDP frames the *upstream* switch
receives on the port the node is cabled to, so the node has to emit them:
that is the one thing installed on a node (`apt-get install lldpd`). The
driver keeps lldpd's configuration itself (`/etc/lldpd.d/disunified.conf`,
rewritten and lldpd restarted whenever it differs; `manage_lldpd: "false"`
to opt out):

- chassis ID = the bridge MAC (`configure system chassisid`; lldpd would
  otherwise pick some other NIC's MAC, such as an unused onboard port's);
- announce only on the bond's active slave: with both slaves of an
  active-backup bond announcing, the controller drew the nodes under the
  backup link's upstream switch (a 10G one), not the one carrying
  the traffic (seen 2026-09-20);
- port ID = the NIC's port number on this switch (`"Port 54"`), the form the
  controller maps back to our port table.

Verified 2026-09-20: the aggregation switch (`USWF066`) lists all three nodes
in its `lldp_table` and `downlink_table` on ports 50-52; the nodes see
the aggregation switch on the 100G slave, and the controller composes each
node's uplink (`uplink_source: lldp_uplink`, remote port 50/51/52, 100G
QSFP28, depth 1) once the inform carries `uplink: "eth0"`, `if_table` with
the netmask and `gateway_mac` (the peer's finding on the Arista; the driver
fills `System.Addresses` and `System.ARP` from `ip addr` / `ip neigh`). The driver picks the uplink port from
the neighbour with the Router capability (the aggregation switch), falling
back to the bond's active slave when LLDP is silent (`Snapshot.UplinkHint`).

## 4b. Spanning tree: optional, through mstpd

By default a Proxmox bridge runs with `bridge-stp off` and the driver
claims no STP capability, so the controller never pushes its STP settings
at the node and the UI shows no STP controls for it. With a bond as the
only uplink nothing needs spanning tree.

If the node runs **mstpd** and the bridge is under it (`stp_state` 2), the
driver claims STP and does the following, detected per node at startup:

- reports the version (`rstp`/`stp`), the bridge priority, the root bridge,
  and per port the role, state, path cost, edge and BPDU-guard state,
  transitions and guard errors (`mstpctl -f json showbridge/showportdetail`);
- **pins the bridge priority at 61440** (the maximum: the node is never
  elected root) and **keeps every guest port an edge port** (a starting
  guest forwards at once instead of waiting out the forward delay; Proxmox
  creates taps without it), both re-asserted at runtime every poll with
  `mstpctl`, because the interfaces-file `mstpctl-treeprio` is known not to
  apply on Proxmox (mstpd issue #155);
- honours the controller's STP **version** (`switch.stp.version`) and
  per-guest-port **BPDU guard**; the controller's priority and "STP off"
  are logged and not applied.

Installing it (mstpd is not in Debian trixie; it entered Debian in 2026-05
and is in forky/sid only; the kernel has no RSTP, so nothing else provides
it; the pool package installs cleanly on trixie):

```bash
curl -O http://deb.debian.org/debian/pool/main/m/mstpd/mstpd_0.2.0-2_amd64.deb
apt-get install ./mstpd_0.2.0-2_amd64.deb
```

then in `/etc/network/interfaces` under the bridge, replacing
`bridge-stp off` / `bridge-fd 0` (Proxmox's own config writer preserves
these lines; verified on PVE 9.1):

```
	bridge-stp on
	mstpctl-forcevers rstp
	mstpctl-treeprio 61440
	mstpctl-hello 2
	mstpctl-maxage 6
	mstpctl-fdelay 4
```

and `ifreload -a`. The kernel's `/sbin/bridge-stp` hook (shipped by the
package) starts mstpd and hands it the bridge; no service unit is needed.
mstpd requires the 2 s hello for RSTP; 6/4 are the shortest max-age and
forward-delay it accepts. **Before turning it on, make sure the upstream
switch ports accept BPDUs** (no BPDU guard; loop protection left as is): a
UniFi port that blocks a node's BPDUs disconnects the node, and a Proxmox
node that loses its uplinks fences itself. Turning STP on live costs the
guests about a second (the taps become edge ports on the driver's next
poll; set them by hand right after the reload to avoid the forward delay).

## 4c. Rogue DHCP server detection (2026-11-14)

UniFi's Rogue DHCP Server Detection (Settings → Networks → Global Switch
Settings; each network's DHCP Guarding lists its trusted servers) is
claimed on a node that has `nft` and a VLAN-aware bridge, and applied
through an nftables table of the driver's own, `table bridge disunified`.
Proxmox's firewall is not read or touched: on these nodes it is the
iptables `pve-firewall` (no nftables tables at all before the driver's),
and the driver's table sits beside whichever one runs.

- **What is guarded:** every guest NIC on this node's bridge, running or
  stopped (a stopped guest's interface gets its name when it starts, and the
  rule is already there). It is matched by name: `tap<id>i<n>` (`veth<id>i<n>`
  for a container) and `fwpr<id>p<n>`, the bridge side of Proxmox's
  firewall bridge for a NIC with `firewall=1`. A DHCP server reply
  (UDP 67 → 68) entering the bridge through one, on a guarded VLAN, from an
  address that is not one of that VLAN's trusted servers, is counted and
  dropped. A VLAN with no trusted server is not guarded. The node's own
  uplinks are not guarded: replies from the LAN have already crossed the
  upstream switch.
- **VLANs:** a NIC's untagged frames are its own VLAN (`tag=`, else 1) and
  are matched as `ether type ip` on its interface: on the node's kernel an
  untagged frame carries no VLAN id at the bridge hooks, not even the
  port's PVID (tested). Its tagged frames (a NIC with no `tag`, or with
  `trunks`) are matched by `vlan id`, one rule per guarded VLAN it carries.
  UniFi's VLAN 1 is the bridge's VLAN 1.
- **Hook:** the bridge's `prerouting`, which sees each frame once as it
  enters. The `forward` hook runs once per egress port of a flooded frame:
  there one broadcast offer counted once for every other port in its VLAN
  (tested, then moved).
- **Writing:** the table is replaced as a whole in one `nft -f -`
  transaction when the rules change (a guest added, a VLAN changed, a
  server list changed), and deleted (`nft delete table bridge disunified`)
  when the controller turns detection off or nothing on the node is
  guarded. The table's comment holds a fingerprint of its rules
  (`dui:<12 hex>`), which is how a collect tells that the node already holds
  what the push asks for; a node reboot loses the table and the next
  reconcile writes it again. A push without `switch.dhcp_snoop.status`
  leaves the table as it is.
- **Reading:** `collect.sh` lists the table (`nft -j list table bridge
  disunified`); each port's rules' packet counters, summed, are
  `Port.Health.RogueDHCPDrops`, and the bridge raises the port's rogue-DHCP
  anomaly (64) when that grows between informs. A rewrite resets the
  counters to 0, which raises nothing.

**Basis.** The rule set is tested on a node's kernel (PVE 9.1.6, kernel
6.17, nftables 1.1.3) inside a throwaway network namespace with its own
bridge, veths and table, so nothing of the node's own was touched:
`docs/fixtures/proxmox-9.1.6/dhcpguard-netns-test.sh` loads exactly the
driver's write for `collect-node2-nft.txt` under the ECS's captured
trusted servers (`dhcpguard_test.go` holds the two equal), twice (no table,
then a table present), sends raw frames and counts what reaches a receiving
port: untrusted replies dropped untagged and on a tagged VLAN, through a
plain tap and through an `fwpr` member; trusted replies, a server trusted
only on another VLAN, and other UDP as they should be; one count per
dropped frame; the delete leaves no table (`dhcpguard-netns-test.txt`;
the table's listing after the frames is `nft-dhcpguard.json`, the
read-back fixture). A worktree build's `-collect-once` against a node read
the new sections (no table there; every port 0). **Not yet seen:** the
controller pushing `switch.dhcp_snoop.status` to a node that claims the
capability, the table on a node's own bridge, and the alert in the UI.
**Before the first push with detection on:** a guest that is itself a DHCP
server on a guarded network (a router VM) loses its clients behind the same
node unless that network's DHCP Guarding lists it; a two-minute capture of
UDP 67 → 68 on all three nodes saw no guest answering (2026-11-14), which
settles nothing about a renewal-time server.

**Not reported by a node** (the other switch-detected anomalies): broadcast,
multicast and unicast storms (1, 2, 4) and the DHCP packet rate (32) are
counts a UniFi switch takes from limiters it applies, and a node applies
none: no storm control or DHCP rate limit is built, on purpose, on
production hosts. Loop protection (512) and the rest of the table
(802.1X, MAC locking, DoS, PHY, PoE) have nothing on a node that detects
them.

## 4d. UniFi's client Block (2026-11-15)

The controller's blocked clients (`blocked_sta` and the `block-sta` cmd,
`docs/feature-map.md` §3) are dropped as they enter the node's bridge from
any port, guest NIC or uplink, as UniFi's own SONiC switch drops them on
every front-panel port (`docs/drivers/sonic.md` §11.33). Bridge prerouting
runs before the bridge's local delivery, so a blocked client reaches
neither another port nor the node's own address.

- **Built (`block.go`).** An nftables table of the driver's own, `table
  bridge disunified_block`, apart from the DHCP guard's table and from
  Proxmox's firewall: one chain hooked at prerouting, one rule `meta ibrname
  "<bridge>" ether saddr { <macs> } counter drop`, the table comment
  `dui:<fingerprint>`. Replaced whole in one `nft -f -` transaction when the
  list changes, deleted when none is blocked; `collect.sh` lists it
  (`macblock`). Needs `nft` only (not a VLAN-aware bridge). The node's own
  bridge MAC is never written.
- **Tests.** `block_test.go`, from the bridged switches' captured setparam
  with two clients, against node 2's capture.
- **Not verified on a node.** The nodes are production; no rule set has been
  loaded, not even in a throwaway namespace as the guard's was (§4c).

## 5. Setup

The bridge can also run on each node itself, as a root systemd service from
the `.deb` with `options.transport: local`: no SSH key to any node, one
bridge per node, each config naming only its own node, the nodes sharing
nothing but pmxcfs. Step 1 below is then not needed; steps 2 and 3 are.
Install and config: [`docs/install.md`](../install.md) §7b; the design, the
cluster's behaviour with three independent bridges and moving an adopted
node: [`proxmox-on-node.md`](proxmox-on-node.md). Not yet verified live.

Per node, once (over SSH):

1. **Root SSH with a key.** Put the bridge's public key in
   `/root/.ssh/authorized_keys` on the node. The driver reads over SSH and
   writes with `qm set` / `pct set`; Proxmox API tokens cannot do the read
   side (no MAC table, counters, VLAN state, sensors or LLDP through the API).
2. **`apt-get install -y lldpd`.** Nothing to configure: the driver writes
   its config on the first poll (§4).
3. A VLAN-aware bridge (`bridge-vlan-aware yes` on `vmbr0`); the driver reads
   a plain bridge too but tags then mean per-VLAN bridges, which it does not
   model. `ethtool` and `chrony` are there by default.

Then one `devices:` entry per node:

```yaml
devices:
  - name: proxmox-2
    driver: proxmox
    ssh: root@192.0.2.102        # key auth; the node's in-band address
    ip: 192.0.2.102              # what the controller reaches the switch at
    model: UDC48X6              # USW Leaf; auto picks it from the 48+6 layout
    options:
      bridge: vmbr0             # default
      # ports: "54"             # default; the node's NICs take the top ports, guests the rest
      # numbering: node         # per-node guest ports instead of cluster-wide; untested live
      # manage_lldpd: "false"   # leave lldpd alone
      # ssh_key: /etc/disunified/id_ed25519       # in a container
      # known_hosts: /etc/disunified/known_hosts
    control:
      ports: all                # port state and VLANs -> qm/pct set; see §3 before "all"
```

In a container ([`deploy/compose.yaml`](../../deploy/compose.yaml)), mount the private key and a
`known_hosts` holding each node's host key read-only and name them in
`options`; the container has no home directory or agent, and the key must
be readable by uid 65532. The driver keeps no file of its own: a guest's
port lives in its tags (§1c), and `state/<name>/device.json` holds only
the adopted key.

## 6. Controller quirks met on Network 10.6.106

- **Adopting a node removes its DNS name.** The controller publishes
  `<hostname>.local.<domain>` for *clients*; once the node's MAC is an
  adopted device it is no longer a client and the record disappears
  (`<node>.local.<domain>` went NXDOMAIN on 2026-09-20 while
  every VM name kept resolving). Give each node a static DNS record in the
  controller (Settings → Routing → DNS) or point the bridge's `ssh:` at the
  node's IP.

- **`api.err.OobPortNotSupported`** on every REST update of the device:
  the controller stores `oob_port_config: [{"enabled": true}]` for the ECS
  Core (real hardware has an OOB port) and then rejects the record because
  the device claims no such port. The naming provisioner sends
  `oob_port_config: []` with its update, which the controller accepts.
- The controller gives a new device its own display name for the model
  ("USW Leaf" for `UDC48X6`, "ECS Core" for `USWF07D`), absent from the
  catalogue; `unifimodel.ControllerDisplayNames` lists such names so the
  first provision can rename the device after the node.
- Ports with `sfp_found: false` (unassigned slots) draw as empty cages; a
  stopped guest's port draws as a cabled, down port.
- **Names the driver gave count as defaults.** The provisioner renames a
  port or the device only while its controller-side name is one of the
  profile's defaults or one this driver could have set earlier (`Open-25`
  before a guest took the slot, `VM-999` after it left, the `pve-<node>`
  device name of an earlier build), so an operator's own name always
  survives. A free slot is seeded disabled in the controller too (the
  Port State the UI writes), with any config its last guest left stripped;
  when a guest takes a slot whose override is that free-slot seed and whose
  name is one the driver gave, the guest's own state replaces it, so a new
  guest is never left switched off by the slot's previous life. A disabled
  port with an operator's own name is left alone.
- **Disabling a port through the REST API** needs the whole combination the
  UI writes, or the controller silently normalises `forward` back to
  `all`: `{"forward":"disabled","port_security_enabled":true,
  "port_security_mac_address":[],"native_networkconf_id":"",
  "tagged_vlan_mgmt":"block_all"}`. The UI's Port State → Disabled radio
  does exactly that and the push then carries `switch.port.N.status=disabled`.
