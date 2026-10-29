#!/bin/sh
# Installs the icebath release binary for this machine into ICEBATH_INSTALL_DIR or ~/.local/bin
set -eu

releases=https://github.com/nickheyer/icebath/releases
dir=${ICEBATH_INSTALL_DIR:-$HOME/.local/bin}
if [ -n "${ICEBATH_VERSION:-}" ]; then
  base=$releases/download/v${ICEBATH_VERSION#v}
else
  base=$releases/latest/download
fi

# Prints a message to stderr and exits
die() {
  printf 'icebath: %s\n' "$*" >&2
  exit 1
}

# Downloads a URL to a file with curl or wget
fetch() {
  if command -v curl > /dev/null; then
    curl -fsSL -o "$2" "$1" || die "download failed $1"
  elif command -v wget > /dev/null; then
    wget -qO "$2" "$1" || die "download failed $1"
  else
    die "curl or wget is required"
  fi
}

# Prints the sha256 digest of a file
sha256() {
  if command -v sha256sum > /dev/null; then
    sha256sum "$1" | cut -d ' ' -f 1
  elif command -v shasum > /dev/null; then
    shasum -a 256 "$1" | cut -d ' ' -f 1
  elif command -v openssl > /dev/null; then
    openssl dgst -sha256 "$1" | sed 's/.* //'
  else
    die "sha256sum, shasum or openssl is required"
  fi
}

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  MINGW* | MSYS* | CYGWIN*) die "on Windows run this in PowerShell instead   irm icebath.app | iex" ;;
  *) die "no build for $(uname -s), see https://icebath.app" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "no build for $(uname -m), see https://icebath.app" ;;
esac
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2> /dev/null)" = 1 ]; then
  arch=arm64
fi

name=icebath-$os-$arch
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fetch "$base/$name" "$tmp/icebath"
fetch "$base/checksums.txt" "$tmp/checksums.txt"
want=$(awk -v n="$name" '$2 == n { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "checksums.txt has no entry for $name"
[ "$(sha256 "$tmp/icebath")" = "$want" ] || die "checksum mismatch for $name"

mkdir -p "$dir"
chmod 755 "$tmp/icebath"
mv -f "$tmp/icebath" "$dir/icebath"
printf 'installed %s to %s\n' "$("$dir/icebath" -version)" "$dir/icebath"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) printf 'add %s to your PATH to run icebath\n' "$dir" ;;
esac
