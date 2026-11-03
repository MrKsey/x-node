#!/usr/bin/env bash
#
# Builds the Linux server binaries into bin/. Run it from anywhere; the output
# is always next to this script.
#
#   ./build.sh                 linux/amd64
#   GOARCH=arm64 ./build.sh    linux/arm64
#   VERSION=1.4.0 ./build.sh   stamp a release version
#
# The version is baked into the binary and printed by "x-node version".

set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
mkdir -p bin

TARGET_ARCH="${GOARCH:-amd64}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
LDFLAGS="-s -w -X main.daemonVersion=${VERSION}"

GOOS=linux GOARCH="$TARGET_ARCH" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$LDFLAGS" -o bin/x-node ./cmd/x-node

# The probe is its own module: it depends on the Yggdrasil library alone, and
# keeping it separate keeps that dependency out of the daemon's go.sum.
(
    cd cmd/x-node-peerprobe
    GOOS=linux GOARCH="$TARGET_ARCH" CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w" -o ../../bin/x-node-peerprobe .
)

ls -lh bin/x-node bin/x-node-peerprobe
