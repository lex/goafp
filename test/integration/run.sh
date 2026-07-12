#!/bin/sh
# Run goafp integration tests against netatalk in Docker.
set -eu

PORT="${GOAFP_TEST_PORT:-15548}"
USER=gotest
PASS=gotest-pw
VOLUME=TestShare
CONTAINER=goafp-netatalk-test
IMAGE="${GOAFP_NETATALK_IMAGE:-netatalk/netatalk:latest}"

cd "$(dirname "$0")/../.."

share=$(mktemp -d)
trap 'docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; rm -rf "$share"' EXIT

# Seed the share. The unicode name tests decomposed/precomposed handling.
printf 'hello, world!\n' > "$share/hello.txt"
mkdir "$share/subdir"
printf 'nested\n' > "$share/subdir/nested.txt"
printf 'yum\n' > "$share/smörgåsbord.txt"
# A few MB filled with a repeating 0..250 byte pattern, for the pipelined
# large-read test. Built with a tiny Go program to match patternData.
go run ./test/integration/mkbig "$share/big.bin" 4194304
chmod -R a+rwX "$share"

docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$CONTAINER" \
    -p "127.0.0.1:$PORT:548" \
    -v "$share:/mnt/afpshare" \
    -e AFP_USER="$USER" \
    -e AFP_PASS="$PASS" \
    -e AFP_GROUP="$USER" \
    -e SHARE_NAME="$VOLUME" \
    -e SERVER_NAME=goafp-test \
    "$IMAGE" >/dev/null

echo "waiting for netatalk on port $PORT..."
i=0
until nc -z 127.0.0.1 "$PORT" 2>/dev/null; do
    i=$((i+1))
    if [ "$i" -gt 60 ]; then
        echo "netatalk did not come up; container logs:" >&2
        docker logs "$CONTAINER" >&2
        exit 1
    fi
    sleep 1
done
# The port opens before afpd fully finishes user setup; give it a moment.
sleep 2

GOAFP_TEST_ADDR="127.0.0.1:$PORT" \
GOAFP_TEST_USER="$USER" \
GOAFP_TEST_PASS="$PASS" \
GOAFP_TEST_VOLUME="$VOLUME" \
    go test -tags integration -count=1 -v ./test/integration/
