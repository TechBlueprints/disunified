# The bridge on the switch itself (Arista EOS)

The `arista-eos` driver's own document is [`arista-eos.md`](arista-eos.md).
This one is about running the bridge **on** the EOS switch it presents,
under EOS's own process manager, instead of on a host behind the switch --
the same move as [`sonic-on-switch.md`](sonic-on-switch.md) and
[`proxmox-on-node.md`](proxmox-on-node.md), for the same reason: a switch
that stays adopted and controllable with no other machine up.

Status (2026-11-18): the transport, the build and one full collect are
verified on a DCS-7160-48TC6-F on EOS 4.26.14M (§1, §6). **Not yet run:**
the bridge as an EOS daemon holding an adoption, a reboot of the switch
with it configured, and an address or ACL change confirmed by the probe on
a real switch (both are fixture-tested, §5).

## 1. What the switch offers (read on 4.26.14M, 2026-11-18)

- **eAPI has a unix socket.** `show management api http-commands` lists a
  `Unix Socket server` beside the HTTPS one, off by default. With

  ```
  configure
  management api http-commands
     protocol unix-socket
  end
  ```

  `/var/run/command-api.sock` appears (mode `srw-rw-rw-`, root). It takes
  no credentials; a request that starts with `enable` runs at privilege
  15. The JSON-RPC envelope and the replies are the HTTPS server's, the
  text format included (`show interfaces <lane> phy detail`), so the FEC
  codeword counters and the optic flags are read as over HTTPS. Turning
  the socket on or off left the HTTPS server running.
- **EOS is Linux with a 64-bit kernel** under its i686 userland
  (`uname -m`: `x86_64`, kernel 4.19): a static `linux/amd64` Go binary
  runs as it is. 7.9 GB of memory with 6.4 GB available, four cores.
- **The only store that survives a reboot is `/mnt/flash`**, and it is FAT
  (`vfat`, mounted `dirsync,flush`, every file `rwxrwx---` root:eosadmin).
  It runs a binary and renames a file over another; it has no symbolic
  links and no file modes. `/var/log` and `/tmp` are tmpfs.
- **EOS runs and restarts a process named in its configuration**:

  ```
  daemon disunified
     exec /mnt/flash/disunified/run.sh
     no shutdown
  ```

  The process runs as root and its output goes to
  `/var/log/agents/<name>-<pid>` (seen with a throwaway daemon). The block
  is ordinary configuration: `write memory` keeps it across a reboot.
- **A process in the switch's shell reaches the network through the
  switch's own interfaces** (default VRF): the route to the controller
  leaves the in-band VLAN interface from the switch's own address, and
  the controller's inform port and API answered from there.

## 2. What the driver does differently

`options.transport: local` on an `arista-eos` device; no `url:`, no
`ssh:`, no credentials.

| Remote (`url:`) | On the switch (`transport: local`) |
|---|---|
| eAPI over HTTPS with a user and password | eAPI over the unix socket (`options.socket`, default `/var/run/command-api.sock`), no credentials |
| The switch's address is the dial target | The address the switch reaches `options.probe` from (a route lookup; nothing is sent), or `options.address` |
| An address move is confirmed by dialling the new address | By a TCP connect to `options.probe` **bound to the new address** (§5) |
| An ACL change is confirmed over a fresh connection | By a TCP connect to `options.probe`, then the commit over the socket (§5) |
| IP rules that would block the bridge's own connection to eAPI are held | No such connection exists; the probe before the commit is the guard |
| The controller's SSH keys go to the bridge's own user | To `options.ssh_key_user`, which must be set for keys to be installed |

`options.probe` is `host:port`; `main` fills it with the controller's
inform host and port when the device names none, as it does for the sonic
driver.

Everything else -- the command list, the parsing, every write, the lane
switches of the switch's split cages (they run in the parent's process) --
is the same code over the same transport interface.

## 3. Where things live

| What | Where | Why |
|---|---|---|
| Binary, `run.sh`, `config.yaml`, `env` | `/mnt/flash/disunified/` | survives reboots and EOS upgrades |
| `state/<name>/device.json`, `layout.json` | `state_dir: /mnt/flash/disunified` | the adopted key; written when the controller's config changes |
| Reply log, `payload-last.json` | `log_dir: /var/log/disunified` | written every inform; a FAT flash mounted `dirsync` is the wrong place. Lost at a reboot: it is a diagnostic |
| The bridge's own log | `/var/log/agents/disunified-<pid>` | EOS puts a daemon's output there |

`log_dir` is a top-level config key (unset = `state_dir`, as before).

Two things FAT changes. `device.json` holds the adoption key and the
applied push, and its `0600` means nothing there: every user who can reach
the switch's shell and is in `eosadmin` can read it, as they can read the
startup configuration. And `env` (the controller API key, when naming is
wanted) is as readable; leave `api_url` out to keep the key off the switch.

## 4. Install

1. Build on a workstation: `scripts/build-eos.sh` (`amd64`; `386` for a
   switch whose `bash uname -m` says `i686`, not tried). It leaves
   `disunified`, `run.sh` and `config.example.yaml` in `dist/eos/disunified/`.
2. Copy `disunified` and `run.sh` to `/mnt/flash/disunified/` on the
   switch. `scp` to the switch needs a user whose login shell reaches
   privileged mode; otherwise serve the two files over HTTP for a moment
   and fetch them from the switch's shell (`bash curl -o ...`).
3. Write `/mnt/flash/disunified/config.yaml` from
   [`deploy/eos/config.example.yaml`](../../deploy/eos/config.example.yaml),
   and `env` with the API key if `api_url` is set.
4. Try it: `bash /mnt/flash/disunified/disunified -config
   /mnt/flash/disunified/config.yaml -collect-once` prints one snapshot and
   contacts no controller.
5. Configure the switch:

   ```
   configure
   management api http-commands
      protocol unix-socket
   daemon disunified
      exec /mnt/flash/disunified/run.sh
      no shutdown
   end
   write memory
   ```

Moving an adopted switch from a remote bridge: stop the remote bridge
first (one bridge per adopted key), copy its `state/<name>/` directory to
`/mnt/flash/disunified/state/<name>/`, then step 5. The switch stays
adopted; the first push applies with nothing changed.

Update: replace the binary, then `daemon disunified` / `shutdown` / `no
shutdown`.

## 5. A change that could cut the switch off

Both changes already run inside a configuration session committed with a
timer, which the switch reverts by itself unless it is confirmed
(`arista-eos.md` §5, §9). Remote, the confirmation is a fresh connection
to the switch. On the switch the socket answers whatever happened to the
network, so the proof is the other direction:

- **Address move:** after `commit timer`, a TCP connect to the probe
  address with the new address as its source. It succeeds only if the new
  address is on an interface and its route (the new gateway included)
  reaches the controller. Then `configure session <name> commit` and `write
  memory`; otherwise an error, and the timer puts the old address back.
- **ACL change:** after the settle time, a TCP connect to the probe
  address. The lists are ingress on every port, the uplink included, so a
  rule that drops the controller's replies fails the probe and the timer
  removes the lists.

Tests: `internal/drivers/arista-eos/local_test.go` (the start-up read set
over a real unix socket, served from the 4.26.14M captures; the move
confirmed and left to revert; the probe's source binding).

## 6. Verified live (2026-11-18, DCS-7160-48TC6-F, 4.26.14M)

- The socket: on, a JSON batch and a text batch through it with `curl
  --unix-socket`, off again.
- The build (`linux/amd64`, static) fetched to the switch's `/tmp` and run
  from its shell: `-collect-once` with `transport: local` and no address
  read all 54 ports in 17 s, found the management interface and its
  gateway from the route to the controller, and filled the FEC counters
  from the text commands.
- The binary also runs from `/mnt/flash`; a rename over an existing file
  and nested directories work there; a symbolic link is refused.
- A throwaway `daemon` block: started as root, logged to
  `/var/log/agents/`, removed with `no daemon`.

The switch was left as it was: no socket, no daemon, nothing on the flash.
