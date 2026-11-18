#!/bin/sh
# Removes the relay runtime the post install step provisioned, keeping it through an upgrade whose post install refreshes it
set -eu
case "${1:-}" in
    upgrade | failed-upgrade | deconfigure | 1) exit 0 ;;
esac
rm -rf /usr/lib/icebath/venv /usr/lib/icebath/venv-runtime
