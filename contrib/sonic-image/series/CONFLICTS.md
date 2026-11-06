# The series: what was rebased, what conflicted, what was changed

Prepared 2026-11-01. Every series applies with `git am` (no fuzz, no `-3`) on
the commit named in `BASES`, and `../dry-run.sh` proves it again. The four
sonic-net bases are the **current 202511 branch heads**, and they are also the
exact commits sonic-buildimage 202511 pins for its submodules, so the series
and the build tree agree.

| Series | Upstream source | Base (202511 head) | Source's merge base | Base is newer by |
|---|---|---|---|---|
| sonic-buildimage (12) | sonic-net/sonic-buildimage#24327 @ `80aa33e2de01` (draft, 10 commits) + 10 of ours + #26983 unchanged (0011); 0012 (the DX010 fan floor, 2026-11-03) touches only `fancontrol-F2B` | `1354e0ce4630` 2026-11-01 | `97a82c196d` 2026-06-11 | 154 commits |
| sonic-swss (1) | sonic-net/sonic-swss#3950 @ `0a10a61735df` (draft, 13 commits) | `177c7a009f5e` 2026-09-19 | `d43da01824` 2026-06-07 | 14 commits |
| sonic-swss-common (2) | sonic-net/sonic-swss-common#1093 @ `a78b0db83f5e` (draft, 7 commits, 5 of them L2MC); 0002 upstream `1431c3d` (libyang 3) | `e59457c6da43` 2026-07-21 | `7e5d563980` 2025-12-17 | 7 commits |
| sonic-utilities (1) | sonic-net/sonic-utilities#4100 @ `226ec54abc25` (draft, 19 commits) | `73f8c019ff7c` 2026-10-20 | `ae048e7021` 2026-06-03 | 21 commits |
| sonic-mgmt-common (1) | upstream `cdb612e` (#157, libyang 3), item 8 | `b5f84b0644d9` 2025-12-05 | n/a | n/a |
| sonic-l2mcd (7) | `jasper-hou-micas/sonic-l2mcd-micas` `dev` = `l2mcd-pre` = `651a13bdf036` 2026-08-14 + 7 of ours | `651a13bdf036` | n/a | n/a |

## Textual conflicts

**None.** Each PR was squashed and three-way merged onto its base
(`git merge --squash`); git auto-merged `orchagent/portsorch.cpp`,
`orchagent/port.h`, `orchagent/p4orch/tests/test_main.cpp`,
`tests/mock_tests/Makefile.am` (swss), `config/main.py` (utilities) and
`src/sonic-yang-models/yang-models/sonic-device_metadata.yang` (buildimage)
without a conflict hunk. `git merge-tree --write-tree` agreed before the merge.
The squash commits keep the PR author (Jasper Hou, Micas Networks) with the
co-author named, and their messages record the PR head and merge base.

## Semantic checks after the merge (nothing needed changing)

- **Debian base.** 202511's dockers are still bookworm (`docker-config-engine-bookworm`,
  `libpython3.11`, `SONIC_BOOKWORM_DOCKERS`); the host image is trixie. The PR's
  docker-l2mcd matches docker-stp's pattern on 202511 line for line.
- **Submodule pins.** sonic-buildimage 202511 pins swss `177c7a0`, swss-common
  `e59457c`, utilities `73f8c01`: the series' bases.
- **APIs the new code calls** exist in 202511 with the same signatures:
  `Orch(DBConnector *, const vector<string> &)`, `PortsOrch::allPortsReady/getPort/setPort`,
  `SUBJECT_TYPE_PORT_CHANGE`/`PortUpdate{port, add}`, `sai_api_query(SAI_API_L2MC, …)`;
  the SAI attributes l2mcorch sets (`SAI_VLAN_ATTR_CUSTOM_IGMP_SNOOPING_ENABLE`,
  `SAI_VLAN_ATTR_UNKNOWN_{IPV4,IPV6,LINKLOCAL}_MCAST_OUTPUT_GROUP_ID`, L2MC entry
  XG/SG, L2MC group and member) are in sairedis 202511's SAI submodule (`0ac228e5`,
  SAI 1.17.5) and in libsaibcm-dev 14.3's headers (SAI 1.17.4).
- **CoPP.** All nine trap names the PR adds (`igmp_query`, `igmp_leave`,
  `igmp_v{1,2,3}_report`, `mld_v1_v2`, `mld_v1_report`, `mld_v1_done`, `mld_v2_report`)
  are in 202511 copporch's map (0006 then keeps the five IGMP ones). The trap is not `always_enabled`: it is installed
  only while the `l2mcd` feature is enabled. A trap the SAI refuses is logged and
  marks orchagent unhealthy; it does not abort it (`handleSaiFailure(…, false)`).
- **Feature default.** `FEATURE|l2mcd` ships `disabled`; with it disabled the l2mcd
  container does not run and `L2mcOrch` (always constructed in orchagent) only
  observes port changes; its VLAN-removal hooks return early for VLANs without
  snooping.
- **Make variables.** Everything the new rules reference is defined in 202511
  except `$(LIBEVENT)`, which expands empty; `libevent-dev` is in the bookworm slave
  and l2mcd links `libevent.a` statically, so nothing is missing.

## What the series changes beyond the PRs, and why

sonic-buildimage:

1. `0001` is #24327 verbatim.
2. `0002` repoints the submodule. The PR's `.gitmodules` names
   `https://github.com/sonic-net/sonic-l2mcd`, which does not exist, **and adds no
   gitlink**, so `make init` would never check the daemon out. Now:
   `jasper-hou-micas/sonic-l2mcd-micas` pinned at `651a13bdf036`. Also fixes
   `rules/l2mcd.dep` (`SMDEP_SPATH` pointed at `src/sonic-l2mcd/sonic-l2mcd`, which
   does not exist) and makes `INCLUDE_L2MCD ?= y`.
3. `0003` takes out four unrelated changes the PR branch carried from other work:
   `ENABLE_TRANSLIB_WRITE = y` (gNMI write access), `telemetry_vars.j2` guards and
   the `x509` container in `sonic-device_metadata.yang` (both from the closed gNMI
   PR #24328), and an `init_cfg.json.j2` default `MGMT_PORT|eth0 admin_status up`,
   which would fight a switch that keeps its management port down on purpose.
4. `0004` adds `SAI_STATS_ST_CAPABILITY_SUPPORTED=0` and
   `SAI_STATS_EXT_SWITCH_SUPPORTED=0` to the eight Seastone `sai.profile`s (the DX010
   part of sonic-buildimage#28160, open against master). Not snooping: 202511's
   orchagent makes a streaming-telemetry capability query at start that 202505's
   did not, and the Broadcom SAI null-dereferences on it on a Tomahawk-1
   (sonic-sairedis#1788; #28127). Every libsaibcm this build can install on the
   DX010 (13.2.1.36, 13.2.1.100, 14.3.0.0.0.0.41.1) exports the crashing
   `brcm_sai_st_pd_ctr_cap_list_get`.
5. `0005` moves the DX010 to `platform_asic broadcom-legacy-th` (libsaibcm
   13.2.1.100) and adds the legacy-TH BDE modules to the ONIE image's
   `LAZY_BUILD_INSTALLS` (#28125). 202511 split Tomahawk-1 off the 14.3 XGS SAI
   ("no upgrade plan" for TH, #25878) but moved only the Arista 7060CX-32S; the
   DX010 is the same BCM56960. With 0005 the DX010's image is
   **`target/sonic-broadcom-legacy-th.bin`** (sonic-broadcom.bin then refuses it).
   `build.sh DX010_SAI=xgs` leaves 0005 out.
6. `0006` traps IGMP the way a UniFi ECS switch (Ubiquiti's own SONiC, read
   2026-11-01) does: the five IGMP trap types only, at cir 128 / cbs 160 pps on
   queue 1, priority 1. The PR also trapped the four MLD types, which sends every
   MLD packet on every VLAN (IPv6 neighbour discovery's joins among them) to the
   CPU once the feature is on; UniFi configures no MLD snooping and the ECS traps
   none. The ECS also traps `pim` in that group; not followed, because this
   l2mcd's socket filter only takes IGMP. Applies with or without 0005.
7. `0007` is sonic-net/sonic-buildimage#27355, "[platform] Restore Celestica
   platform API wheels" (Rustiqly, open since 2026-05, review required, base master),
   cherry-picked unchanged from its head `748ad3bb4c40` with author and title kept; it
   applies to 202511 without conflict. It fixes #27291: 202511's kernel 6.12/trixie
   rework of the Celestica packaging (d2e8ac700) moved the non-PDDF platforms to
   pybuild and dropped the lines that installed their `sonic_platform` wheel into the
   device directory, which is where pmon's container installs the platform API from;
   without it `show platform fan/psustatus/temperature` report nothing on a 202511
   DX010. The same unit on 202505 reports drawers and PSUs from that file. The PR
   builds each non-PDDF board's wheel with `python3 setup.py bdist_wheel` and installs
   it for dx010, haliburton, questone2, seastone2 and silverstone. Checked here in the
   202511 trixie build slave (setuptools 75.8, wheel 0.46.1): all five build; the
   DX010's is 42 KB with its 18 `sonic_platform` modules. (Our own version of the same
   fix, written before the PR was found, is replaced by it.)
8. `0008` and `0009` make 202511 build gNMI and the management framework, which the
   first build could not (and an earlier 0009 left out). libyang 1 and 3 are built
   side by side and their -dev packages are one Debian package, so a compile meets
   whichever was installed last; sonic-mgmt-common's Go validation code (compiled
   into sonic-gnmi and sonic-mgmt-framework too) failed against libyang 3's headers
   ("too many arguments to function 'ly_set_log_clb'", upstream #26165), and no rule
   ordering fixes it while both are installed in one tree (#26165 alone was tried:
   gnmi and mgmt-framework built alone still failed). `0008` builds swss-common,
   sonic-mgmt-common, sonic-gnmi and sonic-mgmt-framework against libyang-dev 3, as
   master and 202605 do; `0009` retires the libyang3-dev-done gate, which nothing
   needs any more (master did the same, #27629). With them, `sonic-swss-common/0002`
   (upstream 1431c3d) and `sonic-mgmt-common/0001` (upstream cdb612e, #157, its CI
   hunk rewritten for 202511's bookworm paths) port those two to libyang 3. Proven by
   the libyang session in every build mode (parallel, resumed, serial; libyang-dev 1
   never installed; `rest_server` and `telemetry` start and parse the CVL schema).
   libyang 1 stays for python3-yang, so sonic-yang-mgmt, GCU and `config` validation
   are unchanged and 0010's bisect holds. The earlier 0008/0009 (libyang-dev 1
   declared; the two containers left out) are in `../igmp-build-retired/`.
9. (see 8)
10. `0010` changes #24327's two YANG models. With them installed, 7 sonic-utilities
   tests of the generic config updater (GCU) fail and the sonic-utilities wheel,
   whose tests run in the build, is not produced. Bisected in the bookworm slave:
   any new module importing `sonic-vlan` (a dummy with one VLAN-name leafref is
   enough) changes the references GCU's libyang 1 walk reports for existing tables
   (`VLAN_MEMBER` begins to show as referencing `PORT`); an unrelated dummy module
   does not, and neither do the two models without those imports. Since that is
   GCU's behaviour for `config apply-patch`/`replace`, not only a test, the fix is in
   the models: interface and VLAN-name fields become plain strings and the imports
   of sonic-vlan, sonic-port and sonic-portchannel go. All 214 tests in the two GCU
   test files pass and all 141 models load. Lost: YANG's check that a named VLAN or
   port exists, which nothing here uses (the snooping CLI writes through an
   unvalidated connector, the disunified driver writes rows directly).

sonic-swss-common: the five L2MC commits of #1093 only (net effect `common/schema.h`).
Left out: the two upstream commits the PR branch carries (3cc4d68 sonic-db-cli #1070,
ab3ae1e #1087; not on 202511, unrelated) and four `APP_DEBUG*_TABLE_NAME` defines left
over from a debug framework the PR added and removed again (nothing uses them).

sonic-l2mcd: the memcpy fix (0001) and the receive path (0002-0007), both below.

Not taken: sonic-sairedis#1672 (warm-reboot view comparison for L2MC objects; this
build is cold-boot only) and sonic-mgmt-common#185 (gNMI YANG import; no gNMI use).

## The memcpy bug

The bug acknowledged on sonic-swss#3950 (review of 2026-08-12, "all ten IP-address
copies use `memcpy(…, string.c_str(), L2MCD_IP_ADDR_STR_SIZE)`"; 46 bytes read from a
string that guarantees `size() + 1`) **was fixed upstream** in `379cfb9` (2026-08-19,
"fix l2mc memy issue"): all ten copies in `cfgmgr/l2mcmgr.cpp` are now
`strncpy(dst, s.c_str(), SIZE - 1)` with a terminating NUL, and the series carries
that fix. The remaining `memcpy`s there copy interface names with a length bound of
`min(length, 16)` into zeroed buffers, which is safe for SONiC names (at most 15
characters).

**The daemon has the same bug and upstream did not touch it**:
`L2mcSync::dumpL2mcVlanEntry` in `l2mcd_sync.cpp` copied the port name (16 bytes) and
group/source addresses (46 bytes each) with `memcpy(dst, s.c_str(), FIXED_SIZE)`
(lines 762-764 and 823 at `651a13b`). That path runs on a warm restart, but the
read past the string is real. `sonic-l2mcd/0001` bounds them the same way (the entry
is zeroed just above). The C sources' other fixed-size copies read from fixed-size
IPC buffers of at least that size and are left alone.

## The receive path (sonic-l2mcd 0002-0007)

Reported against the daemon (posted 2026-11-05 as PRs on the Micas `l2mcd-pre`
branch: `docs/prior-art.md` §6.1) and confirmed in-process
with the harness in `../l2mcd-fuzz/` (ASan + UBSan, the daemon's own receive path,
one heap buffer per packet of exactly the received size): every IGMPv3 and MLDv2
record walk trusted the packet's own counts, and the receive paths trusted the IP
header's lengths. 0002 adds the drop counter and rate-limited log the rest use;
0003 the IGMP parsers; 0004 the IPv4 receive-length check (and the tagged-frame MLD
offset); 0005 the MLD path; 0006 an off-by-one in the group state machine that every
well-formed report with sources hit; 0007 a receive-socket leak found on the VS (every
port update opened another IGMP socket, each forwarding its own copy of every packet). `l2mcd-fuzz/run.sh regress` runs one named
case per finding: on `651a13b`+0001, 21 of the 25 overrun or leak (23 with the per-VLAN
debug log's arguments evaluated; the two that never do are an MLD misparse and an
MLD record type MLD does not index by), and all 25 pass on the series either way. Details and the fuzzing result:
`docs/drivers/sonic.md` section 8.

## Querier behaviour (sonic-l2mcd 0008, 0009)

Not a parser fix: found with the series live on a DX010 beside an IGMPv2 router
(2026-11-05). At version 3 `igmp_process_query()` returned on the version mismatch
before the election and the router-port learning, so no IGMPv2 querier was ever seen;
and the VLAN's own address was looked up only through `vlan_node->ve_ifindex`, while
SONiC binds no VE and l2mcmgrd keeps the address in `ve_portdb_tree` under the VLAN
id, so queries left from 0.0.0.0, which wins every election. 0008 fell back to the
VLAN id only for `ve_ifindex` 0; the DX010 runs with the bare VLAN id there (the
address arrives after the snooping enable), which 0009 covers, in every reader of
the address (IGMP and MLD). Upstream, 0008's address hunks and 0009 go as one
change. Query path only; the harness's packet cases are unaffected, and its three
`query-src-*` cases cover both start-up orders and the DX010's state.

## Logging (sonic-l2mcd 0010)

docker-l2mcd starts `l2mcd -n 1`, as the ECS image does. `-n 1` routes every
per-VLAN debug line to syslog at NOTICE, and l2mcmgrd sets l2mcd's level to its own
swss level, NOTICE by default, right after start: so all of them were written. The
ECS writes them only to its local syslog (its rsyslog has no remote rule); SONiC
forwards syslog to the remote host the controller pushes, which received ~75 lines/s.
0010 routes those lines at INFO and turns the IPv6 receive path's per-frame ERRs
into counters; `-n 1` and the start line stay the ECS's. A start-up level in l2mcd
itself would not hold: l2mcmgrd overrides it within a second.

## The review comments on #3950, checked against the PR head

30 comments (2026-02-20) from one reviewer, answered "will further revise"; the
2026-07-17 "fix issues from code review" commit addressed the crash-class ones.
Checked on the merged code: the use-after-erase in `RemoveL2mcMrouterPort` (ids
copied before the erase), the `continue`-without-advance loop in
`RemoveL2mcGroupMembers` (iterator advanced), the `.at()` throw (`hasL2mcGroup` uses
`find`), the relative `#include "../../sonic-l2mcd/include/…"` (now `-I` in
`cfgmgr/Makefile.am`), the undefined `isRestoreDone` (gone) and l2mcmgrd's unbounded
start-up waits (now bounded: a retry limit on `PortInitDone` and on vlanmgrd's
warm-start state, a timeout on the port wait) are all fixed. Left as they are, none of
them a crash: an invalid mrouter protocol erases the bad entry and `return`s instead of
`continue` (the rest are processed on the next pass); a single link-local suppression
attribute serves IPv4 and IPv6 (that is how the SAI defines it).

## Compile status

Nothing here has been compiled: the Mac has no container runtime and creating one
would mean a VM. `build.sh` builds `libswsscommon`, `l2mcd`, `swss` and
`docker-l2mcd` first (`FAILFAST=1`, in the bookworm slave) so a compile error in the
series surfaces in about an hour instead of after the full image.
