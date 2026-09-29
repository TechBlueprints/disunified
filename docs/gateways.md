# Presenting a gateway: researched, not attempted

Asked 2026-09-20: could this bridge present a non-UniFi router (a cellular
router at another location) to the controller as a UniFi **gateway**, so that
location shows up as another network with its own gateway? Researched against
unifi-emu v0.5.5 and the controller's catalogue, then parked. This is the
record, so the next agent starts from the constraints rather than from zero.

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
