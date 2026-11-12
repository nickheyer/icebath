#!/usr/bin/env bash
# Developer setup of a source checkout: builds the dashboard bundle and the
# icebath binary with it embedded, then hands the binary to its own
# install-self verb to publish the command, seed the home and sync skills.
#   --runtime-only        build only, publish nothing
#   --test-environment    also prepare every workspace the test suites need (default)
set -e

runtime_only=false
test_environment="--test-environment"
for option in "$@"; do
    case "$option" in
        --runtime-only) runtime_only=true ;;
        --test-environment|--test-environment=*) test_environment="$option" ;;
        *) printf 'Unknown setup option: %s\n' "$option" >&2; exit 2 ;;
    esac
done

GREEN='\033[0;32m'
CYAN='\033[0;36m'
RED='\033[0;31m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo ""
echo -e "${CYAN}Icebath Setup${NC}"
echo ""
command -v go >/dev/null 2>&1 || { echo -e "${RED}✗${NC} Go is required (see go.mod for the version)" >&2; exit 1; }

echo -e "${CYAN}→${NC} Building the dashboard bundle..."
go run ./cmd/icebath-build web
echo -e "${CYAN}→${NC} Building icebath..."
CGO_ENABLED=0 go build -tags webdist -trimpath -o icebath ./cmd/icebath
echo -e "${GREEN}✓${NC} Built $SCRIPT_DIR/icebath"

if [ "$test_environment" != "" ]; then
    echo -e "${CYAN}→${NC} Preparing the test environment..."
    go mod download
    workspaces="${test_environment#--test-environment}"
    workspaces="${workspaces#=}"
    if [ -n "$workspaces" ]; then
        args=()
        IFS=',' read -r -a selected <<< "$workspaces"
        for w in "${selected[@]}"; do args+=(--workspace "$w"); done
        npm ci --no-audit --no-fund --include-workspace-root=true "${args[@]}"
    else
        npm ci --no-audit --no-fund
    fi
    echo -e "${GREEN}✓${NC} Test environment ready"
fi

if [ "$runtime_only" = true ]; then
    exit 0
fi

if [ ! -f ".env" ]; then
    if [ -f ".env.example" ]; then
        cp .env.example .env
        chmod 600 .env 2>/dev/null || true
        echo -e "${GREEN}✓${NC} Created .env from template"
    fi
else
    chmod 600 .env 2>/dev/null || true
    echo -e "${GREEN}✓${NC} .env exists"
fi

./icebath install-self --dir "$SCRIPT_DIR" --from-checkout --non-interactive
echo ""
echo -e "${GREEN}✓ Setup complete!${NC}"
echo ""
