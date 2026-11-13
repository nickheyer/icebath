.PHONY: test build dev release setup

GOTAGS ?= goolm

test:
	go test -tags $(GOTAGS) ./...

build:
	go build -tags $(GOTAGS) -o dist/icebath ./cmd/icebath

dev:
	go run -tags $(GOTAGS) ./cmd/icebath

release:
	bash scripts/release/pushReleaseTag.sh $(RELEASE_FLAGS)

setup:
	bash scripts/release/setup.sh
