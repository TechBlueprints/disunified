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

- **No per-outlet metering.** This unit reports 0.0 A, and its own Load
  Management page agrees, so the reading is the card's truth and not an SNMP
  artefact. The driver reports outlets as relay-only and omits the measurement
  keys rather than sending zeros, because a zero reads as a real measurement
  of no load. A model that does meter fills `HasMetering` and the payload
  switches to the decimal-string form this device family uses.
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

## 6. Model claimed

`USPPDUP` — the USP-PDU-Pro. Its profile is a `usw` with one network port,
which is the shape a bridged rack PDU presents, and its 16 AC outlets line up
one-for-one with the AP7931's 16. `unifimodel.SuggestFor` picks it for any
snapshot that has outlets, rather than ranking the card's single network port
against every one-port switch in the catalogue.

## 7. Things that will bite

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
