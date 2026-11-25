# whatbark — build, test and deployment entry points.
#
# Targets:
#   build        static whatbark binary into bin/
#   test         full test suite (G3 gate: go test ./... -race)
#   vet          G2 gate (go vet)
#   smoke        end-to-end smoke (A1-A16, S-series) against a real serve
#   docker       build the whatbark:latest image (A12)
#   docker-run   run the image: host 18080 -> container 8080, sqlite in named volume
#   clean        remove build artifacts

GO     ?= go
BINARY ?= bin/whatbark
IMG    ?= whatbark:latest
PORT   ?= 18080
# Stamped into the binary via -ldflags and reported by GET /api/info
# (protocol-v2, docs/PROTOCOL.md §2). Falls back to "dev" without git.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test vet smoke docker docker-run clean

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X whatbark/internal/api.ServerVersion=$(VERSION)" -o $(BINARY) .

test:
	$(GO) test ./... -race -count=1

vet:
	$(GO) vet ./...

smoke: build
	bash scripts/smoke.sh

docker:
	docker build -t $(IMG) .

docker-run:
	docker run --rm -p $(PORT):8080 -v whatbark-data:/data $(IMG)

clean:
	rm -rf bin
