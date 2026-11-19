.PHONY: test build build-full build-noextras build-core dev release setup

GOTAGS ?= goolm
# Memory ceiling of the Go toolchain for every compile
GOMEMLIMIT ?= 6GiB
GO = GOMEMLIMIT=$(GOMEMLIMIT) go

test:
	$(GO) test -tags "$(GOTAGS)" ./...

build: build-full

# Full build, every tier linked, the binary releases ship
build-full:
	$(GO) build -tags "$(GOTAGS)" -o dist/icebath ./cmd/icebath

# Core and plugins tiers, no extras
build-noextras:
	$(GO) build -tags "$(GOTAGS) icebath_noextras" -o dist/icebath-noextras ./cmd/icebath

# Core tier only, one terminal session with no Python
build-core:
	$(GO) build -tags "$(GOTAGS) icebath_core" -o dist/icebath-core ./cmd/icebath

dev:
	$(GO) run -tags "$(GOTAGS)" ./cmd/icebath

release:
	bash scripts/release/pushReleaseTag.sh $(RELEASE_FLAGS)

setup:
	bash scripts/release/setup.sh
