#!/bin/bash
# Build a static Linux binary (no CGO) with the version from git, e.g. ./build.sh, then ./go-sendemail -version
set -euo pipefail
cd "$(dirname "$0")"
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$version" -o go-sendemail .
echo "built ./go-sendemail $version"
