#!/bin/bash
# Runs the §19.5 several-boxes acceptance on bench against the deployed api, as a scratch user only.
# CB_USER (default mb1) needs max boxes 2 and exactly its box main, and its console password in <CB_USER>_PASSWORD
# (MB1_PASSWORD); it gets a fresh API token. The script refuses test, cmgs, dev and a tester, checks that sandboxd
# (max_claims CB_MAX_CLAIMS, default 6) has the claims it needs, and reads the sandboxd and MinIO root secrets with sudo.
# A failed run can leave the box test; delete it in the console before the next run.
# CB_ENDPOINT overrides https://box.example.com.
set -euo pipefail
exec python3 "$(dirname "$0")/multibox.py"
