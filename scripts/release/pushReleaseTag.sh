#!/usr/bin/env bash
# Tags the next release and pushes it so release.yml publishes every package
set -euo pipefail

usage() {
  cat << 'USAGE'
usage: pushReleaseTag.sh [--patch | --minor | --major | --set X.Y.Z] [--dry-run]
  --patch        v1.0.8 -> v1.0.9 (default)
  --minor        v1.0.8 -> v1.1.0
  --major        v1.0.8 -> v2.0.0
  --set X.Y.Z    exactly vX.Y.Z
  --dry-run      print the plan and touch nothing
USAGE
}

BUMP='patch'
SET=''
DRY=false
while [ $# -gt 0 ]; do
  case "$1" in
    --major | major) BUMP=major ;;
    --minor | minor) BUMP=minor ;;
    --patch | patch) BUMP='patch' ;;
    --set)
      SET="${2:?--set needs a version}"
      shift
      ;;
    --dry-run | -n) DRY=true ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "pushReleaseTag: unknown argument '$1'" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

cd "$(git rev-parse --show-toplevel)"
git symbolic-ref -q HEAD > /dev/null || {
  echo "pushReleaseTag: detached HEAD - check out a branch first" >&2
  exit 1
}

git fetch --tags --quiet origin

BASE=$(git tag --list 'v[0-9]*' | sed 's/^v//' | sort -V | tail -1)
BASE=${BASE:-0.0.0}
BASE=${BASE%%-*}
[[ $BASE =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "pushReleaseTag: highest tag v$BASE is not X.Y.Z" >&2
  exit 1
}

if [ -n "$SET" ]; then
  NEW=$SET
  [[ $NEW =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    echo "pushReleaseTag: --set '$SET' is not X.Y.Z" >&2
    exit 1
  }
else
  IFS=. read -r MA MI PA <<< "$BASE"
  case "$BUMP" in
    major) NEW="$((MA + 1)).0.0" ;;
    minor) NEW="$MA.$((MI + 1)).0" ;;
    patch) NEW="$MA.$MI.$((PA + 1))" ;;
  esac
fi
TAG="v$NEW"

git rev-parse -q --verify "refs/tags/$TAG" > /dev/null && {
  echo "pushReleaseTag: $TAG already exists locally" >&2
  exit 1
}
[ -n "$(git ls-remote --tags origin "$TAG")" ] && {
  echo "pushReleaseTag: $TAG already exists on origin" >&2
  exit 1
}
[ -z "$(git status --porcelain)" ] || {
  echo "pushReleaseTag: uncommitted changes would be left out of $TAG" >&2
  git status --short >&2
  exit 1
}

echo "pushReleaseTag: v$BASE -> $TAG"
if $DRY; then
  echo "  would tag HEAD as $TAG and push HEAD + $TAG to origin"
  exit 0
fi

git tag -a "$TAG" -m "icebath $NEW"
git push origin HEAD "refs/tags/$TAG"
echo "pushed $TAG - release.yml is publishing it"
