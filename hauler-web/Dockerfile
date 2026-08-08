# hauler-web ships with the hauler binary it drives, pinned by tag. Pulling
# hauler from its published image rather than re-building it keeps the two
# versions unambiguous: what is in this image is exactly what runs.
ARG HAULER_VERSION=v2.0.0

FROM ghcr.io/hauler-dev/hauler:${HAULER_VERSION} AS hauler

FROM golang:1.26-alpine AS builder
WORKDIR /src
# Dependencies first so a source-only change reuses the module cache layer.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=devel
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/hauler-web ./cmd/hauler-web

FROM alpine:3.21
# git is a runtime dependency: the reconciler clones and fetches manifest
# repositories. ca-certificates is needed to reach registries over TLS.
RUN apk add --no-cache ca-certificates git tzdata \
 && addgroup -g 1001 hauler \
 && adduser -D -u 1001 -G hauler hauler \
 && mkdir -p /var/lib/hauler-web/store /var/lib/hauler-web/work \
 && chown -R hauler:hauler /var/lib/hauler-web

COPY --from=hauler  /hauler          /usr/local/bin/hauler
COPY --from=builder /out/hauler-web  /usr/local/bin/hauler-web

# Matches the uid in the hauler image, and is what the chart's
# securityContext expects. Nothing is written outside the two volumes below.
USER 1001:1001
ENV HAULERWEB_STORE_DIR=/var/lib/hauler-web/store \
    HAULERWEB_WORK_DIR=/var/lib/hauler-web/work \
    HAULERWEB_HAULER_BIN=/usr/local/bin/hauler \
    HAULER_DIR=/var/lib/hauler-web/work/.hauler
VOLUME ["/var/lib/hauler-web/store", "/var/lib/hauler-web/work"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/hauler-web"]
CMD ["serve"]
