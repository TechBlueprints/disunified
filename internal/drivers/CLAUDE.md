# Rules for `internal/drivers/*`

You are in the vendor layer. Read `docs/adding-a-device.md` first; the
contract is `internal/devicemodel/driver.go` + `model.go`.

- One directory per vendor/OS, package named after it, registered in `init()`
  with `devicemodel.RegisterDriver`, blank-imported from `cmd/disunified`.
- Nothing UniFi-specific in here: no `port_table` keys, no `system_cfg` keys,
  no capability bits. Translate to and from `devicemodel` types only.
- Every command the driver uses exists on the OS version named in the
  driver's doc comment, and there is a scrubbed fixture for it under
  `docs/fixtures/<os>-<version>/`. `Start` runs all of them once and fails
  loudly. If a command is missing on the target version, find the right one
  on that version; do not assume newer syntax (see `docs/drivers/arista-eos.md`).
- Speed capabilities per port come from the switch's own capability data.
- No secrets or site-specific values in code, comments, tests, fixtures or
  docs — no real addresses, names, MACs or serials, not even as a sample
  input in a unit test: documentation values only (`192.0.2.0/24`,
  `02:00:00:xx:xx:xx`, `SSJ00000000`). Captures go through
  `scripts/sanitize-*.py` first; `scripts/check-site-info.sh --staged`
  must pass before every commit. The real values live in
  `local-information/` (gitignored).
- Writes are diffs against the last snapshot and idempotent; the loop
  re-applies after every inform.
- Breakout cages: fold lanes; speed to lane 1 to split/join, to all lanes
  for a lane-speed change; everything else to all lanes; set `LanesDiverge`
  when lanes differ; claim the cage's speeds.
- Never set an optical port to auto speed; only write FEC when asked.
- Tests use a fixture transport (`aristaeos.FixtureTransport` for a
  command API; `proxmox`/`podman.FixtureRunner` for a script over SSH;
  `apcups.FixtureRunner` for a register map) over real captures — never
  hand-written samples — and cover parsing, every apply sequence, and
  idempotence. `go test ./...` must pass
  before a live run, including the wire-contract and replay tests, which
  drive your driver end to end against real controller data.
- The management address the bridge uses must be in-band (behind the
  uplink); report dedicated OOB management interfaces in
  `System.OOBInterfaces` so the loop can warn. See `docs/adding-a-device.md` §2c.
- Fill `Port.Health` and the `System` reachability/health fields listed in
  `docs/adding-a-device.md` §2; counters that exist only as text go through
  a `TextRunner`-style capability, never through guessing.
- Live verification order and what to record is in `docs/adding-a-device.md` §4.
- **A host read over SSH uses `internal/sshrun`** (`Runner` + `SSH`), one
  script per poll printing tagged sections, and a `FixtureRunner` that
  serves the capture and records writes. Reduce the script's output *on
  the host* to the fields the driver reads: `podman inspect` carries every
  container's environment, which is where API keys live, and a raw capture
  of it must never be committed (2026-09-27).
- **Power devices:** outlets are `Snapshot.Outlets`, the battery is
  `System.Battery`; an outlet is real load, so `ApplyOutlets` is a strict
  diff, `OutletPlanner` lets the loop hold a first push that would switch
  anything, an unswitchable group is reported without a relay bit and
  refused if pushed, and a placeholder slot the model draws but the device
  lacks is reported present-but-off with no relay. A configuration written
  to the unit (a load-shed policy) sits behind an explicit driver option,
  is idempotent, and is read back before it is logged.
- **A device's own address (`AddressController`)** is applied only when the
  driver can verify it will still reach the device afterwards; DHCP is
  declined by a driver that cannot predict the lease. Adopting a device
  deletes its client record, fixed-IP reservation and client DNS name --
  the controller's IP Settings are the replacement (`docs/drivers/podman.md` §5).
