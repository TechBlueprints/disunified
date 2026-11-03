# The bridge on the switch itself (SONiC) -- specification

Written 2026-11-02, before any code. The sonic driver's own document is
[`sonic.md`](sonic.md); this one is the plan for running the bridge **on**
the SONiC switch it manages, as a host-level systemd service, instead of in
a container somewhere behind that switch. The bridge was deployed in the
one container on the Podman host (`CLAUDE.md` §4); once the SONiC switch is
the LAN's core, a bridge that reaches it through the very links it
configures is a dependency the operator wants gone.

The verification plan (§9) and the cutover (§10) are written for the
Celestica DX010 on the custom 202511 image (`sonic.md` §8.14, §11.8); the
design holds for any community SONiC switch.

## 0. What is there to build on (read on the DX010, 2026-11-02, read-only)

- The login user `admin` (uid 1000, groups `sudo`, `docker`, `redis`) has
  `%sudo ALL=(ALL:ALL) NOPASSWD: ALL`. Every write the driver makes already
  carries its own `sudo`; `collect.py` reads Redis as the login user
  through the `redis` group's socket.
- The image is Debian 13 (trixie), systemd 257, Python 3.13, `dpkg` and
  `apt-get` present. 3.9 GB of RAM (1.4 GB available with every SONiC
  container running); `/host` (ext4, `rw,relatime`, no `noexec`) has
  9.6 GB free; `/var/log` is a 3.9 GB loop image; journald is capped at
  `SystemMaxUse=50M`.
- **Only `/host` and `/etc/sonic` persist across an image install.** The
  rest of the filesystem is the running image's overlay
  (`/host/image-<version>/rw`), which a new image does not see.
- **SONiC installs extra debs on an image's first boot.** `/etc/rc.local`
  (lines 324-339 on this image, the `firsttime` block) runs `apt-get -y
  install /host/image-$SONIC_VERSION/platform/$platform/*.deb` -- a glob,
  with `dpkg -i` as the fallback when `platform/common/Packages.gz` is
  missing. A deb that is not in `Packages.gz` is installed from the file
  itself. **Proven on this switch** on 2026-11-02, when the
  `platform-modules-dx010 1.0+dx010cpld1` deb was placed there by hand and
  installed by the first boot (`sonic.md` §8.14; the `dx010-cpld-fix`
  hand-over).
- The build kit's `sonic-upgrade.sh` (`~/techblueprints/sonic/igmp-build/`,
  a copy at `/host/sonic-upgrade.sh` on the switch) already plants files
  into the new image's overlay before the reboot: the dead-man timer, the
  SSH host keys, admin's `authorized_keys` and password, and the static
  `mstpd`/`mstpctl` binaries. Everything in §5 is one more such step.
- The driver reaches the switch only through `sshrun.Runner` --
  `Run(ctx, command, stdin)` and `Close()` -- held in `Collector.r`, swapped
  by an address move. Three places assume a dialled address: `dial.go`
  (`run`'s fallback to other known addresses on a dial error, `Redial`,
  `adoptDial`), `address.go` (`ApplyAddress` proves a new address by
  re-dialling it) and `l2.go` (`confirmL2` cancels the first-enable dead-man
  over a fresh connection). `acl.go` holds an IP ACL set that would cut the
  bridge's SSH session (`sshFrom`/`sshTo` from `collect.py`'s `ssh`
  section, which reads `SSH_CONNECTION`).

## 1. Decisions

| # | Question | Decision |
|---|---|---|
| 1 | How does the driver run commands on-box? | A second `sshrun.Runner`, `sshrun.Local`, that execs the command through `bash -c` with the given stdin, as the service's own user. Selected per device by `options.transport: local`; `ssh` (the default) is unchanged. §2 |
| 2 | What does `ssh: admin@<address>` mean then? | The same two things it means now, minus the dialling: the user the driver acts as (it must be the process's user, or `Open` refuses) and the switch's own in-band address -- the address the loop reports, the address controller's starting point, and what the bridge "dials" in every log line. §2 |
| 3 | How is a new address proven when there is nothing to dial? | A TCP connect to a probe address (`options.probe`, `host:port`; `main` fills it from the controller's inform URL when the device sets none), **bound to the new address as its source**. Fails: the same `undoAddress`. §3 |
| 4 | And the first STP enable's dead-man? | The same probe, unbound: the switch still reaches the controller's host and port, so the uplink carries traffic. Cancel the timer; otherwise it fires as today. §3 |
| 5 | What about an ACL set that would cut the bridge off? | Same hold as today, with the probe's host as the far end and the switch's own addresses as the near end. §3 |
| 6 | Where do binary, config, secrets and state live? | The binary and unit in the image (from a deb, §4); `config.yaml`, `env` (0600) and `state/` in `/host/disunified`, owned by the login user, mode 0700; `/etc/disunified` is a symlink to it so the unit and the docs stay generic. §4 |
| 7 | How does the service survive an image install? | **The deb rides SONiC's own first-boot install**: `sonic-upgrade.sh arm` copies `/host/disunified/disunified_*.deb` into the new image's `platform/<platform>/` directory, and `rc.local` installs it on the first boot; the deb's postinst re-creates the symlink and the drop-in and enables the unit. Not the image kit: §5 says why. |
| 8 | Packaging | `deploy/deb/` + `scripts/build-deb.sh` build a `.deb` for linux/amd64 and linux/arm64; the release workflow attaches both to the GitHub release next to the tarballs. §6 |
| 9 | Reboot requested by the controller | Unchanged: `sudo reboot` -- the service goes down with the box and `Restart=always` brings it back after boot, resuming from `state/`. §7 |
| 10 | Cutover | Container's `dx010` entry removed and the container restarted, state copied to the switch, service started: same MAC, same key, no re-adoption. §10 |

## 2. The local runner

`internal/sshrun/local.go`:

```go
// Local runs commands on this host, for a bridge that lives on the device
// it manages. Run execs `bash -c command` with stdin on its standard
// input, as the process's own user; a driver's `sudo …` is sudo on the box.
type Local struct {
	Shell string // "" = /bin/bash
}
func (l *Local) Run(ctx context.Context, command, stdin string) (string, error)
func (l *Local) Close() error // nothing to close
```

- Output and errors have the same shape as `SSH.Run`: stdout on success;
  on failure `"<first six words of the command>...: <err>: <stderr | stdout tail>"`
  through the same `failureOutput`, so every driver test that matches an
  error string holds for both runners.
- A cancelled or expired context kills the process group (`Setpgid`,
  `syscall.Kill(-pid, SIGKILL)`), as `SSH.Run` sends `SIGKILL` to the
  session, and returns `ctx.Err()`.
- `SessionDropped` already returns false for the `ssh …`/`known_hosts …`
  prefixes and true for a deadline; a local runner never produces the
  former. A `sudo reboot` on-box returns 0 before the box goes down (§7).
- Environment: the service's own (`PATH` with `/usr/local/bin`, `HOME` for
  the authorized_keys script, no `SSH_CONNECTION`). The unit sets nothing
  the SSH session would not have.
- Tests (`local_test.go`, no SONiC needed): stdout, stdin passed through
  (`cat`), a failing command's error string carries the exit status and
  stderr, a context deadline kills a `sleep`, `Close` is a no-op.

`internal/drivers/sonic/driver.go`, `Open`:

- `options.transport`: `ssh` (default) or `local`; anything else refused.
- `local`: the user part of `ssh:` must equal `user.Current().Username`
  (the keys script writes `$HOME/.ssh/authorized_keys`, `collect.py` reads
  the managed block from `$HOME`, and the switch's `admin` is the login
  user the controller's SSH keys are for); otherwise `Open` fails:
  `sonic: transport local runs as <x>, but ssh: names <y>; the service must
  run as the login user`. `ssh_key` and `known_hosts` are ignored with a
  log line if set.
- `c.r = &sshrun.Local{}`; `c.Redial = func(string) sshrun.Runner { return
  &sshrun.Local{} }` (a "fresh connection" is a fresh process; what it
  proves is the probe's job, §3); `c.Dial`, `c.configHost` and the persisted
  `dial` in `sonic-managed.json` work as before, so `CurrentAddress` reports
  the configured address, then the one a move adopted. `dial.go`'s
  fallback never triggers: `isDialError` matches the SSH prefixes only.
- `c.Probe` (§3) is set from `options.probe`; with `transport: local` and
  no probe, `Open` refuses (`set options.probe host:port, or let the bridge
  fill it from the controller`) -- `main` always fills it, so only a
  hand-run `-collect-once` can hit this.
- `Describe()` mentions both transports; the doc comment lists the options.

`cmd/disunified/main.go`:

- `opts["probe"]` defaults to the inform URL's `host:port` (`192.0.2.1:8080`
  for `controller.host: 192.0.2.1`) for every driver, unless the device's
  options set it. Nothing UniFi-specific crosses into the driver: it is "an
  address this bridge must keep reaching".
- `-collect-once` honours `-config`: each device is opened and `Start`ed
  and its snapshot printed, nothing informed -- the way to try the local
  runner on the real switch before the cutover (§9) without touching the
  adopted key.

## 3. Proving reachability without a dial

`internal/drivers/sonic/probe.go`:

```go
// Probe says whether the switch reaches addr (host:port, TCP) -- from
// fromIP when it is set (a candidate address for the switch itself), else
// from whatever the kernel picks. nil on the SSH transport, where a fresh
// dial is the proof.
type probeFunc func(ctx context.Context, addr, fromIP string) error
func tcpProbe(ctx context.Context, addr, fromIP string) error  // net.Dialer{LocalAddr: &net.TCPAddr{IP: fromIP}}, 5 s, Close
```

The Collector gains `Probe probeFunc` and `ProbeAddr string`. Three call
sites:

1. **`ApplyAddress`.** Today: add the new address → `nr := redial(d.IP)`;
   `nr.Run("true")` → finish over `nr` → `adoptDial`. New: after the add,
   when `c.Probe != nil`, `c.Probe(ctx, c.ProbeAddr, d.IP)`; a failure takes
   the existing `undoAddress` path with the reason `the switch does not
   reach <probe> from <new address>`. On success the finish batch runs
   through `nr` (local: the same runner) exactly as now --
   `promote_secondaries`, remove the old address, verify the new one
   survived, stale route, `config save -y` -- then `adoptDial`. Nothing in
   the batch changes. What the probe proves: the new address has a route
   to the controller's host and port and the far side answers a SYN from
   it; the old address goes only after that. What it does not prove: that
   the controller accepts an inform from it (the same gap the SSH dial has).
   **The bridge's own inform sockets** are per-request HTTP connections
   from the kernel's chosen source; after the old address is removed they
   come from the new one; the loop already reports `CurrentAddress`.
2. **`confirmL2`.** Today: redial, `l2CheckCommand` must print `running`,
   then `l2DeadmanCancel` over that connection. New: the same two commands
   on a fresh runner, and between them, when `c.Probe != nil`,
   `c.Probe(ctx, c.ProbeAddr, "")`; a failure is retried like a failed dial
   (10 tries, `L2Wait` apart) and then the dead-man is left armed with the
   reason in the error. Unbound: the point is that the uplink still carries
   traffic after STP's first enable, not which address it came from.
3. **`aclWant` (`acl.go`).** `cutsManagement(d.IPACLs, from, dial)` is fed
   `from = host of ProbeAddr` when `sshFrom` is empty and `ProbeAddr` set;
   `dial` is `dialAddrs(CurrentAddress())` as now. The controller's replies
   arrive on the uplink's ingress, so a rule set that would drop them from
   the controller's address to the switch's own is held with the same note.

Tests, all on the existing VS/DX010 fixtures with `FixtureRunner`:

- `ApplyAddress` with `Probe` returning nil: the move's batches as the
  existing `TestApplyAddress*` expect, **no entry in `FixtureRunner.Dials`**
  (the probe replaced the dial), `CurrentAddress` is the new address.
- `Probe` returning an error: `undoAddress`'s batch, the old address kept,
  the error names the probe; the fixture's `IgnorePromote` path unchanged.
- The probe is called with `fromIP` = the new address (a recording probe).
- `confirmL2` with a failing probe: `l2DeadmanCancel` never runs, the error
  says so; with a passing one the timer is cancelled as today.
- `cutsManagement` gets the probe host when `sshFrom` is empty.
- `tcpProbe` itself against a `net.Listener` on 127.0.0.1: success bound to
  `127.0.0.1`, failure on a closed port, failure bound to an address the
  host does not have (`bind: can't assign requested address`).
- The replay tests (`replay_sonic_live_test.go`, `TestReplaySonicLabSession`)
  run unchanged on the SSH transport.

## 4. Files on the switch, the unit, the deb

The deb is generic Debian packaging (`deploy/deb/`), and its postinst does
two SONiC-specific things when it sees `/host/machine.conf`.

| Path | From | Survives an image install? |
|---|---|---|
| `/usr/sbin/disunified` | the deb | no -- reinstalled (§5) |
| `/usr/lib/systemd/system/disunified.service` | the deb | no -- reinstalled |
| `/etc/systemd/system/disunified.service.d/sonic.conf` (`User=`/`Group=` the login user) | postinst on SONiC | no -- postinst re-creates it |
| `/etc/disunified` → `/host/disunified` (symlink) | postinst on SONiC | no -- postinst re-creates it |
| `/host/disunified/config.yaml` | the operator (§8) | **yes** |
| `/host/disunified/env` (0600: the API key) | the operator | **yes** |
| `/host/disunified/disunified_<version>_<arch>.deb` | the operator; what §5 carries | **yes** |
| `/host/disunified/state/<name>/device.json`, `sonic-managed.json`, `inform-log/<name>/` | the bridge (`state_dir: /host/disunified`) | **yes** |
| `/etc/sonic/disunified/l2.json`, `/usr/local/sbin/disunified-l2d`, mstpd | the driver, as today (`sonic.md` §11.5) | l2.json yes; the rest as today (mstpd carried by `arm`, l2d reinstalled by the driver) |

The unit (`deploy/deb/disunified.service`):

```ini
[Unit]
Description=disunified bridge: this device as an adopted UniFi switch
Documentation=https://github.com/TechBlueprints/disunified
# On SONiC: the Redis databases and swss must be up before a collect can
# succeed; Start retries with backoff anyway (main.go).
After=network-online.target database.service swss.service
Wants=network-online.target

[Service]
ExecStart=/usr/sbin/disunified -config /etc/disunified/config.yaml
EnvironmentFile=-/etc/disunified/env
WorkingDirectory=/etc/disunified
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

No sandboxing directives (`ProtectSystem`, `NoNewPrivileges`): the
driver's writes are `sudo` children of the service and share its mount
namespace, so a read-only `/etc` or `/usr` would break `config save`
(`/etc/sonic/config_db.json`) and the l2d installs under `/usr/local/sbin`
and `/etc/systemd/system`. The service's own user is the confinement, the
same as an SSH login.

`deploy/deb/postinst` (sh, runs on install and upgrade):

```sh
if [ -f /host/machine.conf ]; then            # a SONiC host: persist in /host
    u=admin                                   # SONiC's login user
    [ -d /host/disunified ] || install -d -m 0700 -o "$u" -g "$u" /host/disunified
    [ -e /etc/disunified ] || ln -s /host/disunified /etc/disunified
    install -d /etc/systemd/system/disunified.service.d
    printf '[Service]\nUser=%s\nGroup=%s\n' "$u" "$u" > /etc/systemd/system/disunified.service.d/sonic.conf
fi
systemctl daemon-reload
systemctl enable disunified >/dev/null 2>&1 || true
# Start only with a config in place: a fresh install has none yet.
if [ -f /etc/disunified/config.yaml ]; then systemctl restart disunified; fi
```

As built (`deploy/deb/postinst`), the script acts only on `configure`,
reloads and restarts only when systemd is running (`/run/systemd/system`),
and restarts with `--no-block`: on an image's first boot it runs inside
`rc.local`, and a blocking restart of a unit ordered after swss could wait
on rc.local itself. The unit ships in `/usr/lib/systemd/system` (trixie is
merged-`/usr`).

`prerm` stops the service on remove; `postrm purge` removes the drop-in and
the symlink, never `/host/disunified`. On a non-SONiC Debian the deb is a
plain service as root with `/etc/disunified/` as a directory the operator
creates (the arista-eos and proxmox drivers over SSH would run this way;
not verified here, documented as such).

`deploy/deb/control`:

```
Package: disunified
Version: <from the build>
Architecture: amd64 | arm64
Maintainer: TechBlueprints <https://github.com/TechBlueprints/disunified>
Section: net
Priority: optional
Depends: systemd
Description: presents a non-UniFi device to a UniFi Network controller as an adopted UniFi switch
 Independent project, not affiliated with or endorsed by Ubiquiti Inc.
 On a SONiC switch, run it on the switch itself: docs/install.md.
```

The version is the tag without `v` (`1.2.3`; a prerelease `v1.2.3-rc1`
becomes `1.2.3~rc1`, below `1.2.3`), `<tag>+<yyyymmdd>.<sha>` for a build
after a tag, or `0.0.0+<yyyymmdd>.<sha>` when there is no tag at all
(there is none yet; Debian versions sort these below any `1.x`). `-X main.buildVersion` carries the same string.

## 5. Surviving an image install: why rc.local's first boot, not the image kit

The two options the brief named:

- **(a) a `disunified` deb in the image series** (`SONIC_COPY_DEBS` +
  `$(SONIC_ONE_IMAGE)_INSTALLS` in a kit patch, the deb file checked into
  the kit). It works, but every bridge release then means a new image
  series entry and, to take effect, an image rebuild (hours on VM 116) and
  an install (a reboot of the core switch). A bridge update that needs
  neither becomes one that needs both.
- **(b) the upgrade script plants the unit into the new image's overlay.**
  The unit alone is not enough: the binary is in the overlay too.

The decision is a third form of (b) that is SONiC's own mechanism:

- `/host/disunified/disunified_<version>_<arch>.deb` is the installed
  package, kept beside the config. Updating the bridge is `dpkg -i` of a
  new deb and replacing that file (`scripts/build-deb.sh` prints the two
  commands); nothing else.
- `sonic-upgrade.sh arm` gains, next to the host keys and mstpd:

  ```sh
  # the bridge, when it runs on this switch: SONiC installs every deb in the
  # new image's platform/<platform>/ on its first boot (rc.local), so the
  # package comes back with the image; its config and state are in /host
  d=$(ls /host/disunified/disunified_*.deb 2>/dev/null | tail -n 1)
  if [ -n "$d" ]; then
    p=$(sed -n 's/^onie_platform=//p' /host/machine.conf)   # x86_64-cel_seastone-r0, as rc.local reads it
    sudo install -m 0644 "$d" "/host/image-${new#SONiC-OS-}/platform/$p/"
    say "the bridge's $d will be installed by the new image's first boot"
  fi
  ```

  and `confirm` checks `dpkg-query -W disunified` and `systemctl is-active
  disunified` and says so. The `platform/<platform>` directory exists in
  every image (it holds the platform-modules deb symlink); `rc.local`
  installs the glob with `apt-get` from the file path (`Packages.gz` is
  not consulted for an explicit file -- proven with the CPLD deb).
- The dead-man still covers the whole first boot: if the bridge did not
  come up on the new image, the switch is back on the old one after the
  timer, bridge included.
- The image kit stays as it is. If a bridge version is ever to be baked
  into an image (a switch with no `/host` copy), (a) is the way and is
  one rules file; not done now.

What else lived in the overlay: the fan floor file, until series patch
0012 (2026-11-03) put the 20 % floor into the image's own `fancontrol-F2B`;
an image built without it still needs the floor re-applied by hand. The
l2d pieces are reinstalled by the driver within a cycle.

## 6. Packaging and the release

- `scripts/build-deb.sh [amd64|arm64]`: `CGO_ENABLED=0 GOOS=linux GOARCH=$arch
  go build -trimpath -ldflags "-s -w -X main.buildVersion=$VERSION"`, stage
  `usr/sbin/disunified`, `lib/systemd/system/disunified.service`,
  `usr/share/doc/disunified/{config.example.yaml,env.example,copyright}`,
  `DEBIAN/{control,postinst,prerm,postrm}` (0755 for the scripts), then
  `dpkg-deb --root-owner-group -b`. Needs `dpkg-deb` (Linux; the Mac has
  none -- the trial packs the staged tree on the switch, which has it, or
  in a Debian container on the Podman host). Output `dist/`.
- `.github/workflows/release.yml`, `release` job: after the tarballs, the
  two debs into `dist/` and into `SHA256SUMS`; the release notes gain a
  line: "On a SONiC switch: the `.deb`, docs/install.md §7".
- `.github/workflows/ci.yml`: `scripts/build-deb.sh amd64` runs (Ubuntu
  has `dpkg-deb`) and `dpkg-deb -I`/`-c` list the result, so a broken
  control file fails CI, not a release.
- `deploy/compose.yaml` and the Quadlet unit are untouched.

## 7. What changes for the loop and the driver on-box, and what does not

- **Reboot.** `Rebooter.Reboot` runs `sudo reboot`; locally it returns 0 and
  systemd stops the service seconds later. The loop may or may not get its
  "reload issued" inform out; the controller shows the device offline
  until the bridge is back after boot (2 min on the DX010), as it does for
  a real switch. `Restart=always` and `state/` make the return automatic.
  `sshrun.SessionDropped` is not involved.
- **A write that cuts the uplink** (a VLAN, ACL or STP change) used to cut
  the bridge off the switch too; on-box the bridge keeps its hands on the
  switch and loses only the controller. The guards are the same -- the
  first-push hold, the ACL hold (§3.3), the l2d dead-man (§3.2) -- and the
  on-box bridge can additionally undo an address move it cannot prove (§3.1)
  without ever losing the switch.
- **`sshFrom`/`sshTo`** are empty on-box (no `SSH_CONNECTION`); §3.3 covers
  the one use.
- **`Start`** runs every command once: on-box it also proves the local
  runner, `sudo` and the Redis socket in one go, and the unit's retry loop
  covers a boot where swss is not up yet.
- **Logging** goes to the journal (`journalctl -u disunified`, 50 MB cap
  shared with everything else; the bridge logs a few lines per cycle).
  The reply log `inform-log/<name>/*.ndjson` grows on `/host` with no
  rotation (as in the container; the DX010's size is measured at the
  cutover and noted). The state dir is 0700.
- **The API key** sits on the switch in `env` (0600, the login user's). The
  login user has passwordless root, so nothing is lost that the switch did
  not already hold.
- **Memory**: the bridge is a static Go binary, tens of MB resident; 1.4 GB
  is free on the DX010.
- **The container's SSH key** stays in admin's `authorized_keys` until it is
  removed by hand; the controller's pushed keys (the managed block) are
  untouched by the move.
- **`hostname`** in the config stays what the controller stored at
  adoption; the loop reports it as before.
- Nothing changes for the other drivers or the other devices in the
  container.

## 8. The on-box config (documentation values)

`/host/disunified/config.yaml`:

```yaml
controller:
  host: 192.0.2.1
  api_url: https://unifi.example.net
  api_key_env: DUI_UNIFI_API_KEY
  site: default

state_dir: /host/disunified

devices:
  - name: dx010
    driver: sonic
    ssh: admin@192.0.2.10        # the user the service runs as, and the switch's in-band address
    model: USWF07D
    hostname: dx010
    options:
      transport: local           # exec on this switch; no key, no known_hosts
      native_vlan: "4094"
      l2d: "on"
      # probe: 192.0.2.1:8080    # default: the controller's inform host:port
```

`/host/disunified/env` (0600): `DUI_UNIFI_API_KEY=…` and nothing else.

`docs/install.md` gains "§7. On a SONiC switch" (get the deb, `dpkg -i`,
write the two files, `systemctl start`, the check, and that an image
install re-installs it when the deb is in `/host/disunified`), and
`deploy/config.example.yaml` the `transport`/`probe` comments.
`docs/drivers/sonic.md` gets a §12 pointer to this document and
`internal/drivers/sonic/CLAUDE.md` the facts that cost time.

## 9. Verification before the cutover (nothing touches the adopted key)

1. `go vet ./... && go test ./...` clean with §2-3's tests.
2. The deb built for amd64, `dpkg-deb -I` and `-c` as expected.
3. **On the DX010 (Clint's go; root writes, no effect on Ethernet0 or the
   address):** `scp` the deb to `/host/disunified/`, `sudo dpkg -i` --
   expect the unit installed and enabled, not started (no config), the
   symlink and the drop-in in place, `systemctl cat disunified` showing
   `User=admin`.
4. Write `config.yaml` and `env` under `/host/disunified`, then as admin:
   `disunified -config /host/disunified/config.yaml -collect-once` --
   expect `Start`'s command check to pass over the local runner and the
   snapshot (ports, VLANs, the l2d section, `DHCP false`, the dialled
   address first in `Addresses`) to match what the container's bridge
   reports. No inform is sent.
5. The reachability probe by hand: `disunified` logs the probe address at
   `Open`; a `-collect-once` with `options.probe: 192.0.2.1:1` (a closed
   port) must still collect (the probe is not part of `Start`), so the
   check that it *works* is the fixture test plus one `nc -z` from the
   switch.

Not run live, by the standing rule: an address move. Its on-box path is
fixture-tested (§3). The first STP enable's probe path is exercised live
only if l2d is ever re-enabled from off; today's switch has it on, so the
cutover's first apply finds it `running` and arms nothing.

## 10. Cutover (Clint's word; the controller shows the switch offline for about a minute)

1. On the Podman host: back up `config.yaml`, remove the `dx010` entry,
   `podman restart disunified` (the config is a bind mount; no rebuild).
   The other bridges resume in ~20 s. Check the log: no `[dx010]` lines.
2. Copy the state from the volume's mountpoint
   (`podman volume inspect disunified-state`): `state/dx010/device.json`,
   `state/dx010/sonic-managed.json`, `inform-log/dx010/` → the switch's
   `/host/disunified/state/dx010/` and `/host/disunified/inform-log/dx010/`,
   owner admin, modes 0700/0600. Record the reply log's size.
3. `sudo systemctl start disunified`; `journalctl -u disunified -f`: expect
   `resuming adopted state`, the first inform answered 200/404, CONNECTED.
4. In the controller: the switch connected, same MAC, same uplink (the
   core's port), same overrides; the device's address unchanged.
5. A push round-trip on a test port (the same rule as every live test:
   never a port with a live link): a port name change through the UI →
   `CONFIG_DB` `PORT|…` `description` within one cycle, and back.
6. A reboot through the UI is **not** part of the cutover (it was verified
   on 2026-11-02 with the container bridge; on-box it is §7's path,
   fixture-covered). Left for the next time a reboot is wanted anyway.
7. `local-information/site.md`: the DX010's bridge is on the switch (the
   files, the update command: build the deb, `scp`, `dpkg -i`, replace the
   `/host` copy); the container's `dx010` entry gone; the container's key
   line in admin's `authorized_keys` noted as removable.
   `local-information/follow-ups.md`: the entry closed.

**Rollback** at any step: `sudo systemctl stop disunified` on the switch,
copy `state/dx010/` back into the volume, restore the container's
`config.yaml` from the backup, `podman restart disunified`. The adopted
key is the same file either way; the rule is one running bridge per key.

## 10a. What ran live (2026-11-02)

§9 steps 1-5 all ran: tests and vet clean; the deb staged on the Mac and
packed with `dpkg-deb` on the switch (the Mac has none); installed as
spec'd (unit enabled, `User=admin` drop-in, `/etc/disunified` symlink);
`-collect-once` over the local runner matched the container bridge's
snapshot; the probe logged at `Open`. §10 steps 1-3 ran: the container's
`dx010` entry removed and the container restarted, the state and the reply
log copied to `/host/disunified`, the service started and resuming the
adopted state at 12 MB RSS.

**Step 3 then failed on a fact nothing in this document foresaw**: every
inform POST ended in `EOF`. The switch's VLAN netdev (`Vlan<native>`) was
at SONiC's default MTU 9100 while the ports ran 1500 (jumbo off); the
gateway advertises a jumbo MSS, so the kernel sent the 60 KB inform in
segments the 1500-byte uplink dropped. A bridge over SSH never sends
such segments *from* the switch, which is why two days in the container
never showed it. The fix is a driver rule, not an on-box one
(`sonic.md` §12, `vlanMTUs`): each managed VLAN's MTU follows the ports',
written to CONFIG_DB and to the netdev (vlanmgrd never applies the table's
field to the kernel). Landed in main the same evening. The on-box
bridge's start-up reconcile (`informloop.Loop.Run` reconciles before the
first inform) applies it on the next service start, so installing the deb
built from that commit is the whole repair. The same evening the loop was
changed to reconcile every cycle whether or not the controller answered
(`informloop.Loop.informOnce`): a swss restart under a running on-box
bridge recreates the netdevs at 9100 and breaks the bridge's own informs,
and until then the reconcile that restores them ran only after an answered
inform. Now the next cycle restores them.

Steps 4-5 of §10 ran once that deb was installed: the start-up reconcile
logged the VLAN MTU batch (0 ports changed), the next inform was answered
200 and the controller showed the switch connected on the same uplink
port within a minute; a port name set through the REST API on a port with
no link reached `PORT|…` `description` in 24 s and was cleared again on
the restore. The on-box bridge runs at about 19 MB RSS.

## 11. What this specification does not do

- It does not change the SSH transport, any other driver, or the container
  install.
- It does not put l2d or mstpd into the deb (`sonic.md` §11.5 keeps them
  driver-installed; the upgrade script carries mstpd).
- It does not add reply-log rotation (noted in §7; the size is measured).
- It does not give the on-box bridge a way to prove an address to the
  controller beyond a TCP connect; an inform from the new address would
  need the session's key inside the driver, which stays UniFi-free.
- Gateway changes, DHCP (declined as today) and the undo of a move whose
  new address does not reach the controller are fixture-tested only, like
  their SSH counterparts (`sonic.md` §6.6).
