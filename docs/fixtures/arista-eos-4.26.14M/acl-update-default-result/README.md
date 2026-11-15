# `hardware access-list update default-result permit` (EOS 4.26.14M, 2026-11-15)

Does the knob shorten the drop while a bound IPv4 list is rewritten
(`docs/drivers/arista-eos.md` §9.7 item 1)? Measured with the switch's own
traffic, as §9.8 did: pings every 20 ms from a LAN host to the switch's
in-band address, which enter on the uplink (Ethernet1), with a test list
`DUI_RW_TEST` bound inbound there. Every rule matched documentation
addresses only, before a final `permit ip any any`, so nothing real was
denied. Each step ran its CLI batch 2 s into a 400- or 800-ping window
(`ping-runs.txt`: the pings lost in each, as icmp_seq ranges).

All of it ran under a hold session (`configure session dui-rw-hold`, an
`alias`, `commit timer 00:10:00`), the changes in plain `configure` (EOS:
"Any changes done here will be reverted if the session is not
committed"), and was rolled back by aborting the hold.

- Baseline (no change): 0-2 scattered losses per 400 (the host's path).
- Binding a 1-rule list, and rewriting a 4-rule one (`cli-bind.txt`,
  `cli-rewrite-small-a.txt`): no loss in the batch's window.
- Rewriting a 500-rule list (each `no <seq>` of the old rules, 500 new
  `deny tcp host 192.0.2.N any eq P`, the final permit): **10 consecutive
  pings lost, every time** (six runs: about 200 ms), at the end of the
  batch, when EOS programs the list.
- With `hardware access-list update default-result permit` set
  (`cli-knob.txt`; setting it lost nothing): the same rewrites, six runs,
  **no ping lost**.
- The abort (a config replace: the list, its binding and the knob
  removed): no ping lost.

Afterwards no session was pending, `DUI_RW_TEST`, its binding, the knob and
the alias were gone, and the running config matched the startup config.
