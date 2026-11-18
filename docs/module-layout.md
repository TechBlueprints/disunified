# What a module runs: one table for every driver (spec, 2026-11-17)

Status: **spec, not built.** Today only `sonic` takes the speed or breakout
a module calls for when UniFi says "auto" (`moduleRuns` in
`internal/drivers/sonic/optic.go`, [`drivers/sonic.md`](drivers/sonic.md)
§10.9), from a table keyed by xcvrd's compliance *name*. `arista-eos` has no
such rule: on auto it writes no speed on an optical port at all. The FEC half
is already shared (`internal/fecpolicy`, [`fec-by-standard.md`](fec-by-standard.md)).
This is the speed half: one table, keyed by the module's own EEPROM codes,
used by both parents.

## 1. The shared package: `internal/modulelayout`

Beside `internal/fecpolicy`, with the same rules: no vendor, no controller,
no I/O.

```go
// Module is an SFF-8636 module's two compliance bytes.
type Module struct {
    Ethernet int // byte 131: the 10/40G Ethernet compliance bits; bit 7 = "Extended"
    Extended int // byte 192: the SFF-8024 extended specification compliance code
}

// Layout is one way to run a cage: Ports interfaces of SpeedMbps each.
type Layout struct{ Ports, SpeedMbps int }

// Runs is what the module runs in a QSFP28 cage, its native layout first.
// ok is false for a module the table does not know.
func Runs(m Module) (layouts []Layout, ok bool)
```

The table is §10.9's, unchanged in content, re-keyed. The code numbers are
SFF-8024 Table 4-4 as sonic-platform-common's `sff8024.py`
(`EXT_SPEC_COMPLIANCE`) and `sff8636.py` (`ETHERNET_10_40G_COMPLIANCE`) list
them; the build takes them from those files, not from this page.

When `Ethernet` has bit 7 (Extended), by `Extended`:

| Extended code | Name | Runs | Native |
|---|---|---|---|
| 0x03, 0x04, 0x06, 0x17, 0x20, 0x21, 0x25, 0x26, 0x27 | 100GBASE-LR4, -ER4, CWDM4, CLR4, SWDM4, PAM4 BiDi, 100GBASE-DR, 100G-FR, 100G-LR | 1x100G | 1x100G |
| 0x02, 0x07, 0x01, 0x08, 0x18, 0x19 | 100GBASE-SR4, PSM4, 100G AOC and ACC (both BER grades) | 1x100G, 4x25G | 1x100G |
| 0x0B | 100GBASE-CR4 / CA-25G-L / 50GBASE-CR2 with RS | 1x100G, 4x25G, 2x50G | 1x100G |
| 0x0C, 0x0D | CA-25G-S, CA-25G-N (25G/50G-only codes: a breakout cable) | 4x25G, 2x50G | 4x25G |
| 0x10, 0x1F | 40GBASE-ER4, 40G SWDM4 | 1x40G | 1x40G |
| 0x12 | 40G PSM4 Parallel SMF | 1x40G, 4x10G | 1x40G |
| 0x11 | 4 x 10GBASE-SR | 4x10G | 4x10G |

Otherwise (and when the extended code names nothing above but the Ethernet
byte has another bit), by the `Ethernet` bits:

| Bit | Name | Runs | Native |
|---|---|---|---|
| 0x02 | 40GBASE-LR4 | 1x40G | 1x40G |
| 0x04, 0x08, 0x01 | 40GBASE-SR4, 40GBASE-CR4, 40G Active Cable (XLPPI) | 1x40G, 4x10G | 1x40G |

Anything else: not known (`ok` false). The EEPROM has no "far end broken
out" field, so a code that fits a straight cable and a breakout runs both
and a split made for it is kept (§10.9's reasoning, unchanged).

The package's own test holds every row above, and holds that every extended
code `fecpolicy.FromSFF8024` gives a medium other than `Other` for has a row
here (the two tables speak for the same modules).

## 2. `sonic` moves onto it, with no change in behaviour

- `optic.go` gains the reverse of what it has: xcvrd's compliance name to a
  `modulelayout.Module` (the `sff8024Codes` map widened to every name in
  §1's first table; the 10/40G names to their Ethernet bit). `moduleRuns`
  and `laneCfg`'s table go; `laneCfg` becomes `modulelayout.Layout` or a
  thin conversion.
- `moduleSpeed`, the breakout path, `controllerOnly`, `owned`, `PlanLayout`
  and every note text stay as they are.
- Done when every existing sonic test passes **unchanged** (`speed_test.go`,
  `hold_sonic_test.go`, the breakout and lane tests). A test that had to
  change is a behaviour change: stop and say why.

## 3. `arista-eos` gains the rule (EOS 4.26.14M)

### 3.1 What EOS can be told per cage

All already used by the driver and captured (`apply.go`, the driver's
`CLAUDE.md`):

- `speed forced 25gfull` / `10gfull` on lane 1 splits a cage into four
  lanes; `speed forced 100gfull` / `40gfull` on lane 1 joins it.
- A lane-speed change on a split cage goes to every lane, or the others
  errdisable ("speed-misconfigured").
- **An optical port is never set to auto speed.** The rule below only ever
  writes `speed forced <native>`; it never writes `speed auto` or `default
  speed`, and never touches a cage whose configuration the module runs.

Not known, to capture before it is relied on: how the fold reads a cage run
as 2x50G (`speed forced 50gfull`; EOS names the interfaces `/1` and `/3`),
and what `show interfaces transceiver properties` gives as `adminSpeed` for
a QSFP28 cage with no speed line. Until captured: a 2x50G cage is a layout
the rule recognises and keeps if the fold can already represent it, and
never one it writes (no native layout is 2x50G); a cage with no speed line
is read as "one port, speed EOS's own" and satisfies any module that runs
one port.

### 3.2 The module

`moduleCodes` (`fec.go`: `show interfaces transceiver eeprom`, already read
for the FEC rule, the same staleness rule) is the `Module`. When the codes
were not read or are stale, the media type speaks as it does for FEC:
`100GBASE-CWDM4` is `{0x80, 0x06}`, `100GBASE-CR4` is `{0x80, 0x0B}` (the
class that runs every copper layout, so it never changes a split or a
whole cage; a CA-S or CA-N cable is then corrected once its codes are
read). Any other string: no opinion. Map no further string without a
capture.

### 3.3 The rule on auto

For a QSFP28 cage the controller pushes no speed for (`SpeedMbps == 0`),
not copper RJ45, with a module seated and known to §1:

1. Current layout = the number of lanes the fold shows, by their admin
   speed (all lanes alike; lanes apart, or `LanesDiverge`, match nothing).
2. A layout the module runs stays. Nothing is written.
3. Otherwise the cage takes the module's native layout as a forced speed:
   a different port count is a split or join on lane 1; the same port count
   is a lane speed on every lane. Sequence and `c.refresh` as for a pushed
   speed.
4. A cage with a lane switch attached (owned): the module decides the port
   count only, as on sonic; a join dissolves the child's aggregates and
   mirror sessions first, by the existing `dissolveLanes` path.
5. Refused with a note, as sonic refuses a breakout: a cage that is a
   Port-Channel member or a mirror source/destination.
6. No module, or one the table does not know (noted once): nothing, as
   today.
7. The FEC rule waits a cycle on a port whose speed this rule changes
   (`fecPlan`'s "the speed is changing" branch must see the module's speed
   as it sees a pushed one), and the EEPROM codes are re-read after the
   write, as after any written speed.

These are the driver's own writes: `PlanPorts` and `PlanLanes`
(`planScope.controllerOnly`) plan **entirely** without them, so the
first-push hold and the fresh-port withholding never wait on a write no
controller setting can match (sonic's mutation test is the pattern: a
planned join must not leave anything the controller-only plan counts).

### 3.4 `LayoutPlanner`

`Collector.PlanLayout(desired)` reports the ports whose lane count
`ApplyPorts(desired)` would change, the module rule's and a pushed speed's
alike, as sonic's does, so the loop does not take a cage the rule split or
joined for a new port and withhold it (`internal/informloop`,
`devicemodel.LayoutPlanner`). With `lane_switches: auto` a cage the rule
splits then gets its pending lane switch by the existing watcher; nothing
new is needed there, but the test covers it.

## 4. Tests and the live run

- Fixtures: the `fec-auto/` read set (one sitting: cage 49 CWDM4 whole,
  cage 50 a CA-25G-S breakout cable split 4x25G, cages 51-53 CA-25G-L
  whole). As captured the rule writes nothing and logs no note, for the
  device plan, the lane plan and `PlanLayout`.
- The states the rule acts on need captures of their own, taken in one
  sitting each under the Arista test lock, in a new dated directory beside
  `fec-auto/`: cage 50 joined at 100G with its CA-S cable in (the rule must
  split it), and cage 50 split at 10G lanes (the rule must set 25G on every
  lane). Then, from those: the split, the lane-speed write, idempotence
  after each (the fixture transport's remembered writes), the
  controller-only plan writing none of it, `PlanLayout` naming the cage,
  the Port-Channel and mirror refusals, the owned-cage case, the stale-codes
  fallback, and the FEC rule skipping the cycle of the speed change.
- Live, on cage 50 only (never cages 49, 51-53, Port-Channel1 or
  Management1): before anything, read link state and LLDP on cage 50's four
  lanes and whether its lane switch is adopted. A lane with link, or an
  adopted lane switch: stop and report instead. Otherwise: join it by hand,
  capture, let a test build's rule split it on auto, capture, and leave the
  cage as found (`write memory`, `show running-config diffs` empty). The
  pending automatic lane switch for the cage goes away and comes back with
  the split; say so in the report.

## 5. Docs the build updates

`drivers/sonic.md` §10.9 (the table moves here; the section points to it),
`drivers/arista-eos.md` §14 (new: the rule, the captures, what ran live),
`internal/drivers/CLAUDE.md` ("`sonic` also takes the speed..." becomes
both drivers, "never a table of the driver's own"), the two drivers'
`CLAUDE.md`, `support-matrix.md`, and `drivers/breakout.md` §16.10's note
that the Arista parent has no module rule.
