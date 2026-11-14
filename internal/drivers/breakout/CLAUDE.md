# breakout — facts that cost time

Read `internal/drivers/CLAUDE.md` first. Write-up: `docs/drivers/breakout.md`
(§0 as built, §14 open, §15 the first live adoption). Built 2026-11-14 with
`sonic` as the parent; nothing of it has run live.

- **No device, no transport.** The child is a view over its parent's last
  snapshot (`devicemodel.LaneProvider.Lanes`) and a caller into the parent's
  plan (`ApplyLanes`/`PlanLanes`). It runs in the parent's process: an entry
  of the same config file, found by name (`devicemodel.OpenedDevice`).
- **main attaches, the child reads back.** `cmd/disunified` calls
  `AttachLaneSwitch` before the parent's loop starts (else the parent's first
  apply writes the cage's intent over the lanes on every restart) and computes
  the MAC once (`devicemodel.LaneSwitchMAC` or `options.mac`); the child takes
  it from `LaneView.Child`, never computes its own.
- **The controller side is proven by the stub, not by this driver**
  (`docs/breakout-lag.md` §6.6): placement by the parent's MAC-table or LLDP
  entry, the shared address, the Flex XG profile (port 5 the uplink), the
  aggregate push. The child reports the stub's shapes on purpose (one
  address on `eth0`, the cable as port 5).
- **The aggregate's id is the push's `lag`** (`lag=1` for the first aggregate,
  whatever its members), so the first lane aggregate of cage N is
  `PortChannel<100*N+1>` on SONiC.
- **Speed auto on a lane = what it runs.** Forced speeds the lane cannot run
  are refused with a note; a lane never splits or joins its cage. The
  controller's stored speed follows a forced override, not the inform
  (§6.6.3): after a re-split under an aggregate the record can show the old
  forced speed until the lane is set to auto (open, §14.1).
- **Stale parent = no answer.** Older than `options.max_age` (3 min), the
  child's collect fails and the loop withholds its informs as for any device.
