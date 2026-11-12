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
