# breakout — facts that cost time

Read `internal/drivers/CLAUDE.md` first. Write-up: `docs/drivers/breakout.md`
(§0 as built, §14 open, §15 the first live adoption, §16 automatic lane
switches, §16.10 the live run). Built 2026-11-14 with `sonic` as the
parent; automatic since 2026-11-16 and adopted live that day on the
DX010's cage 3. The Arista parent is fixture-tested only.

- **No device, no transport.** The child is a view over its parent's last
  snapshot (`devicemodel.LaneProvider.Lanes`) and a caller into the parent's
  plan (`ApplyLanes`/`PlanLanes`). It runs in the parent's process: an entry
  of the same config file, found by name (`devicemodel.OpenedDevice`).
- **Attachment is ownership, and the child answers without it.** Unattached,
  the child reports the lanes under its own MAC and name and refuses every
  write (`attached()` asks the parent each time): that is an automatic lane
  switch waiting for its adoption (`cmd/disunified/lanes.go`, §16). Its loop
  attaches on `OnConnected` and detaches on `OnUnadopted` (`setdefault`). A
  declared entry, and an automatic one whose state file is adopted, is
  attached by main **before the parent's loop starts** (else the parent's
  first apply writes the cage's intent over the lanes on every restart).
- **The MAC is the child's own**: `options.mac`, else
  `devicemodel.LaneSwitchMAC(parent MAC, cage)`, the same derivation main
  uses for the attachment; the name is `options.name` (main passes the
  entry's name). A cage attached to a different MAC does not answer. A test
  that attaches a made-up MAC must pass the same one in the options.
- **A `setdefault` drops the stored push** (`device.Session`, every driver):
  a forgotten device stops reconciling, and its next adoption's first push
  is held as a first push again. Without it a forgotten lane switch tried to
  apply its old push every cycle and was refused.
- **The controller side was first proven by the stub**
  (`docs/breakout-lag.md` §6.6): placement by the parent's MAC-table or LLDP
  entry, the Flex XG profile (port 5 the uplink), the aggregate push. The
  child reports the stub's shapes on purpose (one address on `eth0`, the
  cable as port 5) -- except the address.
- **Never the parent's address.** A second adopted device reporting a
  statically addressed device's IP makes the controller refuse every save
  of that device (`api.err.StaticIpAlreadyUsedByDevice`). The lane switch
  reports `breakout.Address(mac)`, link-local.
- **The controller checks every stored override on every save of a
  device.** A forced lane speed left on the cage's port from its time as a
  fold (SFP28) is invalid once the cage reports QSFP28, and blocks all the
  parent's saves (`api.err.InvalidFecMode`, the cage's `port_idx`); main
  sets that port to Auto at the adoption (`unifiapi.CageToAuto`), and for
  a declared entry also at the start, since it owns the cage from then.
- **An automatic lane switch is adopted by the bridge** (2026-11-17,
  `breakout.md` §16.12): main's watcher asks the controller, by the exact
  MAC, once the parent is adopted. A test or a live run that wants one to
  stay pending needs `lane_switches: manual` on the parent; a Forget is
  followed by a re-adoption while the cage is split (unplug or join the
  cage first, then forget the disconnected record). A declared entry is
  still adopted by hand.
- **A joined cage retires its lane switch** (2026-11-18, `breakout.md`
  §16.13; `cmd/disunified/lanes_retire.go`): four joined reads of the
  parent (`LaneProvider.CageLanes`, a new snapshot each) over two minutes
  stop the child; an automatic one is detached, forgotten by its exact MAC
  (`unifiapi.ForgetLaneSwitch`, `api.err.DeviceBusy` retried) and its
  state directory removed. `manual`, no API and declared: stopped only. A
  new parent implementing `LaneProvider` must answer `CageLanes` 0 for any
  read it cannot vouch for (starting, the fold disagreeing with its lanes):
  a 1 retires. A live test that wants a joined cage's lane switch kept
  needs it declared, or `lane_switches: manual`.
- **Its UniFi name is `<parent's UniFi name>-cage<N>`** (2026-11-18,
  `breakout.md` §16.14): main's provision reads the parent's record by
  `ParentMAC()` and names the lane switch over a default name only
  ("USW Flex XG" is the controller's default for a `USFXG`, now in
  `unifimodel.ControllerDisplayNames`; before that the provision took it
  for an operator's name and named nothing). `DeviceName` here is the
  fallback form and counts as a default.
- **A Forget sent to see what the controller answers is a Forget**
  (2026-11-18: two such probes forgot cage 26's lane switch, each costing a
  re-adoption or the bridge's own Forget). To see whether it is busy, read
  the record in `stat/device`; let the bridge's retire make the call.
- **A declared entry is never detached**: a Forget of it is logged (the
  file still declares it; Adopt or removing the entry ends it). The
  decision and its reason are `breakout.md` §0.
- **A Forget is `_type: setdefault`, sent once**; afterwards the old key
  gets HTTP 400 for good. A Forget made while the bridge is down still
  reaches it at its next inform (seen after 44 s). One it never saved (a
  crash, a restored state file) is read from the 400s: three over two
  minutes on the adopted key release a lane switch, declared or automatic,
  as its `setdefault` would (`informloop/rejectedkey.go`, `breakout.md`
  §16.11). No other device acts on them: it logs a WARNING.
- **The aggregate's id is the push's `lag`** (`lag=1` for the first aggregate,
  whatever its members), so the first lane aggregate of cage N is
  `PortChannel<100*N+1>` on SONiC.
- **Speed auto on a lane = what it runs.** A lane claims the speeds it
  runs plus the cage's other split modes' lane speeds, and a forced one of
  another mode re-splits the cage through the parent (4x25G to 2x50G and
  back, `breakout.md` §16.15); speeds of two modes at once change nothing
  (a note); a lane never joins its cage. **Every lane is SFP28**, whatever
  it runs: the controller takes 50G only without `fec_mode` and a 25G FEC
  only from an SFP28, and the drawer sends a FEC with 25G and none with
  50G (captured 2026-11-18); as a QSFP28 a 2x50G lane could not be set back
  to 25G from the drawer. The split hands the parent all four lane slots
  (a 2x50G cage's 3-4 too: a re-split back writes their intent in the same
  batch), and its empty slots are `Reserved` (the provision keeps their
  overrides). The controller's stored speed follows a forced override, not
  the inform (§6.6.3).
- **The cable (port 5) is up while attached**, the cage enabled on the
  parent, lanes with link or not; its link changes are its own drops
  (`Uplink`, counted across collects), never a lane's: summed lane flaps
  could raise a link-flap alert on the uplink (§16.15).
- **A re-split is not a new port to either loop**: the parent counts its
  lane switches' re-splits and names the cage in `Snapshot.LayoutWritten`
  at its next collect; the child sees `LaneView.LaneLayouts` change and
  names every lane slot. The lane plan writes nothing until the parent has
  read the switch again.
- **Stale parent = no answer.** Older than `options.max_age` (3 min), the
  child's collect fails and the loop withholds its informs as for any device.
