#!/bin/bash
# Checks cut.sh -n: runtime order, the cbcore default runtime, the guest gate family and the refusals.
set -euo pipefail
export LC_ALL=C AUDIT=/dev/null

IMAGE="$(cd "$(dirname "$0")/.." && pwd)"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/one" "$WORK/two" "$WORK/empty"
touch "$WORK/one/provision.sh" "$WORK/two/provision.sh"
WORK=$(cd "$WORK" && pwd -P)
fails=0

expect() {
	local name=$1 want=$2
	shift 2
	local got
	got=$("$IMAGE/cut.sh" -n "$@" 2>&1) || got="exit $?: $got"
	if [ "$got" != "$want" ]; then
		printf 'FAIL %s\n--- want\n%s\n--- got\n%s\n' "$name" "$want" "$got"
		fails=$((fails + 1))
	fi
}

expect "runtimes run in the order given" "boot base:v1 as cbcut-fam-v2
runtime $WORK/two
runtime $WORK/one
seal
stop
export /srv/cb/sbx/build/cocoonbox-fam-v2.tar.gz
import fam:v2" -f fam -r "$WORK/two" -r "$WORK/one" base:v1 v2

expect "cbcore without -r adds the desk runtime and the guest gate" "boot base:v1 as cbcut-cbcore-v2
provision
runtime $IMAGE/desk
clean gate
seal
stop
export /srv/cb/sbx/build/cocoonbox-cbcore-v2.tar.gz
import cbcore:v2" -p -f cbcore base:v1 v2

expect "cbcore with -r uses only the runtimes given" "boot base:v1 as cbcut-cbcore-v2
runtime $WORK/one
clean gate
seal
stop
export /srv/cb/sbx/build/cocoonbox-cbcore-v2.tar.gz
import cbcore:v2" -f cbcore -r "$WORK/one" base:v1 v2

expect "cbdesk keeps its tarball name" "boot base:v1 as cbcut-v2
seal
stop
export /srv/cb/sbx/build/cocoonbox-desktop-v2.tar.gz
import cbdesk:v2" base:v1 v2

expect "a runtime without provision.sh is refused" "exit 2: runtime $WORK/empty has no provision.sh" -f fam -r "$WORK/empty" base:v1 v2
expect "a family off the grammar is refused" "exit 2: family Fam is not [a-z][a-z0-9]*" -f Fam base:v1 v2

[ "$fails" = 0 ] || exit 1
echo "cut.sh dry run: ok"
