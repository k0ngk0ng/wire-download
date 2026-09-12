#!/bin/sh
# Installs an already self-contained release. No network or package manager.
set -eu
install_source=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
install_prefix=${1:-"$HOME/.local"}
case "$install_prefix" in /*) ;; *) echo "Installation prefix must be absolute" >&2; exit 1;; esac
for install_binary in bin/wirectl bin/wirectl-download libexec/wirectl-download/bin/aria2c libexec/wirectl-download/bin/amuled libexec/wirectl-download/bin/amulecmd; do
  test -x "$install_source/$install_binary" || { echo "Incomplete release: $install_binary is missing" >&2; exit 1; }
done
mkdir -p "$install_prefix/bin" "$install_prefix/libexec" "$install_prefix/share/wirectl-download"
if test -e "$install_prefix/libexec/wirectl-download"; then
  echo "An engine bundle is already installed. Stop its daemon before replacing it." >&2
  echo "Choose a new prefix for a side-by-side upgrade." >&2
  exit 1
fi
cp -R "$install_source/libexec/wirectl-download" "$install_prefix/libexec/"
install -m 755 "$install_source/bin/wirectl" "$install_prefix/bin/wirectl"
install -m 755 "$install_source/bin/wirectl-download" "$install_prefix/bin/wirectl-download"
cp -R "$install_source/deploy" "$install_source/licenses" "$install_source/README.md" "$install_prefix/share/wirectl-download/"
printf 'Installed. Add %s/bin to PATH, then run:\n  wirectl download init\n  wirectl download daemon start\n' "$install_prefix"
