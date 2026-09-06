#!/bin/sh
# Download from the same HTTP or HTTPS site. No sudo and no Agent dependency.
set -eu
site=${1:?Usage: sh dune-install.sh SITE ONE_TIME_TOKEN [CA_CERT]}
site=${site%/}
token=${2:?one-time token required}
cert=${3:-}
case "$site" in http://*|https://*) ;; *) echo 'HTTP or HTTPS site required' >&2; exit 1;; esac
case "$(uname -s)" in Linux) platform=linux;; Darwin) platform=darwin;; *) echo 'Linux/macOS required' >&2; exit 1;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; *) echo 'amd64/arm64 required' >&2; exit 1;; esac
umask 077
root=${DUNE_INSTALL_DIR:-"$HOME/.local/share/dune"}
config=${DUNE_CONFIG:-"$HOME/.config/dune/config.yaml"}
name=${DUNE_SERVICE_NAME:-dune}
mkdir -p "$root" "$(dirname "$config")"
tmp=$(mktemp -d "$root/.install-XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
if [ -n "$cert" ]; then
 curl --fail --silent --show-error --proto '=http,https' --cacert "$cert" "$site/downloads/dune-$platform-$arch.tar.gz" -o "$tmp/package.tar.gz"
else
 curl --fail --silent --show-error --proto '=http,https' "$site/downloads/dune-$platform-$arch.tar.gz" -o "$tmp/package.tar.gz"
fi
tar -xzf "$tmp/package.tar.gz" -C "$tmp"
chmod 700 "$tmp/dune" "$tmp/tmux"
if [ ! -e "$config" ]; then
 if [ -n "$cert" ]; then
  cp "$cert" "$root/trust.crt"
  "$tmp/dune" --config "$config" enroll --site "$site" --token "$token" --certificate "$root/trust.crt"
 else
  "$tmp/dune" --config "$config" enroll --site "$site" --token "$token"
 fi
else
 echo "Keeping existing binding: $config (upgrading connector only)"
fi
# Rename executable files so running processes keep their open image.
mv "$tmp/dune" "$root/dune"
mv "$tmp/tmux" "$root/tmux"
mkdir -p "$root/licenses"
cp -R "$tmp/licenses/." "$root/licenses/"
"$root/dune" --config "$config" service install --name "$name"
printf '\nDune installed: %s\nStatus: %s --config %s service status --name %s\n' "$root/dune" "$root/dune" "$config" "$name"
printf 'PTY sessions are managed by the bundled private tmux server.\n'
