# apc-backups — facts that cost time

Read `internal/drivers/CLAUDE.md` first. Full write-up: `docs/drivers/apc-backups.md`.
Built 2026-09-27 for a Back-UPS Pro 500 (BG500) behind its embedded NMC
(AP9537AV, AOS 6.0.1, app `gsn` 6.0.4 -- APC's "Gassan" family).

- **SNMP knows nothing about this UPS.** The card answers `sysDescr` and
  its own `318.1.4` branch; every device OID (`318.1.1.1` UPS, `.12` rack
  PDU, `.4` MasterSwitch, `.8` ATS) is *no such name*, and a walk of
  `318.1.1` is empty. Do not spend time on MIBs: the web pages are the
  interface.
- **Login gives a token in the path.** `POST /Forms/login1`
  (`login_username`, `login_password`, `submit=Log On`) → 303 to
  `/NMC/<token>/home.htm`; every page and form lives under that token;
  `logout.htm` releases the session. One login per poll.
- **Outlet control is two posts.** `Forms/ulsogctl1` with
  `sog_control?N=<code>` (`01000000` on, `03000000` off, `05000000` reboot,
  `00000000` none) + `submit=Next ››` → 303 to `ulsogcfm.htm`, whose hidden
  `SogControl` (`none,off,none,none,`) is posted to `Forms/ulsogcfm1` with
  `submit=Apply`. Nothing switches before the second post (verified by
  abandoning at the confirmation: both groups stayed on). A push with no
  action redirects straight back to `ulsogctl.htm`.
- **Rows on the pages: MOG1, MOG2, SOG1, SOG2** (main groups always on,
  switched groups with a relay). `config.ini` numbers them differently
  (`Outlet1Name` is SOG1); the pages' order is the driver's.
- **Per-outlet watts are real** (`uloutcfg2.htm` cells; `home.htm` writes
  the same figures with inline scripts). Volts are the rated output from
  `ulinput.htm` (120), amps computed -- the card's own arithmetic.
- **The `usp` IP Settings push has no gateway.** `netconf.1.*` and
  `resolv.nameserver.*` arrive, `route.1.gateway` does not (the switch
  path sends it). The first static write left the card at
  `DefaultGateway=0.0.0.0`; `options.gateway` is the fallback and the
  gateway is part of the address diff.
- **config.ini over FTP works as on the PDU**: `[NetworkTCP/IP]` with
  `Override=<MAC as "00 C0 B7 ..">`, `BootMode=Manual|DHCP Only`; applies
  live. The whole file carries SNMP communities, e-mail and users -- the
  fixture keeps only the four sections the driver reads.
- **Adoption deleted the card's client record** (fixed IP + DNS name);
  the card's DHCP hostname is `apcXXXXXX`, so dial by address and set the
  device `hostname` to the name you want resolvable.
- Status text captured so far is only "UPS is online."; on-battery / low
  battery / overload are matched on the words and unverified live.
- The UPS off/reboot form (`ulctrl1`) is never posted.
