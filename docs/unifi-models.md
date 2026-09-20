# Which UniFi model to claim, and how to keep the catalogue current

The controller renders a device from **its own** hardware profile for the
model string the device claims. The claim therefore has to exist in the
controller's database, and its port count has to match the switch: the
controller draws exactly the profile's ports.

## What the profile does and does not decide (Network 10.6.106, observed)

| Decided by the profile | Decided by the device's reports |
|---|---|
| number of ports and their numbering | per-port media icon (`port_table[].media`, e.g. `10GbE` turns an SFP28 slot into an RJ45 icon) |
| display name ("USW Leaf"), PoE controls if the profile has PoE | speed picker contents (`speed_caps`, from the switch's hardware table) |
| which advanced settings are hidden as "known unsupported" | FEC control and options (`speed_caps` FEC bit + `fec_mode`) |
| default port names ("SFP28 1") until renamed | everything live: link, speed, counters, LLDP, MAC table, optics |

Capability claims (`fw_caps`, `switch_caps`, `speed_caps`) are only stored
when the inform carries `udapi_version` (the bridge sends `1.0.0` by
default). Without it the controller keeps the profile's defaults and hides
storm control, STP options, FEC and the rest.

## The catalogue

`-model auto` and `-list-models` use unifi-emu's `model_profiles.json`
(built by that project from Network 10.4.57 at the time of writing).
[`internal/unifimodel`](../internal/unifimodel) ranks it against the switch's port layout:
port count first, QSFP28 cage count second, everything else cosmetic, and a
penalty for internal model codes (display name == code; real products such
as `USWF066` = ECS Aggregation, but a later controller may rename them).

## Refreshing the catalogue for a newer controller

A newer controller may offer models the catalogue lacks. unifi-emu owns the
catalogue and documents how to regenerate `model_profiles.json` with its
`cmd/modelgen` (see that repo's README); regenerate there, then bump the
unifi-emu dependency or vendor the generated file. Until then, pass
`-model <CODE>` explicitly for a model your controller lists.

## Choosing: match the port count, else take the Leaf

1. **Find a model whose port count matches the switch** (`-model auto` does
   this; `-collect-once` prints the ranking). The count is what the
   controller draws; the media of the front ports is cosmetic because the
   device's own media report replaces it, and the speed pickers come from the
   device's `speed_caps`.
2. **If no model has the right count, claim `UDC48X6` (the USW Leaf)** rather
   than the nearest count. Two reasons, both Clint's call (2026-09-19):
   the Leaf was a limited Early Access product, so a controller has no
   expectations about it and no fleet of real ones to confuse it with; and
   it has **no firmware releases**, so the controller never offers a
   firmware update. For any other model the controller will push
   `upgrade` commands with real version strings; the bridge emulates those
   (accepts, "reboots", reports the new version, persists it in
   `State.Firmware`) but it is noise you can avoid. Ports beyond 54 are not
   drawn; fewer are shown as empty.
3. Avoid PoE models unless the switch has PoE: the profile adds PoE columns
   and controls that will never work.

## Catalogue: every switch model Network 10.6.106 lists (2026-09-19)

111 entries, 100 switches + 11 power devices typed as switches. Port classes
in the controller's vocabulary (`RJ45` = `standard`, any copper speed).
Codes without a product name are internal codes but shipping hardware
(`USWF066` = ECS Aggregation).

| Ports | Layout | Models (PoE marked) |
|---|---|---|
| 56 | 48 SFP28 + 8 QSFP28 | USWF064 |
| 54 | 48 SFP28 + 6 QSFP28 | **UDC48X6** (Leaf), USWF066 (ECS Aggregation) |
| 54 | 48 RJ45 + 4 SFP28 + 2 QSFP28 | USWF006 (PoE), USWF007 |
| 54 | 48 RJ45 + 6 SFP+ | US648P (PoE) |
| 52 | 48 RJ45 + 4 SFP28 | USWED42 (PoE), USWED43, USWF069 (PoE), USWF070 |
| 52 | 48 RJ45 + 4 SFP+ | US48PRO (PoE), US48PRO2, USPM48 (PoE), USPM48P (PoE), USLP48P (PoE) |
| 52 | 48 RJ45 + 2 SFP + 2 SFP+ | US48, US48P500 (PoE), US48P750 (PoE), US48PL2 (PoE), S248500 (PoE), S248750 (PoE) |
| 52 | 48 RJ45 + 4 SFP | USL48, USL48B, USL48P (PoE), USL48PB (PoE) |
| 32 | 32 QSFP28 | USWF07D |
| 32 | 32 SFP28 | USWF003 |
| 32 | 28 SFP+ + 4 SFP28 | USAGGPRO (Pro Aggregation) |
| 30 | 24 SFP+ + 4 SFP28 + 2 QSFP28 | USWF004 (PoE), USWF005 |
| 28 | 24 RJ45 + 4 SFP28 | USWF001 (PoE) |
| 28 | 24 RJ45 + 4 SFP+ | USWED72 (PoE), USWED73 |
| 26 | 24 RJ45 + 2 SFP28 | USWED44 (PoE), USWED45, USWF067 (PoE), USWF068 |
| 26 | 24 RJ45 + 2 SFP+ | US24PRO (PoE), US24PRO2, US624P (PoE), USLP24P (PoE), USPM24, USPM24P (PoE), USXG24 (Enterprise XG 24, 10G copper) |
| 26 | 24 RJ45 + 2 SFP | US24, US24P250 (PoE), US24P500 (PoE), US24PL2 (PoE), S224250 (PoE), S224500 (PoE), USL24, USL24B, USL24P (PoE), USL24PB (PoE), USWED08 (PoE) |
| 22 | 20 SFP+ + 2 SFP28 | USWF002 |
| 18 | 16 RJ45 + 2 SFP | US16P150 (PoE), S216150 (PoE), USL16P (PoE), USL16PB (PoE) |
| 18 | 16 RJ45 + 2 SFP+ | USPM16, USPM16P (PoE) |
| 16 | 16 RJ45 | USL16LP (PoE), USL16LPB (PoE), USWED06 (PoE) |
| 16 | 4 RJ45 + 12 SFP+ | USXG (XG 16) |
| 14 | 12 RJ45 + 2 SFP28 | USWF008 (PoE) |
| 12 | 10 RJ45 + 2 SFP+ | USWED77 (PoE) |
| 10 | 8 RJ45 + 2 SFP+ | USLP8P (PoE), USWED76 (PoE) |
| 10 | 8 RJ45 + 2 SFP | US8P150 (PoE), USC8P150 (PoE), US68P (PoE), S28150 (PoE), USWED97 (PoE), USWED98 |
| 10 | 10 RJ45 | USC8P450 (PoE), USWED05 (PoE), USWED36, USWED37 (PoE), USWED99 (PoE) |
| 10 | 4 RJ45 + 3 SFP+ + 3 WAN | USWED74 (USW_WAN) |
| 9 | 9 RJ45 | USL8MP (PoE, battery) |
| 8 | 8 SFP+ | USL8A (Aggregation 8) |
| 8 | 8 RJ45 | US8 (PoE), US8P60 (PoE), USC8 (PoE), USC8P60 (PoE), USL8LP (PoE), USL8LPB (PoE) |
| 8 | 7 RJ45 + 1 SFP+ | USM8P (PoE), USM8P60 (PoE), USM8P210 (PoE) |
| 7 | 4 RJ45 + 3 WAN | USWED75 (USW_WAN) |
| 6 | 4 RJ45 + 2 SFP+ | US6XG150 (PoE) |
| 5 | 5 RJ45 | USF5P (PoE), USFXG, USMINI, USMINI2, USWED35 |
| 1 | 1 RJ45 (power devices) | USPPDUP, USPPDUHD, USPED18, USPDA2B, USPDA2C, USWDA23-26, USPRPS, USPRPSP |

Notes: `UDC48X6` carries `SWITCH_LEAF` and a `knownUnsupportedFeatures` list
(DOT1X, LLDP_MED, EGRESS_RATE_LIMIT) that does not hide UI controls. No
profile has a distinct 10G-copper class. Refresh the catalogue (above)
after a controller upgrade.

## Arista DCS-7160-48TC6-F → UDC48X6

48x 10GBASE-T + 6x QSFP28. No catalogue model has 10G copper (the controller
has no such class; RJ45 is `standard`), and UDC48X6 is the only shipping
54-port model with six QSFP28 cages. With the device's media report the
front ports draw as RJ45. Alternatives considered: `USWF007` (48 copper +
4 SFP28 + 2 QSFP28, internal code) and `US648P` (48 copper + 6 SFP+, PoE).
