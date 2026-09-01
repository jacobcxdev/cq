#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 6 ]]; then
  echo "usage: validate-homebrew-install.sh PREVIOUS_CASK PREVIOUS_ARCHIVE PREVIOUS_VERSION CURRENT_CASK CURRENT_ARCHIVE CURRENT_VERSION" >&2
  exit 64
fi
if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "Homebrew lifecycle validation requires macOS" >&2
  exit 69
fi

previous_cask=$1
previous_archive=$2
previous_version=${3#v}
current_cask=$4
current_archive=$5
current_version=${6#v}
for path in "$previous_cask" "$previous_archive" "$current_cask" "$current_archive"; do
  [[ -f "$path" ]] || { echo "validation input is not a regular file: $path" >&2; exit 66; }
done
[[ "$previous_version" != "$current_version" ]] || { echo "versions must differ" >&2; exit 64; }

validation_tap=cq-validation/lifecycle
installed_cq="$(brew --prefix)/bin/cq"
caskroom="$(brew --caskroom)/cq"
proxy_label=dev.jacobcx.cq.proxy
refresh_label=dev.jacobcx.cq.refresh
proxy_plist="$HOME/Library/LaunchAgents/$proxy_label.plist"
refresh_plist="$HOME/Library/LaunchAgents/$refresh_label.plist"
config_root="$HOME/.config/cq"
codex_root="$HOME/.codex"
logs_root="$HOME/Library/Logs/cq"
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/cq-homebrew-install.XXXXXX")
probe_executable="$temporary_root/native-transport-probe"
address_file="$temporary_root/upstream-address.txt"
upstream_pid=''
owns_state=0

cleanup() {
  result=$?
  trap - EXIT
  set +e
  if [[ -x "$installed_cq" ]]; then
    "$installed_cq" service uninstall --owner=homebrew --service-executable="$installed_cq" >/dev/null 2>&1
  fi
  HOMEBREW_NO_AUTO_UPDATE=1 brew uninstall --cask --force cq >/dev/null 2>&1
  for label in "$proxy_label" "$refresh_label"; do
    launchctl bootout "gui/$UID/$label" >/dev/null 2>&1
  done
  HOMEBREW_NO_AUTO_UPDATE=1 brew untap "$validation_tap" >/dev/null 2>&1
  if [[ "$upstream_pid" =~ ^[1-9][0-9]*$ ]]; then
    kill "$upstream_pid" >/dev/null 2>&1
    wait "$upstream_pid" >/dev/null 2>&1
  fi
  if [[ "$owns_state" -eq 1 ]]; then
    for path in "$proxy_plist" "$refresh_plist" "$config_root" "$codex_root" "$logs_root"; do
      if [[ -e "$path" || -L "$path" ]]; then
        chmod -R u+rwX "$path" 2>/dev/null
        find "$path" -depth -delete 2>/dev/null
      fi
    done
  fi
  if [[ -d "$temporary_root" ]]; then
    chmod -R u+rwX "$temporary_root" 2>/dev/null
    find "$temporary_root" -depth -delete 2>/dev/null
  fi
  exit "$result"
}
trap cleanup EXIT

if brew list --cask cq >/dev/null 2>&1 || [[ -e "$installed_cq" || -L "$installed_cq" || -e "$caskroom" || -L "$caskroom" ]]; then
  echo "refusing to replace existing CQ Cask installation" >&2
  exit 73
fi
if brew tap | grep -Fxq "$validation_tap"; then
  echo "refusing to replace existing $validation_tap tap" >&2
  exit 73
fi
for label in "$proxy_label" "$refresh_label"; do
  if launchctl print "gui/$UID/$label" >/dev/null 2>&1; then
    echo "refusing to replace existing $label service" >&2
    exit 73
  fi
done
for path in "$proxy_plist" "$refresh_plist" "$config_root" "$codex_root" "$logs_root"; do
  if [[ -e "$path" || -L "$path" ]]; then
    echo "refusing to replace existing validation path $path" >&2
    exit 73
  fi
done
if lsof -nP -iTCP:19280 -sTCP:LISTEN >/dev/null 2>&1; then
  echo "refusing to replace existing listener on port 19280" >&2
  exit 73
fi
owns_state=1

go build -o "$probe_executable" ./.github/scripts/native-transport-probe.go
"$probe_executable" serve --address-file "$address_file" &
upstream_pid=$!
for _ in $(seq 1 100); do
  [[ -f "$address_file" ]] && break
  kill -0 "$upstream_pid"
  sleep 0.1
done
[[ -f "$address_file" ]]
upstream=$(tr -d '\r\n' <"$address_file")
"$probe_executable" fixtures \
  --config "$config_root/proxy.json" \
  --auth "$codex_root/auth.json" \
  --state-root "$config_root/state/proxy-resilience" \
  --upstream "$upstream" \
  --port 19280
chmod -R go-rwx "$config_root" "$codex_root"

rewrite_cask() {
  local source=$1
  local archive=$2
  local destination=$3
  ruby - "$source" "$archive" "$destination" <<'RUBY'
source, archive, destination = ARGV
text = File.read(source)
text.gsub!(/^(\s*)url ".*"$/) { "#{Regexp.last_match(1)}url \"file://#{archive}\"" }
preflight = <<~'BLOCK'
  preflight do
    system_command "/usr/bin/xattr",
                   args: ["-w", "com.apple.quarantine", "0081;00000000;CQValidation;", "#{staged_path}/cq"]
  end

BLOCK
text.sub!(/^  postflight do$/, preflight + "  postflight do") or abort "missing postflight hook"
File.write(destination, text)
RUBY
}

HOMEBREW_NO_AUTO_UPDATE=1 brew tap-new --no-git "$validation_tap" >/dev/null
tap_root=$(brew --repository "$validation_tap")
mkdir -p "$tap_root/Casks"
validation_cask="$tap_root/Casks/cq.rb"
rewrite_cask "$previous_cask" "$previous_archive" "$validation_cask"

assert_installed() {
  local expected_version=$1
  [[ "$($installed_cq --version)" == "v$expected_version" ]]
  if xattr -p com.apple.quarantine "$installed_cq" >/dev/null 2>&1; then
    echo "Homebrew Cask left cq quarantined" >&2
    return 1
  fi
  local status_json=''
  for _ in $(seq 1 60); do
    status_json=$($installed_cq service status --json 2>/dev/null) || true
    if jq -e \
      --arg executable "$installed_cq" \
      '.owner == "homebrew" and .proxy.running and .proxy.healthy and .refresh.healthy and
       .proxy.configured_executable == $executable and .proxy.live_executable == $executable and
       .proxy.listener == "127.0.0.1:19280" and (.proxy.pid > 0)' <<<"$status_json" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "CQ $expected_version Homebrew services did not become healthy" >&2
  return 1
}

HOMEBREW_NO_AUTO_UPDATE=1 brew install --cask "$validation_tap/cq"
assert_installed "$previous_version"

rewrite_cask "$current_cask" "$current_archive" "$validation_cask"
HOMEBREW_NO_AUTO_UPDATE=1 brew upgrade --cask "$validation_tap/cq"
assert_installed "$current_version"
"$probe_executable" probe --address http://127.0.0.1:19280 --token cq-native-local

find "$installed_cq" -depth -delete
HOMEBREW_NO_AUTO_UPDATE=1 brew uninstall --cask --force cq
for label in "$proxy_label" "$refresh_label"; do
  if launchctl print "gui/$UID/$label" >/dev/null 2>&1; then
    echo "$label remains after Cask fallback uninstall" >&2
    exit 1
  fi
done
for path in "$installed_cq" "$proxy_plist" "$refresh_plist"; do
  [[ ! -e "$path" && ! -L "$path" ]]
done
[[ -f "$config_root/proxy.json" && -f "$codex_root/auth.json" ]]

echo "Homebrew Cask install, upgrade, transport, and fallback uninstall validation passed"
