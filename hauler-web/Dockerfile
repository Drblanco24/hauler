# hauler-web ships with the hauler binary it drives.
#
# Every base image is pinned by digest as well as tag. The tag alone was not
# enough here in a literal sense: an earlier revision of this file pinned
# `ghcr.io/hauler-dev/hauler:v2.0.0`, which does not exist -- hauler dropped
# the `v` prefix from its image tags after v1.0.8, so the real tag is `2.0.0`.
# The digests below were resolved against the live registries.
ARG HAULER_VERSION=2.0.3
ARG HAULER_DIGEST=sha256:0f9bd75b19ce42111bde464709e3a7682b9cec8e160f63f1e38b9bfd9e8930c5

FROM ghcr.io/hauler-dev/hauler:${HAULER_VERSION}@${HAULER_DIGEST} AS hauler

# Must satisfy the `go` directive in go.mod.
FROM golang:1.25-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS builder
WORKDIR /src
# Dependencies first so a source-only change reuses the module cache layer.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=devel
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/hauler-web ./cmd/hauler-web

FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d
# git is a runtime dependency: the reconciler clones and fetches manifest
# repositories. ca-certificates is needed to reach registries over TLS.
RUN apk add --no-cache ca-certificates git tzdata \
 && addgroup -g 1001 hauler \
 && adduser -D -u 1001 -G hauler hauler \
 && mkdir -p /var/lib/hauler-web/store /var/lib/hauler-web/work \
 && chown -R hauler:hauler /var/lib/hauler-web

COPY --from=hauler  /hauler          /usr/local/bin/hauler
COPY --from=builder /out/hauler-web  /usr/local/bin/hauler-web

# Matches the uid in the hauler image, and is what the chart's securityContext
# expects. Nothing is written outside the two volumes below.
USER 1001:1001
ENV HAULERWEB_STORE_DIR=/var/lib/hauler-web/store \
    HAULERWEB_WORK_DIR=/var/lib/hauler-web/work \
    HAULERWEB_HAULER_BIN=/usr/local/bin/hauler \
    HAULER_DIR=/var/lib/hauler-web/work/.hauler
VOLUME ["/var/lib/hauler-web/store", "/var/lib/hauler-web/work"]

# `worker` is the long-running process: it polls each source and reconciles
# changes. It is the default because it is the only subcommand that is
# meaningful for a Deployment -- `serve` does not exist yet, and naming it here
# is what made an earlier revision of this image crash-loop on startup.
ENTRYPOINT ["/usr/local/bin/hauler-web"]
CMD ["worker"]
