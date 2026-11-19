#!/usr/bin/env bash
# Pull the images that a command needs BEFORE that command, so that the command itself never pulls.
# Anonymous pulls from the public registries (Amazon ECR Public, Docker Hub, ghcr.io, quay.io) are counted per source
# address, and the CI runners share their addresses with many other projects: now and then a pull is refused ("Data limit
# exceeded", "toomanyrequests") and goes through a minute later. So: 5 attempts, 15, 30, 60 and 120 seconds apart, a line on
# stderr for each failure, one BLOCKED line and exit 2 after the last. An image that is already in the Docker daemon is not
# pulled, so SKIP_BUILD runs and offline work behave as before.
#
#   bash deploy/pull-image.sh IMAGE...        (no argument: nothing to do)
#   IDP_PULL_WAITS="1 1 1 1"                   the waits in seconds, for the test of this script (tests/deploy)
set -uo pipefail
read -ra WAITS <<<"${IDP_PULL_WAITS:-15 30 60 120}"
ATTEMPTS=5
for img in "$@"; do
  docker image inspect "$img" >/dev/null 2>&1 && continue
  for ((try = 1; ; try++)); do
    # stderr only: with -q the image ID on stdout is of no use here
    msg="$(docker pull -q "$img" 2>&1 >/dev/null)" && break
    msg="$(tail -n 1 <<<"$msg" | cut -c1-300)"
    if ((try >= ATTEMPTS)); then
      echo "BLOCKED: could not pull $img after $ATTEMPTS attempts: $msg" >&2
      exit 2
    fi
    wait="${WAITS[try - 1]:-${WAITS[-1]}}"
    echo "pull $img: attempt $try of $ATTEMPTS failed: $msg; again in $wait s" >&2
    sleep "$wait"
  done
done
