# apc-pdu — APC rack PDU

Presents an APC switched rack PDU as an adopted UniFi power distribution unit:
each of the card's outlets becomes an outlet the controller can see, name and
switch.

Verified on an **AP7931** (16 switched outlets, single phase) with a Network
Management Card running **AOS 3.9.2** — hardware revision B2, manufactured
2008. Fixtures in `docs/fixtures/apc-aos-3.9.2`, captured with `snmpwalk -On`
and scrubbed by `scripts/sanitize-apc.py`.

## 1. Why SNMP

This card has **no API**. `/api` and `/rest` are 404, and its web UI is
form posts whose login page sniffs for MSIE 5.5 — the `/Forms/*` handlers are
the only HTTP interface, and there is no JSON anywhere. APC added a REST API
on **NMC3** cards; this is NMC1. SNMP (the PowerNet MIB) is the vendor's own
control plane for this generation, and it is what every Home Assistant
integration for these PDUs uses.

Telnet's menu console can do everything SNMP can and rename outlets besides,
but it is stateful screen-scraping, the card allows very few sessions (so a
bridge polling every cycle would fight the operator for one), and it sends the
admin password in clear text on every login. SNMP is stateless, survives the
card's reboots, and keeps the admin password off the wire except for the rare
config write.

## 2. Two transports, because one is not enough

| What | How |
|---|---|
| Identity, uptime, outlet names, outlet state | SNMPv1 GET/GETNEXT |
| Switching an outlet | SNMPv1 SET on the control table |
| **Setting an outlet's name** | **partial `config.ini` uploaded over FTP** |

The outlet-name objects are **read-only** over SNMP. In SNMPv1 a read-only
object answers a SET with `noSuchName` — the same error as a missing OID — so
this looks like a wrong OID until you check. Names therefore go through the
card's own configuration file.

### SNMP writes need access type `Write+`

The card's SNMPv1 access-control entry used for writes must have access type
**`Write+`**, not `Write`. With `Write` the card answers every SET with
**nothing at all**: no error, no response, no log entry — indistinguishable
from a network fault. This cost an hour; the driver's error message says so.

Restrict that community to the bridge's own address. `Write+` also permits
changing the access-control settings themselves over SNMP.

### The config upload

A **partial** file is enough — no header, no round-trip of the whole config:

```ini
[RackPDUOutlet]
Name5=Rack Fan
Name8=NAS
```

It applies **live, with no reboot**, and several names can go in one file. Two
things the driver has to respect:

- **Application is asynchronous**, a few seconds after the transfer. A read
  straight after the upload can still return the old name, so the driver
  confirms on the next poll rather than immediately.
- **A second upload arriving while the first is still being applied is
  dropped.** All renames in a cycle are therefore batched into one upload.
- Names are truncated to **23 characters**, the card's own limit, so the
  device cannot silently disagree with the desired name forever.

## 3. The two outlet tables

Easy to confuse, and one of them switches power. Mapped column by column
against the live card:

- **`…12.3.3` is the CONTROL table** (5 columns): index, name, phase,
  **command**, bank. Column 4 reads back the outlet's current state and is
  what you write to switch it (`1` on, `2` off, `3` reboot). It is the only
  writable outlet object.
- **`…12.3.5` is the CONFIG table** (6 columns): index, name, phase, power-on
  time, power-off time, reboot duration.

Writing an outlet command to `12.3.3.1.1.4.<n>` switches an outlet. Writing to
`12.3.5.1.1.4.<n>` sets a power-on delay. They look alike.

## 4. What is not claimed

- **Metering is aggregate, not per-outlet — by hardware generation.** APC's
  families: AP78xx Metered (1G), **AP79xx Switched (1G, this unit)**, AP84xx
  Metered-by-Outlet (2G), AP86xx Metered-by-Outlet with Switching (2G),
  AP88xx Metered (2G), AP89xx Switched (2G). Per-outlet current and power
  exist only in the **rPDU2** branch (`318.1.1.26.9.4.3.1.6/.7`), which this
  card cannot serve: rPDU2 arrived in MIB v3.9.9 and is implemented by the
  hw05 `rpdu2g` application, while this is hw02 (`apc_hw02_rpdu_392.bin`).
  That is a hardware-generation boundary with no firmware path. The 1G branch
  does define `rPDUOutletStatusLoad` (`.12.3.5.1.1.7`), but its own MIB text
  says "For other models this OID is not supported" — and the card duly
  answers `noSuchName` for it.

  The driver therefore reports outlets as relay-only and omits the per-outlet
  measurement keys rather than sending zeros, because a zero reads as a real
  measurement of no load. A model that does meter fills `HasMetering` and the
  payload switches to the decimal-string form this family uses.

- **Aggregate metering works, and a reading of 0.0 A does not mean zero.**
  APC FAQ FA156074: on AP7xxx the current monitor is +/- 1.0 A between 0 and
  1 A, and **firmware 3.3.1 and later report any load under 1 amp as zero**.
  So `0.0 A` means "under roughly 1 A" (~120 W here), not "nothing". The
  card's own data log (`data.txt` over FTP, columns `I / IMax / IMin`)
  recorded `IMax 1.4` on 09/13/2026, which proves the sensor works.

  Watts are not measured: the card derives them from the current and the
  operator-set line voltage and power factor (its PDU Configuration screen,
  `rPDUIdentDeviceLinetoLineVoltage` is read-write and exists only for this
  purpose), so the driver does the same arithmetic. The budget is the PDU's
  own rating x that voltage = **1440 VA**, which is exactly the load capacity
  APC publishes for an AP7931 -- 12 A is the UL 80%-continuous derated figure
  for its 15 A input, not a fault.
- **No switch capabilities at all.** A rack PDU is not a switch; claiming one
  would put a control in the UI that the driver cannot honour.
- No fans, PSUs or temperature: the AP7931 has none.

## 5. Configuration

```yaml
devices:
  - name: pdu-1
    driver: apc-pdu
    url: 192.0.2.20            # the card's address
    username_env: DUI_APC_USER # the card's admin login, for the FTP name write
    password_env: DUI_APC_PASS
    options:
      read_community: public
      write_community: private # must be access type Write+ on the card
    control:
      outlets: all             # the default; "off", or "2,5-8"
```

`control.outlets` is the power-device equivalent of `control.ports`. It is off
by default: an outlet carries real load.

## 6. Outlet indices, and what the controller actually pushes

Two things were only learnable by adopting the device live (Network 10.6.106,
2026-09-21).

**The controller merges its own outlet names onto the rows it is sent, by
index.** The USP-PDU-Pro's own layout is four USB outlets at 1-4 and sixteen
AC outlets at 5-20, so a 16-outlet rack PDU reporting its outlets at 1..16 is
adopted and then labelled "USB Outlet 1" through "USB Outlet 4". The bridge
therefore reports the AC outlets at the model's AC positions, **5..20**
(`device.OutletIndexBase`), and the loop translates the controller's index
back to the device's own numbering on the way in. The driver only ever speaks
its device's numbering.

**The controller then pushes `outlet.<n>.relay_state` at the same indices the
device reported** — 5..20 here — so every outlet is controllable. (A config
captured before the realignment carries the old indices and looks like only 12
of 16 outlets are managed; it is stale, and the next push corrects it.)

**The controller does NOT push outlet names.** The system_cfg it sends carries
`outlet.status` and one `relay_state` line per outlet, and nothing else: names
live only in the controller's own `outlet_overrides`. So naming is one-way in
the sense that matters -- the device must never report names back, or its
whole outlet table is dropped -- but the bridge has no name to push *down*
from the inform protocol either. The FTP config write exists and is tested;
its source would have to be the controller's REST API, not the inform. In
practice the name that matters is the one in UniFi, which is what the operator
sees; pushing it onto the card is cosmetic.

## 7. Model claimed

`USPPDUP` — the USP-PDU-Pro. Its profile is a `usw` with one network port,
which is the shape a bridged rack PDU presents, and its 16 AC outlets line up
one-for-one with the AP7931's 16. `unifimodel.SuggestFor` picks it for any
snapshot that has outlets, rather than ranking the card's single network port
against every one-port switch in the catalogue.

## 8. Where the UI keeps the outlet controls

Clicking an outlet **row** in the device's Outlets tab expands an inline
editor beneath it: Link Device, Name, **Power Cycle**, Active / Disabled,
Power Cycle on Internet Loss, Apply Changes. Clicking the label or the icon
alone does nothing; it is the row. Active/Disabled arrives as
`outlet.<n>.relay_state` in a system_cfg push; Power Cycle arrives as the
`relayctl` command with a selection list.

## 9. Verified live

Round trip on Network 10.6.106, 2026-09-21, against the real AP7931:

| Step | Result |
|---|---|
| Adoption handshake | 404 -> mgmt_cfg/authkey -> ADOPTING -> CONNECTED |
| Outlets rendered | 16, at indices 5-20, named "Outlet 5".."Outlet 20", `hw_caps` 128 |
| Switch **off** from the controller | relay open **~25 s** later; `1 of 16 outlets changed` |
| Switch **on** from the controller | relay closed ~60 s later; `1 of 16 outlets changed` |
| Reconcile with no change | `0 of 16 outlets changed` — no SNMP write at all |
| UI outlet editor: **Disabled** → Apply Changes | relay open ~20 s later; `1 of 16 outlets changed` |
| UI outlet editor: **Active** → Apply Changes | relay closed; `1 of 16 outlets changed` |
| UI **Power Cycle** button | `relayctl` → the card's immediate-reboot: relay open 4 s, then closed; the following reconcile writes nothing |
| Overview | model, parent (the switch port it is cabled to), IP, MAC, version, uptime; Power Usage / Current / `0W of 1440W` |
| Reachability keys | `gateway_mac` (matches what the real PDUs report for the same gateway), `lldp_table: []`, `netmask` |

Every UI screen for the device was walked: list entry, overview, Outlets
(graphic and list, USB 1-4 grey/Disabled, the 16 real outlets green), the
per-outlet editor, Settings, History, System Statistics. The one visible
artefact of claiming the model is a **Touchscreen** section under Settings,
which the APC does not have -- the same class as Etherlighting on a switch.

The apply is a true diff: only the outlet whose intent changed was written,
and a steady state writes nothing, which matters because the loop re-applies
the same intent on every cycle and an outlet is real load.

## 10. Give the card a manual address before adopting it

Adopting a device into UniFi **deletes its client record**, and with it any
fixed-IP reservation the controller held for that MAC. This card had
its address (say `192.0.2.20`) as a DHCP reservation; the morning after adoption its lease
renewed as a pool address and the bridge lost it. The same thing removed the
Proxmox nodes' DNS records. Set `BootMode=Manual` on the card itself, before
adopting or straight after.

Doing that through `config.ini` needs the section's `Override=<MAC>` line
(`02 00 00 00 00 01`, spaces and capitals as the card writes it): APC applies
uploaded TCP/IP settings only when that key matches the card's own MAC, so a
file meant for one card cannot re-address another. Without it the section is
silently ignored. The change applied live, about a minute after the upload,
with no reboot.

The bridge now retries a device that does not answer at startup (15 s
doubling to a 5-minute cap) rather than dropping it until a restart.

### Or let UniFi own the address (the default)

The controller pushes the device's own IP Settings in every system_cfg, and
the driver applies them to the card (no key since 2026-10-31), so the
address is set where everything else is -- in UniFi -- instead of by hand on
the card. Both forms were captured on 10.6.106:

| IP Setting | What the push carries |
|---|---|
| Using DHCP (the default) | `netconf.1.ip=0.0.0.0`, `dhcpc.1.status=enabled` |
| Static | `netconf.1.ip`, `netconf.1.netmask`, `route.1.gateway` (`route.1.ip=0.0.0.0` marks the default route), `resolv.nameserver.N.ip`, and **no** `dhcpc.1.*` lines at all |

**"Using DHCP" is applied only when it replaces a static setting.** It is
also the controller's default for every device it adopts, and a chosen DHCP
and the default DHCP are the same push, so the push alone cannot say whether
anyone meant it. The transition can: the previous push having carried a
static address means someone changed the setting by hand, and only then is
DHCP applied. A device that was never static keeps its address -- a bridge
that honoured the default would have moved this card off its manual address
the moment the flag was turned on; the replay shows exactly that push
arriving first. The apply is a diff against the card's current mode and
address, so re-sending what it already has uploads nothing (the driver's
tests), and a different static address produces one `[NetworkTCP/IP]` upload
with the Override line (the replay). After a move to DHCP the bridge's own
`url:` is stale until the new lease is known; the log says so.

### And where it logs: the controller's syslog host

With remote logging switched on in UniFi (Settings → System), every push --
the power path's included -- carries `syslog.remote.ip`, `.port` and
`.status=enabled` (captured from this PDU on Network 10.6.109), and the
driver writes that host into the card's own syslog servers through the same
config file (no key):

```
[NetworkSYSLOG]
Syslog=enabled
Server1=192.0.2.40
Port1=514
```

Any other slot that holds a server is emptied (`ServerN=0.0.0.0`): the
controller's host replaces the card's, as it does on a switch. Remote logging switched off in UniFi uploads `Syslog=disabled` and
leaves the slots alone. The facility and the severity mapping are never
written. The section is read from the card's `config.ini` at Start and
logged; an upload is a diff against it, and about a minute later (the card
applies an upload a little after the transfer) the whole file is read
back and logged, so the log shows what the card holds, not what was sent.
A card that does not take it is written three times and then left alone
until the controller's host changes.

What reaches the log host is the card's decision: each event's actions in
`[EventActionConfig]` say whether it goes to syslog (`ES`) or only to the
card's own event log (`E`). On AOS 3.9.2 out of the box, outlet on/off,
bank and phase load thresholds, the internal power supplies, cold and warm
start and unauthorised access (SNMP, web, console, FTP) go to syslog;
logins, logouts and configuration changes do not, so the bridge's own FTP
sessions add nothing. A quick end-to-end test that switches nothing is an
SNMP read with a wrong community: the card logs it as an unauthorised SNMP
access and sends that line.

Verified live 2026-10-30 (the card already listed the site's log host, so
the bridge wrote nothing): the line arrives as RFC 3164 with the card's
**IP address as HOSTNAME** (no name), tag `System:`, facility user,
severity informational, and the event code at the end
(`Detected an unauthorized user attempting to access the SNMP interface
from <addr>. 0x0004`). Its timestamp is the card's local time with no
zone: this card runs NTP at `NTPTimeZone=-07:00` with
`DSTConfiguration=Disabled` (`[SystemDate/Time]`), so from March to
November its lines are stamped an hour behind. Since 2026-11-15 the
controller's time zone, DST rule and NTP servers are written to the card
too (§12).

## 11. Things that will bite

- `hw_caps` bit 128 is load-bearing. Without it the controller accepts the
  inform, stores no outlet table and logs nothing: the device adopts and shows
  no outlets at all.
- The device must **never report outlet names** on its inform. The controller
  merges its own names onto the row it stores, and that merge is also its
  change test — a device that reports the operator's own name back reports
  nothing new and has its whole outlet table dropped, silently.
- SNMPv1 has no GETBULK, so every OID is a GETNEXT round trip. Walking the
  whole rPDU tree is ~340 of them and took 15 s against the real card; the
  driver walks only the four subtrees it reads and collects in under 3 s.
- `ifPhysAddress` is six raw bytes on the wire, not text. `snmpwalk` renders
  it as hex, so a fixture-only test passes while the live path puts binary
  into the snapshot.

## 12. Its clock: the controller's time zone, DST rule and NTP servers

Every power-path push carries the site's time zone as a POSIX TZ string
with its daylight saving rule, and its NTP servers (captured from this PDU
on Network 10.6.109, `controller-10.6.109/system_cfg-pdu-syslog-remote.txt`):

```
system.timezone=MST7MDT,M3.2.0,M11.1.0
ntpclient.1.server=192.0.2.4
ntpclient.2.server=0.ubnt.pool.ntp.org
```

The card keeps its clock in `[SystemDate/Time]`: a fixed UTC offset, a
daylight saving choice, and two NTP servers. It shipped with
`NTPTimeZone=-07:00`, `DSTConfiguration=Disabled` and servers of its own
(`apc-aos-3.9.2/config.ini`). The bridge now writes the controller's values
into that section through the same config file, as it does the syslog host
(`apcnmc.ClockApplier`, behind `devicemodel.ClockController`).

**What the card can say.** Its Date/Time forms, captured read-only
(`apc-aos-3.9.2/dateman.htm`, `datentp.htm`), set the limits:

- *Time Zone* is a list of 32 fixed offsets, from -12:00 to +12:00. They
  include the half-hour zones and +05:45, and none of them carries a DST
  rule.
- *Daylight Saving* has three choices: "Disable DST", "Traditional US DST
  (Second Sunday in March to First Sunday in November)", and a custom rule
  that "Adds 1 hour at start, subtracts 1 hour at end". The custom rule is
  set by week (First ... Fifth/Last), weekday, month and hour.
- The NTP fields take "the IP address or domain name" (the card's help
  page), up to 64 characters.

**The mapping.** It is exact, or nothing is written:

| Push | Card |
|---|---|
| the standard offset (`MST7`: 7 h west of UTC) | `NTPTimeZone=-07:00`, if that offset is in the list |
| no DST part | `DSTConfiguration=Disabled` |
| `M3.2.0,M11.1.0` (02:00 both ends, the default), shifting by one hour | `DSTConfiguration=Traditional US` |
| one or two NTP servers | `NTPEnable=enabled`, `NTPPrimaryServer`, `NTPSecondaryServer` (`0.0.0.0` when there is one) |

The config file does not spell the choices the way the form does: the
form's radio values are `None`, `Continental_United_States` and `Custom`,
the file says `Disabled`. The value `Traditional US` comes from an exported
config.ini quoted on APC's own community forum. No card on site has held it
before this change. The read back after the first upload is what confirms
it. A value the card rejects is dropped from the upload, and the card's
event log records the error (its help page: "The upload succeeds even if
the file contains errors").

The bridge refuses, logs once and leaves alone:

- a zone name such as `America/Denver` (the ECS's push carries one; the
  power path's does not);
- any other DST rule or a shift other than one hour. The custom rule's
  config.ini keys are not captured, and they appear in the file only once a
  custom rule is set;
- an offset not in the card's list;
- a zero or positive offset, whose config.ini spelling is not captured;
- more than two NTP servers, or a server that is neither an address nor a
  host name.

A refused part leaves only that part alone. A push with an unsupported
zone still sets the NTP servers.

**The write.** It writes only the keys whose value differs, in the card's
own spelling and order. The site's push against this card is one upload:

```
[SystemDate/Time]
DSTConfiguration=Traditional US
NTPPrimaryServer=192.0.2.4
NTPSecondaryServer=0.ubnt.pool.ntp.org
```

The section is read at Start and logged. About a minute after an upload,
the whole file is read back and logged. A card that does not take the
upload is written three times and then left alone until the controller's
settings change. The syslog, clock and address (IP Settings) uploads take
turns (`apcnmc.Uploads`): the card drops an upload that arrives while it is
still applying the last one, so the second waits out the first's settle
time. On a first start that needs both, the clock follows the syslog host a
minute later. An address change that has to wait is logged as an error and
goes on a later cycle; a switch to DHCP is not retried.

**Verified live 2026-11-16** (Network 11.0.99), on both cards:

- The spelling, first, by hand on this card. A one-key upload of
  `DSTConfiguration=Traditional US` was held in the file read back 90 s
  later. The card's event log said `Configuration change. Daylight saving
  time mode.` and `Configuration file upload complete, with 1 valid values.`
- Then the deployed bridge. Here it uploaded only `NTPSecondaryServer`
  (the primary server and DST already matched) and read the file back
  90 s later: `now reads ... DSTConfiguration=Traditional US,
  NTPPrimaryServer=<the controller's first>, NTPSecondaryServer=<its
  second>, NTPTimeZone=-07:00`. The card logged `Configuration change.
  Secondary NTP server address.`
- The card's clock and its event log read MST to the second, as they
  should in November.
- A line reached the log host within a second: an SNMP read with a wrong
  community, sent from the Podman host. The Mac's attempt never reached the
  card, and a failed FTP login is not an event on AOS 3.9.2.
- The log host (VictoriaLogs) files a line by the time it was received.
  The card's own RFC 3164 stamp is not kept there, so its hour can be
  read only on the card (the event log, which stamps the same clock) or
  in a packet capture.

**Not verifiable before March.** With `Traditional US` set, the card's
clock in November still reads standard time, as it should. The first DST
change it makes is 02:00 on 14 March 2027. The form names the dates but
not the hour; the help says only "as implemented in most areas of the
United States", and the US changes at 02:00 local time.

