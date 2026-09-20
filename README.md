# switch-to-unifi

A bridge that makes a non-UniFi switch appear as a real, adopted UniFi
switch inside the UniFi Network controller — ports, stats, topology, and
control — starting with Arista EOS.

The controller only speaks its own device protocol. This bridge speaks the
device side of it (the inform protocol, adoption, capabilities) on behalf of
a switch it reads and configures through the vendor's API. UniFi becomes the
place the switch is managed from.

Verified live on Network 10.6.106 with an Arista DCS-7160-48TC6-F (EOS
4.26.14M) claimed as a UniFi Data Center 100G-48X6: adoption, live port
stats and counters, LLDP and MAC-table topology (clients appear behind the
right port), optics, FEC, fans/PSUs; and control of port enable, names,
speed, VLAN membership (plus VLAN creation), FEC, storm control, BPDU guard,
STP mode/priority, IGMP snooping, and QSFP breakout split/join — all
end-to-end from the UniFi UI.

## How it works

```
UniFi controller  <── inform (TNBU/AES-GCM, every ~70 s) ──  switch-to-unifi  <── eAPI/SSH ──  the switch
                  ── system_cfg pushes / adoption ──>                         ── config diffs ──>
```

- `internal/switchmodel` — the vendor-neutral model of a switch and the
  driver contract.
- `internal/drivers/<vendor>` — one driver per vendor/OS (`arista-eos` today).
- `internal/device` — the inform session (forked from unifi-emu), payload,
  capability claims, persisted adoption state.
- `internal/unificfg` — parses the controller's `system_cfg` pushes.
- `internal/informloop` — collect → inform → apply/reconcile, every cycle.
- `internal/unifimodel` — picks the UniFi model to claim from the port layout.
- `internal/unifiapi` — controller REST API, used only to name the device and
  its ports after the switch on first provision.

## Install

`docs/install.md` — about 20 minutes: build, `-collect-once` to check the
switch, one small YAML config (`deploy/config.example.yaml`) with secrets in
the environment, adopt in the UI, then turn on control. Run it as a
container with `deploy/compose.yaml` or the Podman Quadlet unit in
`deploy/`.

```sh
go build ./cmd/switch-to-unifi
export STU_SWITCH_USER=stu STU_SWITCH_PASS=...
./switch-to-unifi -collect-once -switch-url https://192.0.2.3/command-api   # prints the snapshot + suggested model
./switch-to-unifi -config config.yaml                                       # bridges every switch in the file
```

Identity (MAC, serial, hostname, uplink), the port layout, the port speed
capabilities and the UniFi model to claim all come from the switch. The
adopted key lives under `state/`; keep it, one bridge per switch. Every
controller reply is recorded under `inform-log/`.

## For an AI agent adding a new switch

Read, in this order: `CLAUDE.md` (repo map and the rules that came from
live failures), `docs/adding-a-switch.md` (the checklist, with the proof
each step needs), `internal/drivers/CLAUDE.md` (rules for the vendor
layer), `docs/unifi-models.md` (which UniFi model to claim and how
the catalogue is refreshed), and `docs/feature-map.md` (every protocol
feature and its status). Then follow `CONTRIBUTING.md`: issue first, PR
against it, fixtures captured and scrubbed before code, tests against the
fixtures, and end-to-end verification on a real controller with a browser
automation tool driving the UniFi UI. The Arista EOS driver is the
reference; copy its shape, not its commands.

## Not affiliated with Ubiquiti

This is an independent, unofficial project by a home user and fan of UniFi
equipment who wanted to manage more switches from the same controller. It
is not affiliated with, endorsed by, or supported by Ubiquiti Inc. "UniFi"
and "Ubiquiti" are trademarks of Ubiquiti Inc., used here only to describe
what the software talks to. The device protocol it speaks comes from the
open-source unifi-emu library (see `docs/prior-art.md`); use it on
controllers you own, at your own risk.

This work exists strictly for integration: it lets a switch you already
own be managed from a UniFi Network controller that you are authorized to
use and that runs on genuine Ubiquiti hardware. It does not replace, clone
or bypass any Ubiquiti product, licence or service, and it is not intended
to be run against a controller you do not administer.

If Ubiquiti sees this repository and has any concerns, they are welcome to
contact me: open an issue on this repository, message
[@cgoudie](https://github.com/cgoudie) on GitHub, or reach me on Discord
(user ID `360473903417786371`; I am on the official Ubiquiti Discord server).

## License and credit

MIT — see `LICENSE` (attribution and trademark notes in `NOTICE`). The inform wire format, crypto and model catalogue come
from [jamesbraid/unifi-emu](https://github.com/jamesbraid/unifi-emu) (MIT),
whose protocol documentation made this possible; `internal/device` is a fork
of its inform session.
