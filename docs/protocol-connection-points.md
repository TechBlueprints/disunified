# UniFi bridge: protocol and connection points

Distilled from reading the source of three projects (cloned and read 2026-09-19), not
from their READMEs. Nothing here has been run against a real controller yet.

- `jamesbraid/unifi-emu` (Go, MIT) — the modern, complete reference. Its `docs/PROTOCOL.md`
  is an implementer's spec, and its `model_profiles.json` is generated from a real
  **controller 10.4.57** hardware database.
- `wvengen/unifi-controllable-switch` (C/shell, archived 2019) — the only *switch* prior art,
  and the only one that implements the **L2/SSH adoption** side.
- `amd989/unifi-gateway` (Python 3, MIT) — a maintained daemon; weakest on protocol, best on
  operational shape (collector abstraction, unknown-response logging).

---

## 1. The wire packet ("TNBU")

Same packet both directions. Device POSTs it as the HTTP body to
`http://<controller>:8080/inform`.

**Header is 40 bytes** (unifi-emu's `docs/PROTOCOL.md` layout):

| Off | Size | Field |
|---:|---:|---|
| 0 | 4 | magic `TNBU` |
| 4 | 4 | packet version, big-endian uint32, **always 1** (the old docs say 0) |
| 8 | 6 | device MAC, raw bytes |
| 14 | 2 | flags, big-endian uint16 |
| 16 | 16 | IV (also the GCM nonce) |
| 32 | 4 | payload version, big-endian uint32, always 1 |
| 36 | 4 | body length |
| 40 | n | body |

Flags: `0x01` encrypted, `0x02` zlib, `0x04` snappy, `0x08` AES-GCM.

**HTTP details** (from `amd989/unifi-gateway`, which the specs omit):

```
Content-Type: application/x-binary
User-Agent:   AirControl Agent v1.0
```

### Crypto gotchas

- Key = plain hex decode of the 32-char authkey. **No KDF.**
- Unadopted default key is `MD5("ubnt")` = `ba86f2bbe107c7c57eb5f2690775c712`.
- CBC: PKCS#7 padding, IV from the header, fresh per packet.
- **GCM uses a 16-byte nonce, not 12.** Most AEAD libraries default to 12 and fail with no
  useful error. Go: `cipher.NewGCMWithNonceSize(block, 16)`. OpenSSL: `EVP_CTRL_GCM_SET_IVLEN`.
- **GCM AAD is the entire 40-byte header**, assembled including the body-length field before
  the tag is computed. Tag is appended to the ciphertext.
- A device starts on CBC. The controller flips it by sending `mgmt_cfg.use_aes_gcm=true`.
  There is no path back to CBC. **So we must implement both.**
- zlib (RFC 1950) is applied to the JSON before encryption. Snappy only needs decoding.

## 2. The switch payload

Flat JSON object. Sent on **every** inform, pending or adopted:

`mac`, `serial`, `model`, `model_display`, `version`, `ip`, `hostname`, `inform_url`,
`uptime`, `time`, `cfgversion`, `x_authkey`, `default`, `_default_key`, `state`, `fw_caps`,
`isolated`, `locating`, `selfrun_beacon`.

Once adopted, `state` becomes **4** (a device-side enum, unrelated to the controller's REST
`stat/device.state`), plus `bootrom_version`, `sys_stats` (`cpu`, `mem_total`, `mem_used`,
`mem_buffer`), and for `type: "usw"`:

- **`port_table`** — one entry per port
- **`ethernet_table`** — a single entry: `mac`, `name`, `num_port`

`unifi-emu`'s `portTable()` (`inform/tables.go:19`) emits the minimum the controller needs:

```
ifname, name, port_idx, media, poe_caps, is_uplink,
up, speed, full_duplex, rx_bytes, tx_bytes
```

`wvengen`'s `unifi-inform-status` adds the fields that make the UI's port detail view
actually populate:

```
enable, stp_state, mtu,
poe_enable, poe_mode, poe_voltage,
rx_packets, tx_packets, rx_errors, tx_errors
```

**Use the union of the two.** `unifi-emu` is minimal-to-adopt; `wvengen` is what the switch
view reads. One wvengen quirk worth copying: strip the firmware version to its numeric part
and prefix `v`, or the controller rejects it.

### Capability bitmaps — claim nothing you can't service

The controller gates features on self-reported bitmaps and trusts them blindly. Asking for
an unclaimed feature 404s.

- `fw_caps` — top-level int; the controller tests **22 distinct bits**. Use
  `inform.PlaceholderFWCaps` = **3** (bits 0 and 1), which is deliberately outside those 22,
  so it reads as a claim to nothing. Other small values are *not* equally safe.
- `switch_caps` — a nested object of sub-bitmaps (feature, STP, storm-control, IGMP-snoop,
  PTP), not one int.
- `hw_caps` — physical features (screen, LCM, PoE class).
- `udapi_caps` — **must** be sent together with `udapi_version`. On firmware 4.1.0+, sending
  `udapi_caps` alone makes the controller drop the *entire* capability update — `fw_caps`,
  `hw_caps`, `switch_caps` and all. Safest for phase 0: send neither.

`unifi-emu` ships `capability_bits.json` mapping named bits to values.

## 3. Adoption — three connection points

### L3 (what we should build)

1. Bridge informs every 5-10s with the default key, `state=1`, `default=true`.
   **HTTP 404 is the normal reply while pending** — no body, not an error. Keep informing.
2. Operator clicks Adopt.
3. The controller delivers a new authkey by **one of two channels, build-dependent**:
   - `{"_type":"cmd","cmd":"set-adopt","key":"<authkey>","uri":"<inform url>"}` — always applies.
   - `setparam` with `mgmt_cfg`: a **newline-separated `key=value` string, not JSON**, carrying
     `authkey=`, `cfgversion=`, `use_aes_gcm=`. This is the common path in practice.
   Support both.
4. Keep informing. Connected is reached when a reply arrives to an inform already sent adopted.

**Two bugs to avoid, both documented from experience:**

- **Gate `mgmt_cfg.authkey` on still holding the default key.** Applying it unconditionally is
  the classic stuck-adopt loop: a replayed `mgmt_cfg` clobbers the adopted key back to one the
  controller no longer knows, and the device drops to pending. `set-adopt` has no such gate.
- **`inform_url` must report an IP literal, never a hostname.** The controller validates it
  post-adoption and returns HTTP 400 `invalid inform_ip <host>`. Resolve the controller name
  to an IPv4 address before reporting it.

### L2/SSH (only if we want to click Adopt the stock way)

`wvengen` implements the device end: the controller SSHes in as `ubnt/ubnt` and runs
`/usr/bin/syswrapper.sh set-adopt <inform_url> <authkey>`; the script stores both and then
informs twice (adoption, then association). We'd need an SSH listener accepting that one
command. **Per unifi-emu, adoption completes over inform alone without any of this**, so skip
it for phase 0.

### Discovery (optional)

UDP 10001 broadcast, only so the device shows up in a controller live-scan on the same L2
segment. `header: version(1) | command(1) | payloadLength(2 BE)` then TLVs
`type(1) | length(2 BE) | value`. Versions 0, 1, 2 in use; a v2 packet needs MAC (type 1),
sequence ≥ 1 (type 18) and source MAC (type 19). Other identity types: 2 MAC+IP, 3 firmware,
10 uptime, 11 hostname, 12 platform, 21 model, 53 netmask. Decoders must skip unknown types.

This is the part that would be awkward from a container on the Podman host or on EOS —
and it is **not required for adoption**. Defer it.

## 4. The control channel (phase 1)

Controller replies carry `_type`: `noop`, `setparam`, `setstate`, `cmd`, `upgrade`, `reboot`,
`setdefault`.

- **`setstate`** carries config tables: `radio_table`, `vap_table`, **`port_table`**,
  **`port_overrides`**. A real device stashes these and **echoes them back verbatim on every
  later inform**, overriding what it would otherwise compute. A device that accepts a push and
  then reports its own defaults looks, to the controller, identical to one that rejected it.
  **`port_overrides` is the phase 1 hook**: what the controller pushes there is the user
  editing a port in the UI, and it's what we'd translate into eAPI config.
- `setparam`/`mgmt_cfg` also carries `interval`, the inform cadence the controller wants.
- `setdefault` = factory reset: clear adopted, key back to default, `cfgversion` to `"0"`,
  drop to CBC, forget provisioned config.
- `reboot`/`upgrade` reset the uptime clock; `upgrade` carries a target version to adopt.
  Faking the reboot is enough.

## 5. Which model to claim — answered

`unifi-emu`'s `model_profiles.json` is generated from controller 10.4.57's own hardware DB:
**182 models, 106 of them switches**, each with its exact port layout. That file is effectively
the list of model strings a current controller will render.

USW-Leaf is **not** in it. But this is:

| Model string | Display name | Ports |
|---|---|---|
| `UDC48X6` | UniFi Data Center 100G-48X6 | 48x SFP28 + 6x QSFP28 |
| `USWF064` | (unnamed/internal) | 48x SFP28 + 8x QSFP28 |
| `USWF066` | (unnamed/internal) | 48x SFP28 + 6x QSFP28 |
| `USWF003` | (unnamed/internal) | 32x SFP28 |
| `USWF07D` | (unnamed/internal) | 32x QSFP28 |
| `USAGGPRO` | UniFi Switch Pro Aggregation | 28x SFP+ + 4x SFP28 |
| `US648P` | UniFi Switch Enterprise 48 PoE | 48x GE + 6x SFP+ |
| `USPM48P` | UniFi Switch Pro Max 48 PoE | 48x GE + 4x SFP+ |

`UDC48X6` is the shipping model with **exactly the USW-Leaf's port layout** (48x SFP28 +
6x QSFP28) — it is what the Leaf became. If the Arista is a 48x25G box, that is the model
string to claim, and the "looks like the Leaf would have" goal is met literally.

Picking the final string needs the actual Arista model. The rule is: match port count and
media type, because the controller renders from *its* profile, not from what we send.

## 6. What to reuse, concretely

- **Vendor `unifi-emu`'s `inform` and `discovery` packages** (MIT). `inform/packet.go`,
  `inform/crypto.go` and `inform/session.go` are the wire format, both crypto modes and the
  adoption state machine, already correct on the GCM-nonce and mgmt_cfg-gating traps.
  `Descriptor` + `Port` are already the right shape to fill from eAPI.
- **The seam is `inform/tables.go:19` `portTable()`** — currently synthetic constants
  (`up: true, speed: 1000, rx_bytes: 0`). Replacing that function's body with live eAPI data
  is, mechanically, most of phase 0.
- **`model_profiles.json`** as the model catalogue, and `capability_bits.json` for the bitmaps.
- **From `amd989`: the collector abstraction.** `collectors/base.py` + per-platform subclasses
  is exactly the pluggable-vendor shape the repo name implies — one `SwitchCollector` interface,
  an `AristaCollector` behind it.
- **From `amd989`: `_record_unhandled()`.** It logs every unrecognised `_type` and every
  unknown `setparam` key to disk. Given that the main risk is a modern controller doing
  something none of these projects saw, build this in from commit one.
- **From `wvengen`: the port field list** and the `v<numeric>` version quirk.

## 7. Suggested first milestone

Inform loop only: hard-coded `Descriptor` for a plausible model, default key, `state=1`,
POST every 10s, log every reply. Success = the switch appears under Pending Adoption on
Clint's controller. No Arista data, no discovery, no port table. Everything else is
downstream of learning whether his controller version accepts the handshake at all.
