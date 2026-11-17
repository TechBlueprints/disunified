# The bridge on the Proxmox node itself -- specification

Status: built and running (2026-11-17). Sections 3 to 6 are built; section 8
is what ran live when a three-node cluster was moved.

The `proxmox` driver was written to run somewhere else and reach each node
as `root@node` over SSH ([`proxmox.md`](proxmox.md) §5). This document moves
the bridge onto the node: one bridge per node, each configured for that node
only, the way the SONiC bridge moved onto its switch
([`sonic-on-switch.md`](sonic-on-switch.md)). It reuses that work:
`internal/sshrun.Local`, `scripts/build-deb.sh`, `deploy/deb/*`.

## 1. Why, and what does not change

A bridge that runs elsewhere depends on that host and on a root SSH key to
every hypervisor, and one process serving a whole cluster restarts every
node's bridge together. On the node, the bridge lives and dies with the
switch it presents, holds no key, and is updated one node at a time.

What does not change: the driver's commands, the model, the port numbering,
the controller's view. A node's adopted key moves with its state file; no
device is re-adopted.

## 2. How it runs

A root systemd service on the Proxmox host, installed from the `.deb`. Not a
container: the driver needs the host's own network namespace (taps, the
bridge, its FDB), `/sys`, `/etc/pve`, `qm`/`pct`, and writes lldpd, chrony
and nftables state. A container (LXC or otherwise) has none of that and would
have to SSH back into its own host as root; an LXC is also a guest on the
bridge and so a port on the switch it presents.

| What | Where |
|---|---|
| binary, unit | `/usr/sbin/disunified`, `disunified.service` (root) |
| `config.yaml` (one device: this node), `env` (the controller API key, 0600) | `/etc/disunified/` (0700) |
| adopted key | `/var/lib/disunified/state/<name>/device.json` |
| reply log | `/var/lib/disunified/inform-log/<name>/` |
| log | journald (`journalctl -u disunified`) |

Nothing here is lost by a Proxmox upgrade: `/etc` and `/var/lib` are the
node's own disk, and the package stays installed.

## 3. The cluster: how three bridges share port numbers

There is no channel between the bridges and no leader. pmxcfs (`/etc/pve`,
replicated by corosync) is the only shared medium, as it already is for
three devices in one process ([`proxmox.md`](proxmox.md) §1c): the three
collectors share no memory today either.

| Event | Behaviour |
|---|---|
| Normal | Each bridge presents its own node's bridge and reads every node's guest configs on each poll. Under `numbering: cluster` a guest has the same port number on every node's switch: live where it runs, a down port on the others. |
| Numbering | The port is in the guest's own tags. The assignment is a pure function of pmxcfs content (`tags.go`, `assignPorts`), and only the node that owns a guest writes its tags. |
| Duplicate claim (a clone or a restore carrying tags, two guests created in the same window) | The lower VMID keeps the port on every node; the other guest's owner renumbers it on its next poll. One move, no oscillation. |
| Migration, HA failover | Config and tags move with the guest. Within one poll the destination shows it live on the same port and the source shows the port down. Port config in the controller is per switch: the arrived port is fresh on the destination and writes to it are withheld until the destination's own config for it matches. |
| Node down | Its bridge is down with it and its switch shows offline. The others are unaffected. |
| No quorum | `/etc/pve` is read-only. Reads continue; tag and port writes fail, are logged and are retried when quorum returns. A stale view cannot be written from. |
| `/etc/pve` not mounted (boot before `pve-cluster`, a `pve-cluster` restart during a package update) | The collect fails and is retried (§4.1). |
| A node added | Install the package, write its one-device config; it reads the tags that exist. |

`ports`, `numbering`, `guest_ports` and `model` must agree across the nodes'
config files; §5.2 warns when a tag is ignored because they do not.

`qm set <id> --tags` on the source node right after a migration fails
without touching the guest (§8). Not captured: the same under `lock: migrate`
or `lock: backup`, and on a node without quorum. The design needs only that
it fails without changing another node's guest.

## 4. Required before a node runs its own bridge

### 4.1 An unmounted `/etc/pve` is not "no guests"

**Built 2026-11-17** (`collector.go` `build`, `parse.go`
`crossCheckGuests`/`nodeName`; tests in `pmxcfs_test.go` and
`internal/informloop/replay_proxmox_test.go`), fixture-tested only; the
unmounted-namespace check of §7 step 2 is the live test. The errors read:

- `proxmox startup: proxmox: /etc/pve/.vmlist is empty: pmxcfs is not
  mounted (is pve-cluster running?); no guest can be read` (at `Start`;
  without the `proxmox startup:` prefix on a later poll);
- `proxmox: /etc/pve changed under the poll (or pmxcfs disagrees with
  itself): qemu 100 has a config under proxmox-2 but is not in .vmlist`
  (each differing guest named, `; `-separated);
- `proxmox: collector output lacks section "lxc"` (or `vmlist`, `qemu`).

Without the guard the loop test shows the hole mid-run as well as at
start: the empty snapshot is a layout change, and the loop's
`OnLayoutChange` provision runs from it. No `readlink /etc/pve/local`
section was added; the first label of the hostname is the comparison.

Today, with pmxcfs not mounted, `collect.sh` prints empty `vmlist`, `qemu`
and `lxc` sections (`cat … 2>/dev/null`, globs that match nothing), and
`build` accepts that: it requires only that the `qemu` section is present,
and an empty `vmlist` decodes to nothing. The result is a valid snapshot in
which every guest slot is free. From there the startup provision strips and
disables the override of every guest port in the controller, and the next
push is applied to the guests as disabled ports. The hole exists for the SSH
deployment too (a `pve-cluster` restart under a poll); on the node the
service also races `pve-cluster` at every boot.

Build:

- `build` requires the `vmlist` and `lxc` sections as well as `qemu`, and
  returns an error when the `vmlist` body is empty (the message says pmxcfs
  is not mounted). `/etc/pve/.vmlist` exists on any mounted pmxcfs, with or
  without guests, so a cluster with no guests still passes.
- `.vmlist` is cross-checked against the config headers read by the globs:
  the same ids on the same nodes, both ways. A mismatch fails that poll (a
  guest created, destroyed or moved between the two reads costs one poll).
- `Start` then fails and `main` retries it with backoff; a mid-run failure
  keeps the previous snapshot, and nothing is provisioned or applied from a
  failed collect.
- Node identity: the node's name is compared with `/etc/pve/nodes/<name>`.
  `collect.sh` prints the kernel hostname, which may carry a domain. The
  driver compares the first label. (A `readlink /etc/pve/local` section
  would be exact but needs a new capture; take it if a capture is made.)

Tests, all from the existing captures in `docs/fixtures/proxmox-9.1.6/`
(the tests already edit a capture's sections; no hand-written output):

- the capture with the `vmlist`, `qemu` and `lxc` bodies emptied: `Start`
  errors and the `FixtureRunner` records no command;
- the capture as it is passes the cross-check; with one config header
  removed, and with one `.vmlist` entry removed, it errors;
- the capture's hostname line with a domain appended still shows that
  node's guests as local;
- at the loop: a collect that fails after a good one leads to no provision
  call and no apply (the replay rig for this driver is
  `internal/informloop/replay_proxmox_test.go`).

### 4.2 `options.transport`

**Built 2026-11-17** (`internal/drivers/proxmox/driver.go`: `openTransport`,
`openSSH`, `openLocal`; `driver_test.go`, `TestOpenTransport`). Not yet run
on a node. The review of every `Run` call found nothing that assumes an SSH
session; the `options.probe` and `options.state_dir` the bridge injects for
every driver are not read. The config loader and `main` had required
`url:` or `ssh:` of every device and, without either, ran it with no
driver; both now take `options.transport: local` as a transport of its own
(`config.LocalTransport`; `TestLocalTransportNeedsNoURLOrSSH`,
`TestLocalTransportOpensTheDriver`).

`ssh` (the default) or `local`; anything else is refused in `Open`. As in
the `sonic` driver (`internal/drivers/sonic/driver.go`, `openLocal`):

- `local` builds the collector on `&sshrun.Local{}`: every command runs
  through `bash -c` as the service's own user.
- `ssh:` is not required under `local`. If it is set, its user must be the
  user the service runs as (the same refusal text as `sonic`); its host is
  not used.
- `ssh_key` and `known_hosts` are ignored under `local`, with one log line.
- No probe: this driver moves no address and arms no dead-man timer.
- The device's `Describe` string and the package doc comment say both
  transports.

The driver has no dependency on SSH beyond `Open` (no error-string
matching, no session state); every command string is plain POSIX and reads
the same under `bash -c`.

Tests at the `Open` level (there are none today): unset and `ssh` build the
SSH runner without dialling; `local` builds without `ssh:`; `local` with an
`ssh:` naming another user is refused; an unknown value is refused.

### 4.3 The unit and the package

**Built 2026-11-17** (`deploy/deb/*`, `deploy/config.example.yaml`,
`docs/install.md` §7b). The package was staged with `scripts/build-deb.sh
amd64` on a host without `dpkg-deb` and never installed on a node;
`prerm` and `postrm` needed no change (purge removes only the SONiC
drop-in directory (`disunified.service.d`) and a symlinked `/etc/disunified`, never the node's
config or state).

- `deploy/deb/disunified.service`: add `pve-cluster.service` to `After=`
  (a unit name that does not exist elsewhere is ignored, as the SONiC names
  are on a Proxmox node). No sandboxing directive, ever: the driver writes
  under `/sys`, `/etc/lldpd.d` and `/etc/chrony`. This ordering narrows the
  boot race; §4.1 is the guard.
- `deploy/deb/postinst`, off SONiC: create `/etc/disunified` and
  `/var/lib/disunified`, root, 0700, when absent. The SONiC branch is
  unchanged.
- `deploy/deb/control`: the description names both on-device cases.
- `deploy/config.example.yaml`, `docs/install.md`: a Proxmox on-node
  section beside the SONiC one, with §6's config.

## 5. Hardening for three independent writers

### 5.1 A two-writer convergence test

Built 2026-11-17 (`converge_test.go`). The two captures
(`collect-node1.txt`, `collect-node2.txt`) hold the same guest set as seen
from two nodes. One collector runs per capture, and each runner's tag
writes land in both captures (`FixtureRunner.Peers`, as pmxcfs replicates
them); a round has both read before either writes. What it showed, with
`assignPorts` unchanged: no duplicate port and the same port for every
guest NIC on both nodes after the first round, each node writing only its
own guests' tags; a second round that writes nothing on either side; and a
forced duplicate (a higher VMID given a lower one's tag, once with the
loser on each node) rewritten once, by the loser's owner only, the winner
kept, and settled the round after. No divergence was found, so no rule was
added; none depends on a process's own memory.

### 5.2 Drift between nodes is visible

Built 2026-11-17 (`tags.go`, `collector.go`, `driver.go`;
`hardening_test.go`).

- A well-formed `unifi.` tag that is ignored for range (above this node's
  guest slots) or scope (the other numbering) is logged once per guest and
  tag (`warnOnce`), on every node that reads it; its owner replaces it.
- `options.guest_ports` fixes the number of guest slots (the count `n` that
  `assignPorts` takes) instead of deriving it as `ports` minus this node's
  physical NICs; `Open` takes 1 to `ports`-1, and `Start` fails when the
  node's NICs do not fit above it. Unset keeps today's derivation. It is
  the setting for a cluster whose nodes have different NIC counts.
- When a duplicate claim is resolved, one log line on each node names both
  guests and the port (`port 1: VM 100 keeps it, VM 119's claim ... is
  dropped and proxmox-1 renumbers it`).

### 5.3 Failures stay contained

Built 2026-11-17 (`tags.go` `writeTags`, `apply.go`; `hardening_test.go`,
with `FixtureRunner.Fail`).

- A tag write that fails is tried every poll and logged once (the command
  and the error) until it succeeds, which is logged once too.
- `ApplyPorts` continues past a port whose write fails, runs the aggregate
  and BPDU-guard steps as well, and returns the first error at the end
  (with a count of the others), so a node without quorum, or one guest
  under a backup or migrate lock, does not block every later port. The
  loop still sees an error: the push is not marked applied and is applied
  again on the next inform, and the diff then writes only what failed
  (the snapshot already holds what succeeded; a BPDU-guard setting is
  recorded only once mstpd has taken it).

### 5.4 Templates

Built 2026-11-17 (`tags.go` `untagTemplates`). A template is not a port,
but a `unifi.` tag left on one is inherited by every clone. The owner node
removes this bridge's `unifi.` tags from its templates (operator tags and
other bridges' tags kept, in order); a template left with no tag gets `qm
set <id> --delete tags`. Neither capture holds a template: the test makes
captured guests templates (`template: 1`) the way other tests edit
captures. Not run live: whether `qm set` on a template takes `--tags` and
`--delete tags` (expected: tags are not a disk change). A template with no
NIC is not seen, since the parser emits a guest through its NICs.

### 5.5 The loop: a snapshot that changes between start and the first cycle

**Built 2026-11-17** (`internal/informloop/layout.go`).

`informloop` treats a port whose occupant changed as fresh and withholds
writes to it, but its layout signature started empty, so the first collect
of a run was never compared with the snapshot the startup provision used:
a guest that arrived between the two got the stored push for its port (the
failing test, on two captures of one node: `qm set 999 --net0 …,tag=2,…`
written to a guest that was on another node when the bridge started).

The loop now starts with a baseline, and the first collect is compared with
it like every later one:

- **The baseline is the previous run's record**, `layout.json` in the
  device's state directory (the layout signature, each port's identity, the
  ports still fresh; written when any of them changes). Without a record
  (the first run of this build, no state directory) it is the startup
  snapshot, the one `main.go` puts in the session before it builds the loop;
  `main.go` hands over nothing new.
- **Between the start and the first collect:** a port whose occupant
  differs is fresh. The seed that follows the change used to fall inside
  the provision's 10 s spacing and be dropped; a layout change now waits
  for the spacing to end (`main.go`, `layoutChanged`).
- **While the bridge was stopped:** a port whose occupant differs from the
  record is fresh at the first collect, though the run's own snapshots
  agree. The node's reboot, a package update and a crash are all this case.
- **Fresh when the bridge stopped:** fresh when it starts again. A restart
  no longer releases a withheld port; the controller's config matching it
  does (the seed, or the port set in the UI), or
  `control.allow_initial_changes: true`, or removing `layout.json` while
  the bridge is stopped.
- **The log line says what changed.** Fresh ports are listed as
  withheld (`new, changed or arrived ports [...] are not written until
  ...`); a change that only frees ports (a guest made a template) names
  them as released and withholds nothing (`ports [...] released, none
  new, changed or arrived`; until 2026-11-17 it printed the withholding
  line with an empty list). `TestLayoutChangeLogSaysWhatChanged`.
- **A start whose first collect equals the baseline is as it was:** no
  seed call, no hold, no log line. Every driver's replay and contract test
  passes unchanged.

Not covered: the first run of this build has no record, so a change made
while the previous build was stopped is not seen (the window before the
first collect is). A record that cannot be read is logged and the run
starts from its own snapshot; one that cannot be written is logged once,
and the ports withheld then are not withheld after a restart. A Forget
drops the fresh set with the controller's config (the first-push hold
covers every port again). The spacing change in `main.go` is compiled and
read, not exercised by a test.

It is a second line behind §4.1, and it closes the same windows for a
restart on any driver.

## 6. The on-node config (documentation values)

**Documented 2026-11-17** in `docs/install.md` §7b and
`deploy/config.example.yaml`.

```yaml
controller:
  host: 192.0.2.1                # the controller, an IP literal
  api_url: https://192.0.2.1
  api_key_env: DUI_UNIFI_API_KEY
  site: default
state_dir: /var/lib/disunified
devices:
  - name: pve-1                  # as adopted; names the state directory
    driver: proxmox
    ip: 192.0.2.11               # this node's in-band address
    model: UDC48X6
    options:
      transport: local
      ports: "54"
    reply_log: {max_size: 8MB, keep: 3}
```

`/etc/disunified/env` (0600) holds `DUI_UNIFI_API_KEY=…`. A node that was
bridged over SSH keeps its entry as it was, less `ssh`, `ssh_key` and
`known_hosts`, plus `transport: local`.

## 7. Moving a node that is already adopted

One bridge per adopted key, at every moment.

1. Install the package on the node (enabled, not started: no config yet).
   Write `config.yaml` and `env`.
2. Before touching the running bridge, on the node:
   - `disunified -config /etc/disunified/config.yaml -collect-once` opens and
     starts the device without informing. Expect the same ports as the
     running bridge reports and no `qm set` and no lldpd write (the running
     bridge already made them).
   - The guard, without touching the cluster: the same command in a private
     mount namespace with `/etc/pve` unmounted there (`unshare -m`, then
     `umount -l /etc/pve` inside it) must fail with §4.1's error.
   - The inform path from the node's own TCP stack: a POST of about 100 KB to
     the inform URL must come back with an HTTP status, not a reset or EOF
     (the lesson of `sonic.md` §12: on the device, its own MTU and MSS are in
     the path).
   - `api_url`'s host resolves on the node, or use the address.
3. Remove the node's entry from the running bridge's config and restart
   that bridge; check its log no longer names the node.
4. Copy `state/<name>/` and `inform-log/<name>/` to `/var/lib/disunified/`
   on the node (root, 0700 and 0600). The reply log holds pushed secrets if
   it predates the redaction (`docs/install.md` §8): move it machine to
   machine, never onto a workstation's disk.
5. `systemctl start disunified`. Expect `resuming adopted state`, the first
   inform answered, CONNECTED; in the controller the same MAC, uplink and
   ports, and a first push applied with nothing changed.

Back: stop the service, copy the state back, restore the entry, restart the
other bridge.

Update afterwards: `dpkg -i` the new package on a node; postinst restarts
that node's bridge only.

Once the nodes have settled on their own bridges, remove the old bridge's
public key from root's `authorized_keys`: nothing dials the nodes any more.
On a cluster that file is one for every node
(`/root/.ssh/authorized_keys` is a link to `/etc/pve/priv/authorized_keys`),
so one edit on one node covers them all. The way back then starts with
putting that key back.

## 8. What ran live (2026-11-17)

A three-node Proxmox VE 9.1.6 cluster, each node adopted and bridged over
SSH from one process elsewhere, was moved one node at a time (§7): a pilot
node first, then the other two together. Package
`disunified_0.0.0+20261117.2e52663_amd64.deb`, packed with `dpkg-deb` on a
node from the tree `scripts/build-deb.sh` stages.

Before the move, on each node:

- A 100 KB POST to the inform URL came back with an HTTP status (400, the
  body being noise); the bridge's MTU is 1500. `api_url`'s name resolved.
- `-collect-once` under `transport: local` reported the node's 54 ports
  with the same guests up as the SSH bridge, and wrote nothing (no
  `qm set`, no lldpd write).
- The guard: the same command inside `unshare -m` with `/etc/pve` lazily
  unmounted in that namespace failed with `/etc/pve/.vmlist is empty:
  pmxcfs is not mounted (is pve-cluster running?); no guest can be read`.
  The node's own namespace kept its mount and its quorum.

The move, per node: the entry removed from the other bridge's config and
that bridge restarted (its log then named the node no more); `state/<name>/`
and `inform-log/<name>/` streamed host to host; `systemctl start`. Each
node logged `resuming adopted state`, `reconciled system_cfg …: 0 of 54
ports changed` and `inform: HTTP 200` within two seconds of starting, and
the controller showed the same MAC, uplink and up ports. No device was
re-adopted. A device that hangs off a node's guest port kept its placement.

Coordination, with a throwaway diskless guest (`qm create … --net0
virtio,bridge=vmbr0`), first with one node local and two over SSH, then
with all three local:

| Step | Seen |
|---|---|
| Guest created on the local node | That node alone ran `qm set <id> --tags 'unifi.p29.c'`; the two SSH bridges showed port 29 for it on their next poll and wrote nothing. |
| Port disabled, then enabled, in the controller | `qm set <id> --net0 '…,link_down=1'` one cycle later (`1 of 54 ports changed`), and the option removed again on enable. |
| Offline migration to another node | Tags unchanged. `qm set <id> --tags …` on the source right after: `Configuration file 'nodes/<source>/qemu-server/<id>.conf' does not exist`, exit 2. A stopped guest has no tap, so neither switch's port changed identity and neither bridge logged anything. |
| Guest started, live migration onto the local node | Same port, same tag. The destination logged the port as arrived (`29(vm<id>-net0/tap<id>i0)`, "not written until the controller's config matches") within one poll; the source logged it gone. |
| Live migration between two local bridges | The same, with the third bridge silent. |
| Guest stopped and made a template | Its owner ran `qm set <id> --delete tags` on the next poll and the slot was released on every node. |
| Guest destroyed | Port 29 back to `Open-29`, disabled. |

An update ran the same day: `dpkg -i` of a newer package on one node at a
time restarted that node's bridge alone (`resuming adopted state`, `0 of 54
ports changed`, `inform: HTTP 200` within two seconds). The SSH bridge's
key was then removed from the cluster's `authorized_keys` and is refused by
every node.

Not run: a node rebooting into its own bridge (the unit's ordering after
`pve-cluster` and the guard cover it; to be read from the journal at each
node's next reboot), a `pve-cluster` restart under a running bridge, and a
node without quorum.
