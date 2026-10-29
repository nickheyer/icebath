#!/usr/bin/env bash
# Builds the icebath.app bundle with signed apt, rpm and apk indexes for the release packages in a directory
set -euo pipefail

usage='usage: build.sh <packages dir> <out dir>'
pkgs=$(realpath "${1:?$usage}")
out=${2:?$usage}
gpg_key=$(realpath "${REPO_GPG_KEY_FILE:?REPO_GPG_KEY_FILE must point at the armored repo signing key}")
apk_key=$(realpath "${APK_RSA_KEY_FILE:?APK_RSA_KEY_FILE must point at the apk RSA signing key}")
site=$(cd "$(dirname "$0")" && pwd)
owner="$(id -u):$(id -g)"

rm -rf "$out"
mkdir -p "$out"
out=$(realpath "$out")
cp -r "$site/public/." "$out/"
mkdir -p "$out/apt/pool" "$out/apk/x86_64" "$out/apk/aarch64"
cp "$pkgs"/*.deb "$out/apt/pool/"
cp "$pkgs"/*.rpm "$out/rpm/"
cp "$pkgs"/*_x86_64.apk "$out/apk/x86_64/"
cp "$pkgs"/*_aarch64.apk "$out/apk/aarch64/"

docker run --rm -v "$out:/out" -v "$gpg_key:/key.asc:ro" -e OWNER="$owner" debian:trixie-slim bash -euo pipefail -c '
apt-get update -qq
apt-get install -qq -y --no-install-recommends apt-utils createrepo-c gnupg rpm-common > /dev/null
export GNUPGHOME=$(mktemp -d)
gpg --batch --quiet --import /key.asc
cd /out/apt
for arch in amd64 arm64; do
  mkdir -p dists/stable/main/binary-$arch
  apt-ftparchive --arch $arch packages pool > dists/stable/main/binary-$arch/Packages
  grep -q "^Package: icebath$" dists/stable/main/binary-$arch/Packages
  gzip -9kf dists/stable/main/binary-$arch/Packages
done
apt-ftparchive \
  -o APT::FTPArchive::Release::Origin=icebath \
  -o APT::FTPArchive::Release::Label=icebath \
  -o APT::FTPArchive::Release::Suite=stable \
  -o APT::FTPArchive::Release::Codename=stable \
  -o "APT::FTPArchive::Release::Architectures=amd64 arm64" \
  -o APT::FTPArchive::Release::Components=main \
  release dists/stable > dists/stable/Release
gpg --batch --yes --clearsign -o dists/stable/InRelease dists/stable/Release
gpg --batch --yes --armor --detach-sign -o dists/stable/Release.gpg dists/stable/Release
createrepo_c --quiet --general-compress-type=gz /out/rpm
gpg --batch --yes --armor --detach-sign /out/rpm/repodata/repomd.xml
chown -R "$OWNER" /out
'

docker run --rm -v "$out/apk:/apk" -v "$apk_key:/icebath.rsa:ro" -e OWNER="$owner" alpine:3.22 sh -euc '
apk add -q abuild > /dev/null
for arch in x86_64 aarch64; do
  cd /apk/$arch
  apk index -q --allow-untrusted -o APKINDEX.tar.gz ./*.apk
  abuild-sign -q -k /icebath.rsa -p icebath.rsa.pub APKINDEX.tar.gz
done
chown -R "$OWNER" /apk
'

rm "$out"/apt/pool/*.deb "$out"/rpm/*.rpm "$out"/apk/*/*.apk
