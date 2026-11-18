#!/bin/sh
# Removes the relay runtime environments that earlier versions provisioned beside the packaged binary
set -eu
rm -rf /usr/lib/icebath/venv /usr/lib/icebath/venv-runtime
