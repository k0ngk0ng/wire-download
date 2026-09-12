#!/bin/sh
# Native releases include matching, verified engines.
set -eu
cd "$(dirname "$0")/.."
release_root=$(pwd)
export GOCACHE="$release_root/.cache/go-build"
export GOPATH="$release_root/.cache/go"
export GOTMPDIR="$release_root/.cache/tmp"
export TMPDIR="$release_root/.cache/tmp"
export CGO_ENABLED=0
mkdir -p "$GOTMPDIR" dist
release_version=${VERSION:-dev}
case "$release_version" in *[!a-zA-Z0-9._-]*) echo "Invalid VERSION" >&2; exit 1;; esac
release_os=$(go env GOOS)
release_arch=$(go env GOARCH)
case "$release_os" in darwin|linux) ;; *) echo 'Unsupported OS' >&2; exit 1;; esac
release_name="wire-download-$release_version-$release_os-$release_arch"
release_dir="$release_root/dist/$release_name"
test ! -e "$release_dir" || { echo "Release directory already exists: $release_dir" >&2; exit 1; }
sh scripts/build-engines.sh
CGO_ENABLED=1 WIRECTL_TEST_ARIA2="$release_root/.cache/engines/aria2-prefix/bin/aria2c" make check TEST_FLAGS=-count=1
mkdir -p "$release_dir/bin" "$release_dir/deploy" "$release_dir/licenses"
go build -trimpath -ldflags="-s -w -X main.version=$release_version" -o "$release_dir/bin/wirectl-download" ./cmd/wirectl-download
go build -trimpath -ldflags="-s -w -X main.version=$release_version" -o "$release_dir/bin/wirectl" ./cmd/wirectl
python3 scripts/bundle-engines.py "$release_dir/libexec/wirectl-download"
cp README.md "$release_dir/README.md"
cp scripts/install.sh "$release_dir/install.sh"
cp deploy/* "$release_dir/deploy/"
cp vendor/golang.org/x/term/LICENSE "$release_dir/licenses/golang-x-term.txt"
cp vendor/golang.org/x/sys/LICENSE "$release_dir/licenses/golang-x-sys.txt"
cp vendor/github.com/gorilla/websocket/LICENSE "$release_dir/licenses/gorilla-websocket.txt"
cp vendor/golang.org/x/net/LICENSE "$release_dir/licenses/golang-x-net.txt"
cp .cache/engines/aria2-1.37.0/COPYING "$release_dir/licenses/aria2.txt"
cp .cache/amule-build/src/aMule-2.3.3/docs/COPYING "$release_dir/licenses/amule.txt"
cp .cache/amule-build/src/wxWidgets-3.0.5.1/docs/licence.txt "$release_dir/licenses/wxwidgets.txt"
cp .cache/amule-build/src/wxWidgets-3.0.5.1/docs/lgpl.txt "$release_dir/licenses/wxwidgets-lgpl.txt"
cp .cache/amule-build/src/cryptopp-CRYPTOPP_8_9_0/License.txt "$release_dir/licenses/cryptopp.txt"
cp .cache/amule-build/src/boost_1_74_0/LICENSE_1_0.txt "$release_dir/licenses/boost.txt"
# Accompany binary releases with corresponding source. Not an install dependency.
release_sources="$release_root/dist/wire-download-$release_version-sources"
mkdir -p "$release_sources/upstream" "$release_sources/wire-download" "$release_sources/wirectl"
cp .cache/amule-build/downloads/amule_2.3.3.orig.tar.xz .cache/amule-build/downloads/wxWidgets-3.0.5.1.tar.bz2 .cache/amule-build/downloads/cryptopp-8.9.0.tar.gz .cache/amule-build/downloads/boost_1_74_0.tar.bz2 .cache/engines/aria2-1.37.0.tar.xz "$release_sources/upstream/"
if test "$release_os" = linux; then
  python3 scripts/bundle-runtime-sources.py "$release_dir/libexec/wirectl-download/runtime-libraries.json" "$release_sources/upstream/debian"
fi
cp -R cmd internal scripts deploy vendor go.mod go.sum Makefile README.md RELEASE_NOTES.md .github "$release_sources/wire-download/"
if test -d ../wirectl; then cp -R ../wirectl/cli ../wirectl/cmd ../wirectl/go.mod ../wirectl/README.md "$release_sources/wirectl/"; fi
printf 'Source archive: wire-download-%s-sources.tar.gz\nBuild recipes and patches are in wire-download/scripts/.\n' "$release_version" > "$release_dir/licenses/SOURCES.txt"
tar -czf "dist/wire-download-$release_version-sources.tar.gz" -C dist "wire-download-$release_version-sources"
tar -czf "dist/$release_name.tar.gz" -C dist "$release_name"
if command -v sha256sum >/dev/null 2>&1; then
  (cd dist && sha256sum ./*.tar.gz > SHA256SUMS)
else
  (cd dist && shasum -a 256 ./*.tar.gz > SHA256SUMS)
fi
ls -lh "dist/$release_name.tar.gz"
