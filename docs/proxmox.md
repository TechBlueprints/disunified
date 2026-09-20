# Proxmox VE driver (`proxmox`)

A Proxmox VE node's Linux bridge (`vmbr0`) presented to a UniFi Network
controller as a USW Leaf (`UDC48X6`, 48 + 6 ports; the 32x100G `USWF07D`
"ECS Core" was used first and works the same with `ports: "32"`). Guest NICs are the ports; the physical NICs the bridge uplinks
through are the last two. Written and verified against **Proxmox VE 9.1.6**
(Debian 13, kernel 6.17) on Clint's three-node cluster, 2026-09-19/20.
Scrubbed captures of the collector's output are in
`docs/fixtures/proxmox-9.1.6/`.

## 0. What is and is not the switch

The switch this driver presents is the **virtual switch inside the node**:
the Linux bridge (`vmbr0`) and the guest NICs plugged into it. That is what
the controller reads and configures: guest ports, their state and VLANs,
the bridge's MAC table, IGMP snooping.

The node's own networking underneath the bridge — its physical NICs, the
bond that joins them, how it fails over, its address — is **not** the
switch and is never configured by the bridge. It is reported the way a
switch reports its uplink: as one link out. A bond is therefore one port
whatever it is made of; an active-backup pair is not two ports with one
"blocking", because from the virtual switch's side there is one path, and
which physical NIC carries it is the node's business, not the controller's.
The host itself is a client behind that switch (port 53), like a server
behind a real one.

This is why a UniFi-side change never touches `/etc/network/interfaces`,
the bond, lldpd's package, or anything a node needs to stay in its cluster,
and why "STP across both links" is not on the table: the failover that
matters happens in the bond (100 ms), below the switch we present.

## 1. What the switch looks like

| Ports | What | Media / speed reported |
|---|---|---|
| 1-48 (`ports` − `uplink_ports`) | one per guest NIC on the bridge, **cluster-wide** | QSFP28; 100G when the guest runs on this node (virtio/vmxnet3 are memory-bound: Clint's call, "the throughput a VM can get across the virtual switch"), 1G for e1000, 100M for rtl8139; down when the guest is stopped or on another node; an empty cage when no guest is assigned |
| 54 (the last port) | the uplink: the bridge's physical path out — a bond folded into **one link** (UniFi has no active-standby notion), showing the active slave's speed, optic and LLDP neighbour with the bond's counters, so a failover just changes what the port reports | from `ethtool` on the active NIC: media from the transceiver EEPROM (`ethtool -m`) or port type, speed caps from the supported link modes, optic vendor/part/serial |
| 53 | **the host itself**: `vmbr0`'s own interface, with the host's MAC learned on it | 100G; counters are the host's own traffic through the bridge |
| 52 downwards | further physical paths under the bridge (a second NIC or bond), if any | as the uplink; an 802.3ad bond is reported as a LAG |

### 1b. Host network layouts

| Host layout | Ports | Status |
|---|---|---|
| one NIC in the bridge | port 54 | verified |
| active-backup bond | one port (54): the active slave's speed, optic and neighbour, the bond's counters; lldpd announces on the active slave | **verified** (Clint's cluster) |
| LACP (802.3ad) or balance-* bond | one port per member, every member in the same LAG (UniFi's aggregate), each with its own counters, optic and LLDP; the first member carries the MAC table; lldpd announces on every member with its own port number | **modelled, not verified live** — no host here has one (the two NICs go to different switches). Unit-tested against the captured bonding text with the mode line changed. If you run LACP, check the first run: two aggregated ports at 54 and 52, each with its upstream neighbour |
| several NICs in the bridge, no bond | one port each | modelled |
| a VLAN device on the uplink (`bond0.10` as the bridge port) | the device underneath, looked through | **modelled, not verified live** |
| several bridges (`vmbr1`…) | one `switches:` entry per bridge (`options.bridge`) | verified for `vmbr0` |
| Open vSwitch bridges | not supported (no `bridge`/`ip` view of the ports) | — |

**The switch is not the host.** The device identifies itself with the
bridge MAC's locally-administered form (`02:00:00:00:00:96` →
`02:00:00:00:00:97`), because the controller treats a MAC as either a
device or a client: with the host's own MAC as the device, the host itself
vanished from the client list, the topology and local DNS. With the derived
identity the node is an ordinary client behind port 53 of its own switch,
name, IP, DNS record and all, which is also the truthful picture of a
hypervisor behind a virtual switch. lldpd announces the same derived MAC
(§4).

**Numbering is cluster-wide and stable.** Every node reads every guest's
config from `/etc/pve/nodes/*/{qemu-server,lxc}/*.conf` (Proxmox
replicates it), so VM 100 is port 1 on all three switches and lights up on
the node it runs on; a migration moves it from one switch's port 1 to
another's. The assignment is seeded in (vmid, net) order and persisted in
`state/<name>/proxmox-ports.json` next to `device.json`; it only grows. A
deleted guest releases its slot, but the slot is reused only when no
never-used slot remains (oldest release first), so the controller's port
config for a dead VM does not land on the next VM created. **Keep that
file with `device.json`**: without it a re-seed could number a VM
differently and, with `control.ports` on, apply another port's VLANs to it.

Port names are provisioned as `VM-<id>` / `CT-<id>` (`VM-119 net1` for a
multi-NIC guest; the guest's own name is what the controller shows as the
client behind the port) and the NIC name for uplinks; the device is named
`pve-<node>` so it never collides with the node's own DNS name (§6). A guest on another node still gets its name here.

## 2. What is read (every inform, one SSH exec)

`internal/drivers/proxmox/collect.sh` runs on the node and prints tagged
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
| `stat`, `meminfo`, `uptime`, `dmi`, `pveversion`, `chrony` | `/proc`, DMI, `pveversion` | CPU % (delta between polls), memory, uptime, serial (Dell service tag), version, NTP servers |

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

**Before turning `control.ports` on for a guest that already has a tag**
(here VM 119's `tag=8`/`tag=9`), set that port's native VLAN in UniFi
first: the controller's default port profile is "All", and the reconcile
would strip the tag. `control.ports` accepts a list, so start with the
ports you have aligned.

Switch-wide: IGMP snooping (`control.igmp`) toggles the bridge's
`multicast_snooping` (bridge-wide: on when UniFi enables it on VLAN 1, else
on any managed VLAN); NTP (`control.ntp`) writes
`/etc/chrony/sources.d/switch-to-unifi.sources` and `chronyc reload sources`.
STP requests are logged once (a Proxmox bridge runs `bridge-stp off` by
design), syslog is logged (journald has no remote target), reboot is never
real (`Rebooter` is not implemented: the controller's Restart is emulated
whatever `control.reboot` says), SSH keys are not installed.

Capabilities claimed (`switch_caps`): IGMP snooping only, so the UI hides
storm control, FEC, LAG, mirroring, STP options and isolation for these
switches; speed pickers offer the one speed each port has.

## 4. Topology: lldpd on the node, configured by the driver

The controller places a switch by the LLDP frames the *upstream* switch
receives on the port the node is cabled to, so the node has to emit them:
that is the one thing installed on a node (`apt-get install lldpd`). The
driver keeps lldpd's configuration itself (`/etc/lldpd.d/switch-to-unifi.conf`,
rewritten and lldpd restarted whenever it differs; `manage_lldpd: "false"`
to opt out):

- chassis ID = the switch's derived device MAC (`configure system chassisid`;
  the "local" subtype, which is what UniFi switches themselves advertise);
- announce only on the bond's active slave: with both slaves of an
  active-backup bond announcing, the controller drew the nodes under the
  backup link's switch (aggregation-secondary, 10G), not the one carrying
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
      # ports: "54"             # default; uplink_ports: "6" (host + NICs at the top)
      # manage_lldpd: "false"   # leave lldpd alone
      # ssh_key: /etc/switch-to-unifi/id_ed25519       # in a container
      # known_hosts: /etc/switch-to-unifi/known_hosts
    control:
      ports: all                # port state and VLANs -> qm/pct set; see §3 before "all"
      igmp: false
```

In a container (`deploy/compose.yaml`), mount the private key and a
`known_hosts` holding each node's host key read-only and name them in
`options`; the container has no home directory or agent, and the key must
be readable by uid 65532. Keep `state/<name>/proxmox-ports.json` with
`device.json` (§1).

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
- The controller names a new `USWF07D` "ECS Core" (its own display name,
  absent from the catalogue); `unifimodel.ControllerDisplayNames` lists
  such names so the first provision can rename the device after the node.
- Ports with `sfp_found: false` (unassigned slots) draw as empty cages; a
  stopped guest's port draws as a cabled, down port.
- **Disabling a port through the REST API** needs the whole combination the
  UI writes, or the controller silently normalises `forward` back to
  `all`: `{"forward":"disabled","port_security_enabled":true,
  "port_security_mac_address":[],"native_networkconf_id":"",
  "tagged_vlan_mgmt":"block_all"}`. The UI's Port State → Disabled radio
  does exactly that and the push then carries `switch.port.N.status=disabled`.
