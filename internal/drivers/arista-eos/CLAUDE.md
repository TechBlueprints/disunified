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
- `ip domain-name` is deprecated on 4.26 ("This command is deprecated by
  'dns domain'"): the DNS domain is `dns domain <name>`; `hostname <name>`
  is unchanged. `show hostname` then reports the FQDN. The driver reads
  the hostname only in `refreshStatic` and `main` captures it once at
  start, so a hostname change on the switch reaches the inform after a
  bridge restart.
- No rogue-DHCP protection on EOS 4.26: `ip dhcp snooping` is Option-82
  insertion only (no `trust`), so `DHCPSnooping` is not claimed and the
  controller's `switch.dhcp_snoop` key is never pushed (verified
  2026-09-20; see `docs/drivers/arista-eos.md` §6). The guard is built as
  an ingress IPv4 ACL instead (`acl.go`, §8 there), sharing the driver's
  one list with UniFi's IP rules; it stays unclaimed until it has run on
  the switch (§8.5 lists what waits).
- An interface takes **one IPv4 ACL per direction**, and the 7160 has no
  hitless ACL update (`hardware access-list update default-result permit`
  is not offered on it): reprogramming a bound list blocks its interfaces'
  traffic while it lasts. ACL changes go through a `commit timer` session
  like an address move, because the list is bound to the uplink too.
- The firmware version reported to the controller is EOS's own
  (`4.26.14M`), not the UniFi model profile's; `-version` overrides it, and
  an emulated upgrade from the controller masks it until EOS itself moves.
- Fans on 4.26: `environment fan-speed auto` or a fixed `override
  <30-100>` (held whatever the temperature), nothing else -- no floor the
  thermal control can raise. `fan_mode: quiet` is `override 30`, read back
  from `show system environment cooling`'s `overrideFanSpeed`; the
  controller never offers Fan Mode for a switch (`docs/drivers/arista-eos.md` §8).
- `show mac address-table`'s `lastMove` is when EOS learned the address on
  that interface (or moved it there): the entry's `LearnedAt`, so `uptime`.
  4.26 has no per-entry last-seen time in any captured command, so `age` is
  0 (`docs/mac-table-times.md`); never fill `age` from `lastMove`.
