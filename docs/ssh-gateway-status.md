# SSH gateway branch — where this got to (2026-09-19)

This branch keeps the SSH gateway (`internal/sshgw`) that main dropped. Read
this before picking it up again.

## What it is

An SSH server inside the bridge (one per switch, `ssh_gateway: {listen,
advertise_ip, switch_ssh}` in the config) that:

- verifies logins against what the controller pushes in `system_cfg`:
  `users.N.name/password` (MD5-crypt, `md5crypt.go`) and
  `sshd.auth.key.N.*` (public keys);
- proxies the session (pty, env, shell, exec, window-change, signal, exit
  status) to the switch's own SSH as the bridge's switch user
  (`stu` on the Arista), authenticating upstream with the bridge's password
  via **keyboard-interactive** — EOS sshd never offers plain `password`;
- makes the bridge report the gateway's IP as the device IP, so
  `ssh admin@<device IP>` from the UniFi device page works with the
  controller's credentials.

Deployment needs the container to own port 22 on a LAN address:
`deploy/compose.macvlan.yaml` (macvlan `lan`, fixed MAC
`02:53:54:55:00:01`, netavark DHCP). The controller holds a DHCP
reservation for that MAC → a fixed LAN address (client record
"switch-to-unifi gateway"); reserve before the bridge reports the address, because the
controller rejects a fixed IP an adopted device already reports
(`api.err.FixedIpAlreadyUsedByDevice`). The Podman host cannot reach its own
macvlan containers; test from another LAN host.

## Verified

- `ssh admin@<gateway address> "show version"` from a LAN host with a
  controller-pushed key returns the Arista's real output (2026-09-19).
- Unit tests: md5crypt vectors, password/key auth, session proxy.

## Why it was parked

The UniFi UI's Manage → Debug terminal **does not SSH to the device IP**.
When the terminal is opened, the controller sends the device an inform-reply
command `build-ssh-session` (seen in this bridge's own inform log, with a
session id, STUN/TURN servers and a TURN username). The device is expected
to establish a WebRTC session with the browser and pipe its shell over the
data channel; the bridge logs the command as unhandled. The Debug entry is
shown only when `fw_caps` includes UTERM (bit value 4,
`internal/device/caps.go`).

unifi-emu does not implement the device side of that session, and how a
device answers `build-ssh-session` is not documented there. Building it
would mean a WebRTC peer (pion) in the bridge plus the undocumented
signalling; Clint chose to park it rather than pursue that.

So the gateway gives plain SSH with UniFi credentials, not the in-UI
terminal.

## To resume

1. Merge or cherry-pick this branch (last gateway commit on main was
   `113b94b`; the removal commit on main is the one after it).
2. Re-add `fwCapUTERM` to `FWCaps` only if the WebRTC terminal is built.
3. Redeploy with `deploy/compose.macvlan.yaml`; the DHCP reservation may
   still exist in the controller.
