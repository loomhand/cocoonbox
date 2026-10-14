#!/bin/bash
# Fails when the guest holds deny-listed code: a file named after it, a text file that names it, a node module of it,
# or an agent binary that names it. Runs as root in the guest (cut.sh -f cbcore after the agent step, or in a live box).
set -uo pipefail
export LC_ALL=C

# the deny list is base64 so the gate does not match itself
PATTERN=$(echo c2ltdWxhcnxzaW11bGFuZ3xjYnNpbQ== | base64 -d)
STEM=$(echo c2ltdWw= | base64 -d)
SCOPE=$(echo c2ltdWxhci1haQ== | base64 -d)
ALLOW='^/usr/share/icons/Humanity/.*/applications-simulation\.svg$'
AGENT=/usr/local/bin/cocoonbox-agent

named=$(find / -xdev \( -path /proc -o -path /sys -o -path /dev -o -path /run \) -prune -o -iname "*$STEM*" -print 2>/dev/null | grep -Ev "$ALLOW")
texts=$(grep -rIl -i -E "$PATTERN" /opt /usr/local /etc /home /workspace /usr/share/applications 2>/dev/null)
modules=$(find / -xdev -type d -path "*/node_modules/@$SCOPE" -print 2>/dev/null)
binary=$(grep -a -c -i -E "$PATTERN" "$AGENT")
count() { [ -z "$1" ] && echo 0 || printf '%s\n' "$1" | wc -l; }
echo "clean gate: named $(count "$named"), texts $(count "$texts"), modules $(count "$modules"), agent strings $binary"
printf '%s\n' "$named" "$texts" "$modules" | grep . | head -40
[ -z "$named$texts$modules" ] && [ "$binary" = 0 ]
