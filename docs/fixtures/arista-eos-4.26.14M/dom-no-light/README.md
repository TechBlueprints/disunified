# An optic with no receive signal (EOS 4.26.14M, 2026-11-14)

`show interfaces transceiver dom thresholds | json` read from the switch
minutes after a 100GBASE-CWDM4 module was seated in a cage with no fibre.
Its `aggrRxPwr` channel is `-Infinity` (a power of zero in dBm), which is
not JSON: until the driver rewrote such tokens (`finiteJSON`), this one
value failed the whole reply of the poll and the bridge withheld its
informs. The per-lane `rxPower` reads -30 dBm against a low alarm of -10.

Scrubbed with `scripts/sanitize-arista-eos.py` (the module's serial).
