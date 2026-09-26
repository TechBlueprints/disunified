# apc-ups — APC Smart-UPS over its SmartConnect port

Presents an APC Smart-UPS to the controller as a UniFi UPS: charge, runtime,
load, real and apparent power, voltages, battery temperature, on-battery and
low-battery state — read over **Modbus TCP from the unit's own SmartConnect
Ethernet port**. No network management card, no serial cable, no USB.

Written against an **SMTL1500RM3UC** (1350 W / 1440 VA, lithium-ion, UPS ID
1026, firmware UPS 15.5). Fixtures in `docs/fixtures/apc-smtl-15.5`.

## 1. Why Modbus on the SmartConnect port

The green SmartConnect jack is documented by Schneider as cloud-only with no
local interface, and for management that is true (80/443/22/23 are all
refused). But the unit's firmware release notes for this exact ID and SKU
record, at UPS 01.3: *"Added support for Modbus over TCP on SmartConnect
port."* Enabled at the display, port 502 serves the full Smart-UPS Modbus
register map — the same one an NMC would serve — from an address the unit
already has on the LAN.

The other local paths were all ruled out or beaten on cost: USB HID (what a
NAS-hosted NUT reads) exposes only charge, runtime and battery voltage on
post-2010 APCs — the load and electrical data moved to Modbus; Modbus over
USB needs a patched libmodbus; serial Modbus needs the 940-0625A cable and a
dongle; a management card costs money and gives the same registers over TCP.

## 2. Enable it on the unit

Modbus ships disabled. On the LCD: **Configuration → Menu Type → Advanced**,
then **Configuration → Modbus → Enable**. Until it is enabled the port
answers 502 with a connection *refused* (a live stack, nothing listening),
which is how to tell "disabled" from "unreachable".

## 3. What is read

Four holding-register blocks per cycle, on one persistent connection — the
same four NUT's `apc_modbus` driver reads, because the map is sparse and a
read straddling an undefined address is refused:

| Block | Holds |
|---|---|
| 0–26 | status bitfield (online, on battery, bypass, off, fault, test, ECO, overload), transfer cause, outlet-group states, shutdown-imminent, calibration |
| 128–171 | runtime, charge, battery V and °C, load %, apparent %, output A/V/Hz, input status (boost/trim), input V, efficiency |
| 516–635 | firmware, model, serial, nameplate W and VA, which outlet groups exist, their names |
| 1026–1047 | transfer thresholds, shutdown/start/reboot delays |

Values are fixed-point (`value × 2^bits`); real watts are load % of the
nameplate rating. Every measurement carries a "was it measured" flag so an
input the unit reports as not applicable (`0xffff`) or an efficiency it
reports as a reason code is omitted from the payload rather than sent as 0.

## 4. What is claimed, and what is not

The controller runs its battery pipeline for any device that sends
`vbms_table`; the model's own flags do the rest. The payload sends:

- `vbms_table.is_battery_mode` — the AC-lost signal.
- `vbms_table.battpool` — `batteryLevel`, `timeToRemain`, `ischarging`,
  `device_total_power_budget` / `_output`, output V/A, input V, power
  factor, with the spellings the controller reads (they are mixed-case and
  the controller republishes them under other names — a device using those
  reports nothing it will read).
- `bms_run_anomaly` — battery-low from the unit's shutdown-imminent signal;
  the two overload bands from the measured load.
- `smart_power_caps: 0` — no beeper, EPO, AC-recovery or NUT-server control
  is claimed, because none is honoured.

**Not claimed:** an outlet table, `hw_caps`, temperature as a chassis sensor
(the unit measures its battery), a replace-battery flag (see the CLAUDE.md
note on NUT's status code). No `battery_table` rows: every public capture
has that array empty.

**This version is read-only.** The unit has a switchable outlet group
("Outlet Group 1"); driving it from the controller's outlet editor is a
follow-on that waits on a discussion with the operator and a test outlet
known to carry no load. The Modbus client implements reads only.

## 5. Configuration

```yaml
devices:
  - name: ups
    driver: apc-ups
    url: 192.0.2.30              # the SmartConnect port's address (:502 implied)
    auth: none                   # the port has no credentials; without this a url is rejected
    model: USWDA25               # UPS 2U; "auto" ranks by port layout and will not pick a UPS
    options:
      mac: 02:00:00:00:00:02     # the SmartConnect port's MAC (Modbus does not expose it)
      netmask: 255.255.255.0     # the unit's network, and
      gateway_mac: 02:00:00:00:00:fe  # its gateway's MAC: neither is readable over Modbus
      # unit_id: 1
      # timeout: 3s
```

No username or password: the port has none. `mac` is worth setting to the
port's real address so the controller sees one device rather than a device
and a separate client — but note §7. `netmask` and `gateway_mac` are what
the controller places a device in a network by (the reachability fields a
real device reports beside `connect_request_ip`); Modbus carries no IP
configuration, so they come from the operator. Leave them out and the UPS
adopts but has no parent in the topology.

## 6. Model claimed

`USWDA25` ("UPS 2U", `type: usw`). It runs the ordinary switch inform path,
which is what a UPS needs — there is no UPS device type on the wire — and it
is the rackmount UniFi UPS. Its profile draws eight outlets and one 100 M
port; this unit has six outlets in two groups and one port. The UPS 2U Pro
(`USPDA2B`) was not chosen: it is `type: usp`, an unexercised path, and it
claims per-outlet metering this unit cannot do.

## 6b. The outlet picture, and why it cannot show two

The controller draws a device's outlet slots from its own hardware profile
for the claimed model, not from what the device reports -- the way it draws
port count from the profile. The PDU work established this live: slots the
device did not report were still drawn, at the controller's defaults, "four
outlets that look enabled and switchable but are not there at all"
(`internal/device/tables.go`). So a UPS 2U shows eight slots however many rows
are sent, and no UniFi power model has two outlets to borrow a picture from
(Tower 10, 2U 8, 2U Pro 8/9, PDUs 20/24, strip 6+1, plug 1 -- and the plug
runs the access-point path).

What is controllable is what each slot *says*. The unit has two outlet
groups, not two outlets: `Unswitched Group` (Main, always on with the output)
and `Outlet Group 1` (one relay for its whole bank; sockets are not
individually switchable -- the command register addresses groups only). When
outlet control is wired, the proposed shape is two real rows for the two
groups plus explicit placeholders for the six slots the unit does not have
(present-but-off, no relay -- the PDU's pattern), so nothing phantom looks
switchable. Group 1 is never mapped onto the profile's surge slots: those are
drawn as non-battery, and every outlet on this unit is battery-backed.

**Open, to be answered at adoption (read-only, no risk):** whether the
Outlets tab appears at all for a device that sends no `outlet_table` and no
`hw_caps` outlet bit, or whether the eight profile slots are drawn regardless.
If they are, the placeholder rows go in immediately, ahead of any control.

## 7. Give the unit a manual address before adopting it

Adopting a device deletes its UniFi client record and any fixed-IP
reservation on it (the rack PDU lost its address this way the morning after).
Set a manual address on the unit's display first, or expect the DHCP lease
to change.

## 8. Verified

- Live register read against the real unit, 2026-09-26: identity, rating,
  load (~80 % of 1350 W), voltages, battery state and both outlet groups
  decode to the values NUT's `apc_modbus` reports for the same unit. That
  capture is the fixture.
- Not yet adopted by a controller: the `vbms_table` emitter is spec-derived
  (unifi-emu `docs/PROTOCOL.md`), there being no real UniFi UPS on site to
  capture an inform from. Adoption and a UI walk are the next step.

## 9. Things that will bite

- A burst of short-lived connections made the unit refuse connections for a
  while; the client keeps one session open and reads the four blocks.
- The two-register status word is big-endian by register (register 0 is the
  high word). Get it backwards and every status bit is silently wrong.
- There is no per-outlet or per-group metering in the map: output power is
  the whole load. "Which outlet is unused" cannot be read; it has to be known.
- The map has no charging bit; charging is "on mains below 100 %".
