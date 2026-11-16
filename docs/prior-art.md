# Prior art, and what this project took from it

Research done 2026-09-19, before the first line of code; kept because the
choices below still explain the shape of the repo.

## 1. How a UniFi device gets into the controller

There is no supported "add a third-party device" path. Ubiquiti's official
UniFi API (<https://help.ui.com/hc/en-us/articles/30076656117655-Getting-Started-with-the-Official-UniFi-API>)
reads and controls devices the controller already owns; it cannot create
one. The only way in is to speak the device side of the **inform
protocol**, as unifi-emu implements and documents it
(<https://github.com/jamesbraid/unifi-emu>, `docs/PROTOCOL.md` there). The
protocol as this bridge speaks it, verified on Network 10.6.106, is in
[`protocol-connection-points.md`](protocol-connection-points.md).

## 2. Projects that already did this

| Project | What it fakes | Lang | License | State (2026-09-19) |
|---|---|---|---|---|
| [jamesbraid/unifi-emu](https://github.com/jamesbraid/unifi-emu) | APs, **switches**, gateways — current lineup | Go | MIT | active, v0.5.5 (2026-09-02) |
| [wvengen/unifi-controllable-switch](https://github.com/wvengen/unifi-controllable-switch) | TOUGHswitch presenting as *UniFi Switch 8 POE-150W* | C/shell/Python | — | archived Jan 2019 |
| [amd989/unifi-gateway](https://github.com/amd989/unifi-gateway) | UGW3 router | Python 3 | MIT | maintained-ish |
| [qvr/unifi-gateway](https://github.com/qvr/unifi-gateway) | UGW router | Python | — | archived 2025-05 |
| [stephanlascar/unifi-gateway](https://github.com/stephanlascar/unifi-gateway) | UGW router (the original) | Python | — | archived Jan 2019, "NOT WORKING" |

## 3. What was taken from each

**jamesbraid/unifi-emu** (MIT, credited in [`NOTICE`](../NOTICE)) is the
foundation:

- its `inform` package is a dependency: the TNBU packet, CBC and GCM
  crypto, `ParseKey` with the MD5-of-`ubnt` default;
- its model catalogue (`model_profiles.json`, generated from a real
  controller's hardware database) and `capability_bits.json` are used at
  build time by [`internal/unifimodel`](../internal/unifimodel) and the
  capability claims;
- [`internal/device`](../internal/device) is a fork of its inform session
  and payload tables, with the synthetic device state replaced by what the
  drivers collect;
- its design rule holds here too: a device enters a controller **only**
  through the real inform and adoption lifecycle, never by seeding the
  controller's database.

**wvengen/unifi-controllable-switch** gave the first field list for a
switch's `port_table` and the `v<numeric>` firmware-string quirk. Both were
superseded by real captures: the wire contract tests now compare our
payload with real switches' informs from a UniFi OS support bundle
([`adding-a-device.md`](adding-a-device.md)).

**amd989/unifi-gateway** gave two operational habits: a pluggable
collector per platform (our drivers) and logging every reply the
controller sends that the bridge does not understand (`inform-log/`, and
the `UNHANDLED` lines).

## 4. The USW Leaf question, answered

USW-Leaf (<https://ubntwiki.com/products/unifi/unifi_switch_leaf>) was 48x
SFP28 + 6x QSFP28, Early Access only, never released. The controller
renders a device from **its own** profile for the model string claimed, so
the claim has to exist in its database. `UDC48X6` is the shipping model
with exactly the Leaf's layout, and Network 10.6 displays it as "USW Leaf":
that is what the Arista (48x 10GBASE-T + 6x QSFP28) and the Proxmox nodes
(54 virtual ports) claim, with the device's own per-port media replacing
the profile's SFP28 icons. The rule and the whole catalogue are in
[`unifi-models.md`](unifi-models.md).

## 5. The Arista side

Data collection goes through **eAPI** (HTTPS JSON-RPC), with SSH as an
alternative transport; both are in [`drivers/arista-eos.md`](drivers/arista-eos.md).
gNMI/OpenConfig streaming was not needed at the inform cadence. Running the
bridge **on the switch** (EOS supports containers through the
`container-manager` extension, or an EOS SDK `.swix`) was considered and
not pursued: the container's traffic lives in the Linux namespace, apart
from EOS's own forwarding, and a bridge on a separate host is simpler to
run and update. The bridge runs in a container on a host next to the
switch.

## 6. What went back upstream, and where it stands

unifi-emu is the dependency, pinned at v0.5.5, and everything learned here
that it gets wrong or omits was offered back. **As of 2026-10-19 the
maintainer has reviewed none of it** — the repo has not been pushed since
2026-09-14. The only response so far is a third party's (below). The PRs come
from the fork `cgoudie/unifi-emu`; #10–#13 are a stacked series and merge in
order, #15 follows #13, #14 is independent.

| Ref | What | State (2026-10-19) |
|---|---|---|
| [#2](https://github.com/jamesbraid/unifi-emu/issues/2) | FYI plus the 10.6.106 findings: `uplink` as a string, capability claims gated on `udapi_version`, `setparam.system_cfg` as the control channel, the real locate command names, `build-ssh-session`, the support bundle as reference | open, no reply |
| [#3](https://github.com/jamesbraid/unifi-emu/pull/3) | `inform`: handle `set-locate`/`unset-locate`, report `locating` | open, no review |
| [#4](https://github.com/jamesbraid/unifi-emu/pull/4) | `inform`: switches report `uplink` as the management interface name plus `if_table` | open, no review |
| [#5](https://github.com/jamesbraid/unifi-emu/pull/5) | `docs/PROTOCOL.md`: the notes verified on 10.6.106 | open, no review |
| [#6](https://github.com/jamesbraid/unifi-emu/issues/6) | Gateways: PROTOCOL.md and the payload both stop at a stub ([`gateways.md`](gateways.md)) | open; a third party's finding, 2026-10-19 (below) |
| [#7](https://github.com/jamesbraid/unifi-emu/issues/7) | Power devices: outlets are neither documented nor expressible | open, no reply |
| [#8](https://github.com/jamesbraid/unifi-emu/issues/8) | `model_profiles.json`: port media, spatial streams and names disagree with published specs; Ubiquiti's unauthenticated device database (`static.ui.com/fingerprint/ui/public.json`) as a harvest source | open, no reply |
| [#9](https://github.com/jamesbraid/unifi-emu/issues/9) | Power devices: four things an outlet table has to get right, none of which fail loudly (follow-on to #7, from real hardware) | open, no reply |
| [#10](https://github.com/jamesbraid/unifi-emu/pull/10) | `inform`: outlets, power supplies, battery state and a real uplink; first of the series. Its comment says the power half is live-validated and the gateway half is not | open, no review |
| [#11](https://github.com/jamesbraid/unifi-emu/pull/11) | `modelgen`: derive outlets, harvest from the published database, let overrides restate ports (stacked on #10) | open, no review |
| [#12](https://github.com/jamesbraid/unifi-emu/pull/12) | catalogue: outlet layouts, corrections against published specs, a newer controller; answers #8 (stacked on #11) | open, no review |
| [#13](https://github.com/jamesbraid/unifi-emu/pull/13) | `docs/PROTOCOL.md`: the power, UPS and gateway payloads (stacked on #12) | open, no review |
| [#14](https://github.com/jamesbraid/unifi-emu/pull/14) | `emu`: record the inform exchange to a file when `SIM_CAPTURE` is set | open, no review |
| [#15](https://github.com/jamesbraid/unifi-emu/pull/15) | `inform`, catalogue: gateway `network_table`, `usg_caps`, `hw_caps` on every path, `if_table` one row per layer-3 interface; measured against a real UniFi OS gateway's inform (branch `pr-gateway-reference`) | open, no review; a third party built on it (below) |
| [#16](https://github.com/jamesbraid/unifi-emu/issues/16) | A `serial` longer than 12 characters blocks a console's update to Network 11: its PostgreSQL `device` table keeps 12, and one longer record halts the migration (found here 2026-11-12, [disunified#3](https://github.com/TechBlueprints/disunified/issues/3)). unifi-emu's default (the MAC) is safe; `DeviceSpec.Serial` passes any length through | open, filed 2026-11-12 |

**The first outside response (2026-10-19).** A third party (GitHub
`snooptheone`) built #15's branch and adopted an emulated UXGB on a UniFi OS
Server running Network 10.6.106. That is the live gateway adoption that #10
and #15 both said they could not run here. The controller then held the
gateway unprovisioned until the device answered a `wg-genkeypair` command.
Answered, the zone-based firewall worked end to end. The device was still
listed OFFLINE, cause unknown. Their write-up is the
[comment on #6](https://github.com/jamesbraid/unifi-emu/issues/6#issuecomment-6266565972),
with a pointer on [#15](https://github.com/jamesbraid/unifi-emu/pull/15#issuecomment-6266567644).
The wire detail is in [`gateways.md`](gateways.md) ("Since then"). They
offered their patch (the command handler, plus an unrelated authkey override)
as a PR. None of it is in any branch of ours or reproduced here.

What that means for work here:

- **Never wait on an upstream merge.** The corrections live in the fork,
  [`internal/device`](../internal/device); the dependency and the catalogue
  stay pinned. A catalogue refresh means running unifi-emu's `modelgen`
  against Clint's controller, not expecting a release
  ([`unifi-models.md`](unifi-models.md)).
- **Check those fourteen before filing anything new upstream**, and read the
  comment on #7: it corrects two errors in its own issue body.
- Where this repo's notes contradict unifi-emu's docs — `uplink` as a string,
  power devices as a payload shape — **ours are the verified ones**, taken from
  real informs on 10.6.106. PROTOCOL.md still documents the emulator's stub.

### 6.1 SONiC: the IGMP/MLD snooping drafts (posted 2026-11-05)

The `sonic` driver's IGMP snooping runs on an image built from Micas
Networks' draft series (`contrib/sonic-image/`,
[`drivers/sonic.md`](drivers/sonic.md) §8). That build's fixes went back to
the drafts as contributions to their work, not as competing PRs. Clint
approved the posts in the posting session. They were made from his account
(`cgoudie`), each commit signed off by him and with no Claude co-author line,
as on sonic-net/sonic-buildimage#30483 (the libyang set of 2026-11-01,
§8.12).

The daemon's review happens in
[jasper-hou-micas/sonic-l2mcd-micas#1](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/1)
(`l2mcd-pre` → `master`). So the daemon fixes are six independent PRs into
`l2mcd-pre`, each based on `651a13b` (= `dev`). They are cut from series
`sonic-l2mcd` 0001-0010 and merge in any order without conflicts:

| PR | Series | What |
|---|---|---|
| [jasper-hou-micas/sonic-l2mcd-micas#6](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/6) | 0001 | `dumpL2mcVlanEntry`'s fixed-size `memcpy` from `c_str()`: the daemon-side twin of the l2mcmgr.cpp bug aidan-gallagher found on sonic-net/sonic-swss#3950 (credited) |
| [jasper-hou-micas/sonic-l2mcd-micas#5](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/5) | 0002-0006 | the receive-path bounds (IGMPv3/MLDv2 walks, IP lengths, MLD drops, the `i <= src_cnt` read), with the harness's 24 cases before and after and the 4-hour fuzz run |
| [jasper-hou-micas/sonic-l2mcd-micas#2](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/2) | 0007 | one IGMP receive socket per port |
| [jasper-hou-micas/sonic-l2mcd-micas#3](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/3) | 0008, election half | a v3 VLAN elects against an IGMPv2 querier |
| [jasper-hou-micas/sonic-l2mcd-micas#7](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/7) | 0008 address half + 0009 | the VLAN's address as the query source, whatever `ve_ifindex` holds |
| [jasper-hou-micas/sonic-l2mcd-micas#4](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/4) | 0010 (counters on their own lines, so it stands alone) | per-packet lines out of syslog at the default level |
| [jasper-hou-micas/sonic-l2mcd-micas#8](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/8) (posted 2026-11-15) | 0011 | a port a breakout or join re-creates keeps its own index, not another port's (§11.25.3 of [`drivers/sonic.md`](drivers/sonic.md)) |
| [jasper-hou-micas/sonic-l2mcd-micas#9](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/9) (posted 2026-11-15) | 0012 | free a closed port socket's libevent event |
| [jasper-hou-micas/sonic-l2mcd-micas#10](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/10) (posted 2026-11-15) | 0013 | `mcgrp_stop_phy_port()` no longer reads and clears a membership it has just freed (the review of #1 flagged it) |
| [jasper-hou-micas/sonic-l2mcd-micas#11](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/11) (posted 2026-11-16) | 0014 | a report, leave or MLD done from a port the port db has no name for is dropped and counted, instead of aborting l2mcd on `std::logic_error` (§8.13 of [`drivers/sonic.md`](drivers/sonic.md), from l2mcd's core of 2026-11-15) |
| [jasper-hou-micas/sonic-l2mcd-micas#12](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/12) (posted 2026-11-16) | 0015 | a static group's refresh timer keeps running when one of its ports stops, so the group's other static members no longer lapse; the refresh renews only a port that is up (§8.17 of [`drivers/sonic.md`](drivers/sonic.md)) |
| [jasper-hou-micas/sonic-l2mcd-micas#13](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/13) (posted 2026-11-16) | 0016 | the port db refuses a second name at a named index and a name at a second index, whose later delete freed a string still in use as a key (§8.13 of [`drivers/sonic.md`](drivers/sonic.md)) |

#8 and #9 were cut the same way, from branches `fix/breakout-port-index` and `fix/free-socket-event`, on `651a13b` (`dev` and `l2mcd-pre`, both still there on
2026-11-15; no upstream change to either file since). Each applies and builds alone, and the
harness case `port-delete` gives 0012 a reproduction of its own on upstream. `port-list-breakout`
is clean only with both. That case's use-after-free on `651a13b` comes from
`mcgrp_stop_phy_port()`, which any member port's link-down reaches. Neither PR fixes it
(§11.25.3). #8 notes it as a separate fault, to be reported separately.

It had been reported already: the review of #1 flagged it as Critical
([r2832640927](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/1#discussion_r2832640927),
aidan-gallagher, 2026-02-20; answered "Will further revise and optimize accordingly"), and
`651a13b` (2026-08-13) still has it. Its fix is the kit's sonic-l2mcd 0013 (§8.13 of
[`drivers/sonic.md`](drivers/sonic.md)), cut the same way as branch
`fix/stop-phy-port-freed-membership` (one commit on `651a13b`, `src/igmp.c` +7 -5) and
credited to that review: [#10](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/10),
posted 2026-11-15 on Clint's approval in the posting session. Its harness link is disunified
`85017b5`. The same commit corrects the harness's APPL_DB
stub, which counted no member deletion at all (l2mcd sends them through
`l2mcsync_add_l2mc_entry()` with `op_code` 0), so #8's "no member deleted" check could not
fail at the commit #8 links; re-run with the counting stub, #8's results are unchanged.

[#11](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/11) (the kit's 0014) was cut
the same way, as branch `fix/unnamed-port-report` (one commit on `651a13b`, `src/igmp_io.c` +11,
`src/mld_io.c` +10), and posted 2026-11-16 on Clint's approval in the posting session. It merges
in any order with #2-#10. Its harness link is disunified `1eee9ba` (the six `port-no-name-*`
cases and a stub of `l2mcd_is_peerlink()` that aborts on NULL as the real one does).

[#12](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/12) (the kit's 0015) and [#13](https://github.com/jasper-hou-micas/sonic-l2mcd-micas/pull/13) (0016) were cut the same way and posted
2026-11-16 on Clint's approval: branches `fix/static-group-refresh` (`src/igmp.c` +5 -12,
`port/l2mcd_mld_port.c` +16 -1) and `fix/portdb-name-overwrite` (`port/l2mcd_portdb.c` +27,
`port/l2mcd_utils.c` +2), one commit each on `651a13b`. Each merges with #2-#11. Their harness
links are disunified `bf75a8d` (the `static-member-*` cases) and `7ccd876` (the three
`portdb-name-*` cases). #12 was tested on `651a13b` with #10 applied, because its case's
link-down reaches #10's use-after-free first under ASan.

The four sonic-net drafts each got a comment linking a rebase branch on
Clint's forks (`igmp-snp-202511`, on the commits `series/BASES` pins):

- [sonic-net/sonic-buildimage#24327](https://github.com/sonic-net/sonic-buildimage/pull/24327#issuecomment-6677001592):
  - the squash plus three fixes: the submodule (series 0002), the unrelated
    changes taken out (0003) and the YANG imports (0010);
  - the CoPP (0006) and `-n 1` points offered as notes, not commits.
- [sonic-net/sonic-swss#3950](https://github.com/sonic-net/sonic-swss/pull/3950#issuecomment-6676995453):
  - the clean rebase;
  - on the Tomahawk-1, optimised flood is refused and orchagent retries it
    twice a second.
- [sonic-net/sonic-swss-common#1093](https://github.com/sonic-net/sonic-swss-common/pull/1093#issuecomment-6676995806).
- [sonic-net/sonic-utilities#4100](https://github.com/sonic-net/sonic-utilities/pull/4100#issuecomment-6676996171).

The tracking issue is Micas' own feature request,
[sonic-net/SONiC#2528](https://github.com/sonic-net/SONiC/issues/2528#issuecomment-6677010084)
("IGMP MLD Snooping"); it has no HLD. It carries the status comment.

**Prior art found before posting** (the posting session searched for each
patch): none of the fixes exists anywhere.

- The IGMP bugs are already in the 2020 Broadcom daemon,
  [Azure/sonic-l2mcd#11](https://github.com/Azure/sonic-l2mcd/pull/11),
  which was closed without a merge; that repository is archived and empty.
- 0007 is a regression from the split of `sock_fd` into
  `igmp_sock_fd`/`mld_sock_fd`.
- The MLD path came into the Micas tree in `0297ac8` (2025-11).
- No CVE or advisory names l2mcd.
- The Micas review (sonic-l2mcd-micas#1, 2026-02-20) covers other functions, with one
  exception found on 2026-11-15: it flagged `mcgrp_stop_phy_port()`'s use-after-free (0013),
  which was never fixed.

**Not posted:**

- the query-source harness cases (in 7b17313) are not in the published copy
  the PRs link (`0f185fa`);
- the DX010-only patches (sonic-buildimage 0004, 0005, 0007-0009, 0011,
  0012; swss 0002) are not part of the snooping drafts.

## Sources

- <https://github.com/jamesbraid/unifi-emu> / <https://pkg.go.dev/github.com/jamesbraid/unifi-emu/inform>
- <https://github.com/wvengen/unifi-controllable-switch>
- <https://github.com/amd989/unifi-gateway>, <https://github.com/qvr/unifi-gateway>, <https://github.com/stephanlascar/unifi-gateway>
- <https://ubntwiki.com/products/unifi/unifi_switch_leaf>, <https://dl.ui.com/product_sheets/USW-Leaf_Brief.pdf>
- <https://help.ui.com/hc/en-us/articles/30076656117655-Getting-Started-with-the-Official-UniFi-API>
- <https://arista.my.site.com/AristaCommunity/s/article/managing-containers-on-eos-container-manager>
- <https://www.arista.com/en/products/eos/open-and-programmable>
