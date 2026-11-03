# Running a SONiC switch under disunified: the two deployments and the image

The `sonic` driver bridges a community SONiC switch into the UniFi Network
controller. There are two places the bridge can run, and two kinds of image
the switch can run. This page is the map of those four combinations: what
each gives you in UniFi, what it costs, and where the detail is. It is
written from the Celestica Seastone DX010 (Broadcom BCM56960 Tomahawk-1,
32x QSFP28), the one unit the driver has run on, as of 2026-11-02.

| | Stock community image | The patched 202511 image (§3) |
|---|---|---|
| **Bridge over SSH** from a container or host (§1) | the general case: any SONiC switch, ports/VLANs/LAG/STP/ACLs; no IGMP snooping, no port mirroring on a 202511 Broadcom image | everything the driver does, plus the image's own features, from a bridge behind the switch |
| **Bridge on the switch** as a systemd service (§2) | the same features with no dependency on anything the switch carries | the DX010 as a UniFi-managed core switch: what runs on Clint's unit today |

The driver itself is the same binary and the same code path in all four;
the deployment changes one option (`options.transport`), the image changes
what SONiC can do when asked.

## 1. Deployment A: the bridge over SSH

The original shape, and the only one for every other driver. The bridge
runs in the release container (or as a binary on any host), logs into the
switch as its login user over SSH with a key, reads the Redis databases in
one `exec` per poll and writes the controller's config through SONiC's
`config` CLI and `sonic-db-cli`. [`docs/install.md`](../install.md) §1-6 is
the procedure; the device entry is the one in
[`deploy/config.example.yaml`](../../deploy/config.example.yaml):

```yaml
  - name: sw1
    driver: sonic
    ssh: admin@192.0.2.10            # the login user (passwordless sudo) and the switch's in-band address
    model: USWF07D
    options:
      ssh_key: /etc/disunified/ssh/id_ed25519
      known_hosts: /etc/disunified/ssh/known_hosts
      # native_vlan: "4094"          # the VLAN that stands in for UniFi's VLAN 1 (SONiC cannot create VLAN 1)
      # l2d: "off"                   # no RSTP / loop protection through disunified-l2d (on by default)
```

What is specific to this shape:

- **Reachability is proven by dialling.** An address move from the
  controller's IP Settings is confirmed by opening a fresh SSH connection to
  the new address before the old one is removed; if it does not answer the
  move is undone. The first spanning-tree enable is confirmed the same way
  (a fresh connection cancels l2d's dead-man timer).
- **The bridge is behind the switch.** If the switch is the core of the
  LAN, the bridge's own path to it and to the controller runs through the
  links it configures. A bad push, a reboot of the bridge's host, or a boot
  order that brings the host up before the switch shows the switch as
  offline in UniFi and leaves nothing to push to it.
- **One SSH login per cycle** (about every 70 s), as the login user. The
  controller's pushed SSH keys land in that user's `authorized_keys` in a
  marked block, beside the bridge's own key.
- Image upgrades on the switch do not touch the bridge; l2d and mstpd are
  re-installed by the driver when it finds them missing.

Verified live in this shape: the SONiC virtual switch on 202505 (every
control feature, [`sonic.md`](sonic.md) §6.6), the DX010 on 202505 (read
side, locate, forced speed, breakout, restart, RSTP; §10-11.6) and the DX010
on the patched 202511 image (the full retest, §11.8). The replay tests in
`internal/informloop/replay_sonic_*_test.go` run every recorded push through
this transport.

## 2. Deployment B: the bridge on the switch

Since 2026-11-02 the bridge can run **on** the SONiC switch, as a host-level
systemd service installed from a `.deb`, executing the driver's commands
locally as the login user. The design and what ran live are in
[`sonic-on-switch.md`](sonic-on-switch.md); the procedure is
[`docs/install.md`](../install.md) §7. The config differs in one option:

```yaml
  - name: sw1
    driver: sonic
    ssh: admin@192.0.2.10            # the user the service runs as, and the switch's in-band address
    model: USWF07D
    options:
      transport: local               # exec on this switch; ssh_key / known_hosts are ignored
      # probe: 192.0.2.1:8080        # host:port the switch must keep reaching (default: the controller's inform address)
```

What is specific to this shape:

- **No dependency on anything the switch carries.** The bridge reboots with
  the switch and resumes from its saved state in `/host/disunified`, as a
  real UniFi switch's agent does. A UniFi Restart is `sudo reboot` on the
  box the bridge runs on; the controller shows it offline for the two
  minutes the DX010 takes to boot and then connected again.
- **Reachability is proven by a TCP probe, not a dial.** There is nothing
  to dial from the switch itself, so an address move is confirmed by a TCP
  connection to `options.probe` (the controller's inform host and port by
  default) bound to the new address as its source; if it fails the move is
  undone the same way. The first STP enable is confirmed by the same probe,
  unbound. The driver stays UniFi-free: `main` fills the probe from the
  inform URL.
- **Everything persistent lives in `/host/disunified`** (the switch's one
  directory that survives an image install): `config.yaml`, `env` (the
  controller API key, mode 0600), `state/`, the reply log, and the
  installed `.deb` itself. The binary and the unit are in the image's
  overlay and vanish with an image install; SONiC's own first-boot
  mechanism puts them back (below).
- **Image upgrades carry the bridge.** On its first boot a new SONiC image
  installs every `.deb` found in its `platform/<platform>/` directory
  (`/etc/rc.local`). The upgrade script copies the installed deb from
  `/host/disunified` there before the reboot, so the bridge comes back with
  its config and state. Updating the bridge is `dpkg -i` of a new deb and
  replacing that one file; no image rebuild, no reboot.
- **The switch's own TCP stack is now in the path.** This is the one thing
  the SSH shape never exercised: the inform is a 60 KB POST sent *from* the
  switch, and SONiC's VLAN netdevs default to MTU 9100 while the ports may
  run 1500. The driver now keeps every managed VLAN's netdev MTU equal to
  the ports' (in CONFIG_DB and in the kernel) and the loop reconciles every
  cycle whether or not the controller answered, so a swss restart that
  recreates the netdevs at 9100 is repaired on the next cycle
  ([`sonic.md`](sonic.md) §12).
- The bridge's secrets sit on the switch. The login user already has
  passwordless root, so the switch holds nothing it could not already reach.
- The journal is the log: `sudo journalctl -u disunified -t disunified`.

**Moving a switch between the two shapes** needs no re-adoption: the
adopted key is `state/<name>/device.json`. Stop the old bridge (remove the
device's entry from the container's config and restart it, or stop the
service), copy that file to the new place, start the new bridge. One running
bridge per adopted key. The DX010 was moved from the container to the switch
on 2026-11-02 this way ([`sonic-on-switch.md`](sonic-on-switch.md) §10a);
the reverse is the same steps backwards.

**Which to choose.** Over SSH for a switch at the edge, for several switches
from one bridge, for a first try (`-collect-once` from anywhere), and for any
non-SONiC device. On the switch for a core switch, or wherever the bridge
must not depend on the links it manages. The driver's features are the same
in both.

## 3. The image: stock, or patched

The driver targets community SONiC as published: the Azure build pipeline's
latest `202505` (or `202411`, `master`) image for the switch's platform
([`sonic.md`](sonic.md) §1). Everything in §1 and §2 above works on a stock
image. Two things do not, and one platform needs help:

| Capability in UniFi | Stock 202505 | Stock 202511 | Patched 202511 (this repo's image) |
|---|---|---|---|
| Ports, names, speed, FEC, VLANs, LAG, jumbo, storm control, NTP, syslog, SNMP, time zone, hostname, SSH keys, Restart, locate, breakout, static IP | yes | yes | yes |
| RSTP, BPDU guard, edge ports, loop protection | yes, through **disunified-l2d** (mstpd built on the switch by the driver; no patch, §4) | yes, the same | yes, the same |
| Rogue DHCP Server Detection, switch IP ACL rules | yes, the driver's own ACL tables (no patch, §4) | yes | yes |
| **Port mirroring** | yes: 202505's orchagent binds without asking (verified on the VS; expected on the DX010, not run there) | **no on Broadcom SAIs that answer the capability query wrongly** (the DX010's legacy-TH SAI, Tomahawk-5 per upstream #28139): the CLI refuses, orchagent creates the session unbound; the driver refuses with a note | yes: `sonic-swss 0002` takes the gate out (the hardware binds, proven by probe) -- image built, install pending on the DX010 |
| **IGMP snooping** | no: community SONiC has no snooping daemon; the driver logs the keys | no | the Micas l2mcd series built in and verified on the virtual switch; **not yet enabled on the DX010, and the driver does not write the keys yet** (§3.3) |
| **The DX010 itself on 202511** | n/a | **does not run**: orchagent's telemetry query crashes the Tomahawk-1 SAI, the CPLD driver panics kernel 6.12 on its first write, pmon has no platform (no fans, PSUs, temperatures) | runs: the four platform patches (§3.1) |
| Port isolation, 802.1X, LLDP-MED off, MAC ACL rules | no (refused with a log; MAC rules need an orchagent patch no one has written) | no | no |

The patched image is **not** required for the driver. It is required for
IGMP snooping anywhere, for port mirroring on a 202511 Broadcom image whose
SAI lies about it, and for a Tomahawk-1 on 202511 at all.

### 3.1 The patch series, by what it enables

The series is maintained as a build kit outside this repository
(`series/`, one directory per SONiC repository with a `series` file,
`BASES` pinning the commit each applies to, `CONFLICTS.md` the rebase
record, `build.sh` the unattended build, `dry-run.sh` the checks that need
no build host, `sonic-upgrade.sh` the install script). Every patch applies
with `git am` to the current `202511` head of its repository. The kit is
described in [`sonic.md`](sonic.md) §8.6 and is not published yet; it is
small (about 600 KB of patches and scripts) and carries no site information.

**Running a Tomahawk-1 (the DX010) on 202511 at all** -- platform patches,
two of them upstream's own fixes carried unchanged:

| Patch | What | Why it is needed | Upstream |
|---|---|---|---|
| sonic-buildimage 0004 | Seastone `sai.profile`: `SAI_STATS_ST_CAPABILITY_SUPPORTED=0`, `SAI_STATS_EXT_SWITCH_SUPPORTED=0` | 202511's orchagent asks the SAI for streaming-telemetry capability at start; the Broadcom SAI null-dereferences on a Tomahawk-1 (syncd SIGSEGV, swss crash-loop) | sonic-sairedis #1788 (the crash), #1795 (the opt-out); the DX010 part of sonic-buildimage #28160 |
| sonic-buildimage 0005 | the DX010 on the **legacy-TH SAI** 13.2.1.100 (`platform_asic broadcom-legacy-th`) and the legacy-TH BDE modules in the ONIE image; the switch then takes `sonic-broadcom-legacy-th.bin` | 202511 moved the XGS SAI to 14.3 and split Tomahawk-1 off to a separate train, but only the Arista 7060CX-32S was moved; the DX010 (the same chip) would get 14.3, which nobody reports running on a TH1 | sonic-buildimage #25878, #26136 (the split), #28125 (the BDE modules) |
| sonic-buildimage 0007 | the Celestica `sonic_platform` wheels installed into the device directory again | 202511's trixie rework of the Celestica packaging dropped them; pmon then has no platform API, so `show platform fan/psustatus/temperature/syseeprom` report nothing and the hardware tables in UniFi are empty | **sonic-buildimage #27355** ("Restore Celestica platform API wheels", open; fixes #27291), cherry-picked unchanged |
| sonic-buildimage 0011 | `dx010_cpld.c` `setreg_store`: `char clone[count + 1]` | `strcpy` into `char clone[count]` is one byte short; kernel 6.12's FORTIFY_SOURCE traps it (`kernel BUG at lib/string_helpers.c:1040`) on the first CPLD register write, which is the platform watchdog's disarm at boot: the switch never finishes booting 202511 | **sonic-buildimage #26983** ("Fix VLA size in setreg_store for DX010", open; issue #26885), cherry-picked unchanged |

**IGMP snooping** -- the Micas Networks series (unmerged upstream drafts,
2025-26) plus what building and testing it needed:

| Patch | What | Upstream |
|---|---|---|
| sonic-buildimage 0001 | `docker-l2mcd`, build rules, service, the CoPP trap group for IGMP/MLD, YANG models | sonic-buildimage #24327 (draft), squashed |
| sonic-buildimage 0002 | the daemon submodule pointed at `jasper-hou-micas/sonic-l2mcd-micas@651a13b` (the PR names a repository that does not exist and adds no gitlink); two rule fixes | -- |
| sonic-buildimage 0003 | the PR's unrelated changes removed (gNMI write access, gNMI/x509 YANG, a default `eth0 admin up`) | -- |
| sonic-buildimage 0006 | CoPP as UniFi's own SONiC switches do it: IGMP only, no MLD, 128/160 pps, queue 1 | the ECS's own `copp_cfg`, described in [`sonic.md`](sonic.md) §8.11 |
| sonic-buildimage 0010 | the two snooping YANG models without imports of `sonic-vlan`/`port`/`portchannel` | -- (those imports changed the generic config updater's behaviour and broke 7 sonic-utilities tests that run in the image build) |
| sonic-swss 0001 | `l2mcorch`, `l2mcmgrd` (with upstream's fix of the reviewed `memcpy` bug) | sonic-swss #3950 (draft), squashed |
| sonic-swss-common 0001 | the `schema.h` L2MC and MLD_L2MC table names | the schema part of sonic-swss-common #1093 |
| sonic-utilities 0001 | `config igmp-snooping …`, `show igmp-snooping …`, the MLD equivalents | sonic-utilities #4100 (draft), squashed |
| sonic-l2mcd 0001 | the daemon's own copy of the `memcpy`-from-`c_str()` bug (`l2mcd_sync.cpp`), which upstream did not fix | -- |
| sonic-l2mcd 0002-0007 | the receive path hardened: malformed packets dropped, counted and rate-limited in the log; the IGMPv3 and MLDv2 parsers held to the message length and the bytes received; a one-past read in the group state machine; one receive socket per port instead of one per resend | -- ([`sonic.md`](sonic.md) §8.13; fuzzed under ASan/UBSan/libFuzzer, the harness is in the kit) |

**Building 202511 with those in** -- not features, fixes the build itself
needed:

| Patch | What | Upstream |
|---|---|---|
| sonic-buildimage 0008 | swss-common, sonic-mgmt-common, sonic-gnmi and sonic-mgmt-framework built against libyang 3 (as master and 202605 do); with libyang-dev 1 and 3 both in the slave, the old rules let a compile meet the wrong one | sonic-buildimage #26165 (the symptom) |
| sonic-buildimage 0009 | the `libyang3-dev-done` ordering gate retired (nothing installs libyang-dev 1 any more) | master #27629 |
| sonic-swss-common 0002 | DefaultValueProvider on libyang 3, with the libyang 1 fallback kept | upstream `1431c3d` |
| sonic-mgmt-common 0001 | CVL on libyang 3 | sonic-mgmt-common #157 (`cdb612e`) |

**Port mirroring** -- one patch, ours:

| Patch | What | Upstream |
|---|---|---|
| sonic-swss 0002 | `SwitchOrch::querySwitchPortMirrorCapability` records `PORT_INGRESS/EGRESS_MIRROR_CAPABLE` as `true` when the SAI answers `set_implemented=false`, the same as the existing branch for a failed query. MirrorOrch and the CLI follow the rows, so the driver's existing SPAN path (`config mirror_session span add … both`) runs; a set the SAI really refuses is logged, not fatal | the gate: sonic-swss #3934 (202511: #4137) and sonic-utilities #4089 (#4204, #4323). The same false answer on Tomahawk-5 is **sonic-buildimage #28139**, with Broadcom's fix in SAI 15.2/14.3 -- trains that do not include the legacy-TH 13.2.1.x the DX010 runs; the proof that the legacy-TH SAI binds a port anyway (a `SAI_PORT_ATTR_INGRESS_MIRROR_SESSION` set pushed through ASIC_DB by hand answered `SAI_STATUS_SUCCESS` and `bcmcmd "mirror show"` listed it) is in [`sonic.md`](sonic.md) §11.9-11.10 and in the comments on #28139. Not proposed upstream: it defeats the query for every platform |

### 3.2 Getting the image onto a switch

The build takes a Debian 12 x86_64 host with Docker, 16 cores, 64 GB and
about 120 GB of disk per platform, and 3-4.5 hours for the Broadcom images
([`sonic.md`](sonic.md) §8.6). The build id is `igmp-202511.<number>-<sha>`;
record it with the image and keep the `.bin` and its sha256 off the switch,
because the pipeline's redirects are rolling and a local build exists
nowhere else.

The install uses the kit's `sonic-upgrade.sh`, which SONiC's own
`sonic-installer` does not replace:

- `arm <image> <sha256>` installs the image beside the running one, keeps
  the running one as GRUB's default, carries the SSH host keys, the login
  user's `authorized_keys`, the static `mstpd`/`mstpctl` binaries and (for
  an on-switch bridge) the installed `.deb` into the new image's overlay,
  and boots the new image **once** under a 20-minute dead-man timer; if
  nothing confirms, the switch is back on the old image after the timer.
- `confirm`, run on the new image once it is checked, makes it the default
  and disarms the timer.
- **Installing deletes every image but the running one and the new one.**
  That is the image's own `install.sh`, not the script; the DX010's factory
  image went that way on the first install (only ONIE can put one back).
  `arm` names what an install will delete before it starts. Never run
  `sonic-installer cleanup`.

The arrival-day procedure for a DX010 is [`sonic.md`](sonic.md) §3; what the
first install on the hardware exposed (two failed boots and their cause,
the CPLD watchdog, the trace that found it) is §8.14.

### 3.3 Status on the DX010, 2026-11-02

- Running: `igmp-202511.20261101-22cac574e` (the series through
  sonic-buildimage 0010, with the CPLD fix as a hand-swapped platform deb
  equivalent to 0011), installed 2026-11-02, the full driver retest passed
  on it (§11.8), adopted as `USWF07D`, the bridge on the switch (§2).
- Built, install pending: `igmp-202511.20261102-e53777446`, the same plus
  sonic-swss 0002 (port mirroring) and 0011 built in. Its read-back plan is
  §11.10.
- IGMP snooping: in the image, `l2mcd` shipped disabled, verified on the
  virtual switch with two test hosts (§8.14 V1-V7); the hardware steps
  (§8.9 H2-H5) and the driver's write path for the controller's
  `igmp_*` keys (§8.10) are not done, so the driver does not claim the
  feature and UniFi does not offer it yet.
- Upstream: #27355 and #26983 are carried unchanged and have now run on a
  DX010; confirmations there are due. The mirror finding is on #28139.

## 4. What is not a patch

Two UniFi features that community SONiC lacks are supplied by the driver
itself and work on any image:

- **RSTP, BPDU guard, edge ports and loop protection** run through
  **disunified-l2d**, a small daemon the driver installs on the switch
  (`l2d.py`, a systemd unit, mstpd built from source on the switch and
  carried across image installs by the upgrade script). It runs mstpd on
  the kernel `Bridge` and mirrors its port states into orchagent's stock
  `StpOrch`, one STP instance per VLAN; loop protection watches FDB moves.
  On by default, `options.l2d: off` opts out. [`sonic.md`](sonic.md) §11.5.
- **Rogue DHCP Server Detection and switch IP ACL rules** are the driver's
  own L3 ACL tables (`DUI_DHCP_GUARD`, `DUI_IP_ACL`): the tables through
  `config acl add table` / `remove table` (never `config acl update`,
  whose loader adds a drop-all rule), the rules as a diff of the
  `ACL_RULE|DUI_*` hashes in CONFIG_DB, orchagent's verdict read back from
  STATE_DB. [`sonic.md`](sonic.md) §11.7.

MAC ACL rules would need an orchagent patch (§11.7.4) and are refused.
