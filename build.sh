#!/bin/sh
# Builds the host binary plus helper binaries the daemon deploys over ssh.
set -e
cd "$(dirname "$0")"
v="$(date +%Y%m%d%H%M%S)-$(git rev-parse --short HEAD 2>/dev/null || echo dev)"
mkdir -p bin
for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
	GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$v" -o "bin/dbuild-${t%/*}-${t#*/}" .
done
# New inode: overwriting a running/cached Mach-O in place gets it SIGKILLed on macOS.
rm -f bin/dbuild && cp "bin/dbuild-$(go env GOOS)-$(go env GOARCH)" bin/dbuild
ln -sf dbuild bin/dbuild-ninja
echo "built $v"
