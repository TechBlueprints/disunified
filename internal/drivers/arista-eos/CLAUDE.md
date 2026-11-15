# Arista EOS driver — working notes for Claude Code

Rules for every driver are in `../CLAUDE.md`; the protocol notes and the
switch write-up are in `docs/drivers/arista-eos.md`. These are the facts
that cost time on Clint's DCS-7160-48TC6-F.

- EOS 4.26.14M is the **last train for the 7160**: verify every command
  against that version (fixtures in `docs/fixtures/arista-eos-4.26.14M/`); eAPI
  batches start with `enable`; eAPI does not expand abbreviations; TLS 1.2
  RSA-kex only.
- 10GBASE-T ports: 1G and 10G only. QSFP28 cages: 10/25/40/50/100G,
  RS-FEC on 100G; a 25G lane offers RS and fire-code (cage 50's hardware
  table; RS accepted and operational on the empty cage's lanes,
  2026-11-15).
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
  interface's list, claimed at 32 (`maxIPRules`) since the same day; MAC
  rules are refused.
- **ACL capacity of the 7160 (measured 2026-11-14, §9.1,
  `docs/fixtures/arista-eos-4.26.14M/acl-capacity/`):** one pool of **4095
  counters for every list together**. Past it, EOS leaves entries uncounted
  **without an error at the commit**; only the summary's
  `countersIncomplete` shows it. So only a guarded list gets `counters
  per-entry`, and past the pool none does. A counted rule is two MaskGroup
  entries, an uncounted one is one (20094 held in five lists). ACL ids are
  one per list in bind order; only five lists were measured at once. No
  DSCP match. ACLs on subinterfaces are unavailable. **A list that fails to
  program still commits** (`! Error in ACL commit`, a warning), and the
  summary still says "Active on Ingress". The commit's warning is the
  signal.
- A rule set blocking TCP to the address and port the bridge dials (eAPI
  443) is held before it is written (`devicemodel.IPRulesCutManagement`),
  every source counted.
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
- **Every configuration session needs a name of its own** (`sessionName`:
  `dui-acl-<suffix>`, `dui-address-<suffix>`). EOS keeps its last completed
  session by name and refuses to enter it again ("Cannot enter session
  dui-acl, it is already complete"): with a fixed name the second ACL change
  failed, live 2026-11-14. `no configure session NAME` deletes a completed
  one by hand, and fails for a name that does not exist.
- **A session pending its commit timer blocks every other commit** ("Cannot
  commit because another session, X is pending commit timer"), the
  bridge's included: open one (a harmless `alias`) to hold the deployed
  bridge's ACL and address changes off a hand-made test, abort it after.
  Reverting it keeps what was committed before it. It does **not** hold off
  the bridge's port writes (plain `configure`): those apply, and are reverted
  with the session, but the bridge's `write memory` saves them, and a hand
  change made meanwhile, into startup-config (2026-11-15: a test
  Port-Channel5). After the abort, `write memory` and check `show
  running-config diffs` is empty.
- Live (2026-11-14, §9.8): the driver's list on all 57 interfaces at once
  programmed (57 port ACL ids); binding them costs one or two 50 ms pings.
  The controller refuses an IP rule within one subnet ("resort to MAC ACL
  instead"). The CPU sees UDP only on ports `default-control-plane-acl`
  names (`bootpc` yes, 9 no): test rules against port 68 of the switch's
  own address.
- No per-port DHCP rate limit (anomaly 32) on 4.26.14M; a QoS policer is in
  bps and counts only conforming packets here (§9.5). `DHCPRateDrops` stays 0.
- Interactive CLI help (`?`) works only on a pty: `ssh -tt`, the command
  typed without a newline, then `?`. Inside `configure session X` ... `abort`
  it shows a mode's options without changing anything.
- The firmware version reported to the controller is EOS's own
  (`4.26.14M`), not the UniFi model profile's; `-version` overrides it, and
  an emulated upgrade from the controller masks it until EOS itself moves.
- Fans on 4.26.14M (the switch's own CLI help, 2026-11-14): `environment
  fan-speed auto`, a fixed `override <30-100>` (held whatever the
  temperature), and `minimum <30-100>`, a floor under the automatic
  control. The floor cannot make the switch quieter: auto already runs
  above 30 % (43 %, with the CPU sensor at its 60 °C setpoint). So
  `fan_mode: quiet` is `override 30`, read back from `show system
  environment cooling`'s `overrideFanSpeed` (`coolingMode` `manual`). Under
  it the trays report 39 %, not 30 %. Verified live 2026-11-14, every
  return-day step; the switch runs Quiet, CPU sensor about 68 °C against a
  90 °C alert. The controller never offers Fan Mode for a switch, on 11.0.99
  either (`docs/drivers/arista-eos.md` §8).
- Storm-control drops are in `show storm-control` (JSON `trafficTypes.<type>.drop`,
  the text table's `Drops`), per interface and per type, types keyed
  `broadcast`, `multicast`, `unknown-unicast` (captured 2026-11-14,
  `docs/fixtures/arista-eos-4.26.14M/storm-control-all-types/`). There is no
  `clear storm-control` on 4.26.14M, and `clear counters` does not reset
  the drop count. The lowest level kept is 0.01 % (0.001 is stored as 0,
  which drops everything); there is no pps unit. A storm was caught live on
  the uplink with targeted unknown unicast (`storm-control-drops/`, §9.8).
  The deployed bridge removes a hand-made storm setting on its next
  reconcile. A hand-made config change on the Arista
  while another session's config session waits on its commit timer is
  reverted with that session (EOS warns "Any changes done here will be
  reverted"): check `show configuration sessions` before a probe.
- **Link and STP counts (2026-11-15, `docs/drivers/arista-eos.md` §4):**
  `linkStatusChanges` counts every transition since `lastClear`, with one
  extra at boot on every interface (a never-up port reads 1) and none
  after a `clear counters` (which zeroes it); `linkDrops` turns it into
  drops by which of the two it is (`lastClear` within 60 s of
  `bootupTimestamp`) and the state now. An admin shutdown of a linkless
  port counts nothing. STP `numChanges` is per link-up: a down interface
  is not listed, a link that comes back starts from 0; `holdSTPChanges`
  keeps a per-interface total that never falls. The 7160 has **no MAC
  loopback** (`traffic-loopback ...` is "not supported on this hardware
  platform"), so a linkless test port cannot be brought up.
- **Lane-switch parent (2026-11-15, `docs/drivers/arista-eos.md` §10,
  `docs/drivers/breakout.md` §7b):** `channel-group` stops at **2000**, so
  a lane switch's groups and mirror sessions are `1000+10*cage+lag|lane`
  (not sonic's `100*cage+lag`); the switch's own stay below 1000 and the
  fold never reads one of 1000+ as its own. Every per-interface parser
  takes an `ifKey`: `parseEthName` for the fold, the lane key for split
  cages' lanes (`buildLanes`) -- a new reading must take one too, or the
  lanes miss it. `lockApply` serialises collects and every write (a lane
  switch applies from its own loop). An `interface A-B` range stops at the
  first warning (an empty cage's "transceiver not present"): write one
  `interface` per lane.
- **EOS keeps an emptied Port-Channel** (2026-11-15, `docs/drivers/arista-eos.md`
  §4, Aggregates): `no channel-group` on the last member leaves `interface
  Port-Channel<n>` with its settings. `no interface Port-Channel<n>` removes
  it and its ACL binding, takes any members out too, and is silent for one
  that does not exist; any `interface Port-Channel<n>` line (an ACL unbind)
  re-creates it empty. The plan removes an empty one in its own range
  (1..last port; a cage's nine for a lane switch) that carries only the
  driver's lines (`portchannel.go`); a hand-made one stays. The test
  fixtures' switch had four (Po1-4): `removeLeftovers` in the test helpers
  takes them out first.
- **An optic's own RX_LOS / TX_FAULT are only in `show interfaces X phy
  detail`** (text; its `| json` wraps the text): the `... since system
  boot` rows' Current State. Per interface, not per channel; no
  `transceiver` form, `eeprom` or `idprom` prints them (2026-11-15,
  `docs/drivers/arista-eos.md` §4). They are `sfp_rxfault` / `sfp_txfault`;
  the DOM thresholds are not (only the SSH transport's fallback).
- `show mac address-table`'s `lastMove` is when EOS learned the address on
  that interface (or moved it there): the entry's `LearnedAt`, so `uptime`.
  4.26 has no per-entry last-seen time in any captured command, so `age` is
  0 (`docs/mac-table-times.md`); never fill `age` from `lastMove`.
- **The fixture transport remembers what it wrote** (2026-11-15,
  `fixture_state.go`, `docs/drivers/arista-eos.md` §11): a new write the
  driver makes needs its read-back form captured on the test ports
  (`write-forms/` is the pattern) or it lands in `Unmodelled`. Only the
  whole `show running-config` has JSON on 4.26.14M (`... interfaces X |
  json` is "unconverted"). The admin speed is a startup read
  (`show interfaces transceiver properties`): a batch that writes a speed
  sets `c.refresh` so the next collect re-reads it.
