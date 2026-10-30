.PHONY: test build dev release setup vsix

VSIX := pkg/ext/vsc/bundle/icebath.vsix
VSIX_SRC := $(wildcard vscode/src/*.ts) vscode/package.json vscode/package-lock.json vscode/esbuild.mjs vscode/tsconfig.json vscode/icon.png vscode/LICENSE vscode/README.md vscode/.vscodeignore

test:
	go test ./...

$(VSIX): $(VSIX_SRC)
	cd vscode && ([ -d node_modules ] || npm ci) && npm run package
	cp vscode/icebath.vsix $@

vsix: $(VSIX)

build: $(VSIX)
	go build -o dist/icebath ./cmd/icebath

dev: $(VSIX)
	go run ./cmd/icebath

release:
	bash scripts/release/pushReleaseTag.sh $(RELEASE_FLAGS)

setup:
	bash scripts/release/setup.sh
