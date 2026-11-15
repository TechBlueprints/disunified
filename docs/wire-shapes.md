# Wire shapes: holding a bridged device's values to a real device's

Status: **built, merged and deployed 2026-11-12**: three work packages,
§4-§6, each with its results in §7. Open after the merge: §8. Network
11.0.97 migrated the controller the next morning: §9.

## 1. Why

Network 11 moves the controller's config to PostgreSQL. Its `device` table
keeps `serial` in 12 characters, and one bridged device with a longer serial
halted the migration four times (CLAUDE.md §5,
[issue #3](https://github.com/TechBlueprints/disunified/issues/3)). The
contract tests did not see it, for two reasons that are not about serials:

- **They compare keys and JSON types, not values.** A string three times
  longer than any real device's passes.
- **Scrubbing changes shapes.** `SSJ00000000` stood in for an empty DMI
  serial, a 36-character UUID's neighbour and a 22-character chassis serial
  alike, so no fixture could have carried the length that failed.

A typed store punishes any value a real device never sends. The serial was
the first; this is the review for the rest.

## 2. The audit (2026-11-12, Network 10.6.109)

Every bridged device was compared with the site's real UniFi devices, path
by path: value type, longest string, character class, number range. Twice:
the stored records (what a migration reads) and the live device objects
(`stat/device`). `scripts/wire-shapes.py audit` repeats the live one.

Stored records: no type differences. Live objects:

| # | Finding | Where | Decision |
|---|---|---|---|
| 1 | Optic readings (`sfp_temperature`, `sfp_voltage`, `sfp_current`, `sfp_txpower`, `sfp_rxpower`) go out as numbers; real switches send strings (`"-1.64"`) | `internal/device/tables.go` | fix, §4 |
| 2 | `version` carries the device's own string: a letter on EOS (`4.26.14M`), a 30-character image name on SONiC, which the controller then keeps as `previous_firmware_version`. Real devices: digits and dots, at most 13 characters | `internal/device/session.go` | fix, §5 |
| 3 | `hostname` is a full DNS name on one device (32 characters; real ones at most 17) | the driver's `System.Hostname` | none: the UI has no hostname field (it edits `name`), the controller stores the inform's hostname once and uses it for nothing (CLAUDE.md §7) |
| 4 | The Podman driver's `port_table[].ifname` is "container network", with a space, up to 29 characters; real switches send no `ifname`, and a gateway's is `eth30`-shaped | `internal/drivers/podman` | fix, §6 |
| 5 | Scrub placeholders change lengths, and the contract tests check two ports' keys and no values | `scripts/sanitize-*.py`, `internal/device/contract_test.go` | fix, §4 |

Also seen, and left: a static `config_network` (a UniFi feature), outlet
names past 24 characters (the UI allows 128), PSU part and serial numbers
(the ECS reports them; this site's real PSUs are blank), `ethtool`'s wording
in `sfp_compliance` (since shortened for Proxmox, §8), memory counters past
32 bits on a host with gigabytes of RAM. Each is an excusal with its reason in §4's test, not a silence.

## 3. The reference

`docs/fixtures/controller-10.6.109/shapes-usw.json` is the shape of every
value the site's 42 real `usw` devices held, written by
`scripts/wire-shapes.py capture`: per path, the types, the longest string,
the character classes, and whether a number was ever negative, fractional
or past 32 bits. **It holds no values**, so it needs no scrubbing, and it is
the one fixture a scrub cannot bend.

Paths are JSON paths with `[]` for an array element; a key that is an id, an
index or a MAC is `<key>`, and `port_16-rx_bytes` is `port_<n>-rx_bytes`.
The source is the controller's view (`stat/device`), which holds the inform's
keys beside the controller's own, so only paths a payload shares are
compared.

## 4. Package A: optic strings, every port, the shape test, the scrubs

Owns: `internal/device/tables.go`, `internal/device/contract_test.go`, a new
`internal/device/shapes_test.go`, `scripts/sanitize-*.py`,
`scripts/check-site-info.sh` and its test, `scripts/wire-shapes.py`.

1. **Optic readings as strings.** The five `sfp_*` readings are emitted as
   strings in the format the ECS Aggregation's inform uses
   (`docs/fixtures/controller-10.6.109/inform-ecs-aggregation-uswf066.json`:
   read each key's decimals there; do not guess). `HasDOM` false still omits
   them.
2. **The contract on every port.** Today a driver's payload is held to the
   reference's uplink port and one down port. Add: for every key that
   appears on any port of a reference inform and on any port of ours, the
   JSON types must agree. This is the check that would have caught 1.
3. **The shape test.** Walk each driver's contract payload with the same
   path rules as `scripts/wire-shapes.py` and hold it to `shapes-usw.json`:
   no type a real device never had, no string longer than the longest real
   one, no new character class, no number negative, fractional or past 32
   bits where no real one was. A difference fails unless the path is in an
   excusal table with its reason, per driver, in the style of
   `contractOmissions`. An excusal that no longer excuses anything fails
   too. `version` and the Podman `ifname` are excused as "fixed by package
   B/C" and nothing else; the merge removes those two.
4. **Scrubs keep lengths.** A serial's placeholder keeps the original's
   length (`SSJ` and zeros; `SSJ00000000` stays the 11-character case) and an
   empty value stays empty, in every `scripts/sanitize-*.py`.
   `sanitize-controller.py` writes a device's `serial` as its scrubbed MAC's
   hex digits, which is what a real serial is; apply that to the committed
   controller informs (the same result a re-scrub would give).
   `check-site-info.sh` allows `SSJ0+` of any length. Other committed
   fixtures are not rewritten: their raw captures are gone.

Done when `go test ./...` and `scripts/check-site-info.sh` pass, and
reverting 1 makes both 2 and 3 fail.

## 5. Package B: the version on the wire

Owns: `internal/device/session.go`, `internal/device/descriptor.go`,
`internal/device/session_test.go`, `cmd/disunified/main.go` (the `-version`
check only).

Real devices report `version` as digits and dots, four groups, at most 13
characters (`7.2.123.16565`); the controller derives the three-group
`displayable_version` itself.

1. **The rule.** The reported `version` is the device's version reduced to
   digits and dots: split on every character that is not a letter or digit;
   skip leading tokens that do not start with a digit; from each following
   token take its leading digits; stop at the first token that does not
   start with a digit; keep at most four groups, and drop trailing groups
   until the result is at most 13 characters. `4.26.14M` gives `4.26.14`;
   `9.1.6` is unchanged; `igmp-202511.20261106-f2e25f54c` gives `202511`
   (with the date it is 15 characters); `202505.1238526-ff3c0c9f6` gives
   `202505`. No padding: a group that was never there is not invented. A
   version with no digits falls back to the model profile's.
2. **On the wire only.** The driver's own string stays in
   `System.Version`, in the start-up log line, and in everything the
   emulated-upgrade logic compares (`State.FirmwareBase`,
   `deviceVersion()`): an adopted device must not lose its emulated upgrade,
   or be offered one again, because of this change. `displayable_version`
   is derived from the reported version.
3. **An operator's `-version`** (and the config's) that does not already
   fit the rule is refused at start, as `-serial` is.
4. Tests take the version strings from the drivers' real captures
   (`docs/fixtures/`), one per driver, and replay a persisted emulated
   upgrade across a restart with a non-conforming device version.

`previous_firmware_version` is the controller's own record of the last
version it saw; the bridge cannot set it. The one stored 30-character value
is corrected on the controller after the merge, not by this package.

Done when `go test ./...` passes and `docs/protocol-connection-points.md`
states the rule in place of "must be `v<numeric>`".

## 6. Package C: port interface names

Owns: `internal/drivers/podman/`, `internal/drivers/proxmox/` (tests only,
unless it fails the rule), a new `internal/device/ifname_test.go`,
`docs/drivers/podman.md`.

1. **The rule, for every driver.** `ifname` is an interface name: no
   whitespace, at most 15 characters (a Linux interface name's limit),
   stable from one collect to the next for the same port. The label a person
   reads belongs in `Name` and `Description`.
2. **Podman.** A container endpoint's `IfName` is the label today
   (`endpointPort`). Give it a compact name in the Proxmox driver's style
   (`vm100-net0`, `ct101-net0`), derived from something that survives a
   container being recreated; the label stays in `Name` and `Description`.
   First trace everything that reads `IfName` (the payload's `ifname` and
   `if_table`, default port names and the controller naming in
   `internal/unifiapi`, any persisted port-slot map, the `children`
   placement) and show with tests that an adopted host's ports keep their
   slots and their names in the controller across the change.
3. **Proxmox.** Its guest names already fit; check the physical and bridge
   port names against the rule on the committed captures.
4. `ifname_test.go` holds every driver's contract payload to the rule,
   through the existing `build*ContractPayload` helpers (read, not edited).

Done when `go test ./...` passes and the Podman fixture's ports show the
new names with their labels unchanged.

## 7. Results

Each package appends here: what it built, what it found that this spec had
wrong, and what it could not test without a live device.

### Package A

Built (branch `claude/wire-shapes-a-6c60a4`):

- **Optic readings as strings** (`internal/device/tables.go`,
  `opticReading`): temperature, voltage and bias current with three
  decimals, the two powers with two, as the ECS Aggregation's inform has them
  (`"42.000"`, `"3.000"`, `"6.000"`, `"-1.64"`); never `"-0.00"`; `HasDOM`
  false still omits all five. No existing test asserted the numbers.
- **The contract on every port**: `TestWireContractEveryPort`
  (`contract_test.go`) holds every key on any port of each reference inform
  and on any port of ours to the reference's JSON types, for every driver's
  contract payload (`contractPayloads`, nine of them: arista-eos, proxmox,
  podman, the SONiC VS, the DX010's hardware and l2d captures, apc-pdu,
  apc-ups, apc-backups), and does the same for a power device's outlet rows.
  It is one table-driven test rather than a call inside `runContract`, so no
  per-driver test file changed; the Back-UPS payload had no builder, so
  `buildBackUPSContractPayload` copies the one inline in its test.
- **The shape test** (`shapes_test.go`, `TestWireShapes`): `wire-shapes.py`'s
  `walk`, `cls`, `norm_key` and `differences` in Go, run over the same nine
  payloads against `shapes-usw.json`. Excusals are keyed by path and kind
  (type, longer, charset, number): `shapeExcusalsAll` for what comes from the
  reference's source or the tests' input (`uplink` is the controller's object
  in `stat/device`; the tests' `inform_url` is one character longer than the
  site's), `shapeExcusals` per driver; an excusal that excuses nothing fails,
  per driver and across all. Go and Python were run on the same nine
  payloads and gave identical difference lists.
- **Scrubs keep lengths**: every `scripts/sanitize-*.py` has
  `placeholder_serial` (SSJ and zeros of the original's length, empty stays
  empty) for every serial it rewrites, optic serials included (`vendorSn` was
  `scrubbed`, Proxmox's DMI and optic serials `SCRUBBED`); the Back-UPS's
  second serial is `serial-00001`, its length; the Smart-UPS's register
  serial keeps its padding. `sanitize-controller.py` writes any object's
  `serial` that has a `mac` beside it as the scrubbed MAC's 12 digits, upper
  case. The four committed controller informs now carry those
  (`020000000002`, `020000000003`); one line each, edited rather than
  re-scrubbed, because a re-run renumbers every MAC. No other fixture was
  rewritten.
- **The checker** allows `SSJ0+` of any length, and its serial pattern grew
  from exactly three letters and eight digits to the shapes the scrubbers
  rewrite (`[A-Z]{2,3}[0-9]{8,10}`, APC's two-and-ten included), with no
  new hit in the tree; `check-site-info_test.sh` covers both.
- Reverting 1 fails both 2 (arista-eos, sonic-hw, sonic-l2: "real string,
  ours number") and 3 (`type port_table[].sfp_* num (real: string)`).

The excusals, and why:

| Driver | Path (kind) | Reason |
|---|---|---|
| all | `uplink` (type) | the reference is `stat/device`, whose `uplink` is the controller's object; informs send the name |
| all | `inform_url` (longer) | test input, one character past the site's controller URL |
| all | `hash_id` (charset) | new finding, below |
| arista, sonic ×3 | `version`, `displayable_version` | "fixed by package B" (charset; SONiC also longer) |
| arista, sonic-hw, sonic-l2 | `port_table[].sfp_current` (longer) | a 100G module's bias is near 40 mA (`"39.508"`); every real one on site is under 10 |
| arista, sonic-hw, sonic-l2 | `port_table[].sfp_part` (longer) | SFF-8636's 16-character part number, filled; the site's longest is 15 |
| arista, sonic-hw, sonic-l2 | `psu_table[].part_no` (+ SONiC `serial_no`) | the supplies report them, as the ECS's inform does; the site's are blank (§2) |
| arista | `lldp_table[].local_port_name` (longer) | `Ethernet49/1`; the ECS sends its vendor names too, the site's longest is 10 |
| arista | `sys_stats.mem_buffer` (number) | bytes past 2^31 (§2) |
| podman-macvlan | `sys_stats.mem_buffer` (number) | bytes past 2^31, as the Arista's (§8); added 2026-11-13 with the capture (`docs/mac-table-times.md` §6) |
| proxmox | `port_table[].sfp_compliance` (longer) | ethtool's wording (§2) |
| proxmox | `fan_table[].label` (longer, charset) | the driver's `Fan N` where the ECS has `FanN`; a follow-up in the driver |
| apc-ups, apc-backups | `netmask` (longer) | test input: the test's `255.255.255.0` against the site's shorter mask |

Gone since, each with its fix (§8): the `version` rows (package B, at the
merge), the `hash_id` row and both `proxmox` rows (2026-11-13).

What this spec had wrong:

- **The Podman `ifname` has no excusal**, because nothing in the reference
  can flag it: no real `usw` sends `ifname`, so `port_table[].ifname` is not
  in `shapes-usw.json` and the shape test compares only shared paths. An
  excusal for it would excuse nothing and fail. The audit's finding 4 came
  from its default reference, every device type (the gateway's `eth30`).
  Package C's `ifname_test.go` is what holds it.
- **`displayable_version` differs too** (derived from the device's string
  today), so the merge with B removes the `version` and the
  `displayable_version` excusals, four entries per SONiC payload, two for
  the Arista.
- **`wire-shapes.py` had a bug in `differences()`**: an all-digit string
  was a new class against a `hex`/`HEX` reference (a serial from an
  all-digit MAC, `020000000001`, against the real `HEX` serials), and
  `ascii-space` against `non-ascii`. Both are narrower classes inside wider
  ones, which the function's own comment exempts; fixed there and in Go.
  `capture` is untouched, so `shapes-usw.json` stands.
- **The ECS's optic readings are whole numbers** at three decimals
  (`"3.000"` V from a 3.3 V module, biases `"6.000"` to `"9.000"`): it
  appears to truncate before formatting. The bridge sends the measured value
  at the ECS's decimals (`"3.322"`) rather than copy the truncation, which
  would throw the reading away. Whether to copy it is the parent's call; the
  `sfp_current` length excusal stands either way (`"39.000"` is six
  characters too).

New findings, outside package A's files, not fixed:

- `hash_id` is `anonID`'s first 16 characters, dashes included
  (`d56e12ed-55f7-56`, `session.go`); real devices send 16 hex digits.
- `sanitize-controller.py` maps `hash_id` to a 32-digit id, so the committed
  informs carry 32 characters where the real ones had 16: a scrub that
  changes a length, as the serial's did. Not rewritten (§4.4's rule).

Not testable without a live device or controller: that the controller and
its UI take the string readings as they take the ECS's (the readings on a
port's SFP tab), and how a bias of 10 mA or more displays; the scrubbers
on a new raw capture (each was run on throwaway input; the Podman and SONiC ones also over
their committed captures, with the same output as before the change).

### Package B

Built (branch `claude/wire-shapes-b-658094`):

- `device.WireVersion` (`internal/device/descriptor.go`) is §5.1's rule as
  written, with `MaxVersionLen` 13 and four groups; `VersionFits` says
  whether a string goes out unchanged. The rule gives the four results §5.1
  names without bending.
- `Session.buildPayload` reports `WireVersion` of the tracked version, or
  the model profile's when it has no digits, and derives
  `displayable_version` from that. `inform.Descriptor.Version`,
  `Session.Version()`, `State.Firmware`/`FirmwareBase`, `deviceVersion()`
  and the comparisons in `NewSession` and `SetSnapshot` keep the device's
  own string, so a persisted `FirmwareBase` of `4.26.14M` still matches
  the device after a restart on this build, and an upgrade requested now
  records the same base the old build did. `Session.ReportedVersion()`
  returns the wire form. Nothing else sends the version out: the API
  client and the loop never read it.
- `-version` that does not fit is refused at start next to the `-serial`
  check, with the form it would have gone out as.
- `internal/device/version_test.go`: every driver's version from its
  capture, reduced and sent (table below); the rule on §5.1's examples and
  its edges; a persisted emulated upgrade replayed through a `Store` across
  a restart with `4.26.14M` underneath; a device with no upgrade left out of
  one; and the device's version moving, under a running bridge and across a
  restart, dropping the fiction.

| Driver (capture) | Device's own | On the wire |
|---|---|---|
| arista-eos (`arista-eos-4.26.14M`) | `4.26.14M` | `4.26.14` |
| proxmox (`proxmox-9.1.6`) | `9.1.6` | `9.1.6` |
| podman (`podman-5.8.2`) | `5.8.2` | `5.8.2` |
| apc-pdu (`apc-aos-3.9.2`) | `3.9.2` | `3.9.2` |
| apc-ups (`apc-smtl-15.5`) | `15.5` | `15.5` |
| apc-backups (`apc-backups-05.3`) | `05.3` | `05.3` |
| sonic, VS guest (`sonic-202505.1238524-ff3c0c9f6`) | `202505.1238524-ff3c0c9f6` | `202505` |
| sonic, DX010 first image (`sonic-202505.1238526-ff3c0c9f6`) | `202505.1238526-ff3c0c9f6` | `202505` |
| sonic, DX010 202511 (`sonic-igmp-202511.20261106-f2e25f54c`) | `igmp-202511.20261106-f2e25f54c` | `202511` |

What the spec had wrong or left out:

- **The config file has no working version key.** `internal/config`'s
  `Device.Firmware` (`firmware:`) has been parsed since the first commit and
  never read: `configOptions` does not pass it on, so a config-file device
  always reports its own version. There was nothing to refuse, and wiring it
  would make a dead key live on every deployed device whose `config.yaml`
  sets it. Left for the parent to decide (wire it with the same check, or
  delete the key).
- **One test outside the package's files asserted the old value.**
  `contract_sonic_hw_test.go` held `displayable_version` to the 30-character
  image name; its expectation is now `202505` (one line).
- **A `v`-prefixed version loses its major.** `v4.26.14` (the `v<numeric>`
  form the protocol notes used to demand) gives `26.14`: `v4` is a leading
  token that does not start with a digit. No capture has one, and an
  operator's is refused, so the rule was left as written.
- Stale elsewhere, not this package's files: `docs/drivers/arista-eos.md`
  (§ on the firmware string) said `4.26.14M` goes out as `v4.26.14`
  (corrected 2026-11-13); `docs/prior-art.md` names the `v<numeric>` quirk
  as history, which it is.
- **The only captured upgrade reply's version is scrubbed to `device-1`**
  (`controller-10.6.106/replies-pdu.ndjson`), which the rule reports as `1`:
  a scrub that changes a value's shape, §4.4's kind. The tests use the model
  profile's catalogue version as the upgrade target instead.
- Two SONiC builds of one branch report the same wire version (`202505`):
  the controller sees no change when the image moves within a branch. The
  emulated upgrade still compares the full string and drops on the move.

Not testable without a live device and controller:

- **What the controller does when an adopted device's version changes from
  `4.26.14M` to `4.26.14`** (and the DX010's from
  `igmp-202511.20261106-f2e25f54c` to `202511`). Expected, not seen: it
  takes the new `version` from the inform, moves the old one into
  `previous_firmware_version` (so the stored 30-character value is replaced
  by the DX010's next change, or by the correction §5 leaves to after the
  merge), and may log a firmware-changed event. Whether the update badge
  moves is open: `4.26.14` is still below the catalogue's `7.3.109.16640`,
  but `202505`/`202511` compare above every catalogue version, so a SONiC
  device may stop being offered an upgrade at all.
- Whether any deployed `device.json` holds a `Firmware` that does not fit
  (the controller's targets are catalogue versions, which do; one that does
  not would now go out reduced).
- The `-version` refusal end to end: `runOne` needs a driver, so only
  `VersionFits` is tested.

### Package C

**Built** (2026-11-12). A Podman container endpoint's `IfName` is `ct` and
eight hex digits of a SHA-256 of its endpoint key, `<container name>/<network>`
(`endpointIfName` in `internal/drivers/podman/collector.go`); `Name` and
`Description` keep the label. The key is the one the host's slot file keeps
the port by, so the name survives a recreate (new id, same name) and a
restart, and changes when a different endpoint takes a freed slot. Deriving it
from the slot alone would also have been stable, but would have broken the
loop's layout signature (`Index` + `IfName` + `Interfaces`,
`internal/informloop/loop.go`), which is what tells the bridge a new
container is on a slot and runs the provisioner to name it. The fixture's
ports:

| Slot | Before: `IfName` = `Name` = `Description` | After: `IfName` | After: `Name` = `Description` |
|---|---|---|---|
| 1 | `app-1` | `ctcce8b580` | `app-1` |
| 2 | `app-2` | `ct6ca8c906` | `app-2` |
| 3 | `app-3` | `cta2357235` | `app-3` |
| 4 | `app-4` | `ctf314d1dd` | `app-4` |
| 5 | `app-5` | `ct3b9b18ba` | `app-5` |
| 6 | `app-6` | `ct429aab53` | `app-6` |
| 7 | `app-7` | `ctb5002961` | `app-7` |
| 8 | `app-8` | `ct3784e40d` | `app-8` |
| 9 | `app-9` | `ct004f256d` | `app-9` |
| 10 | `app-10` | `ct4bdfdbea` | `app-10` |
| 11 | `app-11` | `ct7c36f308` | `app-11` |
| 12 | `app-12` | `cta29a3fe1` | `app-12` |
| 13 | `disunified-ups` | `ctc9907893` | `disunified-ups` |
| 14 | `disunified` | `cta4732075` | `disunified` |
| 15-53 | free (no name; `Open-<n>` in the controller) | unchanged | unchanged |
| 54 | `enp6s18` (the NIC) | unchanged | unchanged |

**The `IfName` trace.** Every reader of a port's `IfName`, and why the change
is safe for it:

- `port_table[].ifname` (`internal/device/tables.go`): the value being fixed.
- `port_table[].name` (`tables.go`): falls back to `IfName` only when the
  port has no `Description`; every container port has one, so the inform's
  port name is the label before and after. This is the reader that would have
  been load-bearing -- the controller shows the inform's name for a port with
  no name override, and the Podman ports were never given overrides because
  the inform already carried the label -- and it is not touched.
  `TestNewIfNameChangesOnlyIfnameOnTheWire` builds the payload from the old
  and the new snapshot: only the container ports' `ifname` differs.
- `lldp_table[].local_port_name` (`tables.go`): only for a port with a
  neighbour. The host runs no lldpd; a `children:` entry on a container port
  would report the new name there, a reported value the controller does not
  place by (placement is by MAC, `docs/topology-placement.md`).
- `if_table` (`tables.go`): the management interface, not a port's `IfName`.
- Naming (`internal/drivers/podman/naming.go`): `PortName` returns
  `Description` first, so every container port is named the label as before;
  `DefaultPortNames` gains the new `IfName` and keeps the label (through
  `Description` and the remembered names), so every name that was a default
  still is.
- The controller REST API (`internal/unifiapi`): `Provision` reads
  `namer.PortName`, the default-name test above, and `IfName == "" &&
  !Present` for released slots, which is the same for every port (a
  container port has a name before and after, a free slot has none);
  `seed.go` uses `IfName` in a log note only.
  `TestNewIfNameChangesNothingSentToTheControllerAPI` runs `Provision`
  against a fake controller for the old and the new snapshot, seeding on and
  off, in three states (just adopted with the profile's names; named through
  the inform with no overrides; name overrides plus an operator's rename):
  the requests are byte-identical.
- The slot file: keyed by the endpoint key, never by `IfName`; slots are
  unchanged (`TestContainerPortsKeepSlotsAndLabels`).
- The loop's layout signature and port freshness
  (`internal/informloop/loop.go`): held in memory only, so a restart onto
  the new build starts a fresh signature -- no port is marked fresh and no
  layout change fires; afterwards the name changes exactly when a slot's
  occupant does. The Podman driver writes no ports, so freshness gates
  nothing on it.
- `children` placement (`internal/informloop/children.go`): by port index.
- The driver's own uses: the start-up log's uplink name (a NIC) and the
  tests, which now find a port by its `Name`.

**Proxmox.** Guest (`vm100-net0`), bond-slave (`bond0-1`), unattached-NIC
(`eno1`) and free-slot (no name) ports fit the rule on both committed node
captures (`internal/drivers/proxmox/ifname_test.go`); the driver is unchanged.

**`internal/device/ifname_test.go`** holds every driver's contract payload to
the rule (no whitespace, at most 15 characters, the same names on a second
build), through `buildContractPayload`, `buildPodmanContractPayload`,
`buildProxmoxContractPayload`, `buildSonicContractPayload`,
`buildPDUContractPayload`, `buildUPSContractPayload`, and every committed
SONiC `collect*.txt` (rendered with `sonicRender`; the 202505 hardware
captures through `sonicCapturePayload`). Every driver passes; no driver other
than Podman needed a change.

**What the spec had wrong, or did not say.**

- The rule's test on the *committed* Podman capture passes on the old build
  too: the scrub maps container names to `app-N` and the capture has no
  container on two networks, so no label there is long or has a space -- §1's
  "scrubbing changes shapes" again. The rule is held for the Podman driver by
  its own tests (`TestEndpointIfNameFitsLongLabels`, and every container port
  matching `ct[0-9a-f]{8}`), not by the payload test alone.
- §2 finding 4 says real switches send no `ifname`; every driver's
  `port_table` carries one (the profile's `eth<n>` for a free slot). That is
  package A's shape question, not changed here.
- The Back-UPS has no payload builder (`contract_backups_test.go` builds
  inline) and `stub` none at all; neither is in `ifname_test.go`. Both are
  power-path or instrument payloads with no port interfaces of their own.

**Not tested without the live host.** That the controller's port names for
the adopted Podman host stay as they are after the deploy (expected: the
inform's port names and the bridge's REST writes are unchanged, so nothing
moves; the only new value the controller sees is each container port's
`ifname`); the live labels with spaces and up to 29 characters (covered by a
constructed endpoint, not a capture); whether a `children:` entry sits on a
container port in the live config.

## 8. After the merge

The three branches merged without a conflict; the only change at the merge
was dropping package A's `version` and `displayable_version` excusals, which
package B made unnecessary (an excusal that excuses nothing fails).

Open, each a decision or a live check:

- ~~`hash_id` goes out as 16 characters of a UUID, dashes included; real
  devices send 16 hex digits~~ (found by package A). **Fixed 2026-11-13**
  (`hashID` in `session.go`: 16 hex digits). What prompted it: the
  controller holds a `hash_id` for every connected real switch and for no
  bridged device, on 10.6.109 and on 11.0.97 alike, while it keeps our
  `anon_id` and `guid` (UUIDs, as real ones are). So ours was most likely
  being dropped for its form; what the controller uses the field for is
  not known. **Confirmed after the deploy:** the controller holds a
  16-digit `hash_id` for every bridged device from its first inform on the
  new build.
- ~~The config file's `firmware:` key is parsed and never read~~ (found by
  package B). **Wired 2026-11-13**: it is the config file's `-version`,
  refused at load unless it is a version as a real device reports one.
- **Only a device claimed as the USW Leaf reports its own version
  (Clint, 2026-11-13).** Every other model is a UniFi product on that
  product's firmware line and reports UniFi firmware numbers only: the
  model's catalogue version, then the controller's upgrade targets, never
  the device's own (`reportsOwnVersion` in `descriptor.go`). The Leaf has
  no firmware releases, so its Version column can say what really runs
  (`docs/unifi-models.md`). Before this a SONiC switch claimed as an ECS
  would have shown its image's branch (`202511`) as firmware, and package
  B's worry that it "may stop being offered an upgrade" goes with it. An
  emulated upgrade recorded by an older build over the device's own
  string stands; the base is dropped. After the deploy every device's
  reported version was what it had been (the four non-Leaf devices each
  held such an upgrade).
- The one stored `previous_firmware_version` that is an image name is the
  controller's record; it is replaced by that device's next version change.
- Scrubs that change a shape: an upgrade reply's version (`device-1`), and
  `anon_id`/`guid`, written as 32 hex digits where the real ones are UUIDs.
  `hash_id` (32 digits for 16) is fixed: the scrubber keeps its length and
  the four committed informs carry 16.
- Optic readings go out at the ECS's decimals but with the measured value;
  the ECS's own look truncated to whole numbers (`"3.000"` V from a 3.3 V
  module). Copying that would throw the reading away. Decided (Clint,
  2026-11-13): keep the measured value; this is a departure from the ECS's
  pattern on purpose.

Closed for the Proxmox driver (2026-11-13; its shape test now has no
excusal):

- **Fan labels** are `Fan1`, `Fan2`..., as the ECS sends them. A chip's own
  `fanN_label` still passes through unchanged; no capture has one.
- **`sfp_compliance`** is ethtool's "Transceiver type" without its category
  prefix (`100G Ethernet: `, `10G Ethernet: `, `Extended: `), cut at 32
  characters, where a real switch cuts the same field. The 100G cable's
  text is 33 characters after the prefix goes, so it ends `CA-`, as the real
  switches' value for that cable ends cut.
- **`sys_stats.mem_buffer`** stays Buffers plus Cached, past 2^31 on a host
  with gigabytes of page cache. Real switches' `mem_total` and `mem_used`
  pass 2^31, so the controller holds memory in a wide type; reporting
  Buffers alone would change what the UI's memory figure means. Decided
  (Clint, 2026-11-13): leave it. The committed Proxmox capture stays under
  2^31, so only the live audit shows it. (The Podman host's second capture,
  `podman-5.8.2/collect-macvlan.txt` of 2026-11-13, passes it: 3.5 GB, the
  `podman-macvlan` excusal.)
- **Top-level `tx_bytes` and `uplink.rx_bytes`** past 2^31 in the live audit
  are the controller's own fields: the bridge sends no top-level byte
  counter and its `uplink` is the string `eth0`, so the only device-level
  counters it sends are `if_table[]`'s (`tables.go`), and the ECS's own
  `if_table` carries values past 2^31 too (its 10.6.109 inform). Nothing to
  change. One difference in what that row counts, every driver alike: ours
  is the uplink port's counters, the ECS's is its management interface's
  (gigabytes where its uplink port has terabytes).

Checked live after the deploy (2026-11-12, and again on Network 11 the
next day):

- The controller's port names, outlet names and overrides for every bridged
  device are what they were before the deploy, the Podman host's included.
- A version change is recorded as expected: the controller takes the new
  `version` and moves the old one to `previous_firmware_version`
  (`4.26.14M` there now, beside `4.26.14`). The device re-provisioned once
  and was connected again within about a minute.
- The stored device object holds the optic readings as strings
  (`"3.317"`, `"-2.97"`). The port's SFP tab in the UI was not looked at.

## 9. Network 11 (2026-11-13)

The console installed Network 11.0.97 by itself the night after the deploy.
Its `migration.log` ends Phase 1 with 97 steps, 97 succeeded, 0 failed, and
the version file stamped 11.0.97; the earlier four tries had each stopped
at `mongo2pg.device`. Every bridged device stayed adopted and connected.

What the migration kept and changed, read from `stat/device` before and
after:

- **Kept:** each bridged device's serial, version, names, port and outlet
  overrides (count and content), and the two `previous_firmware_version`
  values no real device would hold (`4.26.14M` and a 30-character image
  name). So that column is not a short one, and §2's finding 2 was a shape
  difference, not a migration risk.
- **Changed: record ids are UUIDs.** `_id`, `device_id`, `site_id`,
  `mgmt_network_id`, `connection_network_id`, `last_connection_network_id`,
  `native_networkconf_id` (port table and overrides) and `portconf_id` are
  36 characters where 10.x had 24 hex digits. `map_id` and the
  `wlangroup_id_*` fields still hold the old form. The bridge passes ids
  through without reading them.
- **The control channel:** the controller sent every bridge a fresh
  `system_cfg` on its first inform after the upgrade. Each bridge in the
  container applied it with nothing changed (`0 of 54 ports changed`,
  `0 of 16 outlets changed`). The on-switch bridge's log was not read; its
  device is connected with its overrides intact.

`scripts/wire-shapes.py audit` against the 10.6.109 shapes file now reports
every id path on every bridged device (36 characters against 24, `ascii`
against `hex`), and beyond those only differences this page already lists.

Open on Network 11 (as of 2026-11-13; §9.1 closes the first item):

- ~~**Almost no fixture is from Network 11.**~~ Done 2026-11-15, §9.1.
- **Few features were exercised by hand on Network 11.** Since 11.0.99
  (2026-11-14) UI edits reached the Arista (two IP rules, each added and
  removed) and the DX010 (its alert tests), the Arista's bridge renamed
  port 54 in the controller on each split and join of its cage (a REST
  write), a stub switch was adopted and forgotten, and on 11.0.97 a port of
  the Podman bridge was renamed and named back twice through the API
  (2026-11-13). Not run there yet: an outlet switch, a locate, a port's
  state or VLANs changed on a hypervisor bridge.
- The check in issue #3 for an over-long stored serial uses `mongo`, which
  the console no longer runs after the migration. It is only needed before
  it.

### 9.1 Network 11 captures (2026-11-15)

The console runs **11.0.99** (`stat/sysinfo`), so the captures are under
`docs/fixtures/controller-11.0.99/`, read-only (GETs, the alert log's
query, and the bridges' state files).

- **Shapes:** `shapes-usw.json`, the site's 41 real `usw` devices (the nine
  bridged ones left out). Against 10.6.109's: every id is a 36-character
  UUID, byte counters pass 32 bits, the `stat.sw` block is gone, and a
  device adopted that week brings `parent_device`, `adopt_state` and
  `firmware_revert`. The id-key patterns in `wire-shapes.py` and
  `shapes_test.go` know UUIDs now, and the capture leaves out the shape of
  a credential path (Network 11 records hold the adoption login).
- **The shape test** holds every driver's payload to both references
  (`shapesReferences`). Every excusal still excuses something; one new
  difference, against 11.0.99 only: the Smart-UPS's
  `outlet_ac_power_consumption` over 1000 W is 8 characters where every
  real device on the site now draws under 1000 W. 10.6.109's reference
  holds a real 8-character value, so the format is a real device's, and
  it is excused for that reference alone (`shapeExcusalsByRef`). No real
  payload difference was found.
- **Replay:** `replies-arista.ndjson` is the Arista bridge's own reply log
  from the last 10.6.109 push through every setparam since the upgrade
  (each with the reconcile after it): the upgrade's unchanged push,
  interval-only replies, Rogue DHCP detection's status pushed `disabled`,
  port 54 renamed and back twice, two rules added and removed.
  `TestReplayAristaNetwork11` (`internal/informloop`) drives the driver
  through it from an adopted start; each reply writes a reconcile's writes
  plus its own change. No `cmd` (locate or other) reached any bridge after
  the upgrade (the container's reply logs and the DX010's were read), so
  there is none to replay. The DX010's pushes since the upgrade (its alert
  tests) are in its on-switch log and not made a fixture: its replay would
  need a SONiC capture of the same day.
- **Alerts:** `scripts/sanitize-controller.py` scrubs the v2
  `system-log/device-alert` answer itself now (names inside `message`,
  composite names, ids that are names or MACs), checked over 30 days of
  alerts (147, 18 kinds) and by `scripts/sanitize-controller_test.py`;
  `system-log-device-alert-kinds.json` holds one alert of each kind.

`scripts/wire-shapes.py audit --shapes docs/fixtures/controller-11.0.99/shapes-usw.json`
on 2026-11-15 reports no id path. What it still reports, each known:

| Difference | Devices | Why it stays |
|---|---|---|
| `config_network.type` 6 > 4 | five with static IP Settings | `static`; the site's real switches are all on DHCP (§2, "Also seen") |
| `port_table[].sfp_current`, `sfp_part` longer | Arista, DX010 | 100G modules (§4's excusals) |
| `psu_table[].part_no` / `serial_no` | Arista, DX010 | the supplies name themselves, as the ECS does (§2) |
| `sys_stats.mem_buffer` past 32 bits | Arista, Proxmox-1, Podman | memory in bytes on hosts with gigabytes (§8) |
| `previous_firmware_version` | Arista (`4.26.14M`), DX010 (an image name), the three Proxmox nodes (13 > 12) | the controller's record of a version the device reported earlier, kept through the migration (§9); the Arista's and the DX010's are from before §5's fix, and 10.6.109's reference holds 13 characters |
| `outlet_table[].name`, `outlet_overrides[].name` 35 > 24 | the Back-UPS | outlet names set in the UI (it allows 128) |
| `hostname` 32 > 17 | the PDU | §2 finding 3 |
| `uplink.uplink_source` 15 > 13 | the three power devices | the controller's own label for how it placed the device (`legacy_only_sta`: a switch sees its MAC, no LLDP); no real `usw` here is placed that way. Not in the inform, whose `uplink` is a string |

Against 10.6.109's file the same audit reports 14 to 25 per device, the id
paths among them.

## 10. A real shape with the wrong meaning (2026-11-13)

A value can have a real device's type, length and range and still be
wrong. Every bridged `mac_table` entry passed the shape test, yet the
bridge filled `age` and `uptime` from one time, so one of the two always
meant the other: an `uptime` near zero behind the hypervisor bridges, an
`age` as large as the `uptime` behind the Arista. The controller reads
`uptime` as the learn time, and each of its restarts showed every guest
behind a bridge as reconnecting (`docs/mac-table-times.md`).

A shape check compares one path at a time and cannot see that. The
relation between two keys is now a test of its own: `uptime >= age` on
every entry of the real informs and of every driver's payload, the
per-port and top-level tables agreeing
(`internal/device/mactimes_test.go`). Where a real device's keys are tied
to each other, the relation gets its own check beside the shapes.
