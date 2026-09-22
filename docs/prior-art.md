# Prior art, and what this project took from it

Research done 2026-09-19, before the first line of code; kept because the
choices below still explain the shape of the repo.

## 1. How a UniFi device gets into the controller

There is no supported "add a third-party device" path. Ubiquiti's official
UniFi API (<https://help.ui.com/hc/en-us/articles/30076656117655-Getting-Started-with-the-Official-UniFi-API>)
reads and controls devices the controller already owns; it cannot create
one. The only way in is to speak the device side of the **inform
protocol**, as unifi-emu implements and documents it
(<https://github.com/jamesbraid/unifi-emu>, `docs/PROTOCOL.md` there). The
protocol as this bridge speaks it, verified on Network 10.6.106, is in
[`protocol-connection-points.md`](protocol-connection-points.md).

## 2. Projects that already did this

| Project | What it fakes | Lang | License | State (2026-09-19) |
|---|---|---|---|---|
| [jamesbraid/unifi-emu](https://github.com/jamesbraid/unifi-emu) | APs, **switches**, gateways — current lineup | Go | MIT | active, v0.5.5 (2026-09-02) |
| [wvengen/unifi-controllable-switch](https://github.com/wvengen/unifi-controllable-switch) | TOUGHswitch presenting as *UniFi Switch 8 POE-150W* | C/shell/Python | — | archived Jan 2019 |
| [amd989/unifi-gateway](https://github.com/amd989/unifi-gateway) | UGW3 router | Python 3 | MIT | maintained-ish |
| [qvr/unifi-gateway](https://github.com/qvr/unifi-gateway) | UGW router | Python | — | archived 2025-05 |
| [stephanlascar/unifi-gateway](https://github.com/stephanlascar/unifi-gateway) | UGW router (the original) | Python | — | archived Jan 2019, "NOT WORKING" |

## 3. What was taken from each

**jamesbraid/unifi-emu** (MIT, credited in [`NOTICE`](../NOTICE)) is the
foundation:

- its `inform` package is a dependency: the TNBU packet, CBC and GCM
  crypto, `ParseKey` with the MD5-of-`ubnt` default;
- its model catalogue (`model_profiles.json`, generated from a real
  controller's hardware database) and `capability_bits.json` are used at
  build time by [`internal/unifimodel`](../internal/unifimodel) and the
  capability claims;
- [`internal/device`](../internal/device) is a fork of its inform session
  and payload tables, with the synthetic device state replaced by what the
  drivers collect;
- its design rule holds here too: a device enters a controller **only**
  through the real inform and adoption lifecycle, never by seeding the
  controller's database.

**wvengen/unifi-controllable-switch** gave the first field list for a
switch's `port_table` and the `v<numeric>` firmware-string quirk. Both were
superseded by real captures: the wire contract tests now compare our
payload with real switches' informs from a UniFi OS support bundle
([`adding-a-device.md`](adding-a-device.md)).

**amd989/unifi-gateway** gave two operational habits: a pluggable
collector per platform (our drivers) and logging every reply the
controller sends that the bridge does not understand (`inform-log/`, and
the `UNHANDLED` lines).

## 4. The USW Leaf question, answered

USW-Leaf (<https://ubntwiki.com/products/unifi/unifi_switch_leaf>) was 48x
SFP28 + 6x QSFP28, Early Access only, never released. The controller
renders a device from **its own** profile for the model string claimed, so
the claim has to exist in its database. `UDC48X6` is the shipping model
with exactly the Leaf's layout, and Network 10.6 displays it as "USW Leaf":
that is what the Arista (48x 10GBASE-T + 6x QSFP28) and the Proxmox nodes
(54 virtual ports) claim, with the device's own per-port media replacing
the profile's SFP28 icons. The rule and the whole catalogue are in
[`unifi-models.md`](unifi-models.md).

## 5. The Arista side

Data collection goes through **eAPI** (HTTPS JSON-RPC), with SSH as an
alternative transport; both are in [`drivers/arista-eos.md`](drivers/arista-eos.md).
gNMI/OpenConfig streaming was not needed at the inform cadence. Running the
bridge **on the switch** (EOS supports containers through the
`container-manager` extension, or an EOS SDK `.swix`) was considered and
not pursued: the container's traffic lives in the Linux namespace, apart
from EOS's own forwarding, and a bridge on a separate host is simpler to
run and update. The bridge runs in a container on a host next to the
switch.

## Sources

- <https://github.com/jamesbraid/unifi-emu> / <https://pkg.go.dev/github.com/jamesbraid/unifi-emu/inform>
- <https://github.com/wvengen/unifi-controllable-switch>
- <https://github.com/amd989/unifi-gateway>, <https://github.com/qvr/unifi-gateway>, <https://github.com/stephanlascar/unifi-gateway>
- <https://ubntwiki.com/products/unifi/unifi_switch_leaf>, <https://dl.ui.com/product_sheets/USW-Leaf_Brief.pdf>
- <https://help.ui.com/hc/en-us/articles/30076656117655-Getting-Started-with-the-Official-UniFi-API>
- <https://arista.my.site.com/AristaCommunity/s/article/managing-containers-on-eos-container-manager>
- <https://www.arista.com/en/products/eos/open-and-programmable>
