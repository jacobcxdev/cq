#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: validate-linux-install.sh VERSION [PREVIOUS_VERSION]" >&2
  exit 2
fi

version=${1#v}
previous_version=${2:-}
previous_version=${previous_version#v}
case "$version" in
  ''|*[!0-9.]*|.*|*..*|*.) echo "invalid version" >&2; exit 2 ;;
esac

temporary_root=$(mktemp -d /tmp/cqni.XXXXXX)
temporary_root=$(cd "$temporary_root" && pwd -P)
export HOME="$temporary_root/home"
export XDG_CONFIG_HOME="$temporary_root/config"
export XDG_CACHE_HOME="$temporary_root/cache"
export XDG_RUNTIME_DIR="$temporary_root/runtime"
export GOBIN="$temporary_root/bin"
export PATH="$GOBIN:$PATH"
export CODEX_HOME="$HOME/.codex"
install_root="$GOBIN"
installed_cq="$install_root/cq"
probe_executable="$temporary_root/native-transport-probe"
address_file="$temporary_root/upstream-address.txt"
manager_log="$temporary_root/systemd-user.log"
manager_pid=''
upstream_pid=''

cleanup() {
  set +e
  if [[ -x "$installed_cq" ]]; then
    status_json=$($installed_cq service status --json 2>/dev/null)
    proxy_pid=$(jq -r '.proxy.pid // empty' <<<"$status_json" 2>/dev/null)
    if [[ "$proxy_pid" =~ ^[1-9][0-9]*$ && -e "/proc/$proxy_pid/exe" ]]; then
      live_executable=$(readlink "/proc/$proxy_pid/exe" 2>/dev/null)
      if [[ "$live_executable" == "$installed_cq" ]]; then
        kill "$proxy_pid" 2>/dev/null
      fi
    fi
  fi
  systemctl --user stop cq-proxy.service cq-refresh.service cq-refresh.timer 2>/dev/null
  systemctl --user disable cq-proxy.service cq-refresh.timer 2>/dev/null
  for unit in cq-proxy.service cq-refresh.service cq-refresh.timer; do
    unit_path="$XDG_CONFIG_HOME/systemd/user/$unit"
    if [[ -f "$unit_path" ]]; then
      find "$unit_path" -depth -delete
    fi
  done
  systemctl --user daemon-reload 2>/dev/null
  if [[ "$upstream_pid" =~ ^[1-9][0-9]*$ ]]; then
    kill "$upstream_pid" 2>/dev/null
    wait "$upstream_pid" 2>/dev/null
  fi
  systemctl --user exit 2>/dev/null
  if [[ "$manager_pid" =~ ^[1-9][0-9]*$ ]]; then
    kill "$manager_pid" 2>/dev/null
    wait "$manager_pid" 2>/dev/null
  fi
  if [[ -d "$temporary_root" ]]; then
    chmod -R u+rwX "$temporary_root" 2>/dev/null
    find "$temporary_root" -depth -delete
  fi
}
trap cleanup EXIT INT TERM

for command in go jq systemctl readlink; do
  if ! command -v "$command" >/dev/null; then
    echo "missing required command: $command" >&2
    exit 1
  fi
done
systemd_executable=/usr/lib/systemd/systemd
if [[ ! -x "$systemd_executable" ]]; then
  echo "systemd user manager unavailable: $systemd_executable" >&2
  exit 1
fi
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_RUNTIME_DIR" "$GOBIN" "$CODEX_HOME"
chmod 700 "$HOME" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_RUNTIME_DIR" "$GOBIN" "$CODEX_HOME"

"$systemd_executable" --user --unit=basic.target >"$manager_log" 2>&1 &
manager_pid=$!
for _ in $(seq 1 100); do
  if systemctl --user show-environment >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$manager_pid" 2>/dev/null; then
    cat "$manager_log" >&2
    exit 1
  fi
  sleep 0.1
done
systemctl --user show-environment >/dev/null

repository_root=$(cd "$PWD" && pwd -P)
probe_source="$repository_root/.github/scripts/native-transport-probe.go"
go build -o "$probe_executable" "$probe_source"
"$probe_executable" serve --address-file "$address_file" &
upstream_pid=$!
for _ in $(seq 1 100); do
  [[ -f "$address_file" ]] && break
  kill -0 "$upstream_pid"
  sleep 0.1
done
[[ -f "$address_file" ]]
upstream=$(tr -d '\r\n' <"$address_file")
proxy_config="$XDG_CONFIG_HOME/cq/proxy.json"
proxy_state="$XDG_CONFIG_HOME/cq/state/proxy-resilience"
codex_auth="$CODEX_HOME/auth.json"
"$probe_executable" fixtures \
  --config "$proxy_config" \
  --auth "$codex_auth" \
  --state-root "$proxy_state" \
  --upstream "$upstream" \
  --port 19280

run_installer() {
  local requested_version=$1
  go run "github.com/jacobcxdev/cq/cmd/cq-install@v${requested_version}" --silent
}

if [[ -n "$previous_version" ]]; then
  run_installer "$previous_version"
else
  run_installer "$version"
fi
run_installer "$version"

[[ "$($installed_cq --version)" == "v$version" ]]
for unit in cq-proxy.service cq-refresh.service cq-refresh.timer; do
  [[ -f "$XDG_CONFIG_HOME/systemd/user/$unit" ]]
done

proxy_exec=$(systemctl --user show cq-proxy.service -p ExecStart --value)
refresh_exec=$(systemctl --user show cq-refresh.service -p ExecStart --value)
[[ "$proxy_exec" == *"$installed_cq proxy start"* ]]
[[ "$refresh_exec" == *"$installed_cq refresh"* ]]
[[ "$(systemctl --user show cq-refresh.timer -p UnitFileState --value)" == "enabled" ]]
systemctl --user start cq-refresh.service
[[ "$(systemctl --user show cq-refresh.service -p Result --value)" == "success" ]]

for _ in $(seq 1 60); do
  status_json=$($installed_cq service status --json)
  if jq -e \
    --arg executable "$installed_cq" \
    '.proxy.registered and .proxy.running and .proxy.healthy and
     .refresh.registered and .refresh.healthy and
     .proxy.configured_executable == $executable and
     .proxy.live_executable == $executable and
     .proxy.listener == "127.0.0.1:19280" and
     (.proxy.pid > 0)' <<<"$status_json" >/dev/null; then
    break
  fi
  sleep 1
done
jq -e \
  --arg executable "$installed_cq" \
  '.proxy.healthy and .refresh.healthy and
   .proxy.configured_executable == $executable and
   .proxy.live_executable == $executable and
   .proxy.listener == "127.0.0.1:19280"' <<<"$status_json" >/dev/null
proxy_pid=$(jq -r '.proxy.pid' <<<"$status_json")
[[ "$(readlink "/proc/$proxy_pid/exe")" == "$installed_cq" ]]
grep -F "cq-proxy.service" "/proc/$proxy_pid/cgroup" >/dev/null
"$probe_executable" probe --address http://127.0.0.1:19280 --token cq-native-local

go run "github.com/jacobcxdev/cq/cmd/cq-install@v${version}" uninstall --silent
for unit in cq-proxy.service cq-refresh.service cq-refresh.timer; do
  [[ ! -e "$XDG_CONFIG_HOME/systemd/user/$unit" ]]
  [[ "$(systemctl --user show "$unit" -p LoadState --value 2>/dev/null)" == "not-found" ]]
done
[[ ! -e "$installed_cq" ]]
