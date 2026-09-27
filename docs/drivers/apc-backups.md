# APC Back-UPS Pro network model (`apc-backups`)

An APC Back-UPS Pro "network" model -- the BG500/BG1000/BG1500 line with
the embedded AP9537-class Network Management Card (APC's "Gassan" family,
AOS 6) -- presented to the controller as a UniFi UPS 2U Pro. Verified on
a Back-UPS Pro 500 (BG500, UPS 05.3, NMC AOS 6.0.1 / gsn 6.0.4), adopted
2026-09-27.

## 1. Why the web pages

The card fronts the UPS but tells SNMP nothing about it: its PowerNet
subtree holds only the card's own firmware and ports (`318.1.4`); the UPS,
PDU and MasterSwitch identity OIDs all answer *no such name*. There is no
Modbus, no SSH (only telnet), no JSON and no API -- the card's `/Forms/*`
handlers are the machine interface, form-encoded posts a browser would
make, the same surface the apc-pdu driver uses for names. So the driver
reads three pages and posts two forms:

| Page / form | Read for |
|---|---|
| `ulabout.htm` (once) | model, SKU, serial, UPS firmware, VA/W rating, battery chemistry |
| `ulinput.htm` (once) | the rated output voltage (the volts on every outlet row) |
| `home.htm` (every poll) | battery capacity %, runtime (minutes), input voltage, the alarm line and status list, per-outlet load |
| `uloutcfg2.htm` (every poll) | the outlets: two **main outlet groups** (MOG1/MOG2, always on with the UPS) and two **switched outlet groups** (SOG1/SOG2), each with name, state, watts, battery-backup setting, master/watchdog |
| `Forms/ulsogctl1` → `ulsogcfm.htm` → `Forms/ulsogcfm1` | switching a SOG: On / Off / Reboot |
| `config.ini` over FTP | the card's addressing (BootMode, gateway); the TCP/IP section is written back for IP Settings |

One login per poll (`Forms/login1` answers with the session token in the
redirect path, `/NMC/<token>/`), the pages, then `logout.htm`: the card
allows a handful of sessions and times idle ones out, and a bridge that
held one across restarts would run into that limit.

## 2. What is presented

- **Model `USPDA2B` ("UPS 2U Pro")**, the same choice as the Smart-UPS and
  for the same reasons (`docs/drivers/apc-ups.md` §6): eight single-cell
  outlet slots, an Outlets tab with an editor, Safe Shutdown Pairing for a
  console, and per-outlet metering columns -- which this unit actually
  fills. The UPS Tower model would match the form factor, but its picture is
  two multi-cells coloured by outlets 1 and 6 only and it has no editor.
- **Outlets**: rows 1-2 the main groups (on with the UPS, reported without
  a relay bit, refused if pushed), rows 3-4 the switched groups (relay),
  rows 5-8 the placeholders the model draws, present-but-off, seeded off in
  the controller by the provisioner. Every real row is **metered**: the
  card reports watts per outlet; volts are the rated output (120 V) and
  amps follow, the arithmetic the card itself does.
- **Battery** (`vbms_table`): charge, runtime, input voltage, the load as
  the sum of the outlet watts against the 300 W rating; on-battery, low
  battery, overload and output-off from the status list under the alarm
  line ("UPS is online." is the only text captured so far -- no outage
  yet); a critical alarm or "replace battery" as a fault; charging derived
  as on-mains-below-100 %.
- **Address**: the dialled host (an address, or a name resolved each poll)
  with the operator's netmask and gateway MAC; `DHCP` from the card's
  BootMode.

## 3. Control

- **Switched groups** through the card's two-step form: step one posts a
  select per group (`sog_control?N` = `01000000` on, `03000000` off,
  `05000000` reboot, `00000000` no action) and is answered with a redirect
  to the confirmation page; its hidden `SogControl` value (a word per slot,
  `none,off,none,none,`) is posted back with `Apply`. Nothing switches
  until that second post -- verified by staging an Off and logging out
  instead: both groups stayed on. One post carries every change of a push.
  Strict diff against the last poll; the next poll reads the state back.
- **Power Cycle** (`relayctl`) is the card's Reboot action.
- **The UPS itself is never turned off or rebooted** (the card's
  `ulctrl1` form is not posted), and outlet names are not written: the
  controller pushes none.
- **IP Settings** (`control.address`): a partial `config.ini` over FTP with
  `Override=<card MAC>` (the card ignores the section otherwise),
  `BootMode=Manual` + address/mask/gateway or `BootMode=DHCP Only` -- the
  apc-pdu path, and the same rule that DHCP is applied only when it
  replaces a static setting. **The controller's push for a `usp` device
  carries no `route.1.gateway`** (the switch path's does), so the driver
  writes `options.gateway`, or failing that keeps the card's current one,
  and treats the gateway as part of the diff. Found live: the first static
  write left the card at `DefaultGateway=0.0.0.0` until the option existed.

## 4. Configuration

```yaml
devices:
  - name: ups-2
    driver: apc-backups
    url: http://192.0.2.22            # the card's web UI (http; AOS 6 offers no https by default)
    username_env: DUI_APC_USER        # the card's device/admin login -- web and FTP
    password_env: DUI_APC_PASS
    model: USPDA2B
    hostname: ups-2
    options:
      mac: 02:00:00:00:00:03          # the card's MAC (About page / config.ini Override)
      netmask: 255.255.255.0
      gateway_mac: 02:00:00:00:00:fe  # the gateway's L2 MAC as the segment sees it
      gateway: 192.0.2.1              # written with a static address (the usp push has no route)
    control:
      outlets: "3,4"                  # the switched groups; 1-2 are main groups and refused regardless
      address: true
```

Adopt by address, not by the name the client record gave the card: adoption
deletes that record, its fixed-IP reservation and its DNS name (every APC
device so far). The card's own DHCP hostname is `apcXXXXXX`, so the
friendly name does not come back from the lease; it comes back from the
device `hostname` once adopted (the gateway resolves adopted devices by
it). Then set IP Settings → Static in the UI to the address it already
has: the bridge writes it to the card, and UniFi owns the address from
there.

## 5. Verified live (Network 10.6.106, 2026-09-27)

- Adopted by MAC in ~60 s, placed on its switch port, renamed after the
  unit ("APC Back-UPS Pro 500"); the four placeholder overrides seeded off.
- The controller stores eight outlet rows with `outlet_caps` 2 (metered) on
  the main groups and 3 (metered + relay) on the switched ones, with
  `outlet_power` (94.96 W on the main group carrying a NAS), and the battery
  pool (level 100, runtime 900 s, budget 300 W, input 116.7 V).
- IP Settings → Static applied to the card through config.ini (`BootMode`
  went `DHCP Only` → `Manual` at the same address); the gateway needed the
  option (§3). The DNS name kept resolving through the device hostname.
- Outlet switching is built and fixture-tested against the captured
  two-step form; **no switched group has been commanded on the unit yet**
  (both are named "unplugged" and carry 0 W -- the natural test subjects).

## 6. Fixture and scrub

`docs/fixtures/apc-backups-05.3/`: the card's real pages (`home`,
`ulabout`, `ulinput`, `uloutcfg2`, `ulsogctl`, the confirmation page
`ulsogcfm`) and a **reduced** `config.ini` (only the sections the driver
reads: TCP/IP, DNS, SystemID and the UPS section -- the rest is SNMP
communities, e-mail and users), scrubbed by `scripts/sanitize-apc-backups.py`
(session tokens, serials, MAC, addresses, host/domain, outlet names that
name equipment). The fixture runner flips a switched group's state in its
copy of the pages on a control post, so the apply tests converge like the
live driver.

## 7. Things that will bite

- The load on the home page is written by inline scripts
  (`parseFloat(95.35+"")`), not in cells; the Outlet Settings page has the
  same figures as cells and is what the driver sums.
- The card's `config.ini` numbers outlets differently from its pages
  (`Outlet1Name` is a switched group); the pages' order -- main groups then
  switched -- is the driver's.
- A Yes/No cell is plain text on a main group and a `<select>` on a
  switched one (selected `01000000` = No).
- The "Battery Backup" setting on a switched group can be turned off
  (surge-only); the driver reports the row either way and does not change
  the setting.
- Two Back-UPS bridges in one container that both turn on NUT need
  different ports in the UI: the server binds the pushed port.
