# Docker is the only host requirement: every target runs the Go toolchain in a container.
GO_VERSION ?= 1.27.3
VERSION    ?= $(shell sed -n 's/^## \[\([^]]*\)\].*/\1/p' CHANGELOG.md | grep -vi '^unreleased$$' | head -n1)
IMAGE      ?= stowage:$(VERSION)
BUILD      := DOCKER_BUILDKIT=1 docker buildx build --build-arg GO_VERSION=$(GO_VERSION) --build-arg VERSION=$(VERSION)
# Module and build caches persist on the host disk, owned by the caller.
GOCACHE_DIR ?= $(HOME)/.cache/stowage-go
DOCKER_GO  := docker run --rm -u $(shell id -u):$(shell id -g) -e HOME=/tmp -e GOCACHE=/cache/build \
              -e GOMODCACHE=/cache/mod -v $(GOCACHE_DIR):/cache -v $(CURDIR):/src -w /src
GO         := $(DOCKER_GO) -e GOFLAGS=-trimpath golang:$(GO_VERSION)-bookworm

.PHONY: all check build image test e2e e2e-docker e2e-full e2e-full-k8s e2e-stateless e2e-k8s e2e-kairos e2e-vm e2e-full-vm smoke-install gate gate-plan fmt tidy shell hooks clean

all: check build

$(GOCACHE_DIR):
	mkdir -p $@

check:            ## gofmt + vet + staticcheck + govulncheck + race tests
	$(BUILD) --target checks .

build:            ## static binary → ./bin/stowage (only after green checks)
	$(BUILD) --target bin --output type=local,dest=bin .

e2e: build       ## publisher end to end against zot, with real image builds (needs Docker + network)
	bash test/e2e-registry.sh

e2e-docker: build  ## the Docker shape end to end: ship, install, re-run, upgrade --version, rollback (needs Docker + network)
	bash test/e2e-docker.sh

e2e-full: build    ## examples/full on the Docker shape: ship signed CalVer releases, install, the search extension's input set and cleared, upgrade, rollback, a barrier (needs Docker + network)
	bash test/e2e-full.sh

e2e-full-k8s: build ## examples/full on a kind cluster: install against an external Postgres, the search extension's input set, upgrade, rollback, an embedded Postgres reset (needs Docker + network)
	bash test/e2e-full-k8s.sh

e2e-stateless: build ## examples/stateless (no database, nothing built) on the Docker shape: ship, install, the endpoint printed, a re-run, upgrade --version, rollback, reset-db refused (needs Docker + network)
	bash test/e2e-stateless.sh

e2e-k8s: build     ## the Kubernetes shape end to end on a kind cluster: install, upgrade, a failed upgrade, rollback (needs Docker + network)
	bash test/e2e-k8s.sh

e2e-kairos: build  ## the Kairos VM shape's publication: OS bundle and disk images built for real, ship, promote, prune (needs Docker + network, ~30 GB)
	bash test/e2e-kairos.sh

e2e-vm: build      ## a real Kairos VM on TrueNAS: vm create with OpenTofu, first-boot install, the app socket, vm destroy (needs STOWAGE_VM_ENV=<file>, tofu, ssh)
	bash test/e2e-vm.sh

e2e-full-vm: build ## examples/full on a real Kairos VM on TrueNAS: vm create from a TLS mirror, the search extension's input set, upgrade, rollback (needs STOWAGE_VM_ENV=<file>, tofu, ssh)
	bash test/e2e-full-vm.sh

gate:             ## the commit gate: check, build, lint, then only the end-to-end suites the change can affect (test/gate.sh)
	bash test/gate.sh

gate-plan:        ## print the suites `make gate` would run for the change, and why
	bash test/gate.sh --plan

smoke-install: build  ## the installer on the local Docker engine: registry, stack, front, re-run
	./bin/install-smoke.test -test.run Smoke -test.count 1 -test.v; rc=$$?; bash test/sweep.sh; exit $$rc

image:            ## runtime image
	$(BUILD) --target image -t $(IMAGE) --load .

test: | $(GOCACHE_DIR)             ## fast loop: unit tests only, no race detector
	$(GO) go test -count=1 ./...

fmt: | $(GOCACHE_DIR)              ## rewrite sources with gofmt
	$(GO) gofmt -w .

tidy: | $(GOCACHE_DIR)             ## refresh go.mod / go.sum
	$(GO) go mod tidy

shell: | $(GOCACHE_DIR)            ## a shell with the toolchain and the repo mounted
	$(DOCKER_GO) -it golang:$(GO_VERSION)-bookworm bash

hooks:            ## commit as the project identity, with the hooks that hold it
	git config core.hooksPath .githooks
	git config user.name "$$(sed 's/ <.*//' .githooks/identity)"
	git config user.email "$$(sed 's/.*<\(.*\)>/\1/' .githooks/identity)"

clean:
	rm -rf bin
