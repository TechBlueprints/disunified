# local-information/

Everything in this folder except this README is gitignored. It holds the
site-specific facts an operator (or an AI agent working for one) needs to
run and test the bridge against a real network, so that none of it ever
lands in a tracked file:

- IP addresses, subnets, DNS names and domains of the controller, the
  switches and the hosts the bridge runs on
- MAC addresses, serial numbers, device and site names, the LAN topology
  (which port goes where, what is live and must not be touched)
- deployment details: hosts, paths, container names, how to update
- credential locations, key paths, user names

Suggested layout: `site.md` for the notes, plus any local configs or
scripts. Tracked docs and examples use documentation addresses instead
(`192.0.2.0/24`, `2001:db8::/32`, `02:00:00:xx:xx:xx` MACs, `example.net`),
and fixtures are scrubbed with the scripts in `scripts/`.
