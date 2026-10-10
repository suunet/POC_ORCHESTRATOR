#!/bin/sh
# 配布用の実行ファイルを Docker の中でビルドし、bin/ に置く（macOS 用と Linux 用）。
# Go の版は go.mod の toolchain の行に合わせる。ホストに Go は不要。
set -eu

cd "$(dirname "$0")/.."

GO_VERSION=$(sed -n 's/^toolchain go//p' go.mod)
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
TARGETS="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64"

mkdir -p bin
for target in $TARGETS; do
	os=${target%/*}
	arch=${target#*/}
	echo "build: $os/$arch ($VERSION)"
	docker run --rm \
		-v "$PWD":/src -w /src \
		-e CGO_ENABLED=0 -e GOOS="$os" -e GOARCH="$arch" -e GOFLAGS=-buildvcs=false \
		"golang:$GO_VERSION" \
		go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
		-o "bin/gateflow-$os-$arch" ./cmd/gateflow
done
