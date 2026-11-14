VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build build-web ensure-web test test-go test-web test-docker test-system lint lint-go lint-web e2e clean

all: build

# The web UI is embedded with the "webui" build tag. Without the tag the
# binary serves a placeholder page, so go build / go test need no node.
build: ensure-web
	CGO_ENABLED=0 go build -tags=webui -trimpath -ldflags "$(LDFLAGS)" -o bin/upgradoor ./cmd/upgradoor

build-web:
	cd web && npm ci && npm run build

ensure-web:
	@if [ ! -f web/dist/index.html ]; then $(MAKE) build-web; fi

test: test-go test-web

test-go:
	go test -race -timeout 10m ./...

test-web:
	cd web && npm ci && npm run typecheck && npm test

# Needs a Docker daemon: container recreate against real containers.
test-docker:
	go test -race -timeout 15m -tags=docker_integration ./pkg/agent/docker/...

# Needs Docker: runs the package upgrade and reboot paths inside a throwaway
# systemd container. Never touches this machine's packages.
test-system:
	./test/system/run.sh

lint: lint-go lint-web

lint-go:
	golangci-lint run --timeout=10m

lint-web:
	cd web && npm ci && npm run lint

# End-to-end run on a local Kurtosis devnet.
e2e:
	./examples/kurtosis/run.sh

clean:
	rm -rf bin web/dist
