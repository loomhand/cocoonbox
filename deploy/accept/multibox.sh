#!/bin/bash
# Runs deploy/accept/multibox.py on the deployment host; CB_ENDPOINT is the api's public origin.
set -euo pipefail
exec python3 "$(dirname "$0")/multibox.py"
