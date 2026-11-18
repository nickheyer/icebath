#!/bin/sh
# Downloads and verifies the icebath release for this platform, then hands every option to icebath install-self
set -eu

REPO_URL=${ICEBATH_REPO_URL:-https://github.com/nickheyer/icebath}
VERSION=${ICEBATH_VERSION:-}
CHANNEL=${ICEBATH_INSTALL_CHANNEL:-}
INSTALL_DIR=${ICEBATH_INSTALL_DIR:-}
ICEBATH_HOME=${ICEBATH_HOME:-$HOME/.icebath}
FORWARD=""

# Appends one argument to FORWARD single quoted for the eval that replays it
forward() {
    FORWARD="$FORWARD '$(printf '%s' "$1" | sed "s/'/'\\\\''/g")'"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --branch|-Branch|--commit|-Commit|--channel|-Channel|--dir|--icebath-home|-IcebathHome)
            option=$1
            value=${2:-}
            case "$value" in
                ""|-*) printf '%s needs a value\n' "$option" >&2; exit 2 ;;
            esac
            case "$option" in
                --branch|-Branch) forward --branch; forward "$value" ;;
                --commit|-Commit) forward --commit; forward "$value" ;;
                --channel|-Channel) CHANNEL=$value ;;
                --dir) INSTALL_DIR=$value ;;
                --icebath-home|-IcebathHome) ICEBATH_HOME=$value ;;
            esac
            shift 2 ;;
        --non-interactive|-NonInteractive|--skip-setup) forward --non-interactive; shift ;;
        --skip-browser|--no-playwright|-SkipBrowser) forward --skip-browser; shift ;;
        --skip-computer-use|-SkipComputerUse) forward --skip-computer-use; shift ;;
        --verbose|-Verbose) forward --verbose; shift ;;
        -h|--help)
            echo "Usage: install.sh [--channel stable|main] [--branch NAME] [--commit SHA]"
            echo "                  [--dir PATH] [--icebath-home PATH]"
            echo "                  [--non-interactive] [--verbose]"
            echo "                  [--skip-browser] [--skip-computer-use]"
            echo
            echo "  --channel       stable (default): the latest published release, then release"
            echo "                  to release. main: every commit. Also ICEBATH_INSTALL_CHANNEL;"
            echo "                  an explicit choice is saved like 'icebath update --set-channel'."
            echo "  --skip-browser  Do not install the browser (Chromium). Alias: --no-playwright."
            echo "                  Remembered by later installs and 'icebath update';"
            echo "                  undo with 'icebath pm install chromium'."
            echo "  --skip-computer-use"
            echo "                  Do not install the computer-use driver (cua-driver). Remembered"
            echo "                  the same way; undo with 'icebath pm install cua-driver'."
            exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 1 ;;
    esac
done

case "$CHANNEL" in
    ""|stable|main) ;;
    *) printf -- '--channel must be stable or main, not %s\n' "$CHANNEL" >&2; exit 2 ;;
esac
if [ -n "$CHANNEL" ]; then
    forward --channel
    forward "$CHANNEL"
fi
INSTALL_DIR=${INSTALL_DIR:-$ICEBATH_HOME/icebath-agent}
export ICEBATH_HOME

if [ -t 2 ] && [ -z "${NO_COLOR:-}" ]; then
    C_RED=$(printf '\033[0;31m') C_CYAN=$(printf '\033[0;36m') C_NC=$(printf '\033[0m')
else
    C_RED="" C_CYAN="" C_NC=""
fi
log() { printf '%s→%s %s\n' "$C_CYAN" "$C_NC" "$1" >&2; }
fail() { printf '%s✗%s %s\n' "$C_RED" "$C_NC" "$1" >&2; exit 1; }

case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    MINGW*|MSYS*|CYGWIN*) fail "on Windows run this in PowerShell instead: irm icebath.app | iex" ;;
    *) fail "unsupported OS $(uname -s); on Windows run install.ps1" ;;
esac
case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) fail "unsupported architecture $(uname -m)" ;;
esac
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null)" = 1 ]; then
    arch=arm64
fi

fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 --connect-timeout 10 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1"
    else
        fail "curl or wget is required"
    fi
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | cut -d' ' -f1
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$1" | sed 's/.* //'
    else
        fail "sha256sum, shasum or openssl is required to verify the download"
    fi
}

if [ -z "$VERSION" ]; then
    case "$REPO_URL" in
        https://github.com/*) api="https://api.github.com/repos/${REPO_URL#https://github.com/}/releases/latest" ;;
        *) api="$REPO_URL/releases/latest" ;;
    esac
    tmp_json=$(mktemp) || fail "could not create a temp file"
    fetch "$api" "$tmp_json" || { rm -f "$tmp_json"; fail "could not resolve the latest release from $api"; }
    VERSION=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp_json" | head -n1)
    rm -f "$tmp_json"
    [ -n "$VERSION" ] || fail "the latest release has no tag"
fi
VERSION=${VERSION#v}

work=$(mktemp -d "${TMPDIR:-/tmp}/icebath-install-XXXXXX") || fail "could not create a temp dir"
trap 'rm -rf "$work"' EXIT
archive="icebath_${VERSION}_${os}_${arch}.tar.gz"
base="$REPO_URL/releases/download/v$VERSION"
log "Downloading icebath $VERSION for $os/$arch"
fetch "$base/$archive" "$work/$archive" || fail "download failed: $base/$archive"
fetch "$base/checksums.txt" "$work/checksums.txt" || fail "download failed: $base/checksums.txt"
want=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1; exit }' "$work/checksums.txt")
[ -n "$want" ] || fail "checksums.txt has no entry for $archive"
got=$(sha256_of "$work/$archive")
[ "$got" = "$want" ] || fail "checksum mismatch for $archive: expected $want, got $got"
tar -xzf "$work/$archive" -C "$work" icebath || fail "could not unpack $archive"
chmod 0755 "$work/icebath"

eval "set -- $FORWARD"
"$work/icebath" install-self --dir "$INSTALL_DIR" --icebath-home "$ICEBATH_HOME" --version "$VERSION" "$@"
