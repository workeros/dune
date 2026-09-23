#!/bin/sh
# One implementation for personal and host-issued connector installation.
set -eu
umask 077
action=${1:?Usage: install.sh enroll SITE TOKEN RUNNER_ID [CA_CERT] | install SITE [CA_CERT]}
site=${2:?site required}
site=${site%/}
case "$site" in http://*|https://*) ;; *) echo 'HTTP or HTTPS site required' >&2; exit 1;; esac
root=${DUNE_INSTALL_DIR:-"$HOME/.local/share/dune"}
config_file=${DUNE_CONFIG:-"$HOME/.config/dune/config.yaml"}
name=${DUNE_SERVICE_NAME:-dune}
case "$action" in
 enroll)
  # Lstat equivalent: even corrupt configurations and dangling symlinks count.
  if [ -e "$config_file" ] || [ -L "$config_file" ]; then
   echo "Configuration already exists: $config_file. Inspect the existing binding; use the Runner upgrade API." >&2; exit 1
  fi
  token=${3:?one-time token required}
  runner_id=${4:?pending Runner ID required}
  cert=${5:-}
  ;;
 install)
  if [ ! -f "$config_file" ]; then echo 'A valid existing configuration is required. Check and revoke any unknown enrollment before issuing a new command.' >&2; exit 1; fi
  cert=${3:-}
  ;;
 *) echo 'Action must be enroll or install' >&2; exit 1;;
esac
case "$(uname -s)" in Linux) platform=linux;; Darwin) platform=darwin;; *) echo 'Linux/macOS required' >&2; exit 1;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; *) echo 'amd64/arm64 required' >&2; exit 1;; esac
mkdir -p "$root" "$(dirname "$config_file")"
work_dir=$(mktemp -d "$root/.install-XXXXXX")
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM
download() {
 if [ -n "$cert" ]; then curl --fail --silent --show-error --proto '=http,https' --cacert "$cert" "$1" -o "$2"
 else curl --fail --silent --show-error --proto '=http,https' "$1" -o "$2"; fi
}
archive="dune-$platform-$arch.tar.gz"
download "$site/api/v1/downloads/$archive" "$work_dir/$archive"
download "$site/api/v1/downloads/$archive.sha256" "$work_dir/$archive.sha256"
if command -v sha256sum >/dev/null 2>&1; then
 (cd "$work_dir" && sha256sum --check "$archive.sha256")
else
 (cd "$work_dir" && shasum -a 256 --check "$archive.sha256")
fi
tar -xzf "$work_dir/$archive" -C "$work_dir"
chmod 700 "$work_dir/dune" "$work_dir/tmux" "$work_dir/rg"
if [ "$action" = enroll ]; then
 if [ -n "$cert" ]; then
  # Trust material lives with persistent configuration, outside program releases.
  trust_file="$(dirname "$config_file")/trust-$(basename "$work_dir").crt"
  cp "$cert" "$trust_file"
  "$work_dir/dune" --config "$config_file" enroll --site "$site" --token "$token" --runner-id "$runner_id" --certificate "$trust_file"
 else
  "$work_dir/dune" --config "$config_file" enroll --site "$site" --token "$token" --runner-id "$runner_id"
 fi
 action=install
fi
"$work_dir/dune" --config "$config_file" "$action" --root "$root" --name "$name"
printf 'CLI directory for PATH: %s/current\n' "$root"
