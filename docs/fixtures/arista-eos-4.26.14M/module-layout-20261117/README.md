# The module's layout on auto: cage 50 in six states (2026-11-17)

Captured on the DCS-7160-48TC6-F (EOS 4.26.14M) for the module-layout rule
(`internal/drivers/arista-eos/layout.go`, `docs/module-layout.md`,
`docs/drivers/arista-eos.md` §14), under the Arista test lock. Each
directory is the whole read set the driver takes at `Start` and `Collect`,
in one sitting, recorded through the driver's own SSH transport; the
`show interfaces transceiver eeprom` reply is reduced to each cage's
`Identifier` and `Compliance` fields, as in `fec-auto/`. All six scrubbed
with `scripts/sanitize-arista-eos.py` in one run. No `phy detail` texts
(the SSH transport has no text runner).

The rest of the switch as in `fec-auto/`: cage 49 a 100G CWDM4 optic
(linked), cages 51-52 CA-25G-L cables in Port-Channel1, cage 53 a CA-25G-L
cable, cage 54 empty. Cage 50 holds a QSFP28-to-4xSFP28 passive breakout
cable (SFF-8024 extended code 12, CA-25G-S) with no far end, so no lane
ever linked. The controller pushes no speed for port 50 (auto).

| Directory | Cage 50 | How it got there |
|---|---|---|
| `joined/` | one port, `speed forced 100gfull`, adminSpeed 100Gbps | by hand on lane 1, from the 4x25G split |
| `nospeed/` | one port, no speed line, **adminSpeed 100Gbps** | `no speed` on the joined cage's lane 1 |
| `split-by-rule/` | four lanes at 25G | the rule's own write, from `joined/` (the speed line restored first): `interface Ethernet50/1`, `speed forced 25gfull` |
| `split-10g/` | four lanes at 10G | `speed forced 10gfull` on every lane by hand |
| `50g-lane1-only/` | /1 at 50G, /3 and /4 at 25G and **errdisabled `speed-misconfigured`**, /2 gone | `speed forced 50gfull` on lane 1 of the 4x25G split |
| `split-2x50/` | /1 and /3 at 50G (two ports) | then `speed forced 50gfull` on /3 |

From `split-10g/` the rule wrote `speed forced 25gfull` to all four lanes
(live; the read-back is the fixture transport's modelled lane speed). After
`split-2x50/` the running config still carries `speed forced 25gfull` under
the hidden /2 and /4. The cage was put back with `speed forced 25gfull` on
/1 and /3: its four interfaces' running config the same as before the
first change, `write memory`, `show running-config diffs` empty, nothing
errdisabled.
