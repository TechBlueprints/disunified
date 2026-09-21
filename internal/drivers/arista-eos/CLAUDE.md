# Arista EOS driver — working notes for Claude Code

Rules for every driver are in `../CLAUDE.md`; the protocol notes and the
switch write-up are in `docs/drivers/arista-eos.md`. These are the facts
that cost time on Clint's DCS-7160-48TC6-F.

- EOS 4.26.14M is the **last train for the 7160**: verify every command
  against that version (fixtures in `docs/fixtures/arista-eos-4.26.14M/`); eAPI
  batches start with `enable`; eAPI does not expand abbreviations; TLS 1.2
  RSA-kex only.
- 10GBASE-T ports: 1G and 10G only. QSFP28 cages: 10/25/40/50/100G,
  RS-FEC on 100G, fire-code only on 25G lanes.
- Breakout: speed on lane 1 splits/joins; a lane-speed change must reach
  every lane or the others errdisable ("speed-misconfigured").
- `write memory` after every config batch.
- No rogue-DHCP protection on EOS 4.26: `ip dhcp snooping` is Option-82
  insertion only (no `trust`), so `DHCPSnooping` is not claimed and the
  controller's `switch.dhcp_snoop` key is never pushed (verified
  2026-09-20; see `docs/drivers/arista-eos.md` §6).
- The firmware version reported to the controller is EOS's own
  (`4.26.14M`), not the UniFi model profile's; `-version` overrides it, and
  an emulated upgrade from the controller masks it until EOS itself moves.
