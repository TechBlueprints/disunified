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
| `ports` downwards | the node's physical ports: each NIC under the bridge, a bond's slaves as separate ports named `bond0-1`, `bond0-2`… (the first slave at 54); then every other physical NIC on the box (`eno1`), enabled with no link, so UniFi can see it and later bond it | from `ethtool` per NIC: media from the transceiver EEPROM (`ethtool -m`) or port type, speed caps from the supported link modes, optic vendor/part/serial, FEC state, LLDP neighbour. An active-backup bond's active slave forwards and is the uplink; the standby shows link-up but **blocking** and carries no MAC table; neither is a LAG. An 802.3ad/balance bond's slaves form a LAG |

### 1b. Host network layouts

| Host layout | Ports | Status |
|---|---|---|
| one NIC in the bridge | port 54 | verified |
| active-backup bond | one port per slave (`bond0-1` at 54 active/forwarding, `bond0-2` at 53 standby/blocking), no LAG; lldpd announces on the active slave only; the bond stays a bond | **verified** (Clint's cluster) |
| LACP (802.3ad) or balance-* bond | one port per member (`bond0-1`, `bond0-2`), every member in the same LAG (UniFi's aggregate), each with its own counters, optic and LLDP; the first member carries the MAC table; lldpd announces on every member with its own port number; a UniFi aggregate/de-aggregate converts the bond (§3b) | **modelled, not verified live** — no host here has one (the two NICs go to different switches). Unit-tested against the captured bonding text with the mode line changed. If you run LACP, check the first run: two aggregated ports at 54 and 53, each with its upstream neighbour |
| several NICs in the bridge, no bond | one port each | modelled |
| a VLAN device on the uplink (`bond0.10` as the bridge port) | the device underneath, looked through | **modelled, not verified live** |
| several bridges (`vmbr1`…) | one `switches:` entry per bridge (`options.bridge`) | verified for `vmbr0` |
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
  owner. Malformed, out-of-range or wrong-scope `unifi.` tags for this
  bridge are replaced; the operator's own tags and another bridge's tags
  are left in place, in order.
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
| `fdb` | `bridge -j -s fdb show br vmbr0 dynamic` | MAC table with ages (`dynamic` drops the per-VLAN permanent entries, 4.6 MB otherwise) |
| `bridge` | `/sys/class/net/vmbr0/bridge/*`, `bridge-vids` | STP off/on, priority, IGMP snooping, VLAN range |
| `phys`, `bonding`, `ethtool`, `ethtoolm`, `carrier` | sysfs, `/proc/net/bonding`, `ethtool`, `ethtool -m`, `carrier_changes` | uplinks: bond membership and active slave, speed caps, FEC support, optic EEPROM/DOM, link flaps |
| `hwmon` | `/sys/class/hwmon` | temperature (coretemp/k10temp package sensor; overheating at `temp1_max`), fans (`fanN_input` as % of `fanN_max`; Dell's `dell_smm`) |
| `qemu`, `lxc`, `vmlist` | cluster config files | guests, NIC model/MAC/tag/trunks/link_down/firewall |
| `lldp` | `lldpcli -f json0 show neighbors details` | uplink neighbours (needs lldpd, §4) |
| `stat`, `meminfo`, `uptime`, `dmi`, `pveversion`, `chrony` | `/proc`, DMI, `pveversion` | CPU % (delta between polls), memory, uptime, serial (service tag), NTP servers, and the node's PVE version (`9.1.6`, from `pve-manager/9.1.6/…`) — which is the firmware version the controller shows, and the model string `VE 9.1.6` |

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
so until the two agree. What is *not* covered: a tag changed by
hand on a node after adoption — UniFi is the source of truth from then on
and the reconcile puts the controller's value back (change it in UniFi).

Switch-wide: IGMP snooping (`control.igmp`) toggles the bridge's
`multicast_snooping` (bridge-wide: on when UniFi enables it on VLAN 1, else
on any managed VLAN); NTP (`control.ntp`) writes
`/etc/chrony/sources.d/disunified.sources` and `chronyc reload sources`.
STP requests are logged once (a Proxmox bridge runs `bridge-stp off` by
design), syslog is logged (journald has no remote target), reboot is never
real (`Rebooter` is not implemented: the controller's Restart is emulated
whatever `control.reboot` says), SSH keys are not installed.

Capabilities claimed (`switch_caps`): IGMP snooping and link aggregation
(one aggregate session, for §3b), plus STP, BPDU guard and port cost only
on a node whose bridge runs under mstpd (§4b). The UI therefore hides storm
control, FEC, mirroring and isolation for these switches, and STP options
unless mstpd is present; speed pickers offer the one speed each port has.

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

## 5. Setup

Per node, once:

1. **Root SSH with a key.** Put the bridge's public key in
   `/root/.ssh/authorized_keys` on the node. The driver reads over SSH and
   writes with `qm set` / `pct set`; Proxmox API tokens cannot do the read
   side (no MAC table, counters, VLAN state, sensors or LLDP through the API).
2. **`apt-get install -y lldpd`.** Nothing to configure: the driver writes
   its config on the first poll (§4).
3. A VLAN-aware bridge (`bridge-vlan-aware yes` on `vmbr0`); the driver reads
   a plain bridge too but tags then mean per-VLAN bridges, which it does not
   model. `ethtool` and `chrony` are there by default.

Then one `switches:` entry per node:

```yaml
switches:
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
      igmp: false
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
