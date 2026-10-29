.PHONY: test build dev release setup

test:
	go test ./...

build:
	go build -o dist/icebath ./cmd/icebath

dev:
	go run ./cmd/icebath

release:
	bash scripts/release/pushReleaseTag.sh $(RELEASE_FLAGS)

setup:
	bash scripts/release/setup.sh
