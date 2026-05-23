# Multi-stage build for fj-bellows.
#
# Layout: a single `source` stage pulls go.mod + source. `test` and
# `build` both inherit FROM source. CI's `test` job builds `--target
# test`; `build-image` builds the runtime stage with `needs: test` so
# the runtime artifact only exists when the tests it covers passed.
#
# Tests run on the build platform once (linux/amd64 today). The `build`
# stage cross-compiles per TARGETPLATFORM but does NOT depend on test —
# the safety property is enforced at the CI job level, so tests don't
# get re-run once per target arch.

FROM --platform=$BUILDPLATFORM golang:1.26 AS source
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Vet + race + govulncheck on the build platform. Cached layer so an
# unchanged source tree skips re-running the test suite.
FROM --platform=$BUILDPLATFORM source AS test
RUN go vet ./...
RUN go test -race ./...
RUN go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Cross-compile the production binary for the requested TARGETPLATFORM.
FROM --platform=$BUILDPLATFORM source AS build
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/fj-bellows ./cmd/fj-bellows
# Stage an empty lock file owned by the distroless nonroot user (uid 65532),
# so the daemon can open it without write access to /run. Without this the
# nonroot user can't create /run/fj-bellows.lock (parent dir is root-owned)
# and the daemon exits on startup. See #31.
RUN mkdir -p /out/run && touch /out/run/fj-bellows.lock

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fj-bellows /usr/local/bin/fj-bellows
COPY --from=build --chown=65532:65532 /out/run/fj-bellows.lock /run/fj-bellows.lock
ENTRYPOINT ["/usr/local/bin/fj-bellows"]
