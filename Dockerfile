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

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fj-bellows /usr/local/bin/fj-bellows
ENTRYPOINT ["/usr/local/bin/fj-bellows"]
