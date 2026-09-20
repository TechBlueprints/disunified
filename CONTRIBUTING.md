# Contributing

This project grew by driving a real switch against a real controller, one
feature at a time, and verifying every step live. Contributions follow the
same loop, whether the contributor is a person or an AI agent.

## The loop

1. **File an issue first.** Say which switch/OS version and which UniFi
   feature, and paste the relevant scrubbed output (see fixtures below).
2. **Branch and PR against that issue.** One feature or one driver per PR.
3. **Capture before you code.** Save the switch's command output as
   fixtures under `docs/fixtures/<os>-<version>/`, and the controller's
   config push as `docs/fixtures/controller-<version>-*.txt`, both run
   through `scripts/sanitize-fixtures.py` (MACs, IPs, serials, hostnames,
   keys). The repo is public; nothing identifying ships.
4. **Write tests against the fixtures.** Parsing, every apply sequence, and
   idempotence (applying the same intent twice writes nothing).
5. **Verify with real data, end to end.** Run the bridge against a real
   controller, make the change in the UniFi UI (with a browser-automation
   tool such as Claude in Chrome, so the steps are reproducible), and check
   the switch's running config and the controller's device record. Record
   what you saw in the PR: the pushed keys, the commands written, the
   controller's stored values.
6. **Update the docs in the same PR:** `docs/feature-map.md` status rows,
   the driver's `docs/<vendor>-<api>.md`, and `CLAUDE.md` if a rule changed.
7. **Keep `go vet ./... && go test ./...` green.**

## Adding a driver

Follow `docs/adding-a-switch.md` step by step; the rules for the vendor
layer are in `internal/drivers/CLAUDE.md`. Open the issue with the OS
version, the port layout, and the output of `-collect-once` once the read
side works — the UniFi model choice (`docs/unifi-models.md`) is decided
there.

## For AI agents specifically

- Read `CLAUDE.md` (repo map and rules), then `docs/adding-a-switch.md`.
- Never assume a command exists on the target OS version; run it.
- Never write to a port that carries live traffic while testing; use an
  unused port and say which one in the issue.
- Ask before pushing, deploying, or touching anything outside the repo and
  the test ports.
- The maintainer verifies PRs on their own hardware; make that easy by
  keeping fixtures complete and the PR description a reproducible script.

## License

MIT. By contributing you agree your contribution is licensed the same way.
This project is not affiliated with Ubiquiti Inc.
