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
  not. Decided, not forgotten.
- **Slots live on the host** (`/var/lib/disunified/podman-slots.json`),
  keyed `<container name>/<network>` -- names survive a recreate, ids do
  not. Written only on change (`mkdir -p && cat > tmp && mv`); the fixture
  runner applies that write to its own `slots` section so tests see the
  file read back. Removed container = slot freed; stopped = kept, link down.
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
  `ps`/`inspect`/`netns` within one capture).
- **Address control writes NetworkManager** (`nmcli con mod` + `nmcli device
  reapply`, no bounce), only for an address the uplink already carries, and
  refuses DHCP -- the bridge reaches this host by that address and, on site,
  runs on it. The fixture runner answers `nmcli -g ipv4.method con show`
  from its `nmipv4` section and applies `con mod` to it.
