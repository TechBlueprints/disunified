# `show storm-control` with a storm caught (EOS 4.26.14M, 2026-11-14)

Captured on the DCS-7160-48TC6-F during a live test of the storm bits
(`docs/drivers/arista-eos.md` §9.8). Storm control was set by hand on the
uplink in a configuration session under `commit timer 00:02:00`:

    interface Ethernet1
       storm-control unknown-unicast level 0.01

0.01 % is the lowest level EOS keeps (a lower one is stored as 0, which
drops everything); on the 10G port it is about 1 Mb/s. A LAN host sent
4930 frames of 1000 bytes in 5 s to a MAC the switch had not learned but
the switches upstream had learned toward it (one frame from that MAC sent
out of the uplink by the switch's own CPU just before), so the frames
reached only this switch and arrived on the uplink as unknown unicast.
The session was aborted right after the capture.

- `show-storm-control.json` -- `show storm-control | json`: `level` 1
  (hundredths of a percent), `rate` 1 (Mb/s), `drop` 2230.
- `show-storm-control.txt` -- the CLI's table.

Scrubbed with `scripts/sanitize-arista-eos.py` (nothing to replace).
