.PHONY: build test race lint lint-fix fmt vuln proto proto-check

build:
	go build ./...

# Regenerate the control plane protobuf + ConnectRPC stubs into gen/.
# Requires buf, protoc-gen-go, and protoc-gen-connect-go on PATH (see AGENTS.md).
proto:
	buf generate

# CI safety net: regenerate and fail if the committed gen/ tree drifts from
# what the current proto/ would produce.
proto-check:
	buf generate
	git diff --exit-code -- gen/

test:
	go test ./...

race:
	go test -race ./...

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

fmt:
	golangci-lint fmt ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
