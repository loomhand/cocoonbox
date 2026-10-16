#!/bin/bash
# Runs the §19.5 several-boxes acceptance on the deployment host against the deployed api, as a scratch user only.
# CB_USER (default mb1) needs max boxes 2 and exactly its box main, and its console password in <CB_USER>_PASSWORD
# (MB1_PASSWORD); it gets a fresh API token. The script refuses the users in CB_REAL_USERS, checks that sandboxd
# (max_claims CB_MAX_CLAIMS, default 6) has the claims it needs, and reads the sandboxd root token and the state store's credentials with sudo.
# A failed run can leave the box test; delete it in the console before the next run.
# CB_ENDPOINT is the api's public origin.
set -euo pipefail
exec python3 "$(dirname "$0")/multibox.py"
