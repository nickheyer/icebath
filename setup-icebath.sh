#!/usr/bin/env bash
# Builds icebath from this checkout and installs it, or only builds it with --runtime-only
set -e

runtime_only=false
for option in "$@"; do
    case "$option" in
        --runtime-only) runtime_only=true ;;
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

echo -e "${CYAN}→${NC} Building icebath..."
CGO_ENABLED=0 go build -tags goolm -trimpath -o icebath ./cmd/icebath
echo -e "${GREEN}✓${NC} Built $SCRIPT_DIR/icebath"

echo -e "${CYAN}→${NC} Preparing the test environment..."
go mod download
echo -e "${GREEN}✓${NC} Test environment ready"

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
