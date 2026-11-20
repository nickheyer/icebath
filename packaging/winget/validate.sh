#!/bin/sh
# Checks a WinGet manifest set against the schemas of the type and version each file declares and against each other
set -eu
dir=${1:?usage: validate.sh <manifest dir>}
field() {
    awk -v k="$1" 'index($0, k ": ") == 1 { v = substr($0, length(k) + 3); gsub(/^"|"$/, "", v); print v; exit }' "$2"
}
fail() {
    printf 'validate: %s\n' "$*" >&2
    exit 1
}
id='' pkgver='' manver='' versions=0 installers=0 defaults=0 default_locale='' package_locale=''
for f in "$dir"/*.yaml; do
    [ -f "$f" ] || fail "$dir holds no manifest"
    type=$(field ManifestType "$f")
    mv=$(field ManifestVersion "$f")
    [ -n "$type" ] && [ -n "$mv" ] || fail "$f declares no ManifestType or ManifestVersion"
    check-jsonschema --schemafile "https://aka.ms/winget-manifest.$type.$mv.schema.json" "$f"
    fid=$(field PackageIdentifier "$f")
    fver=$(field PackageVersion "$f")
    : "${id:=$fid}" "${pkgver:=$fver}" "${manver:=$mv}"
    [ "$fid" = "$id" ] && [ "$fver" = "$pkgver" ] && [ "$mv" = "$manver" ] ||
        fail "$f declares $fid $fver manifest $mv, another file $id $pkgver manifest $manver"
    case $type in
        version) versions=$((versions + 1)) default_locale=$(field DefaultLocale "$f") want="$id.yaml" ;;
        installer) installers=$((installers + 1)) want="$id.installer.yaml" ;;
        defaultLocale) defaults=$((defaults + 1)) package_locale=$(field PackageLocale "$f") want="$id.locale.$package_locale.yaml" ;;
        locale) want="$id.locale.$(field PackageLocale "$f").yaml" ;;
        *) fail "$f is a $type manifest, a multi file set holds version, installer and locale manifests" ;;
    esac
    [ "$(basename "$f")" = "$want" ] || fail "$f must be named $want"
done
[ "$versions" = 1 ] && [ "$installers" = 1 ] && [ "$defaults" = 1 ] ||
    fail "$dir holds $versions version, $installers installer and $defaults defaultLocale manifests, one of each is required"
[ "$default_locale" = "$package_locale" ] || fail "DefaultLocale $default_locale has no defaultLocale manifest, that one is $package_locale"
printf 'validate: %s %s, manifest %s, every file matches its schema\n' "$id" "$pkgver" "$manver"
