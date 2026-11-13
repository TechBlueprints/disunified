# MAC table times: `age` and `uptime` are two different things

Status: **spec, not built** (written 2026-11-13). This is the plan for the
fix; when it is built, rewrite this page as the description of what the code
does and keep the evidence.

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

## 4. What the bridge sends today

`macEntries` (`internal/device/tables.go`) fills both keys from the one
`devicemodel.MACEntry.LastMove`. The drivers put different things in it:

| Driver | `LastMove` is | `age` | `uptime` |
|---|---|---|---|
| `arista-eos` | EOS `lastMove` (learned or moved) | wrong: as large as `uptime` | right |
| `proxmox` | now minus the kernel FDB's `updated` (last refresh) | right | wrong: 0 to the ageing time |
| `podman` | now | 0 | wrong: always 0 |
| `sonic` | unset | 0 | wrong: always 0 |
| configured `children:` (`internal/informloop/children.go`) | the snapshot time | 0 | wrong: always 0 |

The shape test (`internal/device/shapes_test.go`) could not see this: every
one of these values has the right type and is inside the real range. The
fault is the relation between two keys, and what one of them means.

## 5. The fix

### 5.1 Model

`devicemodel.MACEntry` loses `LastMove` and gains

- `LearnedAt time.Time`: when the address was learned on this port (or last
  moved to it). Zero = the driver cannot know.
- `LastSeen time.Time`: when a frame from it was last seen. Zero = unknown.

### 5.2 Wire

`macEntries`: `uptime` = snapshot time minus `LearnedAt`; `age` = snapshot
time minus `LastSeen`; a zero time gives 0; negative gives 0; `age` is never
more than `uptime` (clamp `age` down). Both stay whole numbers. The per-port
table and the top-level table report the same pair for the same entry.

### 5.3 Learn times for drivers that cannot know them

The Linux bridge FDB, SONiC's `FDB_TABLE` and a container's endpoint have no
learn time. The loop keeps one for them: a tracker in `internal/informloop`,
keyed by (address, VLAN, port index), applied to the snapshot after
`children:` are added and before the payload is built.

- An entry that arrives with `LearnedAt` set is left alone and not tracked.
- First sight of a key: `LearnedAt` = the entry's `LastSeen` if set, else
  the snapshot time. Later sights: the remembered time.
- A key absent from a **successful** collect is forgotten, so an address
  that aged out and came back, or moved port, starts again, as on a real
  switch. A failed collect changes nothing.
- The map is persisted in the device's state directory, in its own file
  (not `device.json`, which holds the adoption key), mode 0600, written
  atomically and only when the set of keys changed; loaded at start. A
  bridge restart or redeploy therefore does not reset every client. What it
  cannot know is whether an address aged out while the bridge was down; an
  address present on both sides of the gap is treated as continuous.
- No state directory (tests, one-shots): the tracker works in memory.

### 5.4 Drivers

- `proxmox`: `LastSeen` = now minus `updated`; `LearnedAt` unset.
- `podman`: `LastSeen` = now; `LearnedAt` = the container's start time (the
  reduced inspect already carries `started`), unset when it does not parse.
- `sonic`: neither (the tracker supplies the learn time; `age` stays 0).
- `arista-eos`: `LearnedAt` = `lastMove`; `LastSeen` unset, so `age` becomes
  0 instead of repeating `uptime`. A per-entry age from EOS is wanted only
  if a command in the committed 4.26.14M captures already has it; no new
  command without a capture.
- `children:`: `LastSeen` = the snapshot time; `LearnedAt` unset.

## 6. Tests (real captures only)

1. The invariant from the real informs: every `mac_table` entry in the
   three fixtures of section 2 has `uptime >= age`. Then the same invariant
   over every driver's payload in the contract tests.
2. The tracker, driven by the committed Proxmox capture collected twice with
   the clock moved between: each entry's `uptime` grows by the interval,
   `age` stays the capture's `updated`; an entry removed and returned starts
   again; a failed collect forgets nothing; the persisted file round-trips
   and a second loop started from it continues the times.
3. `podman` from its capture: `uptime` = snapshot time minus `started`.
4. `arista-eos` from its capture: `uptime` from `lastMove`, `age` 0.
5. `sonic`: the existing "LastMove is zero" assertion becomes the two new
   fields.

## 7. After a deploy (live, read-only)

- The bridges' `payload-last.json`: `uptime` grows from one inform to the
  next for a client that stays; `age` stays small.
- `stat/sta`: `latest_assoc_time` of clients behind the Proxmox and Podman
  bridges stops moving. Their `assoc_time` stays where the last controller
  restart left it; it only becomes right at the controller's next restart
  or the client's next reconnect.

## 8. Docs to bring along

`docs/feature-map.md` (the `mac_table` row), `docs/wire-shapes.md` (a value
can have a real device's shape and the wrong meaning; the relation check
added in section 6), the four drivers' pages and `CLAUDE.md` files where
they describe the MAC table, and the repo `CLAUDE.md` section 5 (the
controller's rule in section 3 of this page, in two or three lines).

## 9. Not in scope

Why some clients on real switches lost `assoc_time` at the restart; the
`static`, `ip` and `hostname` keys real switches add to an entry.
