# FEC by standard, by module, by switch (research, 2026-11-16)

What forward error correction a link is supposed to run, in three layers:
what the standards define per speed and medium, what a module's EEPROM
says it needs, and what a switch says it can do. Written to judge the
DX010's "25G lanes support only none and FC" before trying to override it.
Sections 1 to 4 are research; section 5 is a test run on the DX010 the
same day.

Names: **FC** = BASE-R FEC, "fire code", IEEE 802.3 Clause 74 (UniFi's
`cl-74` / `fc-fec`). **RS** = Reed-Solomon RS(528,514): Clause 91 at 100G,
Clause 108 at 25G (UniFi's `cl-91` / `rs-fec`). RS(544,514), Clause 134, is
the PAM4 generation's and is part of those PHYs, not a setting.

Confidence is marked per row. "IEEE tables" means the standard's own
tables as reproduced in Intel's *25 Gb Ethernet Media Guide* (rev 1.3,
which prints IEEE 802.3-2018 Tables 110-1, 110-2, 112-1 and Annex 110C).
"Secondary" means vendor documentation and encyclopaedia summaries; the
published standard was not read for those rows.

## 1. What the standards say

| Speed | Medium | FEC | Basis |
|---|---|---|---|
| 10G | SR, LR, SFP+ DAC | **none defined** | secondary |
| 10G | KR backplane | FC optional | secondary (802.3ap "optional layer for forward error correction") |
| 10G | 10GBASE-T | LDPC inside the PHY, not a setting | secondary |
| 25G | DAC, `25GBASE-CR` host | must implement FC **and** RS; the cable decides which run (below) | IEEE tables (110-1) |
| 25G | DAC, `25GBASE-CR-S` host | must implement FC; **RS not applicable** | IEEE tables (110-1) |
| 25G | SR, LR, ER optics | **RS required**; no FC is defined for optics; no auto-negotiation | IEEE tables (112-1; figure 114-1) |
| 40G | CR4 (DAC), KR4 | FC optional, per lane, by auto-negotiation | secondary (802.3ba clause 80 summary) |
| 40G | SR4, LR4, ER4 optics | none | from memory of 802.3ba; not confirmed by a source read here |
| 40G | any | **RS does not exist at 40G** | secondary |
| 50G on two 25G lanes (25G/50G Consortium: `50GBASE-CR2`) | DAC | RS, FC or none **by cable**, as at 25G (the EEPROM codes say "with RS", "with BASE-R", "with no FEC") | SFF-8024 code names; the consortium text was not read |
| 50G on one PAM4 lane (IEEE 802.3cd) | CR, SR, FR, LR | RS(544,514) built in | secondary. A QSFP28 cage cannot run this |
| 100G on four 25G lanes | CR4, KR4, SR4 | RS required | secondary + memory (Juniper: SR4 runs 100GAUI-4 "Yes (KR4 FEC)") |
| 100G | CWDM4, PSM4 (MSAs) | RS on the host (CWDM4: needed for full reach; PSM4: sources disagree, "optional" in one) | secondary |
| 100G | LR4 | **none** ("specified to not use any FEC") | secondary |
| 100G | CLR4 | optional | secondary |
| 100G | any 4x25G | FC is not defined | secondary |

So Clint's reading is right: 10G pluggables have no FEC at all, and 40G
has only an optional FC on copper.

### 25G copper, where the cable decides (IEEE 802.3-2018, clause 110)

| Cable class | EEPROM code | Typical length | Modes the link may run | On a `CR` host | On a `CR-S` host |
|---|---|---|---|---|---|
| CA-25G-N | 0Dh | to 3 m | RS, FC or none | yes | yes |
| CA-25G-S | 0Ch | to 3 m | RS or FC | yes | yes (FC) |
| CA-25G-L | 0Bh | to 5 m | **RS only** | yes | **no** (Table 110-2) |

The mode is resolved by clause 73 auto-negotiation; with it off, both ends
must be set alike. Length is a habit of the market, not the rule: the
class is the cable's loss.

### 25G optics and active cables (the same guide's Table 3)

| Module | EEPROM code | FEC |
|---|---|---|
| 25G-SR, worst BER 5e-5 | 02h | RS required |
| 25G-LR, worst BER 5e-5 | 03h | RS required |
| 25G AOC, worst BER 5e-5 | 01h | RS required |
| 25G AOC, BER 1e-12 or better | 18h | none |
| SR or LR built to BER 1e-12 | 02h / 03h (the same codes) | none -- but the code cannot tell these from the ones above |

## 2. What the modules in the DX010 say (read 2026-11-16)

| Cage | Module | Declares | Runs | FEC set / operating | By the standard |
|---|---|---|---|---|---|
| 1 (uplink), 2 | 2 m passive DAC | 0Bh: `100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS` | 100G, up | rs / rs | CR4 requires RS: correct |
| 3 | 1 m QSFP28-to-4xSFP28 passive DAC | 0Ch: `25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R` | 4 x 25G, no far end | fc / (down) | CA-S runs FC or RS: correct, and FC is what a CR-S host uses |

Nothing in the unit is set against its module.

## 3. What the switch says, and how to read it

STATE_DB `supported_fecs` (the SAI's answer, 202511 image):

| Port layout | Reported | Against the standard |
|---|---|---|
| whole cage (100G, 40G) | none, rs | right at 100G. At 40G the list is wrong in both directions: RS does not exist there and FC (optional on CR4) is refused. The list is per port, not per speed |
| 2 x 50G | none, rs | a "with BASE-R" cable's FC is not offered; such a cable's class also allows RS |
| 4 x 25G | none, fc | **exactly a `25GBASE-CR-S` host**: FC, no RS |

That last row is the answer to "suspicious": the standard has a name for a
25G port without RS, because hosts designed before 802.3by added
single-lane RS exist. Intel's guide says the same of switches: some
"entered the ecosystem before the IEEE standard added these clauses". The
DX010's chip is of that generation, and the pattern fits (RS where two or
four chip lanes carry the link, FC on one). One vendor's FEC page read
through a search summary only says the same of the chip family (Tomahawk
and Tomahawk+: Clause 74 at 25G; later chips: Clause 108). It is still the
SAI's word, and this SAI's capability answers have been wrong before
(`sonic.md` §11.9).

What a CR-S lane means in practice:

- **Fine:** CA-N and CA-S DACs (short ones), the breakout cable in cage 3.
- **Not supported by the standard:** CA-L DACs (the code of both 100G DACs
  in the unit, if one were ever split) and **every standard 25G optic**:
  SR, LR and ER require RS and define no FC. An optic on such a lane runs
  without the FEC its budget assumes; short links often come up, none is
  guaranteed. That bears on any SFP28 optic behind a QSA.

## 4. Against the driver's own table (`moduleFEC`, `optic.go`)

- **Wrong:** at 25G, `100GBASE-LR4 or 25GBASE-LR` and `100GBASE-ER4 or
  25GBASE-ER` map to "disabled". The standard requires RS for 25GBASE-LR
  and -ER. In a QSFP28 cage that code at 25G can only be an SFP28 optic
  behind a QSA, so it matters exactly there.
- **Too narrow:** the table gives one mode per cable, the standard a set
  (CA-N: any; CA-S: FC or RS; CA-L: RS). Choosing from the set, within what
  the port supports, would let a "with BASE-R" cable run RS on a 2x50G
  cage instead of being left alone, and would say outright when a cable's
  set and the port's list do not meet (CA-L on a 25G lane).
- **Right:** 10G and 40G to "disabled", 100G CR4/SR4/CWDM4 to RS, LR4 to
  none, CA-S to FC, CA-N to none, the 50G rows.

The controller's own rule (a FEC only at 25G on SFP28, at 40G and 100G on
QSFP28, never at 10G or 50G; `breakout-lag.md` §7) lines up with section 1.

## 5. Setting RS on a 25G lane anyway: tried, and the chip refuses (2026-11-16)

Run on the DX010 (202511 image), on the fourth lane of the split cage
(`Ethernet11`, the chip's `xe9`, no link), with the test lock held, each
layer restored before the next. The chip's shell shows a port's two FEC
switches directly (`bcmcmd "phy control <port>"`: `CL74` is FC, `CL91` is
RS), which is the readback used throughout: the lane read `CL74 = True,
CL91 = False` before and after.

| Layer | What was done | Answer | The chip afterwards |
|---|---|---|---|
| 1. SONiC's CLI | `config interface fec Ethernet11 rs` (known from before) | refused: `fec rs is not in ['none', 'fc']` | unchanged |
| 2. CONFIG_DB, past the CLI | `sonic-db-cli CONFIG_DB hset 'PORT\|Ethernet11' fec rs` | CONFIG_DB and APPL_DB took `rs`; orchagent logged `doPortTask: Unsupported port Ethernet11 FEC mode rs` and dropped the task (its own check of the same list, `isFecModeSupported`) | unchanged: ASIC_DB `SAI_PORT_FEC_MODE_FC`, `CL74 = True` |
| 3. The SAI, past orchagent | `SAI_PORT_ATTR_FEC_MODE = SAI_PORT_FEC_MODE_RS` pushed into ASIC_DB's operation queue (`sonic.md` §11.9's method, op `Sset`) | **`SAI_STATUS_SUCCESS`**, and ASIC_DB's row then said `SAI_PORT_FEC_MODE_RS` | **`CL74 = False, CL91 = False`: no FEC at all.** The SAI turned FC off, did not turn RS on, and reported success |
| 4. The chip's SDK shell, past the SAI | `phy control xe9 CL74=0`, then `phy control xe9 CL91=1` | FC off: accepted. RS on: **`Failed to execute the diagnostic command. Error: Operation failed.`** | `CL91 = False` |
| 4 again, on lane 1 (`Ethernet8`, `xe6`): the lane a QSA uses | `phy control xe6 CL91=1`, with FC off first and then with FC still on | **`Operation failed`** both ways | `CL91 = False` |
| Control | the same `phy control ce2 CL91=1` on an empty whole cage (100G) | accepted | `CL91 = True` (then restored) |

So the answer to "can we set it anyway" is no, at the lowest layer there
is: the chip's own software development kit will not enable Clause 91/108
RS on a single 25G lane of this switch, while the identical command works
on a four-lane 100G port. The SAI's `none, fc` for a 25G lane is honest.
What the standard calls a `25GBASE-CR-S` host is what the DX010's split
lanes are.

Two things the run adds beyond the answer:

- **The SAI's "success" at layer 3 is the dangerous case.** A patched
  orchagent that skipped the support check (the cure that worked for port
  mirroring, `sonic.md` §11.10) would leave a lane that every database
  says runs RS while the chip runs no FEC. The check must stay for FEC.
- The neighbours on the same chip core (`xe6`-`xe8`) and the uplink were
  read after every step and never changed; nothing restarted; orchagent
  discarded the probe's reply with its usual warning (`wait: got not
  expected response: SAI_STATUS_SUCCESS:getresponse`).

**A QSA does not change this.** The adapter is passive: it wires an SFP or
SFP28 to lane 1 of the cage, and the FEC is done by the chip's port
behind that lane, which is the same single-lane 25G port as any split
lane (tested on lane 1 for that reason). On a switch whose chip does
single-lane RS, a QSA28 port runs RS; on this one it runs FC or none.

**An active optical breakout cable** (expected; not yet in the switch):
the EEPROM code decides. `100G AOC ... or 25GAUI C2M AOC` comes in two
grades: code 01h (worst BER 5e-5) expects RS, code 18h (BER 1e-12 or
better) needs no FEC. On a split lane here the 18h grade is within the
standard with FEC off; the 01h grade can only be run with FC or none,
outside what its budget assumes. Two more things to expect: the code
does not say "breakout" (the driver's module rule lists both AOC grades
as running whole or split, so Auto will not split the cage by itself),
and the far ends must be set to the same FEC as the lanes, since optics
do not negotiate it.

Not answered, because it needs a far end: whether a 25G optic links on
such a lane with FC or with no FEC. The standard does not provide for it
(section 3).

## 6. Combinations to try with real far ends (not run)

Everything above is the standard, the modules' own word, and what the chip
will accept. None of it shows a link. This is the matrix for a host whose
25G lanes run FC or nothing (`25GBASE-CR-S`), to be run when the parts and
far ends are on hand. Each row is one lane, on a split cage or behind a
QSA (lane 1); the switch side can only be **none** or **FC**.

For every row, record: the module's EEPROM code at both ends; the far
end's device, port and the FEC modes it offers; auto-negotiation on or off
at the far end (the lanes here run it off, and the standard resolves a
DAC's FEC by auto-negotiation, so a far end left on auto may not link
until it is forced); link up or not and how long it took; what the chip
runs (`bcmcmd "phy control <xeN>"`); the FEC corrected and uncorrected
counters and CRC errors after five minutes of traffic; link flaps.

| Medium (code) | Switch | Far end | By the standard | What it shows |
|---|---|---|---|---|
| DAC, CA-25G-N (0Dh) | none | none | allowed | the no-FEC baseline |
| DAC, CA-25G-N | FC | FC | allowed | FC on a cable that does not need it |
| DAC, CA-25G-S (0Ch) | FC | FC | allowed: the CR-S case | the intended mode for these lanes |
| DAC, CA-25G-S | none | none | not allowed (FC or RS) | whether a short S cable holds with nothing |
| DAC, CA-25G-L (0Bh) | FC | FC | not allowed (RS only) | whether a short L cable holds on FC |
| DAC, CA-25G-L | none | none | not allowed | the same with nothing |
| any DAC | FC | RS | mismatch | the symptom of a FEC mismatch: no link, or link with errors |
| any DAC | none | FC | mismatch | the same, the other way |
| 25G SR optic (02h) | none | none | not allowed (RS required) | whether a short fibre holds without the FEC its budget assumes |
| 25G SR optic | FC | FC | not defined for optics | whether both ends accept it and what the error rate is |
| 25G SR optic | none or FC | RS | mismatch | the symptom, for the record |
| 25G LR optic (03h) | none / FC / against RS | as SR | as SR | the same three, over single-mode |
| AOC, BER 1e-12 grade (18h) | none | none | allowed | should be clean |
| AOC, BER 5e-5 grade (01h) | none | none | not allowed (RS required) | whether it holds |
| AOC, 01h | FC | FC | not defined | the same with FC |
| 10G optic or DAC (lane at 10G) | none | none | the only mode there is | the 10G baseline on a 25G-capable lane |

After the manual rows, the same media on "auto" (no FEC pushed), to see
what the driver's module rule picks for each code and whether that is the
row that worked: the rule's table has one known error (25G LR and ER map
to "disabled" where the standard says RS, section 4), and on these lanes
"disabled" or FC are the only choices anyway.

What a result means for the code: a medium that links cleanly only with
one of the two modes tells the module rule which to pick on this host; a
medium that links with neither is to be reported to the operator as
unsupported on a split lane, not left alone silently.

## Sources

- Intel, *25 Gb Intel Ethernet Media Guide*, application note 630739 rev
  1.3 (IEEE 802.3-2018 Tables 110-1, 110-2, 111-1, 112-1, Annex 110C,
  figures 112-1 and 114-1; EEPROM codes; debug flows).
- Wikipedia: *25 Gigabit Ethernet*, *100 Gigabit Ethernet*, *10 Gigabit
  Ethernet* (summaries; used where marked secondary).
- Juniper, *Know Your 100G Transceiver* (which 100G optics run with host
  FEC).
- IEEE 802.3ba baseline summary and a clause 80 excerpt (40G CR4/KR4
  optional FEC).
- SFF-8024 extended compliance code names, as xcvrd prints them.
