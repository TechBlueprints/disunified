# The ACL commit warning (EOS 4.26.14M, 2026-11-15)

A list the hardware cannot program still commits; EOS reports it only as a
warning on the commit (`docs/drivers/arista-eos.md` §9.4). Provoked without
harm as in `../acl-capacity/01-dscp-ttl-1000.txt`: one list of 1000
`permit ip any any dscp D ttl eq T` rules (counters per-entry) plus a final
`permit ip any any`, bound inbound on Ethernet2 (copper, unused), in a
configuration session committed with `commit timer 00:05:00` and aborted
right after. Run twice, once through eAPI (the bridge's own transport,
`configure session dui-probe-w1`) and once as a CLI script over SSH on
stdin, no pty (as the SSH transport runs it, `dui-probe-w2`).

- `eapi-result-commit-timer.json`: eAPI's result for `commit timer
  00:05:00`, the warning in `warnings`. Every other command's result was
  `{}` but `eapi-result-ip-access-group.json` (the bind: DSCP is not
  supported in the data plane).
- `show-ip-access-lists-summary-not-programmed.json`: the summary while
  the session was pending its timer. Its own top-level `warnings` says
  "Not all ACLs are programmed in the hardware!", while the list is
  "active" on Ethernet2.
- `ssh-session.txt`: the CLI's output up to the commit: the same texts as
  `! ` lines, each ending ` at line N` (the script line).
- `ssh-after-abort.txt`: `show configuration sessions` after the abort.

Afterwards no session was open, no `DUI_PROBE` line was in the running
config, and the running config matched the startup config.
