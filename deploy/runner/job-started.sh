#!/bin/bash
# The runner's job-started hook: fails every job before its first step unless it is CMGS's dispatch of image.yml on main of loomhand/cocoonbox.
set -uo pipefail

want="repository=loomhand/cocoonbox event=workflow_dispatch workflow=loomhand/cocoonbox/.github/workflows/image.yml@refs/heads/main ref=refs/heads/main actor=CMGS"
got="repository=${GITHUB_REPOSITORY:-} event=${GITHUB_EVENT_NAME:-} workflow=${GITHUB_WORKFLOW_REF:-} ref=${GITHUB_REF:-} actor=${GITHUB_TRIGGERING_ACTOR:-}"
if [ "$got" != "$want" ]; then
	echo "cb-runner: refused $got" >&2
	exit 1
fi
echo "cb-runner: accepted $got"
