# A static MAC drop entry (EOS 4.26.14M, 2026-11-15)

How the DCS-7160-48TC6-F shows `mac address-table static <mac> vlan <v> drop`,
the command the driver writes for UniFi's client Block
(`docs/drivers/arista-eos.md` §9.9). The MAC was a locally administered test
address on no device; the entry was committed in a configuration session
with `commit timer 00:02:00` and the session aborted two seconds later
(`%SYS-5-CONFIG_SESSION_COMMIT_TIMER_CANCELED`, then
`CONFIG_REPLACE_SUCCESS`). Afterwards the running config held no
`mac address-table static` line, the MAC table had no entry for the address,
and `show running-config diffs` was empty. Read over eAPI with the entry
committed:

- `show-running-config-mac-drop.json` -- `show running-config` reduced to its
  `mac address-table` keys: the line as written, the MAC in EOS's dotted
  form, a one-liner (null).
- `show-mac-address-table-address-mac-drop.json` -- `show mac address-table
  address <mac>`: `entryType` `static`, `interface` empty (a drop entry has
  no port), the VLAN.

Scrubbed with `scripts/sanitize-arista-eos.py`, which keeps the dotted form
where the capture has it (since this capture).

What the entry does is Arista's to say: the EOS manual's Data Transfer
chapter, "drop entries": frames whose source or destination MAC matches the
entry in that VLAN are discarded; unicast addresses only. No frame with the
test source was sent through the 7160 (no host on its test ports).
