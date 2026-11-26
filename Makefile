.PHONY: test ci build build-full build-noextras build-core dev release setup

GOTAGS ?= goolm
# Flags of the binaries make builds: no debug tables and no build paths, as the release builds have
GOBUILD ?= -trimpath -ldflags "-s -w"
# Memory ceiling of the Go toolchain for every compile
GOMEMLIMIT ?= 12GiB
# Work directories of the Go toolchain, on disk, one per running make command
GOSCRATCH ?= $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/icebath/gotmp

# Drops the work directories of commands that are gone, then gives this command its own and removes it when the command ends
define scratch
s="$(GOSCRATCH)"; mkdir -p "$$s"; \
for d in "$$s"/*; do [ -d "$$d" ] && ! kill -0 "$${d##*/}" 2>/dev/null && rm -rf "$$d"; done; \
d="$$s/$$$$"; mkdir -p "$$d"; trap 'rm -rf "$$d"' EXIT; trap 'exit 130' INT TERM HUP;
endef

# Prints and runs one go command under the memory ceiling with its own work directory
go = @echo 'GOMEMLIMIT=$(GOMEMLIMIT) go $(1)'; $(scratch) GOMEMLIMIT=$(GOMEMLIMIT) GOTMPDIR="$$d" go $(1)

test:
	$(call go,test -tags "$(GOTAGS)" ./...)

# CPUs the CI test run is pinned to, four like the ubuntu-latest runners
CI_CPUS ?= 0-3

# The release preflight's test run on this machine: UTC like the runners, pinned to CI_CPUS, no cached results
ci:
	TZ=UTC GOFLAGS="$(GOFLAGS) -count=1" taskset -c $(CI_CPUS) $(MAKE) test

build: build-full

# Full build, every tier linked, the binary releases ship
build-full:
	$(call go,build $(GOBUILD) -tags "$(GOTAGS)" -o dist/icebath ./cmd/icebath)

# Core and plugins tiers, no extras
build-noextras:
	$(call go,build $(GOBUILD) -tags "$(GOTAGS) icebath_noextras" -o dist/icebath-noextras ./cmd/icebath)

# Core tier only, one terminal session with no Python
build-core:
	$(call go,build $(GOBUILD) -tags "$(GOTAGS) icebath_core" -o dist/icebath-core ./cmd/icebath)

dev:
	$(call go,run -tags "$(GOTAGS)" ./cmd/icebath)

release:
	bash scripts/release/pushReleaseTag.sh $(RELEASE_FLAGS)

setup:
	bash scripts/release/setup.sh
