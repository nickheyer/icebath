#!/bin/sh
# Provisions the NeMo Relay runtime beside the packaged binary through its pm, the dependency step every install path runs
set -eu
home="$(mktemp -d)"
trap 'rm -rf "$home"' EXIT
ICEBATH_HOME="$home" /usr/lib/icebath/icebath pm install --extra nemo-relay
rm -rf /usr/lib/icebath/venv-runtime/uv-cache
chmod -R a+rX,go-w /usr/lib/icebath/venv /usr/lib/icebath/venv-runtime
