SHELL := /bin/bash
VERSION := $(shell cut -d' ' -f1 VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)
IMAGE := ghcr.io/orchard-suite/dispatch
BRANCH ?=

.PHONY: check test build docker run land

# Everything that must pass before a commit. It runs here before a commit
# lands, and in CI on every pull request and release.
check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt: the files above need formatting" >&2; exit 1; }
	go vet ./...
	go test ./...

test:
	go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/dispatch ./cmd/dispatch

docker:
	docker build --platform linux/amd64 -t $(IMAGE):$(VERSION) .

# A local server on :8080 (the desk), :8081 (the API), :8082 (the status page).
# The desk needs the front door's headers, for example:
#   curl -H 'X-Forwarded-Email: you@example.com' -H 'X-Forwarded-Groups: Admin' localhost:8080/
# The API takes the shared secret: curl -H 'Authorization: Bearer local-secret' localhost:8081/v1/incidents
run: build
	mkdir -p data
	PUBLIC_URL=http://localhost:8080 DATA_DIR=data STATUS_ADDR=:8082 ALERTMANAGER_TOKEN=local-secret ./dist/dispatch

# Land the commits on main: release-damnit runs the config's check on the
# tree that lands and signs its proof.
land:
	release-damnit land $(BRANCH)
