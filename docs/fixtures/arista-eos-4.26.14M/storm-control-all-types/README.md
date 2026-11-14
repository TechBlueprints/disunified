# `show storm-control` with all three types (EOS 4.26.14M, 2026-11-14)

Captured on the DCS-7160-48TC6-F with storm control set by hand on an
unused copper port (no link) and removed right after:

    interface Ethernet2
       storm-control broadcast level 5
       storm-control multicast level 1
       storm-control unknown-unicast level 2.5

- `show-storm-control.json` -- `show storm-control | json`: the per-type
  `level` (hundredths of a percent), `rate`, `dormant` and `drop`, keyed
  `broadcast`, `multicast`, `unknown-unicast`.
- `show-storm-control-Ethernet2.json` -- the same for one interface.
- `show-storm-control.txt` -- the CLI's table; `Drops` is the JSON's `drop`.

No traffic reached the port, so every `drop` is 0. The fixture set one
directory up holds the collector's full poll (cage 54 at 5 %/1 %).
Scrubbed with `scripts/sanitize-arista-eos.py` (nothing to replace).
