#!/usr/bin/env sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
DEFAULT_REPO_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
BUILD_DIR=${BUILD_DIR:-$(pwd)}
REPO_DIR=${REPO_DIR:-$(git -C "$BUILD_DIR" rev-parse --show-toplevel 2>/dev/null || echo "$DEFAULT_REPO_DIR")}

OUTPUT_PATH=${1:-"$BUILD_DIR/isola"}
PACKAGE_PATH=${2:-./cmd/server}
GO_BUILD_TAGS=${GO_BUILD_TAGS:-}
VERSION_VALUE=${VERSION:-$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)}
VERSION_VALUE=${VERSION_VALUE#v}
COMMIT_VALUE=${COMMIT:-$(git -C "$REPO_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)}
BUILD_TIME_VALUE=${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}

LDFLAGS="-X github.com/dm0xa/isola-browser/backend/pkg/version.Version=${VERSION_VALUE} -X github.com/dm0xa/isola-browser/backend/pkg/version.Commit=${COMMIT_VALUE} -X github.com/dm0xa/isola-browser/backend/pkg/version.BuildTime=${BUILD_TIME_VALUE}"

cd "$BUILD_DIR"

if [ -n "$GO_BUILD_TAGS" ]; then
  exec go build -tags "$GO_BUILD_TAGS" -ldflags "$LDFLAGS" -o "$OUTPUT_PATH" "$PACKAGE_PATH"
fi

exec go build -ldflags "$LDFLAGS" -o "$OUTPUT_PATH" "$PACKAGE_PATH"
