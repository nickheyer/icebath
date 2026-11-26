#!/bin/sh
# Downloads every Termux pool archive the pm lock pins, checks its sha256 and stores it under its release asset name
# An archive the pool already deleted comes from the newest published release that carries its copy
set -eu
out=${1:?usage: fetch.sh <out dir>}
here=$(cd "$(dirname "$0")" && pwd)
pm=$here/../../pkg/pm
pool=https://packages.termux.dev/apt/termux-main/pool/main/
repo=${GITHUB_REPOSITORY:-nickheyer/icebath}
assets=$(mktemp)
trap 'rm -f "$assets"' EXIT
# Download URL of the newest published release asset with the given name, the asset list read once on first use
release_copy() {
    [ -s "$assets" ] || gh api --paginate "repos/$repo/releases?per_page=100" \
        -q '.[] | select(.draft | not) | .assets[] | "\(.name) \(.browser_download_url)"' > "$assets" || return 1
    awk -v name="$1" '$1 == name { print $2; exit }' "$assets"
}
mkdir -p "$out"
jq -r '.packages[].artifacts[] | if type == "array" then .[] else . end | select(.sha256 != null) | "\(.sha256) \(.url)"' "$pm/lock.json" | awk -v pool="$pool" 'index($2, pool) == 1' | sort -u | while read -r sha url; do
    file=${url##*/}
    case $file in
        *.*) name=termux-$sha.${file##*.} ;;
        *) name=termux-$sha ;;
    esac
    src=$url
    if ! curl -fsSL --retry 3 -o "$out/$name.part" "$url"; then
        copy=$(release_copy "$name") || { printf 'fetch: the pool no longer serves %s and the %s releases could not be listed\n' "$url" "$repo" >&2; exit 1; }
        [ -n "$copy" ] || { printf 'fetch: the pool no longer serves %s and no %s release carries %s, repin with icebath pm update --termux\n' "$url" "$repo" "$name" >&2; exit 1; }
        printf 'fetch: the pool no longer serves %s, taking the release copy %s\n' "$url" "$copy" >&2
        src=$copy
        curl -fsSL --retry 3 -o "$out/$name.part" "$copy"
    fi
    got=$(sha256sum "$out/$name.part" | cut -d' ' -f1)
    [ "$got" = "$sha" ] || { printf 'fetch: %s served %s, the pin says %s\n' "$src" "$got" "$sha" >&2; exit 1; }
    mv "$out/$name.part" "$out/$name"
done
count=$(find "$out" -mindepth 1 -maxdepth 1 -type f -name 'termux-*' | wc -l)
[ "$count" -gt 0 ] || { printf 'fetch: %s/lock.json pins no Termux pool archive\n' "$pm" >&2; exit 1; }
printf 'fetch: %s Termux pool archives in %s\n' "$count" "$out"
