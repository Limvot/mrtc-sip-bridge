#!/usr/bin/env bash
set -e

# Build inside Docker to ensure a repeatable environment with all dependencies
# (CGO, libopus, sqlite-dev). The binary is extracted to the current directory.
# Extra arguments are passed to docker build (e.g. -t williamkray/mrtc-sip-bridge:v0.1.0).
docker build -t mrtc-sip-bridge $(./build-args.sh) "$@" .
container_id=$(docker create mrtc-sip-bridge)
docker cp "$container_id:/app/mrtc-sip-bridge" ./mrtc-sip-bridge
docker rm "$container_id" > /dev/null
echo "Built: ./mrtc-sip-bridge"
