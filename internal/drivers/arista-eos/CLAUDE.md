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
- No rogue-DHCP protection in EOS 4.26's own `ip dhcp snooping` (Option-82
  insertion only, no `trust`; `docs/drivers/arista-eos.md` §6). The guard
  is ingress IPv4 ACLs instead, **one list per interface**
  (`DUI_IP_ACL_<interface>`, `counters per-entry`; `acl.go`, §9), and
  `DHCPSnooping` is claimed since 2026-11-14. IP rules share each
  interface's list and stay unclaimed; MAC rules are refused.
- **The 7160 matches no VLAN in an ACL**: EOS takes `vlan N MASK` in a
  rule and stores it with mask 0 (any VLAN), flagging the list "not fully
  supported by hardware" (measured 2026-11-14). Never write a VLAN
  qualifier. The guard is decided per port from the VLANs it carries
  (`options.dhcp_guard`: `exact`, the default, or `edge`, §9.2).
- **One list bound to several interfaces shares its hardware entries and
  counters**; separate lists get separate ones. That is why the lists are
  per interface, and why `RogueDHCPDrops` is per port (the guard drop
  entry's `packetCount`, `acl_counters.go`, §9.6).
- An interface takes **one IPv4 ACL per direction**; reprogramming a bound
  list may block its traffic while it lasts (`hardware access-list update
  default-result permit` is offered on this switch but not set by the
  driver, and its effect is unmeasured). ACL changes go through a `commit
  timer` session like an address move, confirmed over a fresh connection;
  `configure session NAME abort` reverts a session pending its timer at once.
- No per-port DHCP rate limit (anomaly 32) on 4.26.14M; a QoS policer is in
  bps and counts only conforming packets here (§9.5). `DHCPRateDrops` stays 0.
- Interactive CLI help (`?`) works only on a pty: `ssh -tt`, the command
  typed without a newline, then `?`. Inside `configure session X` ... `abort`
  it shows a mode's options without changing anything.
- The firmware version reported to the controller is EOS's own
  (`4.26.14M`), not the UniFi model profile's; `-version` overrides it, and
  an emulated upgrade from the controller masks it until EOS itself moves.
- Fans on 4.26: `environment fan-speed auto` or a fixed `override
  <30-100>` (held whatever the temperature), nothing else -- no floor the
  thermal control can raise. `fan_mode: quiet` is `override 30`, read back
  from `show system environment cooling`'s `overrideFanSpeed`; the
  controller never offers Fan Mode for a switch (`docs/drivers/arista-eos.md` §8).
- Storm-control drops are in `show storm-control` (JSON `trafficTypes.<type>.drop`,
  the text table's `Drops`), per interface and per type, types keyed
  `broadcast`, `multicast`, `unknown-unicast` (captured 2026-11-14,
  `docs/fixtures/arista-eos-4.26.14M/storm-control-all-types/`). There is no
  `clear storm-control` on 4.26.14M. A hand-made config change on the Arista
  while another session's config session waits on its commit timer is
  reverted with that session (EOS warns "Any changes done here will be
  reverted"): check `show configuration sessions` before a probe.
- `show mac address-table`'s `lastMove` is when EOS learned the address on
  that interface (or moved it there): the entry's `LearnedAt`, so `uptime`.
  4.26 has no per-entry last-seen time in any captured command, so `age` is
  0 (`docs/mac-table-times.md`); never fill `age` from `lastMove`.
