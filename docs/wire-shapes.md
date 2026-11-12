# Wire shapes: holding a bridged device's values to a real device's

Status: **build spec** (2026-11-12). Three work packages, §4-§6; each
records what it built in §7.

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
in `sfp_compliance`, memory counters past 32 bits on a host with gigabytes
of RAM. Each is an excusal with its reason in §4's test, not a silence.

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
  (§ on the firmware string) still says `4.26.14M` goes out as `v4.26.14`,
  and `docs/prior-art.md` names the `v<numeric>` quirk.
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
