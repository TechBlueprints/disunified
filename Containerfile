# switch-to-unifi container image: a static Go binary on a distroless base.
#   podman build -t switch-to-unifi .
#   podman run --rm -v ./config.yaml:/etc/switch-to-unifi/config.yaml:ro \
#     -v switch-to-unifi-state:/var/lib/switch-to-unifi --env-file env switch-to-unifi
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /switch-to-unifi ./cmd/switch-to-unifi

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /switch-to-unifi /switch-to-unifi
# The adopted key and reply logs live here; mount a volume so they survive.
VOLUME /var/lib/switch-to-unifi
WORKDIR /var/lib/switch-to-unifi
ENTRYPOINT ["/switch-to-unifi"]
CMD ["-config", "/etc/switch-to-unifi/config.yaml"]
