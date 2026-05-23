# Build a static fj-bellows binary and ship it on a bare distroless base.
# Cross-compiles for the target platform passed by buildx (amd64/arm64).
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
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
