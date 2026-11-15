# How EOS shows what the driver writes (EOS 4.26.14M, 2026-11-15)

Captured on the DCS-7160-48TC6-F on the two test ports (the unused copper
Ethernet2, no link, and the empty cage Ethernet54) so the fixture transport
(`internal/drivers/arista-eos/fixture_state.go`) shows a write in the next
read only in forms the switch itself printed. Every change was undone in the
same SSH session; the running config before and after was identical, and
`show running-config diffs` (running against startup) was empty after.

Written in plain `configure`, then read with `| json`:

    interface Ethernet2
       description dui-fixture-test
       switchport trunk native vlan 2
       switchport trunk allowed vlan 1-2,8-10,1000
       spanning-tree portfast
       spanning-tree bpdufilter enable
       speed forced 1000full

- `show-interfaces-Ethernet2-described.json` -- `show interfaces Ethernet2`:
  `description` holds the text as written (`dui-fixture-test`; the scrub
  replaces every description with `scrubbed`).
- `show-interfaces-Ethernet2-switchport-trunk.json` -- `trunkingNativeVlanId`
  2 (`trunkingNativeVlanName` the VLAN's name) and `trunkAllowedVlans`
  `1-2,8-10,1000`: the list as written, ranges joined by commas.
- `show-interfaces-Ethernet2-transceiver-properties-1g.json` -- `adminSpeed`
  `1Gbps`/`full`.

Then `switchport mode access` and `switchport access vlan 2`:

- `show-interfaces-Ethernet2-switchport-access.json` -- `mode` `access`,
  `accessVlanId` 2; the trunk's native VLAN and allowed list are kept.

Then, on the empty cage, `error-correction encoding fire-code`, and in the
same running config as the lines above, `no error-correction encoding`; an
unbound list with the rule shapes the controller's IP rules render to; and an
NTP server by name:

    ip access-list DUI_FIXTURE_PROBE
       10 deny tcp 203.0.113.0/24 198.51.100.0/24 eq 9
       20 deny udp 203.0.113.0/24 198.51.100.0/24 eq 9
       30 deny tcp 203.0.113.0/24 198.51.100.0/24 eq 1001
       40 permit tcp any host 192.0.2.1 eq 443
       50 permit tcp any host 192.0.2.1 eq 22
       60 deny tcp 203.0.113.0/24 eq 9 192.0.2.0/24
       70 deny udp 198.51.100.0/24 host 192.0.2.1 eq 68
       80 permit ip 192.0.2.0/24 198.51.100.0/24
       90 deny ip any 198.51.100.0/24
    ntp server 0.ubnt.pool.ntp.org

- `show-running-config-written.json` -- an excerpt of `show running-config`
  (the two test ports' sections, the probe list, the NTP and syslog lines):
  `spanning-tree portfast`, `spanning-tree bpdufilter enable`, `speed forced
  1000full`, `no error-correction encoding` as lines of their own; EOS
  names ports it knows (9 `discard` for TCP and UDP, 22 `ssh`, 443 `https`,
  68 `bootpc`) and keeps 1001 as a number; `ntp server` keeps the name.
- `show-running-config-fire-code.json` -- the cage's section with
  `error-correction encoding fire-code` (a second run).

`show running-config interfaces X | json` answers "This is an unconverted
command" on 4.26.14M: only the whole running config has JSON. The probe
list's addresses are documentation ranges and were put back after the scrub
(`scripts/sanitize-arista-eos.py` maps every IPv4 address); the NTP and
syslog servers are scrubbed.
