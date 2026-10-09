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
go_executable="$(go env GOROOT)/bin/go"
address_file="$temporary_root/upstream-address.txt"
upstream_pid=''
owns_state=0

cleanup() {
  result=$?
  trap - EXIT
  set +e
  if [[ "$owns_state" -eq 1 ]]; then
    if [[ "$result" -ne 0 && -f "$config_root/state/runtime-upgrade.json" ]]; then
      jq '{phase, error_code, generation, previous_version: .previous.version, candidate_version: .candidate.version}' \
        "$config_root/state/runtime-upgrade.json" >&2
    fi
    if [[ -x "$installed_cq" ]]; then
      "$installed_cq" service uninstall --owner=homebrew --service-executable="$installed_cq" >/dev/null 2>&1
    fi
    HOMEBREW_NO_AUTO_UPDATE=1 brew uninstall --cask --force cq >/dev/null 2>&1
    for label in "$proxy_label" "$refresh_label"; do
      launchctl bootout "gui/$UID/$label" >/dev/null 2>&1
    done
    HOMEBREW_NO_AUTO_UPDATE=1 brew untap "$validation_tap" >/dev/null 2>&1
  fi
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

if ! launchctl print "gui/$UID" >/dev/null 2>&1; then
  echo "Homebrew lifecycle validation requires an available gui/$UID launchd domain" >&2
  exit 69
fi
echo "Homebrew lifecycle launchd domain gui/$UID is available"

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
text.sub!(/^  installer script:/) { preflight + "  installer script:" } or abort "missing installer artifact"
File.write(destination, text)
RUBY
}

HOMEBREW_NO_AUTO_UPDATE=1 brew tap-new --no-git "$validation_tap" >/dev/null
tap_root=$(brew --repository "$validation_tap")
mkdir -p "$tap_root/Casks"
validation_cask="$tap_root/Casks/cq.rb"
# Exercise the published uninstall hook; using the candidate hook for both
# versions hides upgrade handoff failures.
rewrite_cask "$previous_cask" "$previous_archive" "$validation_cask"

validate_runtime_status() {
  ruby - "$1" "$installed_cq" "$config_root/state/runtime-artifacts" "$2" "$3" "$("$go_executable" env GOHOSTARCH)" "$go_executable" <<'RUBY'
require "digest"
require "json"
require "open3"

status_json, package, runtime_root, expected_version, mode, architecture, go_executable = ARGV
status = JSON.parse(status_json)
proxy, refresh = status.fetch("proxy"), status.fetch("refresh")
abort "service ownership executable differs" unless status["owner"] == "homebrew" && status["executable"] == package
abort "service health differs" unless proxy["registered"] && proxy["running"] && proxy["healthy"] && refresh["registered"] && refresh["healthy"] && proxy["listener"] == "127.0.0.1:19280" && proxy.fetch("pid", 0) > 1
live = proxy.fetch("live_executable")
configured = proxy.fetch("configured_executable")
refresh_executable = refresh.fetch("configured_executable")
package_digest = Digest::SHA256.file(package).hexdigest
abort "selected runtime differs from package bytes" unless File.file?(live) && Digest::SHA256.file(live).hexdigest == package_digest
version, result = Open3.capture2(live, "--version")
abort "selected runtime version differs" unless result.success? && version.strip == expected_version

verify_build = lambda do |path|
  build, result = Open3.capture2(go_executable, "version", "-m", path)
  abort "runtime build identity differs" unless result.success? && build.match?(/^\tpath\tgithub\.com\/jacobcxdev\/cq\/cmd\/cq$/) && build.match?(/^\tbuild\tGOOS=darwin$/) && build.include?("\tbuild\tGOARCH=#{architecture}\n")
end
verify_build.call(live)
if File.identical?(live, package)
  abort "candidate must run retained executable" unless mode == "legacy-or-retained"
  abort "legacy job executable differs" unless configured == package && refresh_executable == package
else
  abort "active retained version differs" unless status["active_runtime_version"] == expected_version && status.fetch("pending_runtime_version", "").empty?
  abort "refresh does not select active runtime" unless refresh_executable == live
  [runtime_root, File.dirname(runtime_root)].each do |directory|
    info = File.lstat(directory)
    abort "unsafe runtime directory" unless info.directory? && info.uid == Process.uid && (info.mode & 0o7777) == 0o700 && File.realpath(directory) == directory
  end
  [live, configured, refresh_executable].uniq.each do |path|
    digest = File.basename(File.dirname(path))
    abort "runtime path differs from retained store" unless digest.match?(/\A[0-9a-f]{64}\z/) && path == File.join(runtime_root, digest, "cq") && File.realpath(path) == path
    directory, info = File.lstat(File.dirname(path)), File.lstat(path)
    abort "unsafe retained runtime" unless directory.directory? && directory.uid == Process.uid && (directory.mode & 0o7777) == 0o700 && info.file? && info.uid == Process.uid && info.nlink == 1 && (info.mode & 0o7777) == 0o500
    abort "retained digest differs" unless Digest::SHA256.file(path).hexdigest == digest
    verify_build.call(path)
    check, result = Open3.capture2(path, "proxy", "runtime-check", "--json")
    check = JSON.parse(check)
    abort "retained runtime protocol differs" unless result.success? && check["schema_version"] == 1 && check["protocol_version"] == 1 && check["goos"] == "darwin" && check["goarch"] == architecture && !check.fetch("version", "").empty?
    abort "selected runtime check version differs" if path == live && check["version"] != expected_version
  end
end
RUBY
}

assert_installed() {
  local expected_version=$1
  local runtime_mode=${2:-legacy-or-retained}
  [[ "$($installed_cq --version)" == "$expected_version" ]]
  if xattr -p com.apple.quarantine "$installed_cq" >/dev/null 2>&1; then
    echo "Homebrew Cask left cq quarantined" >&2
    return 1
  fi
  local status_json=''
  for _ in $(seq 1 60); do
    status_json=$($installed_cq service status --json 2>/dev/null) || true
    if validate_runtime_status "$status_json" "$expected_version" "$runtime_mode" >"$temporary_root/runtime-status.log" 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "CQ $expected_version Homebrew services did not become healthy" >&2
  jq . <<<"$status_json" >&2 || printf '%s\n' "$status_json" >&2
  cat "$temporary_root/runtime-status.log" >&2
  for log in "$logs_root/proxy.log" "$logs_root/refresh.log"; do
    if [[ -f "$log" ]]; then
      echo "--- $log" >&2
      tail -n 80 "$log" >&2
    fi
  done
  return 1
}

if ! HOMEBREW_NO_AUTO_UPDATE=1 brew install --cask "$validation_tap/cq"; then
  launchctl print-disabled "gui/$UID" >&2 || true
  for path in "$proxy_plist" "$refresh_plist"; do
    if [[ -f "$path" ]]; then
      /usr/bin/stat -f '%Sp %Su:%Sg %N' "$path" >&2 || true
      /usr/bin/plutil -lint "$path" >&2 || true
    else
      echo "LaunchAgent absent after installer rollback: $path" >&2
    fi
  done
  /usr/bin/log show --last 2m --style compact \
    --predicate 'process == "launchd" AND eventMessage CONTAINS "dev.jacobcx.cq"' >&2 || true
  exit 1
fi
assert_installed "$previous_version"

rewrite_cask "$current_cask" "$current_archive" "$validation_cask"
HOMEBREW_NO_AUTO_UPDATE=1 brew upgrade --cask "$validation_tap/cq"
assert_installed "$current_version" retained
"$probe_executable" probe --address http://127.0.0.1:19280 --token cq-native-local

proxy_pid=$($installed_cq service status --json | jq -er '.proxy.pid')
HOMEBREW_NO_AUTO_UPDATE=1 brew reinstall --cask "$validation_tap/cq"
assert_installed "$current_version" retained
[[ "$($installed_cq service status --json | jq -er '.proxy.pid')" == "$proxy_pid" ]]
"$probe_executable" probe --address http://127.0.0.1:19280 --token cq-native-local

preserved_config=$(shasum -a 256 "$config_root/proxy.json")
preserved_auth=$(shasum -a 256 "$codex_root/auth.json")
HOMEBREW_NO_AUTO_UPDATE=1 brew uninstall --cask --force cq
for label in "$proxy_label" "$refresh_label"; do
  if launchctl print "gui/$UID/$label" >/dev/null 2>&1; then
    echo "$label remains after Cask uninstall" >&2
    exit 1
  fi
done
for path in "$installed_cq" "$proxy_plist" "$refresh_plist"; do
  [[ ! -e "$path" && ! -L "$path" ]]
done
[[ -f "$config_root/proxy.json" && -f "$codex_root/auth.json" ]]
[[ "$(shasum -a 256 "$config_root/proxy.json")" == "$preserved_config" && "$(shasum -a 256 "$codex_root/auth.json")" == "$preserved_auth" ]]
for path in "$config_root/state/runtime-artifacts" "$config_root/state/runtime-snapshots" "$config_root/state/runtime-upgrade.json"; do
  [[ ! -e "$path" && ! -L "$path" ]]
done
if lsof -nP -iTCP:19280 -sTCP:LISTEN >/dev/null 2>&1; then
  echo "CQ listener remains after Cask uninstall" >&2
  exit 1
fi

echo "Homebrew Cask install, upgrade, reinstall, transport, and uninstall validation passed"
