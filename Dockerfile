# ---- Build ----
FROM golang:1.26-alpine AS build
# GOTOOLCHAIN=auto because the official golang images set GOTOOLCHAIN=local:
# they then refuse to fetch a newer toolchain, and a
# `go 1.26.6` in go.mod breaks the build with
#   go.mod requires go >= 1.26.6 (running go 1.26.5; GOTOOLCHAIN=local)
# The golang:1.26-alpine tag is a moving target -- which patch version it resolves to
# depends on when the CI runner last pulled it. A build that passes on
# one runner and fails on the next is exactly that.
# With auto, the build fetches the version required by go.mod itself.
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /todo .

# ---- Run ----
# alpine instead of scratch: CA certificates for the Discord webhook (HTTPS) and
# busybox-wget for the healthcheck. Timezone data is bundled via time/tzdata in the binary.
FROM alpine:3.22
# `apk upgrade` before installing: the alpine:3.22 tag does get
# updated, but a locally cached layer stays at whatever
# it was at the last pull. That's how the image scan found CVE-2026-14456
# in libcrypto3/libssl3 (3.5.7-r0, fixed in 3.5.8-r0) -- a vulnerability the
# project itself didn't introduce. With the upgrade, every build pulls
# the current package state instead of carrying an old one forward.
RUN apk --no-cache upgrade  && apk add --no-cache ca-certificates  && mkdir -p /data && chown 10001:10001 /data
COPY --from=build /todo /todo
ENV DB_PATH=/data/todo.db
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=60s --timeout=5s --retries=3 \
  CMD wget -qO- "http://localhost:${PORT:-8080}/healthz" || exit 1
USER 10001:10001
ENTRYPOINT ["/todo"]
