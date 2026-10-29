#!/usr/bin/env bash
# Creates the publishing repos and keys, loads .env into repo secrets and reports every step a release still needs
set -euo pipefail

cd "$(dirname "$0")/../.."
[ -f .env ] || {
  echo "setup: .env is missing" >&2
  exit 1
}
set -a
# shellcheck disable=SC1091
source .env
set +a

REPO=nickheyer/icebath
SECRETS=(AUR_KEY HOMEBREW_TAP_KEY SCOOP_BUCKET_KEY NIXPKG_KEY REPO_GPG_KEY APK_RSA_KEY
  CLOUDFLARE_API_TOKEN WINGET_TOKEN CHOCOLATEY_API_KEY PYPI_API_TOKEN)
todo=()

# Creates a public repo under nickheyer when it does not exist yet
ensure_repo() {
  gh repo view "nickheyer/$1" > /dev/null 2>&1 \
    || gh repo create "nickheyer/$1" --public --description "$2" --homepage https://icebath.app
}

# Adds the public half of a private key as a write deploy key unless the repo already has it
ensure_deploy_key() {
  local pub
  pub=$(ssh-keygen -y -f <(printf '%s\n' "$2") | cut -d ' ' -f 1,2)
  gh repo deploy-key list -R "nickheyer/$1" --json key -q '.[].key' | grep -qxF "$pub" \
    || gh repo deploy-key add <(printf '%s icebath-release\n' "$pub") -R "nickheyer/$1" --allow-write -t "icebath release"
}

ensure_repo homebrew-tap "Homebrew tap for icebath"
ensure_repo scoop-bucket "Scoop bucket for icebath"
gh repo view nickheyer/winget-pkgs > /dev/null 2>&1 || gh repo fork microsoft/winget-pkgs --clone=false
ensure_deploy_key homebrew-tap "$HOMEBREW_TAP_KEY"
ensure_deploy_key scoop-bucket "$SCOOP_BUCKET_KEY"
ensure_deploy_key nixpkg "$NIXPKG_KEY"

for name in "${SECRETS[@]}"; do
  if [ -n "${!name:-}" ]; then
    printf '%s' "${!name}" | gh secret set "$name" -R "$REPO"
  else
    todo+=("put $name in .env")
  fi
done

gh api repos/nickheyer/nixpkg/contents/default.nix -q .content | base64 -d | grep -qF 'pkgs/icebath' \
  || todo+=("push the icebath package in ~/code/nixpkg so nickheyer/nixpkg default.nix calls ./pkgs/icebath")

if [ ${#todo[@]} -gt 0 ]; then
  echo "setup: before make release" >&2
  printf '  %s\n' "${todo[@]}" >&2
  exit 1
fi
echo "setup: every release secret is set on $REPO"
