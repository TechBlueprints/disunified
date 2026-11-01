# teamd member records from upstream SONiC

`lag_member_table.json` is the five STATE_DB `LAG_MEMBER_TABLE` entries
(tlm_teamd's copy of `teamdctl <PortChannel> state dump`) in SONiC's own
test data, from [sonic-net/sonic-utilities](https://github.com/sonic-net/sonic-utilities)
branch `202505`, commit `7511bf2f8ea3582c2b67cf5016f8e8237e62882b`:

- `tests/mock_tables/state_db.json`: `PortChannel0001|Ethernet112` (link
  down, disabled), `PortChannel0002|Ethernet116` and
  `PortChannel0003|Ethernet120` (bundled, a partner answering);
- `tests/dump_input/portchannel/state_db.json`: `PortChannel001|Ethernet0`
  and `|Ethernet4` (link up, defaulted: no partner).

Changed from upstream: the entries only, one per line in the collector's
layout, and their MAC addresses rewritten to `02:00:00:00:00:0N` (this
repository's documentation vocabulary). Licensed under the Apache License,
Version 2.0; upstream's `LICENSE` is beside this file.

The sonic driver's LACP reading (`internal/drivers/sonic/hardware.go`
`lacpOf`) is tested against these: the DX010 has had no aggregate yet, and
the ECS Aggregation's own tables are not kept in this repository.
