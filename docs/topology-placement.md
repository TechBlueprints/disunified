# How the controller places a bridged device in the topology

What "Parent Device", "Connected To" and the topology map are built from, and
therefore what a bridged device must send — and must *be* — to get a parent.
Learned the hard way twice: the Arista (2026-09-20) and the APC UPS
(2026-09-26). Everything below was verified on Network 10.6.106.

## 1. Two separate mechanisms

**Reachability fields** tell the controller *where in the IP network* the
device is. A real device sends, and a bridge must send:

| Key | What it is | Where the bridge gets it |
|---|---|---|
| `connect_request_ip` | the device's own address | the address the bridge dials (`ip:` / `url:`) |
| `netmask` | its mask, dotted | the device (Arista/PDU: its own interface; UPS: operator option) |
| `gateway_mac` | **the gateway's L2 address as the segment sees it** | the device's ARP table (Arista, PDU) or `ip neigh show <gw>` from a host on that segment (UPS) |
| `if_table` | its management interface, named | the device layer renders `eth0` from the uplink port |
| `uplink` | **a string**: the name of that interface, `"eth0"` | the device layer; an object here is silently ignored |

These are necessary. They are not what draws the line to a parent.

**The parent edge** comes from the *upstream UniFi switch*: it reports the
MACs it has learned on each port, and the controller places a device on the
port where the device's MAC appears (for switches, LLDP chassis IDs add to
this). The stored record then carries `uplink.uplink_mac` (the upstream
switch) and `uplink.uplink_remote_port`. A record with the reachability
fields but no `uplink_mac` is exactly a device the controller cannot find on
any port.

## 2. What follows for a bridged device

- **Its reported MAC must be a MAC the LAN actually sees.** The Arista and
  the PDU report their real MACs and are placed on the port that sees them.
  The UPS, adopted under a **synthetic** locally-administered MAC (so the
  real port's client record and DHCP reservation survive), sends every
  reachability field correctly — verified field by field in the controller's
  own record — and is never placed: no switch has ever seen that MAC.
  `gateway_mac` was wrong at first and was fixed to the segment's real
  address (`…:a4`, not the controller's listed device MAC `…:a3`); that
  changed nothing, which is what eliminated it as the cause.
- **The address must be in-band**, behind the same port the MAC is seen on.
  An OOB management address puts the IP behind one switch and the identity
  behind another, and the controller places nothing (the Arista lesson,
  `docs/adding-a-device.md` §2c).
- **A device-side `lldp_table` does not substitute** for being seen. "Port N"
  LLDP port IDs and neighbour keys inside `uplink` were tried on the Arista
  and ruled out; the fix was the uplink string plus reachability plus a MAC
  the aggregation switch sees.

## 3. The trade the UPS makes, and how to undo it

Adopting under the real MAC (`options: mac`) places the device on the switch
port that carries it — and **deletes the unit's client record, its fixed-IP
reservation and its client DNS name** (the PDU lost its address this way the
morning after). Nothing can push an address onto the UPS (Modbus carries no
IP configuration), so the address is then held only by the DHCP server's
habit of re-leasing the same address to the same MAC — reliable in practice
until the lease database is reset — and by the operator updating `url:` if it
ever moves. The bridge's start retry rides out a move until then.

To switch: forget the device in the controller (or `rest/device` delete),
back up and remove `state/<name>/device.json`, set `options: mac` to the
real port's address, restart; the controller answers HTTP 400 for about a
minute after a forget, then the device is pending again (`CLAUDE.md` §7).

## 4. How to verify, without the UI

```
stat/device[mac=<bridged mac>].uplink.uplink_mac        # set => placed
stat/device[mac=<bridged mac>].uplink.uplink_remote_port
stat/device[mac=<bridged mac>].gateway_mac              # what it stored, vs what you sent
stat/sta[mac=<real mac>].sw_mac / .sw_port              # where the LAN actually sees the real port
```

If `uplink_mac` is empty after two inform cycles and the reachability fields
read back correctly, the MAC is not being seen; nothing else in the payload
will fix that.
