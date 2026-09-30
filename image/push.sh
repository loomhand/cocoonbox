#!/bin/bash
# Host-side driver: runs provision.sh inside a cocoon-cb VM and installs its agent.env.
# usage: sudo image/push.sh <vm-name>
set -euo pipefail

VM=${1:?usage: push.sh <vm-name>}
COCOON=${COCOON:-/usr/local/bin/cocoon-cb}
HOST_PUBKEY_FILE=${HOST_PUBKEY_FILE:-/home/builder/.ssh/cocoonbox_deploy.pub}
GW_BASE_URL=http://10.203.0.1:18090/v1

SRC="$(cd "$(dirname "$0")" && pwd)"

tar -C "$SRC" -cf - provision.sh rootfs | "$COCOON" vm exec -i "$VM" -- sh -c \
	'rm -rf /tmp/cb-image && mkdir -p /tmp/cb-image && tar -xf - -C /tmp/cb-image'
"$COCOON" vm exec -e "CB_HOST_PUBKEY=$(cat "$HOST_PUBKEY_FILE")" "$VM" -- bash /tmp/cb-image/provision.sh
"$COCOON" vm exec "$VM" -- rm -rf /tmp/cb-image

{
	printf 'GW_BASE_URL=%s\n' "$GW_BASE_URL"
	printf 'GW_API_KEY=unprovisioned\n'
	printf 'MODEL_MAIN=deepseek/deepseek-v4.1-flash\n'
	printf 'MODEL_COMPUTER=anthropic/claude-sonnet-5\n'
	printf 'AGENT_LISTEN=0.0.0.0:7443\n'
} | "$COCOON" vm exec -i "$VM" -- /usr/local/lib/cocoonbox/set-agent-env
