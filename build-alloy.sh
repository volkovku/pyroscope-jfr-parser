#!/usr/bin/env bash
set -euo pipefail

prepare_only=false
if [[ ${1:-} == --prepare-only ]]; then
    prepare_only=true
    shift
fi
if [[ $# != 1 ]]; then
    echo "Usage: bash build-alloy.sh [--prepare-only] NEW_CHECKOUT_DIRECTORY" >&2
    exit 2
fi

parser_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
alloy_revision=100cfccbc03519f6e29b58ca6bb54b4c63e108ef
alloy_dir=$1
if [[ -e $alloy_dir ]]; then
    echo "Checkout directory already exists: $alloy_dir" >&2
    exit 2
fi
export GOTOOLCHAIN=go1.26.5
export GOWORK=off

git clone --filter=blob:none --no-checkout https://github.com/grafana/alloy.git "$alloy_dir"
alloy_dir=$(cd -- "$alloy_dir" && pwd -P)
git -C "$alloy_dir" fetch origin "$alloy_revision"
git -C "$alloy_dir" checkout --detach "$alloy_revision"

for module_dir in "$alloy_dir" "$alloy_dir/collector"; do
    (
        cd -- "$module_dir"
        go mod edit \
            "-replace=github.com/grafana/jfr-parser=$parser_dir" \
            "-replace=github.com/grafana/jfr-parser/pprof=$parser_dir/pprof"
    )
done

if $prepare_only; then
    echo "Prepared Alloy checkout: $alloy_dir"
    exit 0
fi

(cd -- "$parser_dir" && go test -mod=readonly ./parser/...)
(cd -- "$parser_dir/pprof" && go test -mod=readonly ./...)
cd -- "$alloy_dir"
go test -mod=mod -p=2 -count=1 -tags=nodocker \
    ./internal/component/pyroscope/java ./internal/component/pyroscope/java/asprof
GOFLAGS='-mod=mod -p=2' make alloy CGO_ENABLED=0 RELEASE_BUILD=1 \
    SKIP_UI_BUILD=1 SKIP_CODE_GENERATION=1 GOOS=linux GOARCH=amd64 GO_TAGS='' \
    ALLOY_BINARY=build/alloy-linux-amd64 \
    VERSION=v1.17.0-devel-pr6725-100cfcc-parser116-b18082a
./build/alloy-linux-amd64 --version
sha256sum build/alloy-linux-amd64
