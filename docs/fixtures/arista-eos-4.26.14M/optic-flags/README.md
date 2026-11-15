# The module's own fault flags (EOS 4.26.14M, 2026-11-15)

One read-only pass over the switch, looking for a QSFP module's own receive
loss of signal and transmit fault (`docs/drivers/arista-eos.md` §4, optic
fault flags). At the time cage 49 held a 100GBASE-CWDM4 with no fibre (the
module of the 2026-09-19 capture, then linked), cage 53 a 100G DAC with its
link up, cage 54 nothing. No lit optic was seated.

- `show-interfaces-Ethernet<N>-phy-detail.txt`: `show interfaces EthernetN
  phy detail` (text) for 49/1 (dark), 49/2 (the unsplit cage's inactive
  second lane, also dark), 53/1 (DAC) and 54/1 (empty). The `RX_LOS since
  system boot` and `TX_FAULT since system boot` rows are the module's
  flags; the only place any 4.26.14M command prints them.
- `show-interfaces-Ethernet49_1-phy-detail.json`: the same command `| json`,
  which wraps the text in a string.
- `show-interfaces-transceiver*.json`, `show-interfaces-transceiver-eeprom.txt`:
  `show interfaces Ethernet49/1-4 transceiver` and its `dom`, `dom
  thresholds`, `detail`, `hardware`, `channels`, `performance-monitoring`
  and `eeprom` forms: readings, thresholds and static pages, no flags
  (`dom` and `dom thresholds` hold the token `-Infinity` for the dark
  aggregate RX power, as EOS sends it).

Scrubbed with `scripts/sanitize-arista-eos.py` (the JSON, one run); the two
module serials in the text, and in the JSON's text, replaced by
placeholders of their own length.
