# SONiC 202511 with IGMP snooping and port mirroring: the image build kit

This directory (`contrib/sonic-image/` in the disunified repository) is the
patch series and the scripts that build, check and install the SONiC image
the `sonic` driver's hardware runs. What each patch enables, and when the
image is needed at all, is [`docs/drivers/sonic-running.md`](../../docs/drivers/sonic-running.md)
§3; the record of building and testing it is [`docs/drivers/sonic.md`](../../docs/drivers/sonic.md) §8.

**Licensing.** The patches under `series/` modify SONiC, which is licensed
under the Apache License 2.0 (`series/LICENSE`, SONiC's own copy); they are
offered under the same terms. Four of them are upstream pull requests or
commits carried as they are, rebased onto this series' bases (their PR or
commit ids are in the table below); the rest are this project's. The scripts
and the fuzz harness are MIT like the rest of the repository. **The e-mail
addresses in the patch headers are placeholders** (`name@example.net`): this
repository keeps no e-mail addresses in tracked files, so `git am` records
each author by name and the upstream reference in the table is where the
real authorship lives.

Builds community SONiC **202511** with the Micas Networks IGMP/MLD snooping
series (l2mcd daemon, `l2mcorch`/`l2mcmgrd` in swss, `config igmp-snooping` CLI)
for two targets:

- `sonic-vs.img.gz`, the virtual switch, to prove the control path in a guest;
- the Broadcom ONIE images, of which the Celestica DX010 (BCM56960 Tomahawk-1)
  takes **`sonic-broadcom-legacy-th.bin`** (or `sonic-broadcom.bin` with
  `DX010_SAI=xgs`, see below).

The why, the validation plan and the hard unknowns are in
[`docs/drivers/sonic.md`](../../docs/drivers/sonic.md) §8. This directory is the how.

| File | |
|---|---|
| `build.sh` | the unattended build (preflight, prepare, fail-fast packages, vs image, broadcom images, manifest and checksums) |
| `series/` | the patches, one directory per repository, each with a `series` file; `series/BASES` pins the commit each applies to; `series/CONFLICTS.md` is the rebase record |
| `dry-run.sh` | (in the Claude desktop app's Terminal panel, run it as `env -u GIT_CONFIG_PARAMETERS ./dry-run.sh`: the panel sets `safe.bareRepository=explicit`, which refuses the script's bare object stores) every check that does not need the build host (the series applies, templates render, YANG parses, rules resolve, CLI compiles); it passed on 2026-11-01 (`dry-run.log`) and again on 2026-11-07 with sonic-buildimage 0013 (the Mac's work clones of sonic-swss and sonic-l2mcd lag the series, so that run fetched those two from upstream; it needs bash 4+, e.g. Homebrew's) |

## The series in one table

| Repo | Patch | What |
|---|---|---|
| sonic-buildimage | 0001 | sonic-net/sonic-buildimage#24327: docker-l2mcd, rules, service, CoPP trap group for IGMP/MLD, YANG |
| | 0002 | the daemon submodule pointed at `jasper-hou-micas/sonic-l2mcd-micas@651a13b` (the PR's URL does not exist and it adds no gitlink); two rule fixes |
| | 0003 | drops the PR's unrelated changes (gNMI write access, gNMI templates and YANG, a default `eth0 admin up`) |
| | 0004 | Seastone `sai.profile`: `SAI_STATS_ST_CAPABILITY_SUPPORTED=0`, `SAI_STATS_EXT_SWITCH_SUPPORTED=0` (202511 orchagent's telemetry query crashes the Tomahawk-1 SAI otherwise) |
| | 0005 | DX010 on the legacy-TH SAI 13.2.1.100, like the Arista 7060CX; legacy-TH BDE modules in the ONIE image |
| | 0006 | CoPP as a UniFi ECS switch (Ubiquiti's SONiC) does it: IGMP only, no MLD, 128/160 pps, queue 1 |
| | 0007 | sonic-net/sonic-buildimage#27355 (open, unchanged): the Celestica `sonic_platform` wheels shipped to pmon again (202511 dropped them: no fans/PSUs/temps on a DX010, #27291) |
| | 0008 | swss-common, sonic-mgmt-common, sonic-gnmi and sonic-mgmt-framework build against libyang-dev 3 (as master and 202605); with libyang-dev 1 and 3 one package, the old rules let a compile meet the wrong one |
| | 0009 | the libyang3-dev-done gate retired: nothing installs libyang-dev 1 any more (master: #27629) |
| | 0010 | the two snooping YANG models without imports of sonic-vlan/port/portchannel (they changed the generic config updater's behaviour and broke 7 sonic-utilities tests) |
| | 0011 | sonic-net/sonic-buildimage#26983 (open, unchanged): `dx010_cpld` `setreg_store` one byte short; kernel 6.12's FORTIFY_SOURCE traps it at the watchdog disarm, so a 202511 DX010 never finishes booting |
| | 0012 | Seastone `fancontrol-F2B`: the fan floor PWM 89 -> 51 (35 % -> 20 %) and the ramp starting at an intake of 35 C instead of 26; measured on a DX010 (ASIC ~56 C in a 23-25 C room, fans OK; 15 % rejected). The direction file is what the boot script copies over `fancontrol`, so it is the only file that persists a floor |
| | 0013 | Seastone `sensors.conf`: the SO-DIMM's JC-42.4 thermal sensor (`jc42-i2c-1-18`) given limits of 85/95 C (JEDEC's normal and extended DRAM range). Its registers come up at 0 C, and sensord logged its normal ~35 C as an ALARM once a minute, ~1,400 syslog lines a day; the registers take the write, and `sensors -s` at pmon start applies it |
| | 0014 | the legacy-TH module package ships and loads `linux-bcm-genl`, which it already builds: the legacy-TH SAI sets a port's sFlow rate through its `/proc/bcm/genl/psample/rate`, so without it every sFlow session fails in the SAI (`cannot open knet link sample proc`). The classic-XGS half of sonic-net/sonic-buildimage#26360 (which did the sdklt `linux_bcmgenl`); loaded after `linux-knet-cb`, before syncd makes its netifs. The DX010's unknown-unicast storm detection reads those samples (disunified `docs/drivers/sonic.md` §11.24) |
| sonic-swss | 0001 | sonic-net/sonic-swss#3950: `l2mcorch`, `l2mcmgrd` (includes the upstream fix of the reviewed `memcpy` bug) |
| | 0002 | port mirroring recorded as supported when the SAI answers no to `SAI_PORT_ATTR_INGRESS/EGRESS_MIRROR_SESSION`: the legacy-TH SAI says no and binds anyway (disunified `docs/drivers/sonic.md` §11.9-11.10) |
| | 0003 | orchagent's outer `SRC_MAC`/`DST_MAC` matches (`<mac>` or `<mac>/<mask>`) and a built-in `L2` table type (PORT/LAG; `SRC_MAC`, `DST_MAC`, `ETHER_TYPE`, `OUTER_VLAN_ID`), with `AclOrchTest.L2Acl_Mac_Matches`: a reduced port of sonic-net/sonic-swss#2554 (open since 2022) for UniFi's switch MAC ACL rules (disunified `docs/drivers/sonic.md` §11.7.8). The package build does not run the mock tests; run them in the slave container |
| | 0004 | `l2mcorch`'s count of a port's L2MC group members kept to the SAI members on its bridge port (a port that was a group member and a multicast router port at once kept a count with no member left after its link dropped), and a port's members dropped with its VLAN membership and before its bridge port changes: `removeBridgePort` refused on that count after setting the bridge port admin down, and the next VLAN added reused it, so a native-VLAN move left the port learning nothing until a swss restart (disunified `docs/drivers/sonic.md` §8.15). `L2mcOrchTest`'s four cases fail without it; with it the whole mock suite (607 tests) passes in the slave container |
| | 0005 | a LAG's L2MC group members taken out of the chip before its members change (added, status, removed) and made again after: the legacy-TH SAI fixes a LAG member's egress ports to the members distributing when it is made, and refuses its removal ITEM_NOT_FOUND once another has started distributing; `RemoveL2mcEntry` removes the members before the entry, so a refused one no longer leaves a record of an entry the chip has lost (on the DX010 the uplink's seven reserved groups, out of the chip until a reboot, and 159 `rv:-7` lines; disunified `docs/drivers/sonic.md` §8.16). `L2mcOrchTest`'s three new cases fail without it; with it the whole mock suite (610 tests) passes in the slave container |
| sonic-swss-common | 0001 | the `schema.h` table names from sonic-net/sonic-swss-common#1093 |
| | 0002 | upstream `1431c3d`: DefaultValueProvider on libyang 3 (libyang 1 fallback kept) |
| sonic-mgmt-common | 0001 | upstream `cdb612e` (#157): CVL on libyang 3, CI hunk on 202511's bookworm paths |
| sonic-utilities | 0001 | sonic-net/sonic-utilities#4100: `config igmp-snooping …`, `show igmp-snooping …`, MLD equivalents |
| sonic-l2mcd | 0001 | bounds the same `memcpy`-from-`c_str()` bug in the daemon's `l2mcd_sync.cpp`, which upstream did not fix |
| | 0002 | a malformed IGMP/MLD packet is dropped, counted and logged at most once per family every 10 s |
| | 0003 | the IGMPv3 report and query parsers held to the message length (record walk, aux words, record types, query sources, zero checksum) |
| | 0004 | the IPv4 lengths checked against the bytes received (else `tot_len` bytes are forwarded to router ports); the tagged-MLD buffer offset |
| | 0005 | the MLD receive path and MLDv2 parsers held to the bytes received |
| | 0006 | the group state machine read one source past every record's list |
| | 0007 | one IGMP receive socket per port: every port-list resend or link change opened another, each parsing and forwarding every packet |
| | 0008 | an older-version querier is elected against and its port learned as a router port (a v3 VLAN dropped every IGMPv2 query); queries leave from the VLAN interface's address, which SONiC keeps under the VLAN id, not from 0.0.0.0 (found live on the DX010 2026-11-05, `docs/drivers/sonic.md` §8.14) |
| | 0009 | the VLAN's address found whatever `ve_ifindex` holds: on the DX010 it is the bare VLAN id, which 0008's fallback missed (group-specific queries still from 0.0.0.0); also the MLD query source, static reports to router ports and the querier's own address |
| | 0010 | the per-VLAN debug lines that `-n 1` routes to syslog go at INFO, not NOTICE (l2mcmgrd sets l2mcd to its swss level, NOTICE, at start, so ~75 lines/s reached the remote log host); the per-frame IPv6 ERRs of a VLAN without MLD snooping are counted (`mld-off`, `non-mld` in `l2mcd_ctl -c 10`) |
| | 0011 | a port a breakout deletes and creates again keeps its own index (4096 + N for EthernetN) instead of 4096 + its kernel ifindex, which was another port's: Ethernet0's for a lane reported before its netdev, Ethernet<ifindex>'s otherwise; that port lost its sockets, its tree and its group memberships (found on the DX010 2026-11-15, `docs/drivers/sonic.md` §11.25.3) |
| | 0012 | a closed port socket's libevent event is freed, not only deleted (128 bytes per socket) |
| | 0013 | `mcgrp_stop_phy_port()` no longer reads and clears the membership `mcgrp_destroy_mbrshp_entry()` has just freed: every group member's link-down, a port leaving its VLAN, snooping turned off on a VLAN and a version change reached the freed memory (IGMP and MLD; flagged in the Micas PR's review on 2026-02-20, never fixed; harmless with glibc's allocator, `docs/drivers/sonic.md` §8.13) |
| | 0014 | a report, leave or MLD done from a port the port db holds no name for is dropped and counted (`rx_bad_if`): the IGMP report and leave paths and the three MLD handlers passed the NULL name to `l2mcd_is_peerlink()`, whose `std::string` threw `std::logic_error` and aborted the daemon (the DX010, 2026-11-15: an IGMPv3 report on the index a breakout had left nameless, from l2mcd's core; `docs/drivers/sonic.md` §8.13) |

## `l2mcd-fuzz/`: the daemon's packet parsers under ASan, UBSan and libFuzzer

`harness.c` drives l2mcd's own `receive_igmp_packet()` / `receive_mld_packet()`
after configuring it through its own IPC handler, one heap buffer per packet of
exactly the received size; `stubs.c` replaces the APPL_DB sync layer. Run on the
build host (the bookworm slave image, no network, dummy links; nothing is sent):

```bash
cd l2mcd-fuzz
./run.sh build src-fixed                 # a copy of the daemon's tree, series applied
./run.sh regress src-fixed               # one named case per finding; all must say clean
HARNESS_LOGARGS=/dev/null ./run.sh regress src-fixed    # with the per-VLAN debug log's arguments evaluated
./run.sh fuzz src-fixed igmp 14400 4     # seconds, workers; crashes land in out/<tree>/crash-igmp
```

`run.sh regress` writes each case's packet to `corpus/`, the fuzzers' seeds.
The three `query-src-*` cases (0009) are not packet cases: each sends Vlan100's
address before or after the snooping enable, or forces `ve_ifindex` to the bare
VLAN id the DX010 runs with, and checks the source of the group queries and of a
general query; each must be run by name (`run.sh regress` does).

`member-link-down`, `mld-member-link-down` and `member-vlan-leave` (0013) are not
packet cases: Ethernet4 and Ethernet8 join 239.1.2.3 (ff3e::1 for MLD), then
Ethernet8's link goes down, or it leaves Vlan100, through l2mcd's own link-event
function or IPC handler. Each checks that the group keeps Ethernet4 alone and that
one member deletion went to APPL_DB, under Ethernet8; each must be run by name.
The APPL_DB stub counts a deletion where l2mcd sends it, `l2mcsync_add_l2mc_entry()`
with `op_code` 0 (before 2026-11-15 it counted only `l2mcsync_del_l2mc_entry()`,
which no C file calls, so no case could see a deletion).

The six `port-no-name-*` cases (0014: `port-no-name-v2-report`, `-v3-report`, `-v2-leave`, `mld-port-no-name-v1-report`, `-v2-report`, `-done`) take Ethernet8's name out of the port db with l2mcd's own `portdb_delete_ifname()`, its tree and sockets kept, as a breakout left Ethernet0's on the DX010, and send one message of that type on it. Each checks that it is dropped and counted in `rx_bad_if`, not forwarded, and makes no membership; each must be run by name. The stub of `l2mcd_is_peerlink()` aborts on a NULL name, with the caller's stack, as the real one does (its `std::string` throws `std::logic_error`, which nothing catches); before 2026-11-15 it accepted NULL, so no case could see the abort.

`port-list-breakout` (0011, 0012) is not a packet case either: it drives the port-list messages and link events of a breakout through l2mcd's own handlers, with dummy links it adds and deletes in the container (one created as ifindex 40), and checks that a port outside the split keeps its index, tree, sockets and group membership.

## The build host

Planned for: Debian 12 x86_64, 16 cores, 64 GB, 400 GB disk, Docker. As a
Proxmox guest: CPU type **host** (the vs image is installed by running KVM inside
the build, so the guest needs nested virtualisation and `/dev/kvm`), virtio
SCSI on SSD with discard, one disk of 400 GB.

One-time setup, as a normal user with sudo:

```bash
sudo apt-get update
sudo apt-get install -y git make python3 pipx curl ca-certificates docker.io tmux
sudo usermod -aG docker "$USER"          # then log out and in again
pipx install jinjanator && pipx ensurepath
echo overlay | sudo tee /etc/modules-load.d/overlay.conf && sudo modprobe overlay
```

Debian 12's `docker.io` is 20.10.24, above the 20.10.10 the bookworm/trixie
build slaves need; Docker's own `docker-ce` works too. `j2` must be on `PATH`
(`apt install j2cli` is the alternative to jinjanator). The build refuses to run
as root.

Copy the kit over (this directory, without any `src/` work clones) and check
the host:

```bash
tar -czf sonic-igmp-kit.tgz --exclude=src --exclude=dryrun --exclude=inspect -C contrib sonic-image
scp sonic-igmp-kit.tgz buildhost:
ssh buildhost 'tar -xzf sonic-igmp-kit.tgz && mv sonic-image sonic-igmp-kit'
ssh buildhost 'cd sonic-igmp-kit && ./build.sh preflight'
```

## Running it

```bash
tmux new -s sonic
cd ~/sonic-igmp-kit && ./build.sh 2>&1 | tee ~/sonic-igmp-build.out
```

Stages, each logged to `$WORK/logs/<stage>.log` with its duration in
`$WORK/logs/durations` (`WORK` defaults to `~/sonic-igmp`):

1. **preflight**: OS, not root, Docker version and group, `j2`, overlay,
   `/dev/kvm`, cores, memory, free disk, GitHub reachable.
2. **prepare**: clone sonic-buildimage 202511, check out the base in
   `series/BASES` on a local branch `igmp-202511`, `git am` its series,
   `make init` (all submodules, about 6-10 GB), verify that swss,
   swss-common, utilities and l2mcd are at their series' bases, `git am` theirs,
   write `MANIFEST`. Any patch that does not apply stops the build here.
3. per platform (`vs`, then `broadcom`), in its own copy of the prepared tree
   (`tree-vs`, `tree-broadcom`; the trees are relocatable after `make init`):
   `make configure PLATFORM=…`; on the first platform the **fail-fast** stage
   builds `libswsscommon`, `l2mcd`, `swss` and `docker-l2mcd` in the bookworm
   slave, so a compile error in the series shows within about an hour; then the
   image.
4. **collect**: images to `$OUT/<build id>/` (`OUT` defaults to `$WORK/out`) with `SHA256SUMS`, `MANIFEST`
   (every commit and the series' sha256s) and `BUILD_ID`.

The build id is `igmp-202511.<BUILD_NUMBER>-<short sha of the prepared
buildimage branch>` (SONiC's own `sonic_get_version`); `show version` on the
switch prints it. Record it with the image, as `docs/drivers/sonic.md` §1 asks
for the stock builds (and `docs/drivers/sonic-running.md` §3.3 for the ids the DX010 has run).

Settings (environment variables; all in the header of `build.sh`):
`WORK`, `OUT`, `JOBS` (`SONIC_BUILD_JOBS`, default 4), `DX010_SAI` (`legacy-th` or
`xgs`), `BUILD_NUMBER` (default the UTC date), `FOLLOW_HEAD=1` (build on the
current 202511 tips with `git am -3` instead of the pinned bases),
`FAILFAST`, `PLATFORMS`, `VS_DOCKER=1` (also `docker-sonic-vs.gz`), `RESUME=1`
(re-run one platform without re-copying its tree, so make continues where it
stopped).

After a failure: read the stage log and the package log `build.sh` points at,
fix, then `RESUME=1 ./build.sh vs` (or `broadcom`). A patch that stops applying
under `FOLLOW_HEAD=1` is the moment to rebase the series (the work clones are
under `src/` on the Mac, branch `igmp-202511`).

**A new patch on an already-built tree needs more than `RESUME=1`** (2026-11-15,
0014). make does not track a package's `debian/` files, so after the patch is
applied the old `.deb` counts as up to date. The top Makefile does the same for an
existing `target/sonic-*.bin`. Move aside the changed package's `.deb` under
`target/debs/<dist>/` and the platform's `.bin`s, then run `RESUME=1`. Don't
use a package's clean target: it walks the bookworm phase again and rebuilt
unrelated packages (libswsscommon) for an hour before it was stopped.

## How long

Estimated, not yet measured (the first run's `durations` file replaces this):

| Stage | 16 cores, 64 GB, SSD, `JOBS=4` |
|---|---|
| prepare (clone, `make init`) | 15-30 min |
| build slaves, bookworm and trixie (first time only, shared by both trees) | 40-70 min |
| fail-fast packages | 45-90 min |
| vs image (rest of it) | 2.5-4 h |
| broadcom images (own tree, everything rebuilt including the kernel; three ONIE images: broadcom, broadcom-dnx, broadcom-legacy-th) | 3-4.5 h |
| **total, unattended** | **about 7-10 h** |

Disk: about 120 GB per platform tree after its build plus about 15 GB of slave
images; both trees fit in 400 GB with room for one rebuild. Network: several GB
of Debian, PyPI, GitHub and Broadcom SAI downloads per tree.

## `DX010_SAI`: which Broadcom SAI the DX010 gets

202511 moved the Broadcom XGS SAI to 14.3 and split Tomahawk-1 off to a
"legacy-TH" SAI pinned at 13.2.1.100, moving only the Arista 7060CX-32S to it.
The DX010 runs 13.2.1.36 on 202505 today. Default `legacy-th` keeps it on the
13.2.1 family (patch 0005) and its image is `sonic-broadcom-legacy-th.bin`;
`xgs` leaves 0005 out and the DX010 installs `sonic-broadcom.bin` with SAI 14.3,
which nobody upstream has reported running on a Tomahawk-1. Both SAIs carry the
L2MC entry and group API and default their internal L2MC feature flag to on
(`docs/drivers/sonic.md` §8.8 has the inspection).

## What not to do with the result

Nothing here touches a switch. Installing on the DX010 follows the validation
plan in `docs/drivers/sonic.md` §8 and its rules for live work (§11.3):
`sonic-installer install` adds an image beside the running one, the factory and
202505 images stay, and `sonic-installer cleanup` is never run.

## Installing and testing the result

- **`sonic-upgrade.sh`** (run on the switch as admin): `arm <image.bin> <sha256>
  [minutes]` installs the image beside the running one, keeps the running image as
  the GRUB default, boots the new one once, and puts a dead-man in its overlay that
  reboots back unless `confirm` runs first. It carries the SSH host keys, admin's
  authorized_keys and password, and -- when the bridge built them there -- the
  static `mstpd`/`mstpctl`, so spanning tree needs no rebuild (docker, DNS,
  internet) in the window. `confirm`, `rollback`, `status`. To arm an image that an
  earlier arm installed, `sonic-installer remove -y <image>` first (arm refuses and
  says so): re-arming it in place would boot the config it migrated then. The
  202511 vs image takes 6.8 GB installed plus the installer while it installs (`/tmp`
  is the running image's overlay on `/host`, not memory): delete the installer once
  it is installed, before the reboot. Don't pipe `confirm`/`rollback` into `head`
  (now harmless: SIGPIPE is ignored and sonic-installer logs to
  `/host/sonic-upgrade.log`).
  **Installing deletes every other image.** The image's own `install.sh`, run from
  a running SONiC (as `sonic-installer install` runs it), removes every
  `/host/image-*` except the running image and the new one -- on the DX010 the
  factory 20181130.77 image went that way. `arm` names what it will delete before
  installing; keep a copy of anything that matters (and the ONIE path) first.
- **`arm ... trace`** (the DX010's second trial, 2026-11-02): places in the new
  image's overlay, removed again by `confirm`: a boot trace every 2 s
  (`/host/sonic-upgrade-trace.log`, synced, so a hard reset cannot lose it); a
  feeder that arms the DX010 CPLD watchdog for 120 s once `dx010_cpld` is up and
  feeds it every 20 s, so a kernel hang resets the box into GRUB's default
  (`/host/sonic-upgrade-wdpet.log`; stopping it leaves 300 s, which the next boot
  turns off); `watchdog-control`'s `watchdogutil disarm` run with its output,
  exit code and time in `/host/sonic-upgrade-wdu.log`; and a sysctl file that
  panics on a soft or hard lockup or an oops and reboots 20 s later
  (`hardlockup_panic=1` beside SONiC's own `softlockup_panic=1`,
  `panic_on_oops=1`). Each trace line carries whether `dx010_cpld`'s `setreg` and
  `getreg` exist and are writable and the CPLD's 0x141 as read then. `confirm`
  first turns the CPLD watchdog off and reads 0x141 back (0x0 three tries, then
  `watchdogutil disarm`); if it does not read 0 it stops with exit 4 and leaves
  the dead-man and GRUB's default alone. Then it removes every trial piece and
  restores the sysctls, so the image is a plain install. Background: 202505's `reboot` arms
  the CPLD watchdog for 180 s, and only the next boot's `watchdogutil disarm`
  (the platform `Watchdog()` constructor) turns it off.
- **`vs-test.sh`**: §8.9 V1-V3, V6, V7 on a VS. **`vs-hosts.sh up`** gives a VS
  with one NIC two test hosts (veth pairs `eth3`/`eth4` = Ethernet8/Ethernet12,
  far ends in namespaces h1/h2, nothing leaves the VM; restarts swss), and
  **`vs-join-test.sh`** runs V4 (IGMPv3 and IGMPv2 joins and leaves from the hosts'
  own kernels) and V5 (querier). Snooping at the CLI's default IGMP version 2
  ignores v3 reports: the test sets 3.
- **`vs-inject.sh`**: the l2mcd parser fixes end to end on a VS: h1 sends each of
  `l2mcd-fuzz`'s regression packets (copied to `/tmp/corpus`) as a tagged frame
  into a snooping VLAN; l2mcd's `malformed-igmp` count and pid after each. Inside
  the VM only -- never onto a LAN or at a switch in service.
- The earlier 0009 (in `../igmp-build-retired/`) left gNMI and the management
  framework out; an image built with it needs, after an upgrade from 202505, `config
  feature state gnmi disabled`, the same for `mgmt-framework`, and `config save -y`
  (202505's config enables both; featured and monit report the missing units).
