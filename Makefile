VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)

.PHONY: test-scale all build build-web ensure-web release test test-go test-web test-docker test-system test-reboot lint lint-go lint-web e2e wire clean

all: build

# bin/upgradoor runs on the hosts; bin/upgradoor-controller embeds the web UI
# with the "webui" build tag. Without the tag the controller serves a
# placeholder page, so go build / go test need no node.
build: ensure-web
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/upgradoor ./cmd/upgradoor
	CGO_ENABLED=0 go build -tags=webui -trimpath -ldflags "$(LDFLAGS)" \
	  -o bin/upgradoor-controller ./cmd/upgradoor-controller

# release: the reproducible release binaries dist/upgradoor-linux-<arch> (hosts) and
# dist/upgradoor-controller-linux-<arch> (the controller image), and their manifest
# dist/upgradoor-manifest.txt (VERSION must be exactly vX.Y.Z). Rebuilt from the tag, it gives
# byte-identical files: the Go of go.mod (GOTOOLCHAIN), no VCS stamp, no build id, a web UI
# built afresh from the lock file. CI adds the image's line before the maintainer signs.
RELEASE_PLATFORMS := linux-amd64 linux-arm64
GO_VERSION := $(shell sed -n 's/^go //p' go.mod)
RELEASE_GO = GOTOOLCHAIN=go$(GO_VERSION) CGO_ENABLED=0 GOOS=$${p%-*} GOARCH=$${p\#*-} \
	go build -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X main.version=$(VERSION)"

release: build-web
	@rm -rf dist && mkdir -p dist
	@for p in $(RELEASE_PLATFORMS); do \
	  $(RELEASE_GO) -o dist/upgradoor-$$p ./cmd/upgradoor || exit 1; \
	  $(RELEASE_GO) -tags=webui -o dist/upgradoor-controller-$$p ./cmd/upgradoor-controller || \
	    exit 1; \
	done
	./scripts/release-manifest.sh write $(VERSION) dist

# One npm ci per change of the lock file, however many web targets run.
web/node_modules/.package-lock.json: web/package.json web/package-lock.json
	cd web && npm ci

build-web: web/node_modules/.package-lock.json
	cd web && npm run build

# The built UI is rebuilt whenever one of its sources is newer than it, so
# the binary never embeds a stale page.
WEB_SOURCES := $(shell find web/src web/public -type f 2>/dev/null) \
	web/index.html web/package.json web/package-lock.json web/vite.config.ts \
	web/tsconfig.json web/tsconfig.node.json

web/dist/index.html: $(WEB_SOURCES)
	$(MAKE) build-web
	@touch $@

ensure-web: web/dist/index.html

test:
	$(MAKE) --no-print-directory -j2 test-go test-web

test-go:
	go test -short -race -timeout 10m ./...

# The fleet-size tests (1000 hosts and more) run without the race detector,
# which makes them many times slower without finding more.
test-scale:
	go test -timeout 10m -run 'FleetSize|LargeFleet' ./...

test-web: web/node_modules/.package-lock.json
	cd web && npm run typecheck && npm test

# Needs a Docker daemon: container recreate against real containers.
test-docker:
	go test -race -timeout 15m -tags=docker_integration ./pkg/agent/host/docker/... ./pkg/agent/

# Needs Docker: runs the package upgrade and reboot paths inside a throwaway
# systemd container. Never touches this machine's packages.
test-system:
	./test/system/run.sh

lint: lint-go lint-web

lint-go:
	golangci-lint run --timeout=10m

lint-web: web/node_modules/.package-lock.json
	cd web && npm run lint

# Needs Docker: the full agent under systemd in throwaway hosts, with leases
# that survive real reboots of those hosts.
test-reboot:
	./test/e2e/reboot.sh

# End-to-end run on a local Kurtosis devnet.
e2e:
	./test/e2e/kurtosis/run.sh

# Rewrites the wire schema and golden file the web UI decodes against, from
# the Go types. Run it after changing a type that travels on the stream.
wire:
	go test ./pkg/controller/stream/ -run TestWireFiles -update-wire

clean:
	rm -rf bin dist web/dist
