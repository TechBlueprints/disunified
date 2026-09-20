# Arista side: EOS 4.26.14M on a DCS-7160-48TC6-F

Captured and verified against Clint's switch over SSH on 2026-09-19. Unlike the other two
docs, **everything here was run against the real device.** Raw command output lives in
`fixtures/eos-4.26.14M/`, scrubbed by `scripts/sanitize-fixtures.py` (MACs, IPs, serials,
hostnames and descriptions rewritten; structure and types untouched).

## 1. The version is pinned for good

- EOS **4.26.14M**, i686, hardware revision 11.00. Confirmed live via `show version`.
- **4.26 is the last train that supports the 7160 series.** Arista's end-of-support notice
  states that EOS 4.27 and later will not support the DCS-7160 models
  (<https://www.arista.com/en/support/advisories-notices/end-of-support/17443-end-of-software-support-for-7160-series>).
  So the output schemas in the fixtures are the schemas this switch will have forever. Write
  the collector against the fixtures, not against current Arista docs.
- No PoE: `show poe` is `% Invalid input`. Drop it from the poll list; report `port_poe: false`.
- Two commands from the plan were renamed on this train: `show environment temperature` and
  `show environment cooling` are deprecated in favour of `show system environment temperature`
  / `show system environment cooling` (the old names error out).

## 2. Transport options, as found on the box

`show management api http-commands` on 2026-09-19:

```
Enabled:            No
HTTPS server:       enabled, set to use port 443
HTTP server:        shutdown, set to use port 80
Local HTTP server:  shutdown, no authentication, set to use port 8080
Unix Socket server: shutdown, no authentication
```

So eAPI is **configured for HTTPS on 443 but the service itself is shut down** — which is why
`curl https://192.0.2.3/command-api` gets no response. Enabling it is one config block, run in
enable mode (the `admin` SSH user lands at privilege 1, so Claude cannot do this):

```
configure
management api http-commands
   no shutdown
end
write memory
```

Once it is up, `https://<switch>/explorer.html` is the **Command API Explorer**: it documents
the exact JSON schema of every show command *for the running EOS version*, which is the
authoritative reference for 4.26.14M — better than any external doc.

**SSH is a working transport today, with no switch changes.** `ssh admin@arista` authenticates
by key (privilege 1 is enough for every `show` command), and `show <cmd> | json` emits the
same JSON the eAPI would return — the fixtures were captured this way. Multiple commands can
be sent in one session separated by newlines. Go's `golang.org/x/crypto/ssh` covers it.
Recommended: make the collector's transport an interface with an **SSH implementation first**
(works now, needs nothing) and an **eAPI implementation second** (cleaner, needed for
on-box unix-socket deployment later). Both speak the same command list and parse the same JSON.

For the eventual on-switch deployment, eAPI over the unix socket
(`protocol unix-socket`, `/var/run/command-api.sock`, no authentication) is the on-box path
(<https://arista.my.site.com/AristaCommunity/s/article/arista-eapi-101>).

## 3. Prior art for the API

| Project | Relevance | Verdict |
|---|---|---|
| [aristanetworks/goeapi](https://github.com/aristanetworks/goeapi) — official Go client, BSD-3 | JSON-RPC transport, INI profiles, typed `module/` structs for `show version`, `show interfaces switchport`, system | **Stale**: last release v1.0.0 (2022-07-26), "Go 1.5+", best-effort support. The typed modules cover almost none of what we need. Use it, at most, as a reference for the JSON-RPC envelope; a hand-written client is ~60 lines. |
| [enix/arista-eapi-exporter](https://github.com/enix/arista-eapi-exporter) — Prometheus exporter, Python, MIT | Polls `show version`, `show interfaces`, `show hardware capacity` etc. via eAPI, YAML-driven command list, supports HTTPS-insecure and unix socket | Good operational reference for a polling loop; reads `memFree`/`memTotal`/`uptime` from `show version` and `bandwidth`/`description` from `show interfaces` — same fields we map. |
| [arista-eosplus/pyeapi](https://github.com/arista-eosplus/pyeapi) — official Python client | `api/interfaces.py` etc. parse the same JSON; useful to see which keys Arista themselves consider stable | Reference only (bridge is Go). |
| [ansible-collections/arista.eos](https://github.com/ansible-collections/arista.eos) `eos_eapi` module | Shows the exact config lines to enable eAPI/unix-socket and the `show management api http-commands` state model | Reference for §2. |
| [eos-eapi](https://docs.rs/eos-eapi/latest/eos_eapi/) Rust crate | Minimal JSON-RPC client; confirms how small a client needs to be | Reference only. |

eAPI itself is JSON-RPC 2.0, method `runCmds`, params `{version: 1, cmds: [...], format: "json"}`,
POST to `/command-api` with HTTP basic auth. This envelope has not changed since EOS 4.12, so
version-specific concerns are entirely in the per-command output, which is what the fixtures pin.

## 4. Command → UniFi field mapping (from the fixtures)

Per poll, **four commands** cover everything phase 0 reports:

| Command | Fixture | Feeds |
|---|---|---|
| `show interfaces` | `show-interfaces.json` (131 KB) | the whole `port_table` |
| `show lldp neighbors` | `show-lldp-neighbors.json` | `lldp_table` |
| `show version` | `show-version.json` | identity, `uptime`, `mem_total`/`mem_free` |
| `show processes top once` | `show-processes-top-once.json` | `sys_stats.cpu` (`cpuInfo.%Cpu(s).idle`), `memInfo.physicalMem` |

`show interfaces status` is a lighter alternative for link state only (`interfaceStatuses`),
and is the one place `interfaceType` (media) is reported — poll it once at startup for the
media map, not every cycle.

### `port_table` from `show interfaces` → `interfaces["EthernetN"]`

| UniFi field | EOS source | Notes |
|---|---|---|
| `port_idx` | parsed from `name` | `Ethernet1`..`Ethernet48` → 1..48; `Ethernet49/1`..`Ethernet54/1` → 49..54 |
| `ifname`, `name` | `name`, `description` | description is empty on all ports today |
| `up` | `interfaceStatus == "connected"` | enum seen: `connected`, `notconnect`; `disabled`/`errdisabled` exist on EOS but not in the capture |
| `enable` | `interfaceStatus != "disabled"` | |
| `speed` (Mbps) | `bandwidth / 1_000_000` | 100G ports report `100000000000`; down ports report `0` in `show interfaces status` but the configured value in `show interfaces` |
| `full_duplex` | `duplex == "duplexFull"` | |
| `mtu` | `mtu` | 9214 on the QSFP ports |
| `media` | `interfaceType` from `show interfaces status` | seen: `10GBASE-T` (×48), `100GBASE-CR4`, `100GBASE-CWDM4`, `Not Present`, `N/A` (breakout lanes) |
| `rx_bytes`, `tx_bytes` | `interfaceCounters.inOctets`, `.outOctets` | |
| `rx_packets`, `tx_packets` | `inUcastPkts+inMulticastPkts+inBroadcastPkts` (or `inTotalPkts`), same for out | |
| `rx_errors`, `tx_errors` | `interfaceCounters.totalInErrors`, `.totalOutErrors` | `inputErrorsDetail`/`outputErrorsDetail` break them down |
| `rx_dropped`, `tx_dropped` | `interfaceCounters.inDiscards`, `.outDiscards` | |
| `stp_state` | `show spanning-tree` → `spanningTreeInstances.MST0.interfaces[name].state` | `forwarding` etc.; only ports participating in STP appear (3 of 62) — default the rest to `disabled` |
| `port_poe`, `poe_*` | — | always false / absent, no PoE hardware |
| `is_uplink` | LLDP neighbour that is a UniFi device on `Ethernet49/1` | or simply the port facing the upstream aggregation switch |

### Breakout ports need a rule

`Ethernet50` is configured as 4×25G breakout, so `show interfaces` lists `Ethernet50/1..50/4`,
while every other QSFP slot is a single `EthernetNN/1`. `Ethernet52/1` reports
`interfaceType: "Not Present"` (no optic). The controller-side profile (UDC48X6) has one
QSFP28 port per slot, so the collector must **fold lanes into their parent slot**: port 50 is
"up" if any lane is up, counters summed. Also present and to be ignored: `Management1`,
`Port-Channel1..4` (defined, no members), `Vlan*`.

### `lldp_table` from `show lldp neighbors` → `lldpNeighbors[]`

`{port, neighborDevice, neighborPort, ttl}` per entry; `show lldp neighbors detail` adds the
chassis ID (MAC), management address, system description and capabilities for the UniFi
`lldp_table` `chassis_id`/`port_id` fields.

### `sys_stats`

- `cpu`: `100 - cpuInfo["%Cpu(s)"].idle` from `show processes top once`
- `mem_total`, `mem_used`, `mem_buffer`: `memInfo.physicalMem` (kB) from the same command, or
  `memTotal`/`memFree` from `show version`
- `uptime`: `show version` → `uptime` (seconds, float)
- optional temperature: `show system environment temperature` (`cardSlots[].tempSensors[]`)
- optics: `show interfaces transceiver` gives `rxPower`/`txPower`/`temperature` per port —
  UniFi's SFP detail view reads `sfp_*` fields; worth wiring in phase 0.5

### Health signals (anomaly reporting), all verified on 4.26.14M

- `show interfaces` → `interfaceCounters.linkStatusChanges` (link flaps).
- `show spanning-tree` → `interfaces[].inconsistentFeatures` (guards);
  `show spanning-tree topology status detail` → `topologies.Cist.interfaces[].numChanges`.
- `show interfaces transceiver dom thresholds` → per-parameter `channels` +
  `threshold{lowAlarm,highAlarm}` for optics with DOM (none for DACs/empty cages).
- FEC codeword counters exist **only in text**: `show interfaces EthernetX phy detail`
  prints `FEC corrected codewords N …` / `FEC uncorrected codewords N …` (first
  number is the counter), and for every port — 10GBASE-T included — `PCS err blocks N`
  (per PCS section; summed) and `PCS high BER ok|…`. Copper ports show
  `Forward Error Correction None` (no LDPC counters exposed). `show interfaces error-correction` is config/status only;
  `... error-correction detail`, `... fec` are invalid on 4.26. The eAPI transport
  runs text via `format: "text"` (`TextRunner`); one command per FEC lane.

## 5. Identity for the UniFi descriptor

From `show version`: `systemMacAddress` (the bridge should present the Arista's real MAC so
the controller's client list and LLDP data line up), `serialNumber`, `modelName`, `version`.
The firmware string the controller sees must be `v<numeric>` — `4.26.14M` → `v4.26.14`.

## 6. Not on this switch / not needed

- gNMI is disabled (`show management api gnmi` → `enabled: false`). Not needed for phase 0.
- No PoE. No `show poe`.
- SNMP not checked; not needed.
