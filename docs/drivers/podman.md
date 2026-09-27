# Podman host as a switch (`podman`)

A Podman host presented to the controller the way a Proxmox node is: the
host is the switch, every container network endpoint is a port named after
the container, the host's NICs are the top ports with the default route's
NIC as the uplink. Claimed as `UDC48X6` ("USW Leaf", 54 ports) like the
nodes, so a container host and a VM host look the same in the topology --
and a Podman host that is itself a VM hangs off its node's `VM-<id>` port
with its containers under it.

Verified on Podman 5.8.2 / AlmaLinux 10.2, rootful, netavark, on a host that
is a QEMU guest of a Proxmox node (2026-09-27). Read-only: no port control.

## 1. What a port is

One port per **endpoint** -- a container on a network. A container on two
networks is two ports (`name lan`, `name podman`). Two kinds, told apart by
the network's driver:

| Network | Port | Client behind it |
|---|---|---|
| `macvlan` / `ipvlan` (the container has its own LAN address and MAC) | up when running, 100G | **yes**: the endpoint's MAC is reported on the port, so the controller shows the container as the wired client there -- exactly like a VM on a node |
| `bridge` (Podman's default: private subnet, NAT behind the host) | up when running, 100G, counted | **no**: the LAN never sees that MAC; reporting it would make the controller invent a client with no address. The port is there, named and counting traffic, and that is all it honestly can be |

A container that shares another's network namespace (`--network
container:x`) shows the shared endpoint under its own name too.

Stopped/created containers keep their port and show link down. Removed
containers free their slot. Free slots are reported as empty, disabled ports
named `Open-<n>` (the proxmox convention).

**Slots.** The endpoint → port mapping is kept on the host in
`/var/lib/disunified/podman-slots.json` (`options.slots_file`), written
only when it changes: the Podman analogue of the proxmox driver's guest
tags, so a container keeps its port across its own restarts, recreates and
the bridge's redeploys. New endpoints take the lowest free slot in container
creation order; slots below the NICs are all open to containers (one NIC =
53 container slots).

**Counters** are the container interface's, read inside its network
namespace (`nsenter --net`), so bridge and macvlan endpoints alike have
them; they are turned around to the switch's view (the container's tx is
the port's rx). Speed is 100G for a veth, as the proxmox driver shows a
virtio guest -- memory-bound, not a wire.

## 2. The host

Identity: hostname, the OS (`PRETTY_NAME`) as the model, Podman's version as
the firmware, the DMI product serial/UUID (or the machine id) as the serial,
the uplink NIC's MAC as the device MAC. Reachability: the uplink NIC's
addresses and whether they are leases, the default gateway and its MAC from
the neighbour table -- the podman bridges' own gateway addresses are
internal and deliberately not reported. Health: CPU, memory, load, uptime;
no fans or temperature (said outright: `has_fan`/`has_temperature` false),
no PSU, no spanning tree.

The uplink NIC: speed from ethtool when the kernel knows it; a virtio NIC
(speed `-1`) is shown as 100G, the way its node shows it.

## 3. Reading

One SSH exec of `collect.sh` per poll, root on the host, key auth (the same
transport as the proxmox driver, `internal/sshrun`). The script prints
tagged sections; the container records are **reduced on the host before
they leave it**: `podman inspect` carries every container's environment,
which is where API keys live, so only the fields the driver reads are
printed (ids, names, state, networks, endpoints' MAC/address, the netns
path). Nothing prints `Env`, `Cmd`, `Args` or mounts.

## 4. Configuration

(For a bridge running on the host it presents, see §4b.)

```yaml
devices:
  - name: podman-host
    driver: podman
    ssh: root@podman.example.net      # key auth; the ssh-agent, or options.ssh_key
    model: UDC48X6                    # USW Leaf, as the Proxmox nodes
    options:
      ssh_key: /etc/disunified/ssh/id_ed25519
      known_hosts: /etc/disunified/ssh/known_hosts
      # uplink: enp6s18                # default: the NIC carrying the default route
      # slots_file: /var/lib/disunified/podman-slots.json
```

The bridge's public key goes into the host's `root` `authorized_keys`; the
host's key into `known_hosts`. A bridge running **on** the host it presents
reaches it over SSH like any other host (the container's own address is on
a podman bridge; the host answers on its LAN address).

## 4b. Running the bridge on the host it presents

The bridge that presents the Podman host can run *on* it, as one of its
containers; it then appears as a port (and, on macvlan, a client) of the
switch it is bridging. Facts that cost time doing that (2026-09-27):

- **Two networks.** A macvlan container cannot reach its own host (kernel
  rule: children and the parent's IP stack are isolated), so the container
  keeps a leg on a Podman bridge network and the device entry targets the
  host's address on *that* network (`ssh: root@10.89.x.1`; the bridge
  network's gateway address is the host). The macvlan leg gives it a LAN
  address of its own, which is what makes it a client behind its port and
  what a NUT server (`docs/drivers/apc-ups.md` §12) answers on.
- **`mac_address` applies to the first network in the list** -- put the
  macvlan first, or the fixed MAC lands on the bridge leg and the LAN leg
  gets a random MAC and a new lease on every recreate.
- **A lease can move on a recreate** even with a fixed MAC (the old lease
  is still held under a different hostname); pin it with a client
  reservation -- a container is a *client* in the controller, so a
  reservation and a local DNS record work for it, unlike for an adopted
  device (§5) -- and point clients at the name.
- **compose:** `networks: [default, lan]` on the service needs a top-level
  `networks: default: {}` (podman-compose otherwise fails with "missing
  networks: default"), and `podman-compose up --force-recreate` with a bad
  networks list removes the container *and* its bridge network before
  failing -- every device in that container is offline until the next
  attempt succeeds. Check a compose change with `podman-compose config`
  before recreating a container that holds adopted keys.
- **Counters and MACs still come from inside the netns** (`nsenter`), so the
  bridge's own endpoints report like any other container's.

## 5. Adopting -- what it costs

The host is the device: adopting it under its own MAC **deletes its client
record, any fixed-IP reservation and its client DNS record** (the UPS and
PDU precedents, `docs/drivers/apc-ups.md` §7). A host that serves things at
that address wants its address configured statically on the host
(NetworkManager `ipv4.method manual`) before adoption, not held by a lease
the controller is about to forget. Placement then follows the MAC on the
upstream port within a minute; for a host that is a VM, that port is its
node's `VM-<id>` port.

## 6. Fixture and tests

`docs/fixtures/podman-5.8.2/collect.txt` is a live capture scrubbed by
`scripts/sanitize-podman.py` (MACs, addresses, the host and domain names,
container and network names other than the bridge's own, DMI/machine ids).
`internal/drivers/podman/collector_test.go` drives the driver against it;
`internal/device/contract_podman_test.go` holds its payload to the wire
contract (omissions: PSU, root switch, FDB capacity, fans, temperature, STP
priority; the uplink's FEC).

## 7. Address control (`control.address`)

The controller's IP Settings for the device are applied to the host's
uplink through NetworkManager: `nmcli con mod <profile> ipv4.method manual
ipv4.addresses A/P ipv4.gateway G ipv4.dns D ipv4.ignore-auto-dns yes`,
then `nmcli device reapply <nic>`, which applies it to the live interface
without bouncing it (the SSH session this runs over survives), and the
profile's method read back. Two guards, because the host is the address the
bridge reaches it by (and, on site, the host the bridge runs on): a static
address is applied **only if the uplink already carries it** -- "make the
lease permanent", never "move the host" -- and **DHCP is refused** (after
adoption the controller no longer holds the reservation, so a lease could
land anywhere). Idempotent. This is the answer to "adoption deletes the
reservation": adopt, then set IP Settings → Static with the same address,
and UniFi owns the host's address from then on.

## 8. Not done

- **Port control.** Port state could map to the container's veth (`ip link
  set … down` inside the netns) or to `podman network disconnect`; names
  are the container's. Nothing is written today; `Capabilities{}`.
- **LLDP.** No `lldpd` on the host; placement works by MAC, and the
  upstream device sees the host as a client on its port, which is what a
  switch behind a switch looks like.
- **Bridge-network clients.** A way to show the NAT'd containers as
  something (they have addresses on the host's private subnets) without
  inventing LAN clients has not been found; the port with its counters is
  the honest limit.
