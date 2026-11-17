# FEC by standard, by module, by switch (research, 2026-11-16)

What forward error correction a link is supposed to run, in three layers:
what the standards define per speed and medium, what a module's EEPROM
says it needs, and what a switch says it can do. Written to judge the
DX010's "25G lanes support only none and FC" before trying to override it.
Nothing here was tested by changing a switch.

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

## 5. Setting RS on a 25G lane anyway (not done)

The register's test stands: write `fec rs` into CONFIG_DB on a split lane
past the CLI's check, read orchagent's and the SAI's answer, restore. What
this research adds:

- The likely answer is no: the report is consistent with the chip's
  generation, and a correct "no" would cost a failed SAI call, which on
  this stack can restart swss (`sonic.md` §11.9) and drop the uplink
  briefly.
- A yes would matter only for CA-L DACs and for optics on lanes. For the
  cables in the unit it changes nothing.
- A link test needs a far end that runs Clause 108 RS at 25G; acceptance
  by the chip alone proves the register write, not a link.

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
