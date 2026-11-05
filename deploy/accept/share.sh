#!/bin/bash
# Runs deploy/accept/share.py on the deployment host; CB_ENDPOINT is the api's public origin.
set -euo pipefail
[ -z "${CB_ENV:-}" ] || source "$CB_ENV"
command -v grpcurl >/dev/null || { echo "grpcurl is not on PATH" >&2; exit 1; }
exec python3 "$(dirname "$0")/share.py"
