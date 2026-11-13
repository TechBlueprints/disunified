# MAC table times: `age` and `uptime` are two different things

Status: **built 2026-11-13** (specified the same day). The bridge reports a
learned address's learn time and its last-seen time separately; §5 is what
the code does, §6 the tests, §7 the live checks after the deploy.

## 1. What went wrong

A site's monitoring reported that every guest behind a bridged hypervisor
"dropped and re-associated" during a controller update. Nothing had dropped:
the bridge logged one refused inform per device and a provision that changed
no port. The controller's client records said otherwise, and they said so
because of what the bridge reports for each learned address.

## 2. What a real switch sends

Each `mac_table` entry carries two times, in seconds:

- `age`: since the last frame from that address. Small for a live client.
- `uptime`: since the address was learned on that port. Large; bounded by
  the switch's own uptime.

In the three captured informs that carry a table
(`docs/fixtures/controller-10.6.106/inform-ecs-aggregation-uswf066.json`,
`.../inform-usw-xg16-usxg.json`,
`docs/fixtures/controller-10.6.109/inform-ecs-aggregation-uswf066.json`;
233 entries) `uptime >= age` holds for every entry and the two are equal for
six (addresses that were learned once and have been silent since). Typical
entry: `age` 1-20, `uptime` close to the device's uptime.

## 3. What the controller does with them

Observed on Network 11.0.97, 2026-11-13, read from `stat/sta` after the
controller restarted for its own update:

- A wired client's `assoc_time` (the UI's "connected since", and the base of
  its `uptime`) is **the time of the switch's first full inform after the
  controller starts, minus the `uptime` that inform reports for the
  address**. For a client behind the Arista the stored value equals EOS's
  `lastMove` to the second, a day before the restart. For clients behind a
  hypervisor bridge it equals the bridge's first inform after the restart,
  to the second, on each of three nodes.
- `latest_assoc_time` is moved forward on **every inform** for a client
  whose switch keeps reporting an `uptime` near zero. It was under one
  inform interval old for every client behind the Proxmox and Podman
  bridges, and for no client behind the Arista or a real switch (one
  genuinely flapping device aside).

So an `uptime` that is not the learn time makes every controller restart
look like every client reconnecting, and makes the controller re-register
the association each minute. The effect is on the records only (connected
since, client uptime, whatever reads them); no traffic is involved.

Not explained, and not ours: some clients on real switches also had
`assoc_time` reset at that restart.

## 4. What the bridge sent before the fix

`macEntries` (`internal/device/tables.go`) filled both keys from the one
`devicemodel.MACEntry.LastMove`. The drivers put different things in it:

| Driver | `LastMove` was | `age` | `uptime` |
|---|---|---|---|
| `arista-eos` | EOS `lastMove` (learned or moved) | wrong: as large as `uptime` | right |
| `proxmox` | now minus the kernel FDB's `updated` (last refresh) | right | wrong: 0 to the ageing time |
| `podman` | now | 0 | wrong: always 0 |
| `sonic` | unset | 0 | wrong: always 0 |
| configured `children:` (`internal/informloop/children.go`) | the snapshot time | 0 | wrong: always 0 |

The shape test (`internal/device/shapes_test.go`) could not see this: every
one of these values has the right type and is inside the real range. The
fault is the relation between two keys, and what one of them means
(`docs/wire-shapes.md` §10).

## 5. What the bridge does now

### 5.1 Model

`devicemodel.MACEntry` has two times in place of `LastMove`:

- `LearnedAt`: when the address was learned on this port (or last moved to
  it). Zero = the driver cannot know.
- `LastSeen`: when a frame from it was last seen. Zero = unknown.

### 5.2 Wire

`macEntries`: `uptime` = snapshot time minus `LearnedAt`, `age` = snapshot
time minus `LastSeen`, in whole seconds; a zero time gives 0, a negative
difference gives 0, and `age` is clamped down to `uptime`. The per-port
table and the top-level table render the same pair for the same entry (one
function, one snapshot time).

### 5.3 Learn times for drivers that cannot know them

The Linux bridge FDB, SONiC's `FDB_TABLE` and a configured child have no
learn time. The loop keeps one for them: `learnTimes`
(`internal/informloop/learntimes.go`), keyed by (address, VLAN, port index)
and applied to every snapshot after `children:` are added and before the
session renders it -- the start-up snapshot in `New` and each successful
collect.

- An entry that arrives with `LearnedAt` set is left alone and not tracked.
- First sight of a key: `LearnedAt` = the entry's `LastSeen` if set (and
  not after the snapshot time), else the snapshot time, truncated to the
  second. Later sights: the remembered time. The same time goes into the
  port's table and the switch's.
- A key absent from a **successful** collect is forgotten, so an address
  that aged out and came back, or moved port, starts again, as on a real
  switch. A failed collect never reaches the tracker and changes nothing.
- The driver's snapshot is not modified; a copy carries the times.
- **The file.** `mac-learned.json` in the device's state directory
  (`informloop.Config.StateDir`; `cmd/disunified` passes the directory that
  holds `device.json`, the same one drivers get as `options.state_dir`):
  a sorted JSON list of `{mac, vlan, port, learned}` (`learned` in Unix
  seconds), one row per tracked key. Never inside `device.json`, which
  holds the adoption key. Mode 0600, written to a temp file and renamed,
  and only when the set of keys changed (the times of a kept key never
  change, so the file is otherwise current); loaded at start, and an
  unreadable file is logged and starts empty. A bridge restart or redeploy
  therefore does not reset every client. What it cannot know is whether
  an address aged out while the bridge was down: an address present on
  both sides of the gap is treated as continuous. On a busy uplink the set
  of keys changes on most informs, so the file (some 13 KB for 150
  addresses) is rewritten about once a minute; left unthrottled on purpose
  (Clint, 2026-11-13).
- No state directory (tests, a `-collect-once` from flags): the tracker
  works in memory.

### 5.4 Drivers

| Driver | `LearnedAt` | `LastSeen` | `uptime` | `age` |
|---|---|---|---|---|
| `arista-eos` | EOS `lastMove` | unset | since `lastMove` | 0 |
| `proxmox` | unset (tracker) | now minus the FDB's `updated` | since first seen by the bridge, from the FDB's `updated` then | the FDB's `updated` |
| `podman` | the container's `started` (reduced inspect; unset if it does not parse, then the tracker) | the collect time | since the container started | 0 |
| `sonic` | unset (tracker) | unset | since first seen by the bridge | 0 |
| configured `children:` | unset (tracker) | the snapshot time | since first reported | 0 |

EOS 4.26.14M has no per-entry last-seen time in any committed capture
(`show mac address-table` carries `lastMove` and `moves` only), so the
Arista's `age` is 0; no command was added. The Podman driver reads nothing
new: `started` was already in the reduced inspect.

## 6. Tests (real captures only)

1. `TestMACTimesRealInforms` (`internal/device/mactimes_test.go`): every
   `mac_table` entry of the three informs of §2 (233, all in port tables;
   none of the three has a top-level table) is whole, non-negative, with
   `uptime >= age`. `TestMACTimesEveryDriver` holds every driver's
   contract payload to the same, and a top-level entry to its port-table
   twin.
2. `TestLearnTimesProxmox` (`internal/informloop/learntimes_test.go`): the
   committed Proxmox capture collected by the real driver again and again,
   each collect read as if later (snapshot and entry times moved together):
   first sight `uptime = age =` the capture's `updated`; each later
   collect's `uptime` grows by the interval and `age` stays `updated`; an
   address dropped from one collect (it aged out) starts again when it
   returns; a failed collect forgets nothing; the file is 0600, in the
   state directory, not rewritten when the set is unchanged and rewritten
   when it changes; a second loop started from it continues every time;
   with no state directory the times live in memory.
   `TestLearnTimesChild`: a configured child on the DX010 capture, age 0,
   uptime growing.
3. `TestMACTimesArista`: every entry's `uptime` is the snapshot time minus
   the capture's `lastMove`, `age` 0.
4. `sonic`: `TestSnapshotFromTheVirtualSwitch` asserts both times zero on every
   FDB entry.
5. `podman`, from `docs/fixtures/podman-5.8.2/collect-macvlan.txt`: the
   host captured again on 2026-11-13 through the driver's own `collect.sh`
   (one read-only SSH exec, raw output kept out of the repo), scrubbed by
   `scripts/sanitize-podman.py`. In it the bridge's own container is
   running on the host's macvlan network (`net-2`) as well as its bridge
   network. `TestLANEndpointLearnedAtContainerStart`
   (`internal/drivers/podman/mactimes_test.go`) reads the capture's own
   `networks` and `inspect` sections: every running macvlan/ipvlan
   endpoint, and nothing else, is an entry; its `LearnedAt` is that
   container's `started`, its `LastSeen` the snapshot time, VLAN 1, and its
   port carries the same entry. `TestMACTimesPodman`
   (`internal/device/mactimes_test.go`): on the wire `uptime` is the
   seconds from `started` to the snapshot time and `age` 0, in the port's
   table and the switch's. The capture is also in `contractPayloads`
   (`podman-macvlan`), so `TestMACTimesEveryDriver`, the wire contract and
   the shape test see a real Podman entry. The first capture
   (`collect.txt`) predates the container joining that network: it has the
   macvlan network but nothing on it, and still yields no entry
   (`TestOnlyLANEndpointsAreClients`). The scrub keeps each network's
   `driver`, which is what `isLANNetwork` reads, so renaming networks to
   `net-N` hides nothing.

**Not tested for want of a capture:** a running Podman container whose
`started` is absent or does not parse (the driver then leaves `LearnedAt`
zero and the loop's tracker supplies it). Every running container in both
captures has a `started` that parses;
`TestEveryRunningContainerInTheCaptureHasAStart` fails the day a capture
has one, so the case gets its test then. Also not in a capture: an
`ipvlan` endpoint, a stopped container on a macvlan network, and a
Proxmox FDB that really changed between two captures of one node (the
age-out case drops an address from a later collect of the same capture
instead).

## 7. After a deploy (live, read-only)

- The bridges' `payload-last.json`: `uptime` grows from one inform to the
  next for a client that stays; `age` stays small.
- `stat/sta`: `latest_assoc_time` of clients behind the Proxmox and Podman
  bridges stops moving. Their `assoc_time` stays where the last controller
  restart left it; it only becomes right at the controller's next restart
  or the client's next reconnect -- and "right" for a tracked driver means
  the bridge's first sight, which for the first deploy is the deploy itself
  (there is no `mac-learned.json` before it).
- Each tracked device's state directory holds `mac-learned.json` (0600)
  after its first collect, and a redeploy keeps the times.
- Clients behind the Arista: `assoc_time` unchanged; their `age` is now 0.

**Checked 2026-11-13, a few minutes after the deploy (Network 11.0.97):**

- `payload-last.json`, read twice 75 seconds apart: on the three Proxmox
  nodes the median `uptime` rose by about that much while the median `age`
  stayed under 20 seconds; the Podman host's one entry reported the time
  since its container started; the SONiC switch's entries rose together
  from its bridge's restart; the Arista's `age` is 0 on every entry. No
  entry on any device has `age` above `uptime`.
- `stat/sta`, read twice three minutes apart: `latest_assoc_time` moved for
  none of the wired clients behind the Proxmox nodes, the Podman host or
  the Arista.
- `mac-learned.json` (0600) is in the state directory of each Proxmox node
  and of the SONiC switch; the Arista and the Podman host have none, as
  their drivers supply the learn time.
- **A redeploy keeps the times** (seen two hours later, at the next
  deploy): the Proxmox nodes' median `uptime` was about 6,900 seconds
  before the container was recreated and about 7,050 after it, and the
  SONiC switch's went from 6,854 to 7,020 across its package update. The
  Podman host's one entry started again from zero, rightly: it is the
  bridge's own container, which the redeploy restarts.
- Not seen yet: `assoc_time` after a controller restart.

## 8. Not in scope

Why some clients on real switches lost `assoc_time` at the restart; the
`static`, `ip` and `hostname` keys real switches add to an entry.
