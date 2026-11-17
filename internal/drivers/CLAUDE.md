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
- Never set an optical port to auto speed; only write FEC when asked --
  or, where the platform reads the module, the FEC the standard gives the
  module when UniFi pushes none (UniFi's own "auto"): chosen by
  `internal/fecpolicy` (the one table, `docs/fec-by-standard.md` §4;
  never a table of the driver's own) within what the port supports,
  nothing written when no permitted mode is supported or when the port is
  up on a permitted one (`sonic`, docs/drivers/sonic.md §10.3, §11.37;
  `arista-eos`, docs/drivers/arista-eos.md §13). `sonic` also takes the
  speed (or breakout) the module calls for (§10.9). All of it is planned
  without by the `Planner`, so the first-push hold never waits on it.
- Tests use a fixture transport (`aristaeos.FixtureTransport` for a
  command API; `proxmox`/`podman.FixtureRunner` for a script over SSH;
  `apcups.FixtureRunner` for a register map) over real captures — never
  hand-written samples — and cover parsing, every apply sequence, and
  idempotence. The transport remembers its writes (a later read shows
  them, in forms captured from the device after such a command; the rest
  is recorded as unmodelled), so a replay holds a reconcile after an
  applied push to writing nothing (`docs/drivers/arista-eos.md` §11). `go test ./...` must pass
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
- **A MAC entry has two times** (`MACEntry`): `LearnedAt` only from a learn
  or move time the device itself keeps (EOS `lastMove`, a container's
  start), never from a refresh; `LastSeen` from a last-frame time. Leave
  either zero when unknown: the loop then supplies the learn time. A
  refresh time as the learn time makes every controller restart look like
  every client reconnecting (`docs/mac-table-times.md`).
- **Power devices:** outlets are `Snapshot.Outlets`, the battery is
  `System.Battery`; an outlet is real load, so `ApplyOutlets` is a strict
  diff, `OutletPlanner` lets the loop hold a first push that would switch
  anything, an unswitchable group is reported without a relay bit and
  refused if pushed, and a placeholder slot the model draws but the device
  lacks is reported present-but-off with no relay. A configuration written
  to the unit (a load-shed policy) sits behind an explicit driver option,
  is idempotent, and is read back before it is logged.
- **`apcnmc` is not a driver**: it registers nothing and holds what the
  APC drivers share about the management card itself -- its `config.ini`
  (section parsing, the fixture runners' `Merge`), the syslog section
  (`SyslogApplier`, behind `devicemodel.SyslogController`) and the clock
  (`ClockApplier`, `[SystemDate/Time]`, behind `devicemodel.ClockController`:
  a POSIX zone mapped exactly onto the card's fixed offsets and its
  `Traditional US` DST, or refused). A key it writes must exist in the
  card's own file, and a card's config.ini uploads share one `Uploads` gate
  (the appliers', the driver's `ApplyAddress` and the PDU's outlet names;
  a new upload takes its turn there too, `Uploads.Take`): the card drops an upload that lands
  while it applies the last.
- **`reach` is not a driver** either: the MAC, netmask, gateway, gateway
  MAC and reported address of a device that cannot report them itself
  (`apc-ups`, `apc-backups`, `stub`). The netmask and gateway come from the
  site's network list, which a driver takes by implementing
  `devicemodel.SiteNetworkUser` (`stub` does not: it reports what its file
  and options say). The gateway MAC comes from the bridge's own neighbour
  table, read only on the device's segment. The options override all of
  them (`docs/drivers/apc-ups.md` §14). A new driver with the same need
  embeds `reach.Identity`; it does not copy it.
- **`breakout` has no device** (the lane switch, `docs/drivers/breakout.md`):
  a split cage of a parent that implements `devicemodel.LaneProvider`,
  read from the parent's last snapshot and written through the parent's
  own plan, in the parent's process. A parent keeps its split cages' lanes
  as ports of their own, scopes its plan to one cage's lanes
  (`PortChannel<100*cage+lag>` on sonic; `Port-Channel<1000+10*cage+lag>`
  on EOS, whose ids stop at 2000), leaves an attached cage's lanes to the
  child but for the breakout, the admin state and the STP port settings,
  never touches a channel group outside its own range (but removes an
  empty one of its own making in the lane range of a cage with no lane
  switch attached; one with members stays, noted: breakout.md §7c), and serialises its
  writes and collects (the child applies from its own loop). `sonic` and
  `arista-eos` are the parents.
- **`stub` is an instrument**, not a device: a snapshot from a file, no
  write interface. Use it to ask the controller a question before building
  a driver for the answer (`docs/drivers/stub.md`).
- **A device's own address (`AddressController`)** is applied only when the
  driver can verify it will still reach the device afterwards; DHCP is
  declined by a driver that cannot predict the lease. Adopting a device
  deletes its client record, fixed-IP reservation and client DNS name --
  the controller's IP Settings are the replacement (`docs/drivers/podman.md` §5).
