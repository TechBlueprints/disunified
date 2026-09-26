# apc-ups — facts that cost time

Read `internal/drivers/CLAUDE.md` first. Full write-up: `docs/drivers/apc-ups.md`.

- **The SmartConnect port serves Modbus TCP (502).** Schneider's FAQs say the
  green port has no local interface; that is true of management, not of
  Modbus. APC's own *SMTL Series Firmware Release Notes* (UPS 01.3, ID 1026,
  SKUs incl. SMTL1500RM3UC): "Added support for Modbus over TCP on
  SmartConnect port." It ships **disabled**: LCD → Configuration → Menu Type
  → Advanced, then Configuration → Modbus. Until then 502 is *refused* (the
  stack answers with RST), not filtered — refusal proves the host is up.
- **Read in the four blocks NUT's `apc_modbus` uses** — (0,27), (128,44),
  (516,120), (1026,22) — on one persistent connection. The map is sparse: a
  read across an undefined address gets exception 0x02, and a burst of
  short-lived connections made the real unit refuse connections for a while
  (2026-09-26). Never probe it with a socket-per-register scanner.
- **Two-register values are big-endian by register**: register 0 is the
  high word of `UPSStatus_BF`. A live 0x2002/0x0000 decodes to online + ECO
  exactly as NUT reports; swapping the words silently loses every status
  bit. The outlet-group on-bit is in the *low* word of its field.
- Fixed-point scales (fractional bits) from `apc_modbus.c`, checked live:
  load 8, apparent 8, charge 9, battery V 5 (signed), battery °C 7 (signed),
  output V 6, output A 5, output Hz 7, input V 6, efficiency 7 (signed).
  Real W = load% × register 589 (real-power rating); VA likewise with 588.
- Sentinels: input voltage `0xffff` = not applicable; efficiency negative =
  a reason code, not a value. Leave `HasInput`/`HasEfficiency` false.
- There is **no charging bit** in the map; charging is derived as "on mains
  below 100 %", which is what the USB HID feed reported as `CHRG` for the
  same unit. There is **no per-outlet or per-group metering** — output
  current/W/VA are aggregate.
- NUT's own status code reads `regbuf[18]` for both shutdown-imminent (LB)
  and needs-replacement (RB); the second looks like a bug, so no
  replace-battery flag is derived from it here.
- Modbus does not expose the port's MAC, netmask or gateway. `options:
  mac`, `netmask`, `gateway_mac` supply them; the last two are what the
  controller places the device in a network by (the Arista's missing-parent
  lesson), so without them it adopts with no topology parent. The
  controller already lists the port as a client under that MAC, and
  **adopting a device deletes its client record and any fixed-IP
  reservation** (the PDU lost its address this way) — give the unit a manual
  address, or accept that, before adopting.
- **Read-only in this version by decision, not by limitation.** The unit has
  a switched group (`Outlet Group 1`, SOG0, command register 1538). Switching
  it from the controller is a follow-on that waits on a discussion with
  Clint and a test outlet known to carry nothing; the unit runs a whole rack
  at ~80 % load.
- Fixture `docs/fixtures/apc-smtl-15.5/registers.txt` is the live capture,
  scrubbed by `scripts/sanitize-apc-ups.py` (serial and UPS-name words).
  No real UniFi UPS inform exists on site, so the `vbms_table` emitter is
  spec-derived from unifi-emu's PROTOCOL.md rather than capture-verified.
- **Model choice facts (2026-09-26).** UniFi's whole UPS lineup, per its
  fingerprint DB, is UPS Tower (`USWDA23/24`, 5+5 outlets), UPS 2U
  (`USWDA25/26`, 4+4) and UPS 2U Pro (`USPDA2B/2C`, 8/9 metered). The
  `USPDA29`/`USPDA31` codes in unifi-emu's protocol notes are **not in the
  fingerprint** -- the controller would not know them. The pinned emu
  **v0.5.5 mislabels the 2U Pro as `type usw`**; the newer catalogue and the
  fingerprint say `usp`, a power-path type this bridge has never exercised
  and has no capture for, so its passing the descriptor gate is a catalogue
  bug, not a real option. Tower and 2U draw surge-only outlets the SMTL does
  not have; the Pro claims per-outlet metering the SMTL cannot do. 2U stays.
- The config validator rejects a `url` without username/password (the guard
  for API endpoints). Modbus has none: the device entry needs **`auth: none`**
  or the bridge exits at startup before informing anything.
- **Adopted 2026-09-26; the controller keeps `vbms_table`.** Read back from
  `stat/device`: every `battpool` key stored with the value sent, the UI's
  load ring and battery card live. Verified, not just spec-derived.
- **A synthetic MAC costs the topology parent.** The controller places a
  device by where its MAC appears on a switch port; a locally-administered
  address is seen nowhere, so `uplink_mac` never fills and the page says
  "Connected To -". Real-MAC devices (the PDU) are placed. The trade is the
  reservation on the real client record, which adoption under the real MAC
  deletes.
- The eight profile outlet slots are drawn with no rows sent, all green; the
  controller plants an empty `outlet_table` itself.
- "Update Available" is the honest version "15.5" losing to the UPS 2U's
  release number. The rule is to accept it (ECS Core precedent): the
  emulated upgrade persists and reports the controller's version, base kept.
- **`gateway_mac` is the gateway's L2 address on the segment**, from `ip
  neigh` on a host there -- the UDM answered ARP as `…:0b:a4` while the
  controller lists the device as `…:0b:a3`. The PDU and Arista get it right
  because they read their own ARP tables; an operator option has to be told.
- **The port refuses a new connection for a moment after a session closes.**
  Restarting the bridge (SIGINT, reconnect ~0.3 s later) got `connect:
  connection refused` on the first attempt; `main.go`'s start retry
  succeeded 15 s later. Expected, not a fault -- one Modbus session at a
  time, and the stack takes a beat to free it. Don't tighten the retry.

