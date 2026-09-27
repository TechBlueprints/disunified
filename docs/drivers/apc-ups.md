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

- `outlet_table`, `outlet_enabled`, `hw_caps` bit 128 -- the unit's outlet
  groups as outlets (§6b), in the rack-PDU row encoding a real controller
  has been seen to parse.

**Not claimed:** temperature as a chassis sensor (the unit measures its
battery), a replace-battery flag (see the CLAUDE.md note on NUT's status
code), `smart_power_caps` bits (no beeper/EPO/AC-recovery control is
honoured). No `battery_table` rows: every public capture has that array
empty.

**Control** is limited to switching and power-cycling the switched outlet
group, and only when the operator turns it on (§11).

## 5. Configuration

```yaml
devices:
  - name: ups
    driver: apc-ups
    url: ups.example.net         # the gateway's DNS name for the unit's DHCP lease (or its address; :502 implied)
    auth: none                   # the port has no credentials; without this a url is rejected
    model: USWDA25               # UPS 2U; "auto" ranks by port layout and will not pick a UPS
    options:
      mac: 02:00:00:00:00:02     # the SmartConnect port's MAC (Modbus does not expose it)
      netmask: 255.255.255.0     # the unit's network, and
      gateway_mac: 02:00:00:00:00:fe  # the gateway's L2 MAC as the segment sees it (ip neigh), not its listed device MAC
    # control:
    #   outlets: "2"              # switch Outlet Group 1 (row 2) from the controller; absent = read-only
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

**`USPDA2B` ("UPS 2U Pro", `type: usp`) since 2026-09-26 evening.** It was
first adopted as `USWDA25` ("UPS 2U", `type: usw`, the ordinary switch
inform path), then re-adopted as the 2U Pro for one reason: **Safe Shutdown
Pairing offers a UDM-class console only for the 2U Pro**, and Clint's
gateway and core switches all hang off this unit. What the switch cost and
proved, in one re-adoption (forget, clear state, adopt by MAC, ~2.5 min):

- `usp` is the controller's *power path*; no bridged device had informed as
  it before. The controller accepted the inform, stored `vbms_table`,
  `outlet_table`, `outlet_overrides`, `outlet_enabled`, `hw_caps` and
  `smart_power_caps` as sent, placed the device on its switch port in 40 s
  and set the hostname. Two switch-path keys are **not** stored on this
  path: `total_max_power` and `power_source` (the budget still reaches the
  UI through `battpool.device_total_power_budget`).
- The pinned catalogue (unifi-emu v0.5.5) labels the 2U Pro `usw`;
  Ubiquiti's fingerprint DB says `usp` and the controller resolves the
  family from the model string, so `internal/device/descriptor.go`
  corrects the wire type for `USPDA2B/2C` (`wireType`).
- The record **rejects `port_overrides`** with `api.err.Invalid`, so the
  provisioner sends the name only on this path (`unifiapi.Client.PowerPath`,
  set from the descriptor type). Outlet overrides are accepted.
- The 2U Pro claims per-outlet metering the unit cannot do; the rows are
  reported without the meter bit, so the UI reads `relay_state` for the
  picture rather than `outlet_power` (§6b). The update badge offered
  `1.6.1.4933` and was accepted per the rule (`firmware_base: 15.5` kept).
- The picture is eight individual cells (§6b), which is strictly better
  than the 2U's two multi-cells: Group 1 gets its own cell.

`USWDA25` remains a valid claim (`model:` in the config); switching back is
the same re-adoption. The rest of this section is the 2U's story, kept
because it explains the row layout, which did not change.

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

**Answered at adoption (2026-09-26): the eight profile slots are drawn
regardless.** With no `outlet_table` sent and `hw_caps` 0, the device page
still showed the UPS 2U graphic and every outlet rendered green/enabled; the
controller planted an empty `outlet_table` and `outlet_enabled: true` on
the record itself. So the picture is always eight, and the rows now make it
say the true thing: **row 1** is the Main group (on whenever the output is,
no relay bit, never commanded), **row 2** is Outlet Group 1 (its relay), and
**rows 3-8** are the slots the unit does not have -- present, off, no relay
-- so nothing phantom is offered. Group 1 is never mapped onto the profile's
surge slots. Row shape is the rack-PDU encoding (small `outlet_caps` beside
`outlet_type`), the form a real controller has been seen to parse for this
bridge; the battery-backed models' class-bit form has no capture behind it.

**How the picture is really drawn (read out of the Network 10.6.106 UI
bundle, 2026-09-26 evening).** The UPS 2U's diagram in the controller's
device catalogue is two rows, `01-02-03-04 __ 12 13` and `05-06-07-08 __
__ 11` (12/13 the surge in/out RJ45s, 11 the LAN port). A hyphenated entry
is one *multi-cell*: its four sub-outlets are all rendered from the **same**
data, the first outlet's, with tooltips and clicks disabled on the sub-cells.
So the top row's colour is outlet **1**'s `relay_state` and the bottom
row's is outlet **5**'s; outlets 2-4 and 6-8 are never consulted for the
picture. A cell is active (green) when `relay_state` is true, the device
is online and, for a surge cell, the UPS is not on battery (surge cells dim
in battery mode); a non-metered model reads `relay_state`, a metered one
reads `outlet_power > 0`. The row data is `outlet_table[i]` merged with
`outlet_overrides[i]`, override on top -- and the controller's stored
table already carries the override's `relay_state`, which is why our
reported `false` on the placeholders showed as `true` until the overrides
were set.

Consequences: (1) the provisioner now seeds the placeholders' overrides
off (`device.PlaceholderOutlets` → `unifiapi.SeedOutletOverrides`, on the
adoption handshake and every provision, idempotent), which turned the
bottom row grey "Not Powered" live; the resulting `system_cfg` carried
`outlet.3..8.relay_state=false` and the loop applied it as `0 of 1 outlets
changed` -- rows outside `control.outlets` are never considered. (2) The
top row stays green because it is row 1, the Main group, on. (3) Group 1
(row 2) is **invisible in the picture** at index 2. Mapping it to index 5
instead would make the bottom row follow its relay -- at the price of the
surge-cell rule dimming that row whenever the UPS runs on battery, which
on this unit would read "Not Powered" over a live bank. Not done; Clint's
call. (4) **The UPS 2U has no outlet editor in this Network version**: its
panel tabs are Overview (the picture), Insights and Settings; the PDU's
middle tab is Outlets. Multi-cells do not open one on click. So nothing in
the UI can switch a UPS 2U outlet; the control path that exists is the
API -- `PUT rest/device/<_id> {"outlet_overrides": [...]}` with a
`relay_state`, which the controller pushes as `outlet.<n>.relay_state` in
`system_cfg` and the loop applies (row 2 only, with the first-push hold).

**The 2U Pro's picture is different, and better for this unit** (Ubiquiti's
fingerprint DB, `USPDA2B`): `01 02 03 __ 04 __ 10 11` / `05 06 07 __ 08 __
09 __` -- eight *individual* cells, every outlet in both the `standard` and
the `surge` list (battery-backed with surge), 09 the LAN port, 10/11 surge
in/out. Each cell reads its own row (`outlet_table[i]` merged with the
override), so row 1 (Main) and row 2 (Group 1) each colour their own cell
and the six seeded placeholders draw "Not Powered". Single cells are
clickable (`subPanel: OUTLETS`), where the 2U's multi-cells were not.
**Checked live as the 2U Pro (2026-09-26 21:50 MDT):** the picture draws
cells 1 and 2 green (Main, Group 1) and 3-8 "Not Powered"; every cell
carries the battery-backed icon (the 2U Pro lists all eight as standard
*and* surge). The panel's middle tab is now **Outlets** (it was Insights on
the 2U): eight rows, `Power (W) -` throughout (no meter bit claimed), and
clicking row 2 opens the same editor the PDU has -- Link Device, Name,
**Power Cycle**, Outlet **Active / Disabled**, Power Cycle on Internet Loss,
Apply / Cancel. Cancelled without applying; that is the UI path for the
Group 1 relay, and the loop's first-push hold stands behind it. **Safe
Shutdown Pairing** lists the gateway as "Click to Pair"; clicking it asks
for the console owner's UniFi OS credentials in the panel ("To pair the
<console>, please enter the owner's credentials"), so pairing is the
operator's own step, not the bridge's -- cancelled, left for Clint.

## 7. Addressing: dial the lease's DNS name, and the address follows

Adopting a device deletes its UniFi client record and any fixed-IP
reservation on it (the rack PDU lost its address this way the morning after;
so did this unit). Nothing can push an address onto the UPS -- Modbus carries
no IP configuration -- so its address is held only by the DHCP lease.

What survives adoption is the **gateway's DNS name for the lease itself**:
the name the unit sends as its DHCP hostname still resolved to its address
after the client record was gone (verified 2026-09-26). So point `url:` at
that name rather than the address. The Modbus client dials the name on every
reconnect, the collector re-resolves it each cycle and reports the resolved
address (logging a move), and the bridge's `ip` defaults from it. If the
lease ever changes, the bridge follows; nothing is edited by hand.

The controller itself offers **no** way to learn an adopted device's real
address afterwards: an adopted MAC appears in neither `stat/sta` nor the v2
active-clients list, and there is no lease endpoint. It only knows the
address the device reports.

## 8. Verified live (Network 10.6.106, 2026-09-26)

- Live register read against the real unit: identity, rating, load (~80 % of
  1350 W), voltages, battery state and both outlet groups decode to the
  values NUT's `apc_modbus` reports for the same unit. That capture is the
  fixture.
- **Adopted**, one click in the device list; handshake in ~10 s (`authkey
  adopted` -> `CONNECTED`), both `system_cfg` pushes accepted without
  applying (read-only), device renamed by provisioning, inform interval set
  by the controller (86 s). No unhandled commands.
- **The controller kept `vbms_table` and parsed `battpool` field for field**
  -- read back from its own `stat/device` record: `batteryLevel`,
  `timeToRemain`, `ischarging`, `is_battery_mode`, `device_total_power_output`
  / `_budget`, output V/A, input V, power factor, `bms_run_anomaly`. The
  device page renders a real UPS: "Power Utilization 1075.73/1350 W 80 %",
  "Battery Ready 100 %", "Operating Mode Line", output voltage, power
  factor, power, current, live per cycle. The emitter was spec-derived
  (no real UniFi UPS inform exists on site); the controller's behaviour is
  now the verification.
- `smart_power_caps` 0 and `hw_caps` 0 stored as sent; `total_max_power`
  1350; satisfaction 100; `uplink` composed by the controller from the
  `"eth0"` string, with the operator-supplied netmask carried through.
- **Topology parent: first none, then placed.** Adopted first under a
  synthetic MAC (to keep the real port's client record and reservation): no
  parent after many cycles, every reachability field verified correct in the
  controller's record. Re-adopted the same day under the real MAC: placed on
  `access` port 24 within 20 s of CONNECTED. The controller places by
  finding the device's MAC on a switch port; `docs/topology-placement.md`.
  The trade: adoption under the real MAC deleted the unit's client record,
  its fixed-IP reservation and its client DNS name (accepted).
- **"Update Available"**: the controller offers the UPS 2U firmware
  (`upgradable: true`) because the honest version "15.5" is older than the
  model's release. The rule (as for the ECS Core) is to **accept it**: the
  emulated upgrade persists the requested version and reports it from then
  on (`firmware: 1.6.1.413`, with `firmware_base: 15.5` kept in the state
  file), and never touches the unit. Verified 2026-09-26: `upgrade to
  "1.6.1.413" requested (emulated reboot)`.
- **Safe Shutdown Pairing** lists the gateway as "Not Compatible", tooltip
  "UPS 2U does not support <gateway>. UPS 2U Pro is recommended instead."
  Pairing is the controller shutting a paired UniFi OS console down cleanly
  when the UPS reports battery mode with `timeToRemain` under a threshold;
  the UPS side is those two fields, already sent, and nothing is pushed to
  the UPS. The 2U pairs with UNVR/UNAS-class consoles; the 2U Pro adds the
  UDM class. Claiming `USPDA2B` would offer it, at a cost: it is `type: usp`
  (two gates reject that today, `internal/device/descriptor.go` and
  `main.go`, and no bridged device has informed as `usp`), all eight of its
  outlets are metered (`outlet_caps 65539`; the map has whole-load only),
  firmware 7.3.109, and a re-adoption. Only worth it if the console is
  actually on this UPS. Open.
- **NUT Server** (Settings → UPS Settings): "Shares UPS status with other
  devices. Use the ID / Hostname and port when configuring third-party
  devices for safe shutdown." On a real UniFi UPS it starts a NUT `upsd` on
  the UPS; gated on `smart_power_caps` bit 1, claimed 0 here, so ticking it
  would push `nut_server.*` keys nothing parses and show a hostname/port
  nothing serves. Left off. (A NUT server *in the bridge* would be a real
  feature: it holds every NUT variable, and the upsd protocol is plain text.)
- **No outlet editor for a UPS 2U** in this version (§6b): Overview,
  Insights, Settings only. Outlet control reaches the bridge via the API.

## 9. Things that will bite

- A burst of short-lived connections made the unit refuse connections for a
  while; the client keeps one session open and reads the four blocks.
- The two-register status word is big-endian by register (register 0 is the
  high word). Get it backwards and every status bit is silently wrong.
- There is no per-outlet or per-group metering in the map: output power is
  the whole load. "Which outlet is unused" cannot be read; it has to be known.
- The map has no charging bit; charging is "on mains below 100 %".

## 10. Deploying

Give the UPS its own stack -- compose file, image tag, state volume, env --
rather than adding it to a stack that holds other adopted keys: a rebuild of
this driver then never recreates the switch or PDU bridge. Its env needs
only the controller API variables; the port has no credentials. The state
volume layout is `state/<name>/device.json` (mode 600, the image's uid
65532), the same as every other device. Move an adoption by stopping the old
instance first, then copying that file, then starting the new one -- one
bridge holds a key at a time.

## 11. Outlet control -- built, unit-tested, not exercised on the unit

Clint's call (2026-09-26): the code is in place and covered by synthetic
tests; no command has been sent to the unit, which carries a whole rack at
~80 % load. **Enabled the same evening** (`control: {outlets: "2"}` in the
deployed config, stack rebuilt from `861b3f5`): the container came up with
`control: switching and naming outlets 2`, the startup reconcile applied
`0 of 0 outlets changed` (the hold cleared trivially -- the controller had
no stored outlet state to disagree with), and the controller then built its
overrides from the reported rows. The first real command is still Clint's
to send.

What it does when on:

- The controller's outlet editor (Active / Disabled on row 2) arrives as
  `outlet.2.relay_state`; **Power Cycle** arrives as `relayctl`. Both go to
  the outlet command word at register 1538 (function 16, two registers,
  high word first): command bit | target-group bit -- `0x0204` off,
  `0x0202` on, `0x0210` reboot for Group 1 -- exactly NUT's `apc_modbus`
  encoding for `load.off` / `load.on` / `load.cycle`. The tests hold the
  frame byte for byte.
- **Strict diff.** A push that matches the device writes nothing; the loop
  re-applies every cycle and a group relay is real load.
- **The Main group is never commanded.** Its target bit exists in the
  register; the driver never sets it, refuses a push or a cycle for row 1
  with one log line, and reports the row without a relay bit so the editor
  is not offered in the first place.
- **The first outlet push of a run is held if it would switch anything.**
  Per process, not from state: a bridge that ran read-only has an applied
  cfgversion, yet the controller's stored outlet state and the device may
  disagree the moment control is enabled, or after any restart (someone
  toggled at the LCD; a slot edited while nothing listened). The loop logs
  `HOLDING the first outlet push of this run` and applies nothing until the
  UI is set to match the device -- a push that changes nothing goes through
  and ends the hold. `control.allow_initial_changes: true` overrides.
- Sockets are not individually switchable on this hardware: Group 1's relay
  switches its whole bank. Which sockets that is, is printed on the rear
  panel; nothing in the map says.

Enabling it: `control: {outlets: "2"}` in the deployed config (not "all" --
row 1 is refused anyway, but say what you mean), rebuild and recreate the
stack, then check the log for the hold. The first real test is Clint's to
run, on a bank known to carry nothing.

