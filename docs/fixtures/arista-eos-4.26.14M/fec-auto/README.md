# FEC: what EOS 4.26.14M reports and how its command behaves (2026-11-17)

Captured on the DCS-7160-48TC6-F for "FEC on auto follows the standard"
(`internal/drivers/arista-eos/fec.go`, `docs/drivers/arista-eos.md` §13).
Scrubbed with `scripts/sanitize-arista-eos.py` in one run.

The switch on the day: cage 49 a 100G CWDM4 optic (linked, `error-correction
encoding reed-solomon` configured), cage 50 a QSFP28-to-4xSFP28 passive
breakout cable split 4x25G (no far end, nothing configured, fire-code
running), cages 51 and 52 CA-25G-L 100G cables in Port-Channel1 (51 with
reed-solomon configured, 52 at the default and running reed-solomon), cage
53 the same kind of cable with reed-solomon configured, cage 54 empty.

## The whole switch, read-only

Every command the driver reads (`startupCmds`, `pollCmds`, the per-interface
`phy detail` texts), plus:

- `show-interfaces-transceiver-eeprom.json` -- `show interfaces transceiver
  eeprom | json`, **reduced to each cage's `Identifier` and `Compliance`
  fields** (the full reply is 340 kB and carries the modules' serials and
  date codes; the driver reads only `Compliance`'s `Ethernet` and `Extended`
  subfields). `Extended`'s `raw` is the SFF-8024 extended compliance code:
  6 (`100G CWDM4`) in cage 49, 12 (`25GBASE-CR CA-S`) in cage 50, 11
  (`100GBASE-CR4 or 25GBASE-CR CA-L`) in cages 51-53. EOS's media type for
  all four copper assemblies is `100GBASE-CR4` (`show interfaces status`):
  the string does not say the cable's 25G class. The empty cage is not
  listed.
- `show-interfaces-error-correction.json` -- per interface, the configured
  encodings (`fecEncodingConfig*`), the available ones (`fecEncoding*Available`:
  a 25G lane with the cable lists Reed-Solomon and Fire-code, a 100G cage
  Reed-Solomon) and the operating one (`errCorrEncodingStatus`).

## The command, in a session that was aborted

`session-config-diffs.txt` -- `configure session`, `interface Ethernet54/1`
(the empty test cage), then after each of these a `show session-config
diffs`, then `abort` (nothing was committed):

    error-correction encoding reed-solomon          -> + reed-solomon
    error-correction encoding fire-code             -> + reed-solomon, + fire-code   (additive)
    no error-correction encoding reed-solomon       -> + fire-code
    no error-correction encoding fire-code          -> no diff                       (the default)
    no error-correction encoding                    -> + no error-correction encoding
    error-correction encoding fire-code             -> + fire-code                   (replaces the `no`)
    default error-correction encoding               -> no diff
    reed-solomon, fire-code, default ... reed-solomon -> + fire-code

A step that leaves the default prints no diff, so the file has six diffs
for eight steps. The `?` at the end was not a pty and is an input error.

## The command, committed under a timer on a 25G lane

Ethernet50/4 (cage 50's fourth lane, no link) in `configure session` with
`commit timer 00:04:00`, further lines in plain `configure` while the timer
ran, the session aborted at the end; the lane was back at its default, the
session list empty and `show running-config diffs` empty afterwards. After
each state, `show interfaces error-correction | json` and `show
running-config | json`:

| Files `...-<state>.json` | Written | Configured | Operating |
|---|---|---|---|
| `fc` | `error-correction encoding fire-code` | Fire-code | Fire-code |
| `fc-rs` | then `error-correction encoding reed-solomon` | Reed-Solomon, Fire-code (both lines) | Reed-Solomon |
| `none` | then `no error-correction encoding` | Disabled (that line alone) | Disabled |
| `rs` | then `error-correction encoding reed-solomon` | Reed-Solomon (that line alone) | Reed-Solomon |

After `default error-correction encoding` the lane read as in the main
capture (Default, Fire-code): no separate file.
