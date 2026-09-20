# Rules for `internal/drivers/*`

You are in the vendor layer. Read `docs/adding-a-switch.md` first; the
contract is `internal/switchmodel/driver.go` + `model.go`.

- One directory per vendor/OS, package named after it, registered in `init()`
  with `switchmodel.RegisterDriver`, blank-imported from `cmd/switch-to-unifi`.
- Nothing UniFi-specific in here: no `port_table` keys, no `system_cfg` keys,
  no capability bits. Translate to and from `switchmodel` types only.
- Every command the driver uses exists on the OS version named in the
  driver's doc comment, and there is a scrubbed fixture for it under
  `docs/fixtures/<os>-<version>/`. `Start` runs all of them once and fails
  loudly. If a command is missing on the target version, find the right one
  on that version; do not assume newer syntax (see `docs/arista-eapi.md`).
- Speed capabilities per port come from the switch's own capability data.
- Writes are diffs against the last snapshot and idempotent; the loop
  re-applies after every inform.
- Breakout cages: fold lanes; speed to lane 1 to split/join, to all lanes
  for a lane-speed change; everything else to all lanes; set `LanesDiverge`
  when lanes differ; claim the cage's speeds.
- Never set an optical port to auto speed; only write FEC when asked.
- Tests use a fixture transport and cover parsing, every apply sequence, and
  idempotence. `go test ./...` must pass before a live run.
- The management address the bridge uses must be in-band (behind the
  uplink); report dedicated OOB management interfaces in
  `System.OOBInterfaces` so the loop can warn. See `docs/adding-a-switch.md` §2c.
- Fill `Port.Health` and the `System` reachability/health fields listed in
  `docs/adding-a-switch.md` §2; counters that exist only as text go through
  a `TextRunner`-style capability, never through guessing.
- Live verification order and what to record is in `docs/adding-a-switch.md` §4.
