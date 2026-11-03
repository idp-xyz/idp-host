#!/usr/bin/env bash
# Build the fakemodel test-double image offline (static binary on the host, docker build --network none).
# Usage: bash deploy/fixtures/build-fakemodel.sh [TAG]     (default idp-host-fakemodel:dev)
set -euo pipefail
cd "$(dirname "$0")/../.."
TAG="${1:-idp-host-fakemodel:dev}"
OUT=build/fakemodel-image
umask 022
rm -rf "$OUT" && mkdir -p "$OUT"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$OUT/fakemodel" ./tests/fixtures/fakemodel/cmd
cp deploy/fixtures/Dockerfile.fakemodel "$OUT/Dockerfile"
docker build --network none -q -t "$TAG" "$OUT" > /dev/null
echo "OK: $TAG"
