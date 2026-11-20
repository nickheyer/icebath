#!/bin/sh
# Fills the WinGet manifests for a release tag from its checksums and publish date into an output directory
set -eu
usage='usage: render.sh <tag> <checksums.txt> <release date YYYY-MM-DD> <out dir>'
tag=${1:?$usage}
checksums=${2:?$usage}
date=${3:?$usage}
out=${4:?$usage}
here=$(cd "$(dirname "$0")" && pwd)
version=${tag#v}
case $date in
    [0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]) ;;
    *) printf 'render: release date %s is not YYYY-MM-DD\n' "$date" >&2; exit 1 ;;
esac
sum() {
    want=$(awk -v f="icebath_${version}_windows_$1.zip" '$2 == f || $2 == "*" f { print toupper($1); exit }' "$checksums")
    [ -n "$want" ] || { printf 'render: %s has no entry for icebath_%s_windows_%s.zip\n' "$checksums" "$version" "$1" >&2; exit 1; }
    printf '%s' "$want"
}
amd64=$(sum amd64)
arm64=$(sum arm64)
mkdir -p "$out"
for f in "$here"/nickheyer.icebath*.yaml; do
    sed -e "s/@VERSION@/$version/g" -e "s/@TAG@/$tag/g" -e "s/@DATE@/$date/g" \
        -e "s/@SHA256_AMD64@/$amd64/g" -e "s/@SHA256_ARM64@/$arm64/g" "$f" > "$out/$(basename "$f")"
done
