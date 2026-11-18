#!/bin/sh
# Downloads every Termux pool archive the pm lock pins, checks its sha256 and stores it under its release asset name
set -eu
out=${1:?usage: fetch.sh <out dir>}
here=$(cd "$(dirname "$0")" && pwd)
pm=$here/../../pkg/pm
pool=https://packages.termux.dev/apt/termux-main/pool/main/
mkdir -p "$out"
jq -r '.packages[].artifacts[] | if type == "array" then .[] else . end | select(.sha256 != null) | "\(.sha256) \(.url)"' "$pm/lock.json" | awk -v pool="$pool" 'index($2, pool) == 1' | sort -u | while read -r sha url; do
    file=${url##*/}
    case $file in
        *.*) name=termux-$sha.${file##*.} ;;
        *) name=termux-$sha ;;
    esac
    curl -fsSL --retry 3 -o "$out/$name.part" "$url"
    got=$(sha256sum "$out/$name.part" | cut -d' ' -f1)
    [ "$got" = "$sha" ] || { printf 'fetch: %s served %s, the pin says %s\n' "$url" "$got" "$sha" >&2; exit 1; }
    mv "$out/$name.part" "$out/$name"
done
count=$(find "$out" -mindepth 1 -maxdepth 1 -type f -name 'termux-*' | wc -l)
[ "$count" -gt 0 ] || { printf 'fetch: %s/lock.json pins no Termux pool archive\n' "$pm" >&2; exit 1; }
printf 'fetch: %s Termux pool archives in %s\n' "$count" "$out"
