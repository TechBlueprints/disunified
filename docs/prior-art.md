# Prior art: making a non-Ubiquiti switch appear in the UniFi controller

Research date: 2026-09-19. Everything below is from public sources; nothing here has been
run or verified against Clint's controller yet.

## 1. How a UniFi device gets into the controller

There is no supported "add a third-party device" path. Ubiquiti's official UniFi API
(released 2024/2025, <https://help.ui.com/hc/en-us/articles/30076656117655-Getting-Started-with-the-Official-UniFi-API>)
reads and controls devices the controller already owns; it cannot create one. The only way in
is to speak the device side of the **inform protocol**, as unifi-emu implements and documents
it (<https://github.com/jamesbraid/unifi-emu>, `docs/PROTOCOL.md` there):

- **Discovery.** An unadopted device broadcasts TLV packets on UDP/10001 to
  `255.255.255.255` and multicast `233.89.188.1`. TLV fields carry MAC (0x01), IP (0x02),
  firmware version (0x03), hostname (0x0B, literally `UBNT` when unadopted), platform (0x0C)
  and model (0x14). This is what makes the device show up in "Pending Adoption".
- **Inform.** The device HTTP POSTs to `http://<controller>:8080/inform` with a binary
  envelope: magic `TNBU`, 4-byte version, 6-byte MAC, 2-byte flags
  (0x01 encrypted, 0x02 zlib, 0x04 snappy, 0x08 AES-GCM), 16-byte IV, data version, payload
  length, then the payload.
- **Crypto.** Payload is unpadded AES-128-CBC, or AES-GCM on newer firmware. The key is the
  32-hex-char authkey; before adoption it is the MD5 of `ubnt`. After adoption the controller's
  key is stored device-side (`mgmt.authkey` in `/cfg/mgmt` on real hardware).
- **Adoption.** Layer 2: the controller SSHes in as `ubnt/ubnt` and runs
  `/usr/bin/syswrapper.sh set-adopt <inform-url> <16-byte-hex-key>`. Layer 3: you set the
  inform URL yourself (`set-inform`), the device checks in as unadopted, and the controller
  hands over the key in an inform reply. **For a bridge, the layer 3 path is the one to
  implement** — no SSH server to fake.
- The payload itself is JSON describing device state. Controller replies carry `setparam` /
  `mgmt` config back to the device, which is the hook Phase 1 (control) would use.

## 2. Projects that already do this

| Project | What it fakes | Lang | License | State |
|---|---|---|---|---|
| [jamesbraid/unifi-emu](https://github.com/jamesbraid/unifi-emu) | APs, **switches**, gateways — current lineup | Go | MIT | **Active**, v0.5.5 published 2026-09-02, 188 commits, 1 star |
| [wvengen/unifi-controllable-switch](https://github.com/wvengen/unifi-controllable-switch) | TOUGHswitch presenting as *UniFi Switch 8 POE-150W* | C/shell/Python | — | Archived Jan 2019, 26 stars |
| [amd989/unifi-gateway](https://github.com/amd989/unifi-gateway) | UGW3 router | Python 3 | MIT | Maintained-ish, 25 stars, 32 commits |
| [qvr/unifi-gateway](https://github.com/qvr/unifi-gateway) | UGW router | Python | — | Archived 2025-05-10, 27 stars |
| [stephanlascar/unifi-gateway](https://github.com/stephanlascar/unifi-gateway) | UGW router (the original) | Python | — | Archived Jan 2019, 72 stars, README says "NOT WORKING" |

### The two that matter

**jamesbraid/unifi-emu** is the closest thing to a ready-made foundation, and the only one
that covers switches. Its `inform` package
(<https://pkg.go.dev/github.com/jamesbraid/unifi-emu/inform>) exports exactly the pieces a
bridge needs:

- `Packet.Encode()` (CBC + zlib) and `Packet.EncodeGCM()` (for newer controllers), `Decode()`
- `ParseKey()` for the 32-hex authkey, with the MD5-of-`ubnt` default for unadopted devices
- `Descriptor` — device identity: MAC, serial, model, IP, hostname, firmware caps, **ports**
- `Port` — a switch port: ifname, port index, media (GE/SFP+), PoE capability
- `Session` — the device-side state machine: `BuildPayload()`, `EncodeInform()`, `Apply()`
  for controller replies, and a Pending → Adopting → Connected lifecycle
- `Models()` / `Profile()` for per-model specs, plus a CLI and a container image

Its stated design rule is the useful one for us: "a device enters a controller **only**
through the real inform and adoption lifecycle — never by seeding the database." It was
written for controller integration testing, not as a bridge, so the device state is
synthetic — but that is precisely the seam where Arista data gets plugged in.

Caveat: 1 star, one author, no external validation. Treat it as a high-quality reference
implementation to read and vendor from, not as a dependency to trust blindly.

**wvengen/unifi-controllable-switch** is the only prior art for a *switch* specifically, and
its `src/unifi-inform-status` script
(<https://github.com/wvengen/unifi-controllable-switch/blob/master/src/unifi-inform-status>)
is the concrete answer to "what JSON does a switch send?". Top level: `model`, `cfgversion`,
`hostname`, `version`, `uptime`, `time`, `mac`, `ip`, `serial`, `sys_stats`, `if_table`,
`port_table`. Each `port_table` entry:

```
port_idx, port_poe, enable, up, speed, full_duplex,
poe_enable, poe_mode, poe_voltage,
mtu, stp_state,
tx_packets, rx_packets, tx_bytes, rx_bytes, tx_errors, rx_errors,
name
```

One detail worth copying: it strips the firmware version down to the numeric part and
prefixes `v`, because the controller rejects anything else. Archived in 2019, so the schema
is old — but it is the ground truth for which keys the switch view reads.

## 3. What this says about the UniFi Leaf angle

USW-Leaf (<https://ubntwiki.com/products/unifi/unifi_switch_leaf>) was 48x SFP28 10/25G plus
6x QSFP28 40/100G, 3.6 Tbps, dual 350W PSUs, 1U, managed by the UniFi Network Controller.
It never left Early Access and was cancelled — the community wiki page still carries the
"NOT for production use" banner.

That matters for us in one direction only: **the controller renders a device using its own
built-in profile for the model string you claim.** A model the controller doesn't know either
won't render or will render wrong. So the bridge should claim a *shipping* USW model whose
port count and media types are the closest match to Clint's Arista, not `USW-Leaf`. A 48-port
SFP Arista most plausibly maps to a USW Aggregation/Pro-Aggregation class model; this needs
to be chosen against the actual hardware and confirmed empirically against the controller.

## 4. The Arista side

**Data collection** is the easy half — EOS is the most automatable NOS available:

- **eAPI**: HTTPS JSON-RPC. `show interfaces status`, `show interfaces counters`,
  `show lldp neighbors`, `show version`, `show poe` (on PoE models) all return structured
  JSON that maps almost field-for-field onto `port_table`. Client library:
  [pyeapi](https://github.com/arista-eosplus/pyeapi).
- **gNMI / OpenConfig**: streaming telemetry, better for a long-running bridge that wants
  push updates rather than polling. See
  <https://labguides.testdrive.arista.com/2025.1/automation/gnmi/> and
  [arista-netdevops-community/arista_eos_streaming_telemetry_with_gnmi_and_telegraf](https://github.com/arista-netdevops-community/arista_eos_streaming_telemetry_with_gnmi_and_telegraf).

For Phase 0, eAPI polling on the inform interval (a real device informs roughly every 10s
when connected) is simpler and sufficient.

**Running on the switch itself** is possible but heavier than expected:

- EOS supports Docker containers via the `container-manager` extension
  (<https://arista.my.site.com/AristaCommunity/s/article/managing-containers-on-eos-container-manager>).
  A worked example on a 7280 running EOS 4.32.1F is at
  <https://vegvisir.ie/2024/07/08/deploying-traffic-dictator-in-docker-on-an-arista-switch/>.
  Images load from flash; config under `container-manager` survives EOS upgrades, unlike raw
  `docker run`.
- That example allocated 4-8 GB of memory to the container. A Go inform bridge needs a tiny
  fraction of that, but the platform still has to have the headroom and the docker extension.
- **The networking gotcha**: containers live in the Linux network namespace, and EOS's own
  routing/forwarding is separate. The blog hit exactly this with BGP — ping worked, the
  session didn't. For us this means the container's traffic to the controller, the UDP/10001
  discovery broadcast in particular, needs deliberate plumbing (the right interface/VRF), and
  broadcast from inside a container namespace is the risky part.
- The alternative on-switch path is an **EOS SDK extension** (`.swix`), which runs natively in
  EOS: <https://www.arista.com/en/products/eos/open-and-programmable>.

## 5. Recommended approach for Phase 0

1. **Run it on the Podman host, not the switch.** It removes the container-namespace
   broadcast problem entirely, keeps iteration fast, and the switch stays untouched. Moving it
   onto EOS later is a deployment change, not a rewrite. Design for both: the bridge talks to
   the Arista over eAPI either way.
2. **Build on jamesbraid/unifi-emu's `inform` package.** It already has the wire format, both
   crypto modes, the adoption state machine and a switch `Port` type. Replace its synthetic
   device state with a poller that fills `Descriptor` + `Port` from eAPI. Go also fits the
   "run it on EOS later" goal — one static binary.
3. **Use `wvengen`'s `unifi-inform-status` as the field-level spec** for `port_table` /
   `if_table` / `sys_stats`, then verify against a real modern controller, since that schema
   is from 2019.
4. **Adopt over layer 3** (`set-inform` equivalent: the bridge just starts POSTing to
   `/inform` as unadopted and takes the key from the reply). Avoids faking an SSH server with
   `ubnt/ubnt`.
5. **Claim a shipping USW model** matching the Arista's port count/media, not USW-Leaf.
   Expect this to be the main empirical unknown; plan to try two or three.
6. **Keep the Arista adapter behind an interface** from day one, per the repo's generic name.

### Known risks

- Every project above targets older controller versions. The OPNsense thread on the gateway
  emulator (<https://forum.opnsense.org/index.php?topic=51489.0>) is explicit that modern
  controllers may show no statistics at all for an emulated UGW3, and the author lacks newer
  hardware to update it. Expect the same for switches: the first real task is
  capturing what Clint's actual controller version accepts.
- `unifi-emu` claims adoption "all the way to connected against a real controller" but names
  no controller version. Verify early.
- Nothing here has been tested. The first milestone should be *anything at all* appearing in
  Pending Adoption, before any port data work.

## Sources

- <https://github.com/jamesbraid/unifi-emu> / <https://pkg.go.dev/github.com/jamesbraid/unifi-emu/inform>
- <https://github.com/wvengen/unifi-controllable-switch>
- <https://github.com/amd989/unifi-gateway>, <https://github.com/qvr/unifi-gateway>, <https://github.com/stephanlascar/unifi-gateway>
- <https://forum.opnsense.org/index.php?topic=51489.0>
- <https://ubntwiki.com/products/unifi/unifi_switch_leaf>, <https://dl.ui.com/product_sheets/USW-Leaf_Brief.pdf>
- <https://help.ui.com/hc/en-us/articles/30076656117655-Getting-Started-with-the-Official-UniFi-API>
- <https://arista.my.site.com/AristaCommunity/s/article/managing-containers-on-eos-container-manager>
- <https://vegvisir.ie/2024/07/08/deploying-traffic-dictator-in-docker-on-an-arista-switch/>
- <https://labguides.testdrive.arista.com/2025.1/automation/gnmi/>
- <https://www.arista.com/en/products/eos/open-and-programmable>
