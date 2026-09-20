# The UniFi device protocol as this bridge speaks it

Everything here was verified against UniFi Network 10.6.106 on a UniFi OS
gateway, 2026-09-19/20. The wire format and crypto come from unifi-emu's
`inform` package ([`prior-art.md`](prior-art.md)); the rest was learned by
watching real switches (their decrypted `last.inform` in the UniFi OS
support bundle) and this controller's replies (`inform-log/`). The code is
[`internal/device`](../internal/device) (session, payload, capability
claims, state) and [`internal/informloop`](../internal/informloop) (the
cycle). The per-feature key map is [`feature-map.md`](feature-map.md).

## 1. The wire packet ("TNBU")

Same packet both directions. The device POSTs it as the HTTP body to
`http://<controller>:8080/inform` (`Content-Type: application/x-binary`,
`User-Agent: AirControl Agent v1.0`).

| Off | Size | Field |
|---:|---:|---|
| 0 | 4 | magic `TNBU` |
| 4 | 4 | packet version, big-endian uint32, always 1 |
| 8 | 6 | device MAC, raw bytes |
| 14 | 2 | flags, big-endian uint16 |
| 16 | 16 | IV (also the GCM nonce) |
| 32 | 4 | payload version, big-endian uint32, always 1 |
| 36 | 4 | body length |
| 40 | n | body |

Flags: `0x01` encrypted, `0x02` zlib, `0x04` snappy, `0x08` AES-GCM.

- Key = plain hex decode of the 32-character authkey, no KDF. Before
  adoption it is `MD5("ubnt")` = `ba86f2bbe107c7c57eb5f2690775c712`.
- CBC: PKCS#7 padding, IV from the header, fresh per packet.
- **GCM uses a 16-byte nonce**, not 12 (Go: `cipher.NewGCMWithNonceSize(block, 16)`),
  and **the AAD is the entire 40-byte header** including the body length;
  the tag is appended to the ciphertext.
- A device starts on CBC; the controller flips it with
  `mgmt_cfg.use_aes_gcm=true` and there is no way back, so both are
  implemented. zlib (RFC 1950) is applied to the JSON before encryption.

## 2. The switch payload

A flat JSON object, sent on every inform, pending or adopted. What this
bridge sends is what real switches send, checked key by key against their
informs by [`internal/device/contract_test.go`](../internal/device/contract_test.go);
the last payload sent is `inform-log/<name>/payload-last.json`.

- **Identity and protocol state**: `mac`, `serial`, `model`,
  `model_display`, `version` (must be `v<numeric>`), `ip`, `hostname`,
  `inform_url` (an **IP literal**, never a name: the controller answers
  HTTP 400 `invalid inform_ip` otherwise), `uptime`, `time`, `cfgversion`,
  `x_authkey`, `default`, `_default_key`, `state` (4 once adopted),
  `fw_caps`, `guid`, `bootrom_version`, `isolated`, `locating`,
  `selfrun_beacon`, `inform_min_interval`, `stats_inform_interval`.
- **Reachability**, which the controller uses to place the device:
  `connect_request_ip`/`_port`, `gateway_ip`, `gateway_mac`,
  `ethernet_table` (`eth0` and `srv0`), `if_table` (the management
  interface: ip, netmask, counters), `has_eth1`, `service_mac`. The
  management address must be **in-band**, behind the uplink, or the
  controller finds the IP and the LLDP identity in two places and leaves
  the device without a parent ([`adding-a-switch.md`](adding-a-switch.md) §2c).
- **`uplink` is a string**: the name of the management interface in
  `if_table` (`"eth0"`), not an object. With an object the controller
  silently ignored it for a day. It composes the uplink record itself from
  `port_table[].is_uplink` and `lldp_table`.
- **Tables**: `port_table` (per port: state, speed, counters, media,
  optics `sfp_*`, `stp_*`, `fec_mode`, `anomalies`, `satisfaction`,
  `mac_table`, config echoes), `lldp_table`, `mac_table`, `fan_table`,
  `psu_table`, `ssh_session_table`, `sys_stats`, `system-stats`,
  `stp_version`/`stp_priority`/`root_switch`,
  `total_max_power`, `general_temperature`, `overheating`,
  `total_mac_in_used`, `stp_topology_change_count`. The exact meaning and
  source of each is in [`feature-map.md`](feature-map.md) §1 and §2; the
  `fan_table` and `psu_table` rows must use the real key names or the UI
  shows the hardware as not installed.

### Capability claims: only what the switch can do

The controller gates the UI on self-reported bitmaps and trusts them:
`fw_caps`, `switch_caps` (feature, STP, storm control, IGMP snooping and
other sub-bitmaps), `hw_caps`, and per-port `speed_caps`. **They are stored
only when the inform carries `udapi_version`** (the bridge sends `1.0.0`);
without it the controller keeps the profile's defaults and hides storm
control, STP options, FEC and the rest. The UI validates every speed
request against `speed_caps`, so the claims must come from the switch's
own hardware table. The device's per-port `media` and `speed_caps` replace
the profile's icons and speed pickers; port count, display name, PoE and
default port names come from the profile ([`unifi-models.md`](unifi-models.md)).
`fw_caps` UTERM is deliberately not claimed: the UI's Debug terminal is a
WebRTC session the controller asks the device to build (`build-ssh-session`
command), which this bridge does not implement.

## 3. Adoption (layer 3, no SSH server)

1. The bridge informs with the default key, `state=1`, `default=true`.
   **HTTP 404 is the normal reply while pending**; keep informing. After a
   device is forgotten in the controller it answers HTTP 400 with an empty
   body for about a minute, then 404 again.
2. The operator clicks Adopt (or `cmd/devmgr adopt` on the console).
3. The controller delivers the authkey in a `setparam` reply with
   `mgmt_cfg`, a **newline-separated `key=value` string**, not JSON:
   `authkey=`, `cfgversion=`, `use_aes_gcm=`, `interval=` (the inform
   cadence the controller wants, 65-80 s here). A `cmd: set-adopt` form
   exists too and is handled. **`mgmt_cfg.authkey` is applied only while
   the bridge still holds the default key**; applying it unconditionally
   is the classic stuck-adopt loop.
4. The first `system_cfg` push follows and the device is CONNECTED, about
   25 s after the click. The naming provisioner runs then
   (`OnConnected`), not only at startup.

The adopted key, `cfgversion`, GCM flag, inform URL and the last applied
config live in `state/<name>/device.json`: one bridge per adopted key, keep
the file. Discovery (UDP 10001 broadcasts) is not needed for adoption and
is not implemented. Layer 2 adoption (the controller SSHing in as
`ubnt/ubnt`) is not needed either.

The recorded handshake (the two 400s, adoption, first config) is the head
of [`fixtures/controller-10.6.106/replies.ndjson`](fixtures/controller-10.6.106/replies.ndjson),
replayed by [`internal/informloop/replay_test.go`](../internal/informloop/replay_test.go).

## 4. The control channel

- **`setparam` with `system_cfg`** is the channel: the UniFi device
  configuration file, one `key=value` per line (`switch.port.N.*`,
  `switch.vlan.*`, `switch.stp.*`, `ntpclient.*`, `syslog.*`, `snmp.*`,
  `sshd.auth.key.*`, and more), parsed by
  [`internal/unificfg`](../internal/unificfg). The controller only sends a
  key when the feature is non-default in the site. The device must report
  the pushed `cfgversion` **only after applying** it; until then the
  controller keeps re-sending, which is what lets the loop hold a freshly
  adopted device's first push while it would change ports.
- `setstate` with `port_overrides`/`port_table` exists in prior art and is
  stored and echoed if it arrives, but has **not been observed** on 10.6.
  A `rest/device` PUT of `port_overrides` does not provision on its own;
  the UI's controls do.
- **`cmd`**: `set-adopt`, `setdefault` (factory reset: back to the default
  key, `cfgversion` 0, CBC), `reboot`, `upgrade`/`upgrade2` (emulated:
  accepted, "rebooted", the requested version reported and persisted from
  then on; the controller offers no upgrades for `UDC48X6`, which has no
  firmware releases), `set-locate`/`unset-locate` (those are the 10.6
  names), `power-cycle` with `port_idx` (issued only for a PoE port that is
  powering a device), `build-ssh-session` (the WebRTC terminal, logged as
  UNHANDLED). `cmd/devmgr` on the console answers `rc:ok` to any command
  name, so an ok there proves nothing; only a recorded reply does.

## 5. Which model to claim

The controller renders from **its** profile for the claimed model string,
so the string must be in its database and its port count must match the
switch. The catalogue, the ranking rule and the reasons for `UDC48X6` are
in [`unifi-models.md`](unifi-models.md).
