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
      outlets: all             # "all", or "2,5-8"; empty = read-only
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
| Overview | model, parent (`garage-switch Port 9`), IP, MAC, version, uptime; Power Usage / Current / `0W of 1440W` |
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
