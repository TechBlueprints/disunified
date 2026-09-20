# Proxmox VE driver (`proxmox`)

A Proxmox VE node's Linux bridge (`vmbr0`) presented to a UniFi Network
controller as a 32-port 100G switch (`USWF07D`, which Network 10.6 calls
"ECS Core"). Guest NICs are the ports; the physical NICs the bridge uplinks
through are the last two. Written and verified against **Proxmox VE 9.1.6**
(Debian 13, kernel 6.17) on Clint's three-node cluster, 2026-09-19/20.
Scrubbed captures of the collector's output are in
`docs/fixtures/proxmox-9.1.6/`.

## 1. What the switch looks like

| Ports | What | Media / speed reported |
|---|---|---|
| 1-30 (`ports` − `uplink_ports`) | one per guest NIC on the bridge, **cluster-wide** | QSFP28; 100G when the guest runs on this node (virtio/vmxnet3 are memory-bound: Clint's call, "the throughput a VM can get across the virtual switch"), 1G for e1000, 100M for rtl8139; down when the guest is stopped or on another node; an empty cage when no guest is assigned |
| 31-32 (`uplink_ports`) | the physical NICs under the bridge (bond slaves, primary first, then direct members) | from `ethtool`: media from the transceiver EEPROM (`ethtool -m`) or port type, speed caps from the supported link modes, optic vendor/part/serial |

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

Port names are provisioned as `<vmid> <name>` (`119 FusionHub net1` for a
multi-NIC guest) and the NIC name for uplinks; the device is named
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

## 4. Topology: lldpd on the node

The controller places a switch by LLDP: the upstream UniFi switch must see
the node's chassis ID (= the bridge MAC) on the port it is cabled to. A
stock node sends no LLDP, so install lldpd, pin its chassis ID to the
bond's primary slave (lldpd otherwise picks the unused onboard NIC's MAC),
and **announce only on the primary slave**: with both slaves of an
active-backup bond announcing, the controller drew the nodes under the
backup link's switch (aggregation-secondary, 10G) instead of the one
carrying the traffic (seen 2026-09-20). The port ID "Port 31" is the port
number the driver gives the primary NIC, so the parent's view names it.

```bash
apt-get install -y lldpd
echo 'DAEMON_ARGS="-C ens1f0np0"' >> /etc/default/lldpd
cat > /etc/lldpd.d/switch-to-unifi.conf <<'EOT'
configure system interface pattern ens1f0np0
configure lldp portidsubtype ifname
configure ports ens1f0np0 lldp portidsubtype local "Port 31"
configure ports ens1f0np0 lldp portdescription "ens1f0np0"
EOT
systemctl restart lldpd
```

Verified 2026-09-20: the aggregation switch (`USWF066`) lists all three nodes
in its `lldp_table` and `downlink_table` (ports 50-52, `port_id "Port 31"`);
the nodes see the aggregation switch on the 100G slave and
aggregation-secondary on the 10G slave. The driver picks the uplink port
from the neighbour with the Router capability (the aggregation switch), and
falls back to the bond's active slave when LLDP is silent
(`Snapshot.UplinkHint`).

## 5. Setup

```yaml
switches:
  - name: proxmox-2
    driver: proxmox
    ssh: root@proxmox-2        # key auth; options.ssh_key / known_hosts for a container
    ip: 192.0.2.102             # the node's in-band address (what the controller reaches)
    model: USWF07D             # auto picks it too: 32 QSFP28
    options:
      bridge: vmbr0            # default
      ports: "32"              # default; uplink_ports: "2"
    control:
      ports: all               # or a list; see §3 before "all"
      igmp: false
```

Requirements on the node: root SSH with a key, a VLAN-aware bridge
(`bridge-vlan-aware yes`; the driver reads a plain bridge too but tags
then mean per-VLAN bridges, which it does not model), `ethtool` (installed
by default), optionally `lldpd` (§4) and `chrony` (default).

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
