# Proxmox VE driver — working notes for Claude Code

Rules for every driver are in `../CLAUDE.md`; the full write-up is
`docs/drivers/proxmox.md`. These are the facts and decisions behind the
driver (2026-09-19/20).

`internal/drivers/proxmox` + `docs/drivers/proxmox.md`. Each node's `vmbr0` is a
USW Leaf (`UDC48X6`, 54 ports) named after the node; guests are ports 1-48
numbered cluster-wide, the port recorded in the guest's own Proxmox tags
(`unifi.p25.c`, `unifi.p27.c.net1`; docs/drivers/proxmox.md §1c; the driver keeps
no file, only the owner node writes tags) and named `VM-<id>` (`Open-<port>` when free),
the physical NICs/bond slaves count down from 54 (`bond0-1`, `bond0-2`),
then the NICs that are on the box but under nothing (`eno1`: enabled, no
link, never the uplink); every slot below them is open to guests. Free slots are reported and seeded disabled;
a guest arriving on one is re-seeded from its own state. Read: one SSH exec of `collect.sh` per
poll (root on the node, key auth). Write: `qm set`/`pct set` for
`link_down`/`tag`/`trunks`. Nodes run lldpd bound to the active uplink NIC
so the upstream aggregation switch sees them. Controller quirks: the
device record carries `oob_port_config` that must be sent back empty on
every REST update (`api.err.OobPortNotSupported` otherwise); the
controller's own display names ("USW Leaf", "ECS Core") count as defaults
for renaming. Capabilities claimed: IGMP snooping, LACP (one aggregate
session), and STP/BPDU guard/port cost only under mstpd.
For port-level tests make a throwaway, diskless guest (`qm create <id>
--name stu-test --memory 128 --net0 virtio,bridge=vmbr0`) and destroy it
afterwards (`qm stop <id>; qm destroy <id> --purge`); the last one, VM
999, was destroyed 2026-09-20 once its checks were done.
The firmware version reported to the controller is the node's own PVE
version (`9.1.6`), not the UniFi model profile's.
The node is the switch (device MAC = the bridge MAC, same IP and hostname;
a derived-MAC/host-port variant was tried and dropped 2026-09-20); bond
members are separate top ports (`bond0-1` at 54 is the uplink, the standby
shows blocking); guest ports are `VM-<id>`; lldpd config is written by the
driver.
Uplink/Parent verified 2026-09-20 after merging main's `uplink: "eth0"` fix.
**Adoption is safe by construction (2026-09-20):** the first push after
adoption is held while the driver's plan says it would change ports
(`devicemodel.Planner`), and the bridge seeds the controller's port
overrides from the switch's live VLAN state on the handshake (retrying
while held); verified by re-adopting proxmox-1 with FusionHub's tagged
NICs watched: held → seeded 2 ports → new push applied with 0 changes.
Before this, a re-adoption with control on stripped VM 119's tags for 13 s.
Deployed with the Arista in the one container (config.yaml has four
switches; the container mounts a directory with the bridge's own ed25519
key, installed on the nodes' root user, and a known_hosts — paths in
`local-information/site.md`).
**Wire shapes (2026-11-13, `docs/wire-shapes.md` §8):** a fan with no hwmon
`fanN_label` is `Fan1`..`Fan16`, the ECS's form. A chip's own label passes
through unchanged at any length (with spaces, as `dell_smm`'s can be); no
capture has one (both have empty labels), so that path and the shape test's
4-character limit have never met. The loop numbers fans per chip, so two
chips with fans would both report a `Fan1`: untested, since every capture
and every node so far has one fan chip (four fans). A module's compliance
text (`Optic.MediaType`, sent as `sfp_compliance`) is ethtool's
"Transceiver type" after the first `": "`, cut at 32 characters
(`complianceText`); nothing reads it but the payload, and `mediaOf` picks
the media from the identifier and speeds. ethtool can print several
"Transceiver type" lines for one module; the parser keeps the last, and no
capture has more than one.

**Guest ports, decided 2026-11-13 (Clint):** a guest port reports
`sfp_found: true` with empty module fields, and stays that way -- real
switches send the same for a port with something in it and no module
identity (59 SFP28 ports in the committed informs). A forced speed pushed
for a guest port has nothing to act on (a guest NIC's link has no speed to
set): `ApplyPorts` ignores it with one `NOTE:` line per port and speed and
writes nothing. `sys_stats.mem_buffer` stays Buffers plus Cached.
**Fan names (looked into 2026-11-13).** Proper names are not to be had from
the node: on the hardware seen so far `dell_smm` exposes `fanN_label` but
reading it fails (EINVAL: the BIOS answers no fan type), and SMBIOS type 27
lists more named cooling devices than the sensor chip has fans, with
nothing tying a name to a reading. Nor would a name show: the controller's
UI draws each fan in the device overview by its `index` (a number, unique
across chips: `tables.go` numbers the whole list), not by its `label`
(read from the UI bundle on Network 11.0.97; what a fan alert's name is
built from was not traced). So two chips both labelled `Fan1` would not
be confused on screen, and the labels stay as they are (Clint,
2026-11-13: leave it, closed). Dell's own agents were considered and not
installed: Dell Command | Monitor for Linux is the client-machine one but
supports Red Hat and Ubuntu, not Debian, and whether it names a tower's
fans was not established; OpenManage Server Administrator is the servers'.
Neither would change what the UI shows.
