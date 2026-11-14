# ACL capacity on the DCS-7160-48TC6 (EOS 4.26.14M), 2026-11-14

Probes for sizing `max_custom_ip_acls` (`docs/drivers/arista-eos.md` §9.1,
§9.4). Every probe ran on the two test ports only, Ethernet2 (copper,
unused) and Ethernet54 (an empty QSFP28 cage, split `speed 25g` inside the
session for four lanes), as one configuration session over one SSH
connection, the CLI script fed on stdin:

```
enable
configure session dui-probe
[interface Ethernet54/1 / speed 25g / exit]            (five-list probes)
ip access-list DUI_PROBE_<n>
counters per-entry                                     (all but 07)
10 <rule> ... <seq> permit ip any any
exit
interface <test interface> / ip access-group DUI_PROBE_<n> in / exit
commit timer 00:05:00
show configuration sessions
show ip access-lists summary
show platform xp acl | include Number of valid|AclId:
show platform xp acl | include <the MG rows>
show logging last 1 minutes | include ACL
configure session dui-probe abort
show configuration sessions
show ip access-lists summary
```

Each file holds the EOS messages, the summary, the `Number of valid entries`
lines of the chip's ACL tables (in order: the L4 source and destination port
tables, 65536; the source and destination prefix transform tables, 1536; then
six pairs of MGKS key-profile tables, 4096, and MG entry tables, 8192), the
MG rows the file's header names, and the same reads after the abort. The
full outputs (up to 1.2 MB each) were trimmed to these lines and are not
kept. The probe rules use documentation addresses only; the bridge host's
address in the session log lines is replaced with 192.0.2.250 and the
switch's name with `switch`.

After the last probe, `show configuration sessions` was empty, no
`DUI_PROBE` list was left in the running config or the summary, every MGKS
and MG table read 0 valid entries, Ethernet2 was `switchport mode trunk`
and Ethernet54/1 `speed forced 100gfull` again. The two prefix transform
tables still listed the probes' prefixes (101 and 251 valid entries): the
entries of an earlier day's probe were gone by then, so the slots are
reused, not lost; whether a full table is reclaimed was not measured.

`../show-ip-access-lists-summary-counters-incomplete.json` is the summary's
JSON for one list of 4097 counted rules (`countersIncomplete: true`).
