# switch-to-unifi container image: a static Go binary on a distroless base.
#
# A multi-arch image (linux/amd64, linux/arm64) is published on every release,
# so most installs never build this:
#   podman pull ghcr.io/techblueprints/switch-to-unifi:latest
# Build it yourself with either engine (Docker needs -f; Podman finds it):
#   podman build -t switch-to-unifi .
#   docker build -f Containerfile -t switch-to-unifi .
# Run:
#   podman run --rm -v ./config.yaml:/etc/switch-to-unifi/config.yaml:ro \
#     -v switch-to-unifi-state:/var/lib/switch-to-unifi --env-file env switch-to-unifi
#
# The build stage stays on the builder's architecture and cross-compiles to
# $TARGETARCH, which Go does natively: `buildx --platform
# linux/amd64,linux/arm64` then costs one compile per arch and no emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" \
    -o /switch-to-unifi ./cmd/switch-to-unifi

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="switch-to-unifi" \
      org.opencontainers.image.description="Presents a non-UniFi switch to a UniFi Network controller as an adopted UniFi switch. Independent project, not affiliated with or endorsed by Ubiquiti Inc." \
      org.opencontainers.image.source="https://github.com/TechBlueprints/switch-to-unifi" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /switch-to-unifi /switch-to-unifi
# The adopted key and reply logs live here; mount a volume so they survive.
VOLUME /var/lib/switch-to-unifi
WORKDIR /var/lib/switch-to-unifi
ENTRYPOINT ["/switch-to-unifi"]
CMD ["-config", "/etc/switch-to-unifi/config.yaml"]
