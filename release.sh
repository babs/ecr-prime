#!/usr/bin/env bash
# Cross-compile static binaries for release. Mirrors babs/ecr-repo-creator's
# release.sh (module mode instead of vendored deps).
set -euo pipefail

MODULE=$(grep '^module' go.mod | cut -d' ' -f2)
readonly MODULE
readonly BINBASE=${MODULE##*/}
readonly URL="https://github.com/babs/${BINBASE}"

VERSION=${VERSION:-${GITHUB_REF_NAME:-}}
VERSION=${VERSION:-0.0.0}
COMMIT_HASH="$(git rev-parse --short HEAD 2>/dev/null || echo 0000000)"
DIRTY=$(git diff --quiet 2>/dev/null || echo '-dirty')
BUILD_TIMESTAMP=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
BUILDER=$(go version)
readonly VERSION COMMIT_HASH DIRTY BUILD_TIMESTAMP BUILDER

LDFLAGS=(
  "-s" "-w"
  "-X 'main.Version=${VERSION}'"
  "-X 'main.CommitHash=${COMMIT_HASH}${DIRTY}'"
  "-X 'main.BuildTimestamp=${BUILD_TIMESTAMP}'"
  "-X 'main.Builder=${BUILDER}'"
  "-X 'main.ProjectURL=${URL}'"
)

echo "[*] build info: ${VERSION} ${COMMIT_HASH}${DIRTY} ${BUILD_TIMESTAMP}"

[ -d dist ] && rm -rf dist
mkdir dist

for DIST in linux/{amd64,arm64} darwin/arm64; do
  GOOS=${DIST%/*}
  GOARCH=${DIST#*/}
  TARGET="${BINBASE}-${GOOS}-${GOARCH}"
  echo "[+] ${DIST}"
  env CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -trimpath -ldflags="${LDFLAGS[*]}" -o "dist/${TARGET}" .
  xz "dist/${TARGET}"
done

(cd dist && sha256sum -- * > SHA256SUMS)
echo "[*] done"
