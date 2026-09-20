# Cutting a release

The version is the git tag; nothing in the tree records it. `.github/workflows/release.yml`
does the work: a `v*` tag builds and pushes the multi-arch image to GHCR and
creates the GitHub release with the static binaries; a push to `main` publishes
`:edge` so a change can be tried on a real host before it is a release.

| Trigger | Image tags | GitHub release |
|---|---|---|
| tag `v1.2.3` | `:v1.2.3`, `:1.2.3`, `:1.2`, `:latest` | yes, with binaries + `SHA256SUMS` |
| tag `v1.2.3-rc1` | `:v1.2.3-rc1` only (never moves `:latest`) | yes, marked prerelease |
| push to `main` | `:edge`, `:edge-<sha>` | no |

Platforms: `linux/amd64` and `linux/arm64`, cross-compiled by Go inside one
build stage, so there is no QEMU and the build takes about as long as two
`go build`s. Binaries: those two plus `darwin/amd64` and `darwin/arm64`.

## One-time setup on GitHub

1. **Actions must be allowed to write packages.** The workflow asks for
   `packages: write`, which the automatic `GITHUB_TOKEN` grants — no secret to
   create. If the org restricts Actions token permissions, allow write there.
2. **Make the package public after the first push**, or `docker pull` asks for
   a login: the repo's **Packages** → `switch-to-unifi` → **Package settings** →
   *Danger Zone* → **Change visibility** → Public. Also set **Inherit access
   from repository** so anyone who can push the repo can manage the package.
   The `org.opencontainers.image.source` label in the `Containerfile` is what
   links the package back to the repo on its GHCR page.

## Per release

```sh
go test ./... && go vet ./...
scripts/check-site-info.sh                 # no secrets or site identifiers anywhere
git tag -a v1.2.3 -m 'switch-to-unifi v1.2.3'
git push origin v1.2.3                     # the workflow does the rest
```

Then check what was published:

```sh
docker buildx imagetools inspect ghcr.io/techblueprints/switch-to-unifi:v1.2.3   # both platforms listed
docker run --rm ghcr.io/techblueprints/switch-to-unifi:v1.2.3 -build-version     # prints v1.2.3
```

`-build-version` is the bridge's own build (stamped with
`-ldflags -X main.buildVersion=…`), and it is the first line of every run's log.
It is not `-version`, which is the *firmware* version reported to the
controller.

## What a release should not contain

Everything in `CLAUDE.md` §3 applies to the tag, the release notes and the
image: no real authkey, address, MAC, serial, device or host name. The image is
built from the tree with `.dockerignore` excluding `config.yaml`, `.env`,
`state/` and `inform-log/`, and `scripts/check-site-info.sh` runs in CI on
every push — but the release notes are written by hand, so read them once more
before publishing.

## Release notes

The workflow writes the pull command, links `docs/install.md` and `deploy/` at
that tag, links a compare against the previous tag, and repeats the
experimental-software and no-affiliation notes. Add the human summary (what
changed, anything that needs a config change, any new driver) by editing the
release on GitHub afterwards.
