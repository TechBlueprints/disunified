# podman — facts that cost time

Read `internal/drivers/CLAUDE.md` first. Full write-up: `docs/drivers/podman.md`.
Built 2026-09-27 on the proxmox driver's pattern; shares its SSH transport
(`internal/sshrun`).

- **The host is the switch**, like a Proxmox node: uplink NIC's MAC, the
  host's address and hostname, `UDC48X6`. One port per *endpoint*
  (container × network), slots below the NICs (one NIC = 53 slots).
- **Only macvlan/ipvlan endpoints carry a MAC entry.** A bridge-network
  container's MAC is behind NAT; reporting it makes the controller invent a
  wired client with no address. The port exists and counts; the client does
  not. Decided, not forgotten. Its `LearnedAt` is the container's start
  (the reduced inspect's `started`, already read: never widen inspect for
  it), `LastSeen` the collect time (`docs/mac-table-times.md`). Tested
  from `collect-macvlan.txt` (the bridge's own container on the macvlan
  network); `collect.txt` has no LAN container and yields no entry. An
  unparsed `started` has no capture and no test.
- **Slots live on the host** (`/var/lib/disunified/podman-slots.json`),
  keyed `<container name>/<network>` -- names survive a recreate, ids do
  not. Written only on change (`mkdir -p && cat > tmp && mv`); the fixture
  runner applies that write to its own `slots` section so tests see the
  file read back. Removed container = slot freed; stopped = kept, link down.
- **A container port's `IfName` is `ct<8 hex>`**, a hash of the endpoint
  key (`endpointIfName`), never the label: an interface name has no
  whitespace and at most 15 characters (`docs/wire-shapes.md` §6). The
  label is `Name`/`Description`, which is also the inform's port name, so
  the change (2026-11-12) moved nothing in the controller.
- **`collect.sh` reduces `podman inspect` on the host.** The full record
  carries every container's `Env` -- the API key of this very bridge is in
  there. The script prints only the fields the driver reads. Never widen it
  to raw `podman inspect` output, and never commit a raw capture: the
  first (unreduced) capture went to the scratchpad only.
- **Counters come from inside the container's netns** (`nsenter --net=<SandboxKey>
  ip -j -s link`), which works for macvlan endpoints that have no host-side
  interface. Endpoint ↔ interface is matched by MAC (netavark leaves
  `InterfaceName` null). Turned around to the switch's view.
- **A container sharing another's netns** (`--network container:x`) has no
  `networks` in `ps` but shows the shared endpoint in `inspect` (same MAC
  and address as the owner); it gets its own port.
- Virtio uplink: kernel speed `-1` → shown as 100G QSFP28; a NIC with a
  real ethtool speed gets that speed and media. No `fec` on it (a port-level
  contract omission, `runContractPorts`).
- The Podman host on site is a QEMU guest of a Proxmox node and holds a
  fixed-IP reservation + client DNS record that adoption will delete:
  make its address static on the host first (`docs/drivers/podman.md` §5).
- Scrub: `scripts/sanitize-podman.py` maps container/network names to
  `app-N`/`net-N` (keeps `disunified*`), the host to `host-1.example.net`,
  MACs/IPs/UUIDs/machine-id. Container ids stay (random, needed to join
  `ps`/`inspect`/`netns` within one capture). A network's `driver` stays,
  so a scrubbed macvlan network is still one. `N` is per capture: `net-2`
  in one file need not be `net-2` in another.
- **Taking a capture** is one read-only exec of the driver's own script,
  `ssh root@<host> "bash -s -- '' /var/lib/disunified/podman-slots.json" <
  internal/drivers/podman/collect.sh > <scratchpad>/raw.txt`, then the
  scrub into `docs/fixtures/podman-<ver>/`. The script only reads, and its
  inspect is already reduced. The raw output stays in the scratchpad.
- **Address control writes NetworkManager** (`nmcli con mod` + `nmcli device
  reapply`, no bounce), only for an address the uplink already carries, and
  refuses DHCP -- the bridge reaches this host by that address and, on site,
  runs on it. The fixture runner answers `nmcli -g ipv4.method con show`
  from its `nmipv4` section and applies `con mod` to it.
- **The reported address is the host's LAN address, not the dial target**
  (`CurrentAddress`, an `AddressReporter`, 2026-11-15): a bridge on the host
  dials it at a podman bridge's gateway, which must never be reported. The
  uplink's address, the static profile's when NetworkManager holds one; the
  loop follows it when it changes, and `ip:` only overrides it until then.
- **No rogue DHCP detection, by decision (2026-11-14).** macvlan's bridge
  mode (netavark's default) delivers a container's frames to the host's
  other macvlan containers inside the driver, broadcast offers included,
  without crossing the parent NIC's egress: no single hook sees every reply,
  and a guard on the parent would sit on the host's only NIC, which carries
  the deployed bridge (on site the one macvlan container). Not claimed. No
  storm or DHCP-rate limiter either; `anomalies` stays 0
  (`docs/drivers/podman.md` §8).
- **UniFi's client Block is an nft `netdev` table on the uplink NIC**
  (`block.go`, 2026-11-17, `docs/drivers/podman.md` §9), not proxmox's
  bridge table: the LAN side is a plain NIC with macvlan children, and a
  bridge-family hook would see only the NAT'd containers. Ingress (before
  the macvlan receive handler) and egress, `ether saddr` drop; the host's
  own MAC is never written. Two macvlan containers on one parent still
  talk (bridge mode). Fixture-tested, and loaded by the deployed
  driver 2026-11-17 (written once, read back by its comment); a dropped
  frame is not yet seen, and the listing of a present table is not yet a
  fixture, so the fixture runner does not model the write.
