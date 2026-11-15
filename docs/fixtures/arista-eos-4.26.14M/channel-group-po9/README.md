# An aggregate's join and leave on one port (EOS 4.26.14M, 2026-11-15)

Captured on the DCS-7160-48TC6-F on the unused copper test port Ethernet2
(no link; `speed auto` and `switchport mode trunk` its only lines) inside a
hold session (`configure session ... commit timer`, aborted after; running
and startup identical after). Each step was written in plain `configure`
and read back at once. One sanitizer run; the `switchport` reply is cut to
its Ethernet2 and Port-Channel9 entries and the status table to its header
and the Port-Channel's line. `running-config-sections.txt` is `show
running-config` cut to the two interfaces' sections.

1. `interface Ethernet2` / `channel-group 9 mode active` (`1-channel-group/`):
   EOS creates `interface Port-Channel9` with `switchport mode trunk`, the
   member's own mode, and its `show interfaces switchport` entry is the
   member's. `show port-channel summary` lists it (`lacp`, `active`, link
   `down`) with Ethernet2 in `ports` (`lagMember` false, `linkDown` true).
   The Port-Channel's `physicalAddress` is the member's (`show interfaces
   Port-Channel9`; in `empty-po-lag5/` and `split-cage54-flexxg/` too, the
   first member's).
2. `interface Ethernet2` / `no channel-group` (`2-no-channel-group/`):
   Port-Channel9 stays, `switchport mode trunk` and all, in the running
   config, `show interfaces` (`notconnect`, `physicalAddress` all zeros)
   and `show interfaces status`; `show port-channel summary` no longer
   lists it (`portChannels` empty).
3. `channel-group 9 mode active` again, then `no interface Port-Channel9`
   (`3-no-interface-with-member/`): the interface goes and so does the
   member's `channel-group` line; the summary is empty.

The two-member forms (both members, every read the collector makes) are
`empty-po-none/` -> `empty-po-lag5/` (the driver's build batch on Ethernet2
and the empty cage's Ethernet54/1) -> `empty-po-lag5-removed-fixed/` (the
members out, then `no interface Port-Channel5`), and `split-cage54/` ->
`split-cage54-flexxg/` (three lanes of the split cage into
Port-Channel1541). A copper member's summary entry has no `lacpState`; a
cage lane's has one (`defaulted`, no partner) in both captures. Used by
`internal/drivers/arista-eos/fixture_state.go` (docs/drivers/arista-eos.md
§11).
