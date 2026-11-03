# `stub` — a device described by a file

An instrument for controller and UI experiments, not a device. The stub
reports whatever a snapshot file describes -- the device's hostname, serial
and version, its ports (index, link, admin state, speed, duplex, media,
speed caps, FEC, optic present, which one is the uplink) and the
capabilities the controller is told it honours -- and writes nothing
anywhere. It is how a question about the controller ("does it place a
device here?", "what does the UI draw for a 75000 Mb/s port?", "does it
offer an aggregate on this profile, and what does it push?") is answered
without a device that has the shape in question.

Written for the breakout-lane placement capture
([`docs/breakout-lag.md`](../breakout-lag.md) §6.7).

## Config

```yaml
  - name: lane-stub
    driver: stub
    url: 192.0.2.4                # the address reported as the device's (nothing is dialled)
    auth: none
    model: USFXG                  # whatever profile the experiment needs
    options:
      snapshot: docs/fixtures/stub/flexxg-4x25g.yaml
      mac: "02:00:00:00:00:a5"    # required: the stub has no MAC of its own
      netmask: 255.255.0.0
      gateway_mac: "02:00:00:00:00:fe"
```

`mac`, `netmask` and `gateway_mac` are taken exactly as `apc-ups` takes them
(the shared `internal/drivers/reach`): the MAC is the one the device is
adopted under, and the address, mask and gateway's L2 address are what the
controller reads when it places a device
([`docs/topology-placement.md`](../topology-placement.md)). A `url` that is
a name is resolved every cycle, as for `apc-ups`.

## The file

YAML or JSON. The reference is
[`docs/fixtures/stub/flexxg-4x25g.yaml`](../fixtures/stub/flexxg-4x25g.yaml):

```yaml
hostname: lane-switch
serial: SSJ00000000
version: 1.0.0
capabilities: {fec: true, lacp: true, aggregate_sessions: 8}
ports:
  - {index: 1, ifname: Ethernet4, up: true, enabled: true, speed: 25000, full_duplex: true,
     media: SFP28, speed_caps: [10000, 25000], fec: rs-fec, optic_present: true}
  - {index: 5, up: true, enabled: true, speed: 100000, full_duplex: true, media: QSFP28,
     speed_caps: [100000], optic_present: true, is_uplink: true}
```

- `media`: a `devicemodel.Media` name (`1G-T`, `2.5G-T`, `10G-T`, `SFP`,
  `SFP+`, `SFP28`, `QSFP+`, `QSFP28`); `fec`: `rs-fec`, `fc-fec`,
  `disabled`; speeds in Mb/s. A port that is not up reports no speed.
  Optional per port: `ifname` (default `Port N`), `mtu` (default 1500),
  `neighbor: {chassis_id, system_name, port_id, bridge, router}`.
- `is_uplink` on at most one port: the device's uplink, reported as a real
  switch reports its own (`port_table[].is_uplink`).
- `capabilities`: `stp`, `bpdu_guard`, `loop_protection`, `jumbo`, `fec`,
  `lacp`, `storm_control`, `igmp_snooping`, `mirror_sessions`,
  `aggregate_sessions`. A real driver's claims must be true; the stub's are
  the experiment's -- the UI offers what is claimed, and the push it sends
  is logged and nothing is applied. Read once, at start.
- **The file is re-read every cycle.** Edit it (or copy another file over
  it) and the next inform reports the change; a change of the port layout
  is logged.

The other two files in [`docs/fixtures/stub/`](../fixtures/stub/) are the
same cage split 2x50G (two lanes, two empty ports) and with port 1
reporting 75000 Mb/s ([`docs/breakout-lag.md`](../breakout-lag.md) §5.1).

## What it does with a push

Nothing: the stub implements no `Controller`, `DeviceController`,
`AddressController` or any other write interface, so the loop has nothing
to hand a push to. Every reply the controller sends is in
`inform-log/<name>/` (the reply log) as on every device; a pushed
`system_cfg` lands there whole, which is how a push is captured. Emulated
firmware upgrades are reported as on every device.

## Rules

- Run it from wherever is convenient for the experiment, and stop it when
  the experiment is over; forget the device in the controller
  (`cmd/sitemgr delete-device` by its exact MAC) and delete its state dir.
- Use a locally administered MAC that is nobody's: adopting a MAC deletes
  its client record, and a stub adopted under a real device's MAC would
  take that device's place.
- Only documentation-range identifiers in a committed file
  (`scripts/check-site-info.sh`).

Tests: `internal/drivers/stub/stub_test.go`, over the three files.
