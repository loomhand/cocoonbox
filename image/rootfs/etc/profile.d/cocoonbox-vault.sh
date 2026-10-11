# shellcheck shell=sh
# the box's API keys: each variable holds a placeholder that the egress swaps for the key on the key's hosts
if [ -r /run/cocoonbox-vault/vault.sh ]; then . /run/cocoonbox-vault/vault.sh; fi
