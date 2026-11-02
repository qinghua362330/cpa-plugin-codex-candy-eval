#!/usr/bin/env bash
set -euo pipefail
python3 scripts/company-sync.py verify-release
version="${COMPANY_TAG#v}"
id=cpa-codex-candy-eval
name="${id}_${version}_linux_amd64"
library="${id}-v${version}.so"
mkdir -p dist/company dist/company-build
# Same glibc floor as the upstream release workflow.
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD:/src" -w /src -v "$(go env GOROOT):/opt/go:ro" \
  -e GOROOT=/opt/go -e HOME=/tmp -e GOFLAGS=-buildvcs=false \
  -e CGO_ENABLED=1 -e GOOS=linux -e GOARCH=amd64 \
  quay.io/pypa/manylinux2014_x86_64 \
  /bin/bash -euc '
    /opt/go/bin/go build -trimpath -ldflags="-s -w" -tags cshared \
      -buildmode=c-shared -o "$1" ./cmd/cpa-codex-candy-eval
    /opt/python/cp311-cp311/bin/python3 scripts/smoke-shared-library.py "$1" "$2"
  ' -- "dist/company/$library" "$version"
file "dist/company/$library" | grep -E 'ELF 64-bit LSB shared object, x86-64'
tar -czf "dist/company/$name.tar.gz" -C dist/company "$library"
# Keep the upstream plugin-store ZIP contract as well as the versioned tarball.
cp "dist/company/$library" "dist/company-build/$id.so"
zip -j "dist/company/$name.zip" "dist/company-build/$id.so"
rm "dist/company/${library%.so}.h"
(
  cd dist/company
  sha256sum "$library" "$name.tar.gz" "$name.zip" > checksums.txt
  sha256sum -c checksums.txt
)
