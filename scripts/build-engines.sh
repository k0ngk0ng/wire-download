#!/bin/sh
# Build host-native headless engines. Build dependencies are needed only here,
# never on the machine installing the resulting release.
set -eu
cd "$(dirname "$0")/.."
engine_root=$(pwd)
export TMPDIR="$engine_root/.cache/tmp"
mkdir -p "$TMPDIR" .cache/amule-build/downloads .cache/amule-build/src .cache/amule-build/logs .cache/engines
engine_jobs=${JOBS:-6}
engine_os=$(uname -s)
if test "$engine_os" = Darwin; then export MACOSX_DEPLOYMENT_TARGET=${MACOSX_DEPLOYMENT_TARGET:-13.0}; fi
case "$engine_os" in Darwin|Linux) ;; *) echo 'Only macOS and Linux are supported' >&2; exit 1;; esac
for engine_tool in curl tar make c++; do command -v "$engine_tool" >/dev/null || { echo "Missing build tool: $engine_tool" >&2; exit 1; }; done

verify() {
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s  %s\n' "$2" "$1" | sha256sum -c - >/dev/null
  else
    printf '%s  %s\n' "$2" "$1" | shasum -a 256 -c - >/dev/null
  fi
}
download() {
  # Corresponding-source archives already carry these exact upstream tarballs.
  engine_archived="$engine_root/../upstream/$(basename "$2")"
  if ! test -f "$2" && test -f "$engine_archived"; then
    verify "$engine_archived" "$3"
    cp "$engine_archived" "$2"
  fi
  if ! test -f "$2"; then
    curl --fail --location --retry 3 --connect-timeout 20 --max-time 300 "$1" -o "$2.partial"
    verify "$2.partial" "$3"
    mv "$2.partial" "$2"
  fi
  verify "$2" "$3"
}
engine_downloads="$engine_root/.cache/amule-build/downloads"
engine_cxxflags="-O2 -std=c++11"

download 'https://deb.debian.org/debian/pool/main/a/amule/amule_2.3.3.orig.tar.xz' "$engine_downloads/amule_2.3.3.orig.tar.xz" a647309642331f3e033fdf0196e7232cdc67f46739d12a0294be06885f70c8bd
download 'https://github.com/wxWidgets/wxWidgets/releases/download/v3.0.5.1/wxWidgets-3.0.5.1.tar.bz2' "$engine_downloads/wxWidgets-3.0.5.1.tar.bz2" 440f6e73cf5afb2cbf9af10cec8da6cdd3d3998d527598a53db87099524ac807
download 'https://codeload.github.com/weidai11/cryptopp/tar.gz/refs/tags/CRYPTOPP_8_9_0' "$engine_downloads/cryptopp-8.9.0.tar.gz" ab5174b9b5c6236588e15a1aa1aaecb6658cdbe09501c7981ac8db276a24d9ab
download 'https://archives.boost.io/release/1.74.0/source/boost_1_74_0.tar.bz2' "$engine_downloads/boost_1_74_0.tar.bz2" 83bfc1507731a0906e387fc28b7ef5417d591429e51e788417fe9ff025e116b1
download 'https://github.com/aria2/aria2/releases/download/release-1.37.0/aria2-1.37.0.tar.xz' "$engine_root/.cache/engines/aria2-1.37.0.tar.xz" 60a420ad7085eb616cb6e2bdf0a7206d68ff3d37fb5a956dc44242eb2f79b66b

engine_prefix="$engine_root/.cache/amule-build/prefix"
engine_src="$engine_root/.cache/amule-build/src"
# Native object files must not survive a change of architecture or deployment
# target. In particular, relinking newer macOS objects does not lower minos.
engine_target="$engine_os-$(uname -m)-${MACOSX_DEPLOYMENT_TARGET:-native}-asio-v1"
engine_stamp="$engine_root/.cache/engines/build-target"
if ! test -f "$engine_stamp" || test "$(cat "$engine_stamp")" != "$engine_target"; then
  echo "Preparing clean native build for $engine_target"
  rm -rf "$engine_prefix" "$engine_root/.cache/amule-build/wx30-build" \
    "$engine_root/.cache/amule-build/amule-build" \
    "$engine_root/.cache/engines/aria2-build" "$engine_root/.cache/engines/aria2-prefix" \
    "$engine_src/cryptopp-CRYPTOPP_8_9_0"
  printf '%s\n' "$engine_target" > "$engine_stamp"
fi
test -d "$engine_src/aMule-2.3.3" || tar -xJf "$engine_downloads/amule_2.3.3.orig.tar.xz" -C "$engine_src"
test -d "$engine_src/wxWidgets-3.0.5.1" || tar -xjf "$engine_downloads/wxWidgets-3.0.5.1.tar.bz2" -C "$engine_src"
test -d "$engine_src/cryptopp-CRYPTOPP_8_9_0" || tar -xzf "$engine_downloads/cryptopp-8.9.0.tar.gz" -C "$engine_src"

test -d "$engine_src/boost_1_74_0" || tar -xjf "$engine_downloads/boost_1_74_0.tar.bz2" -C "$engine_src"
if grep -q "mpl::integral_c<" "$engine_src/boost_1_74_0/boost/numeric/conversion/detail/udt_builtin_mixture.hpp"; then
  (cd "$engine_src/boost_1_74_0" && patch -p1 < "$engine_root/scripts/patches/boost-numeric-clang.patch")
fi
if ! grep -q "Keep the managed daemon log bounded" "$engine_src/aMule-2.3.3/src/Logger.cpp"; then
  (cd "$engine_src/aMule-2.3.3" && patch -p1 < "$engine_root/scripts/patches/amule-log-rotation.patch")
fi

if ! test -f "$engine_prefix/lib/libcryptopp.a"; then
  echo 'Building Crypto++ (static)'
  make -C "$engine_src/cryptopp-CRYPTOPP_8_9_0" -j"$engine_jobs" static CXXFLAGS='-DNDEBUG -O2 -fPIC -std=c++11' > .cache/amule-build/logs/crypto-make.log 2>&1
  make -C "$engine_src/cryptopp-CRYPTOPP_8_9_0" install-lib PREFIX="$engine_prefix" > .cache/amule-build/logs/crypto-install.log 2>&1
fi
if test "$engine_os" = Darwin && ! grep -q "wiredown: headless" "$engine_src/wxWidgets-3.0.5.1/src/osx/core/evtloop_cf.cpp"; then
  (cd "$engine_src/wxWidgets-3.0.5.1" && patch -p1 < "$engine_root/scripts/patches/wxbase-headless-macos.patch")
fi
if ! test -x "$engine_prefix/bin/wx-config"; then
  echo 'Building wxBase (static, no GUI)'
  mkdir -p .cache/amule-build/wx30-build
  (cd .cache/amule-build/wx30-build
    "$engine_src/wxWidgets-3.0.5.1/configure" --prefix="$engine_prefix" --disable-gui --disable-shared --disable-debug --with-regex=builtin --with-expat=builtin --with-zlib=sys > ../logs/wx30-configure.log 2>&1
    make -j"$engine_jobs" > ../logs/wx30-make.log 2>&1
    make install > ../logs/wx30-install.log 2>&1)
fi
if ! test -f "$engine_prefix/.wirectl-asio-v1"; then
  echo 'Building aMule daemon and EC client only'
  mkdir -p .cache/amule-build/amule-build
  (cd .cache/amule-build/amule-build
    vl_cv_lib_readline=no "$engine_src/aMule-2.3.3/configure" --prefix="$engine_prefix" --with-wx-config="$engine_prefix/bin/wx-config" --with-crypto-prefix="$engine_prefix" --with-boost="$engine_src/boost_1_74_0" --enable-static-boost --with-wxshared=no --disable-monolithic --enable-amule-daemon --enable-amulecmd --disable-ed2k --disable-upnp --disable-nls --disable-debug --enable-optimize CXXFLAGS="$engine_cxxflags" > ../logs/amule-configure.log 2>&1
    cp "$engine_src/aMule-2.3.3/src/Scanner.cpp" src/Scanner.cpp
    make -j"$engine_jobs" > ../logs/amule-make.log 2>&1
    make install > ../logs/amule-install.log 2>&1)
  touch "$engine_prefix/.wirectl-asio-v1"
fi
if ! test -x .cache/engines/aria2-prefix/bin/aria2c; then
  echo 'Building aria2 (BitTorrent, magnet, HTTP/S)'
  test -d .cache/engines/aria2-1.37.0 || tar -xJf .cache/engines/aria2-1.37.0.tar.xz -C .cache/engines
  mkdir -p .cache/engines/aria2-build
  (cd .cache/engines/aria2-build
    if test "$engine_os" = Darwin; then engine_tls=--without-openssl; else engine_tls=--with-openssl; fi
    ../aria2-1.37.0/configure --prefix="$engine_root/.cache/engines/aria2-prefix" "$engine_tls" --without-gnutls --without-libssh2 --without-libxml2 --without-sqlite3 --without-libcares --disable-nls --disable-shared > configure.log 2>&1
    make -j"$engine_jobs" > make.log 2>&1
    make install > install.log 2>&1)
fi
echo 'Built engines:'
ls -lh "$engine_prefix/bin/amuled" "$engine_prefix/bin/amulecmd" .cache/engines/aria2-prefix/bin/aria2c
