#!/bin/bash
# Runs the §23.3 sharing acceptance on bench against the deployed api and qa1's v18 box.
# Needs scratch users qa1 (with a box), qa2 and qa3, and their console passwords in
# QA1_PASSWORD, QA2_PASSWORD and QA3_PASSWORD; it issues the three a fresh API token.
# CB_ENDPOINT is the api's public origin.
set -euo pipefail
source ~/build/env.sh
command -v grpcurl >/dev/null || { echo "grpcurl is not on PATH" >&2; exit 1; }
exec python3 "$(dirname "$0")/share.py"
