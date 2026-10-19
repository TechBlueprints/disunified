# Presenting a gateway: researched, not attempted

Asked 2026-09-20: could this bridge present a non-UniFi router (a cellular
router at another location) to the controller as a UniFi **gateway**, so that
location shows up as another network with its own gateway? Researched against
unifi-emu v0.5.5 and the controller's catalogue, then parked. This is the
record, so the next agent starts from the constraints rather than from zero.
Items 2 and 3 below have moved since, upstream and from a third party: read
"Since then" at the end first.

## What transfers for free

Everything below the device abstraction is type-neutral: the TNBU packet,
CBC/GCM, the adoption handshake, the inform loop and `State`
([`protocol-connection-points.md`](protocol-connection-points.md)). The pinned
catalogue already carries **eight gateway profiles** — `ugw`: UGW3, UGW4,
UGWXG; `uxg`: UXG (Gateway Lite), UXGB (Max), UXGPRO (Pro), UXGENT
(Enterprise), UXGA6AA. `DescriptorFor` accepts `usw` and `usp`, so a gateway
means a third wire type plus a gateway payload; the power-device work already
proved that pattern (the switch payload plus the family's own tables).

## What stops it

1. **One gateway per site.** Adopting a second answers
   `api.err.NoSecondGateway` — unifi-emu's DESIGN.md, "Verified protocol
   facts". **Prior art, not verified on 10.6.106: verify before building.**
   If it holds, a bridged gateway can never join the site the console's own
   gateway is in; it needs its own site, and "another network" means the site
   switcher. This is a controller constraint, so no amount of payload work in
   this repo moves it.
2. **Nobody has written down what a real gateway informs.** unifi-emu's
   PROTOCOL.md gateway bullet describes its own stub: `system-stats`,
   `config_network_wan` as the constant `{"type": "dhcp"}`, `netmask`, and an
   `uplink` **object** with zeroed counters. That object is suspect — on
   switches an object was silently ignored and only the management interface
   *name as a string*, with the interface in `if_table`, produced a parent
   ([`protocol-connection-points.md`](protocol-connection-points.md) §2). For
   gateways it is simply unverified.
3. **The config push is unknown.** A switch's config arrives as
   `setparam.system_cfg` with port/VLAN/STP keys
   ([`feature-map.md`](feature-map.md) §3). A gateway presumably receives the
   site's networks, firewall and DHCP the same way; a read-only bridge would
   refuse nearly all of it, and nothing says whether the controller then parks
   the device in permanent provisioning.
4. **No gateway model escapes the firmware prompt.** Every gateway profile has
   real firmware in the controller's database, so there is no equivalent of the
   USW Leaf's "no releases" property ([`unifi-models.md`](unifi-models.md)):
   an honest vendor version becomes a standing update offer. Livable — the UPS
   path accepts the badge and keeps the base version — but it is not free.
5. **Capability claims cannot be copied from the switch path.** Of the gateways
   that adopt by inform, only UXG-Enterprise has UDAPI routing and the USG line
   has none at all (comment in unifi-emu's `inform/session.go`), and the
   controller offers every claimed capability against the device.

## The cheaper alternative

Present the router as a **switch**, which is the path this repo has verified:
its interfaces become ports, link state, counters and client attribution all
work, it joins the existing site with no `NoSecondGateway` problem, and the
only real loss is the WAN/Internet panel and the gateway framing. Worth
weighing before anyone builds a gateway payload.

## If it is attempted, capture first

The usual rule. The UniFi OS console support bundle carries every device's
decrypted `last.inform` under `unifi/devices/<type>/<mac>/`; check whether the
console's own gateway appears there before writing a single wire key. Upstream
issue #6 asks the same question of unifi-emu
([`prior-art.md`](prior-art.md) §6).

## Since then

- **2026-09-22: what a real gateway informs is written down, upstream.**
  unifi-emu [#15](https://github.com/jamesbraid/unifi-emu/pull/15) (ours,
  unreviewed) measures the gateway payload against a UniFi OS gateway's
  inform on 10.6.106. It covers `network_table` keyed by interface (each WAN
  uplink by name, the LAN as `br0`), `usg_caps` `12` with the matching `has_*`
  booleans, `hw_caps` on every path, and `if_table` with one row per layer-3
  interface (the WAN uplinks, none of the bridged LAN ports). That answers
  item 2 in unifi-emu, not in this repo.
- **2026-10-19: an emulated gateway adopted, by someone else.** A third party
  built #15's branch and adopted an emulated UXGB on a UniFi OS Server running
  Network 10.6.106. A server has no gateway of its own, so the site's one
  gateway slot was free: item 1 was sidestepped, not tested. Their findings,
  not reproduced here
  ([comment on #6](https://github.com/jamesbraid/unifi-emu/issues/6#issuecomment-6266565972)):
  - After adoption the controller sends
    `{"_type":"cmd","cmd":"wg-genkeypair","request_id":…}` about once a
    minute. Until it is answered, the device stays unprovisioned (`default`
    true, no `setup_provision_completed`). Copying a real UCG-Ultra's
    capability bits and UDAPI map made no difference.
  - The answer rides the next inform as a notification: `inform_as_notif:
    true`, `notif_reason: "cmd"`, and `notif_payload` holding `event_string:
    "wg-genkeypair"`, the same `request_id`, and `params` with a fresh X25519
    `private_key`/`public_key` (base64). The private key goes to the
    controller too.
  - The console then offers the zone-based firewall upgrade. After it, the
    integration API lists the zones and the policies.
  - The gateway is still listed OFFLINE although it informs every cycle.
    Cause unknown.

  That is a partial answer to item 3: the first thing the controller needs
  from an adopted gateway is a command answered, not a config applied.
  Whether a read-only bridge then sits in permanent provisioning is still
  open. This repo's `internal/device` has no notification reply path
  (`inform_as_notif`), so a gateway here would need one.
