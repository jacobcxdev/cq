#!/usr/bin/env bash
set -euo pipefail

root=$(mktemp -d "${TMPDIR:-/tmp}/cq-homebrew-protocol.XXXXXX")
trap 'rm -rf "$root"' EXIT
export CQ_PROTOCOL_TEST_ROOT="$root"
mkdir -p "$root/bin" "$root/config/state" "$root/codex"
ruby - .github/scripts/validate-homebrew-install.sh "$root/functions.sh" <<'RUBY'
source, destination = ARGV
text = File.read(source)
functions = %w[runtime_selection runtime_receipt_digest assert_protocol_rejection upgrade_cq_package].map do |name|
  text.match(/^#{name}\(\) \{\n.*?^\}$/m)&.to_s or abort "missing #{name}"
end
File.write(destination, functions.join("\n"))
RUBY
cat >"$root/bin/cq" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
root=$CQ_PROTOCOL_TEST_ROOT
case "$*" in
  'service status --json') cat "$root/status.json" ;;
  'service uninstall --owner=homebrew --service-executable='*)
    echo uninstall >>"$root/events"
    touch "$root/maintenance"
    if [[ "$CQ_PROTOCOL_TEST_CASE" != service-remains ]]; then rm -f "$root/alive"; fi
    if [[ "$CQ_PROTOCOL_TEST_CASE" == auth-changed ]]; then echo changed >>"$root/codex/auth.json"; fi ;;
  'service upgrade '*)
    echo downgrade >>"$root/events"
    echo 'predecessor needs maintenance adoption; runtime protocol 2 -> 1' >&2
    exit 1 ;;
  *) echo "unexpected cq call: $*" >&2; exit 1 ;;
esac
SH
cat >"$root/bin/brew" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
root=$CQ_PROTOCOL_TEST_ROOT
[[ "$*" == 'upgrade --cask cq-validation/lifecycle/cq' ]]
echo upgrade >>"$root/events"
if [[ ! -f "$root/maintenance" && "$CQ_PROTOCOL_TEST_CASE" != compatible ]]; then
  case "$CQ_PROTOCOL_TEST_CASE" in
    unexpected-success) exit 0 ;;
    unrelated) echo 'download failed' >&2; exit 1 ;;
    pid-changed) sed 's/101/102/' "$root/status.json" >"$root/changed"; mv "$root/changed" "$root/status.json" ;;
    receipt-changed) echo changed >"$root/config/state/runtime-upgrade.json" ;;
    rollback-reconciled)
      echo '{"phase":"committed","previous":{"version":"0.48.3","protocol_version":1},"candidate":{"version":"0.48.3","protocol_version":1}}' >"$root/config/state/runtime-upgrade.json" ;;
    rollback-candidate)
      echo '{"phase":"committed","previous":{"version":"0.48.3","protocol_version":1},"candidate":{"version":"0.48.4","protocol_version":2}}' >"$root/config/state/runtime-upgrade.json" ;;
    rollback-unfinished)
      echo '{"phase":"waiting","previous":{"version":"0.48.3","protocol_version":1},"candidate":{"version":"0.48.3","protocol_version":1}}' >"$root/config/state/runtime-upgrade.json" ;;
  esac
  echo 'predecessor needs maintenance adoption; runtime protocol 1 -> 2' >&2
  exit 1
fi
touch "$root/alive"
SH
cat >"$root/bin/launchctl" <<'SH'
#!/usr/bin/env bash
[[ -f "$CQ_PROTOCOL_TEST_ROOT/alive" ]]
SH
cat >"$root/bin/lsof" <<'SH'
#!/usr/bin/env bash
[[ -f "$CQ_PROTOCOL_TEST_ROOT/alive" || "$CQ_PROTOCOL_TEST_CASE" == listener-remains ]]
SH
cat >"$root/bin/probe" <<'SH'
#!/usr/bin/env bash
[[ -f "$CQ_PROTOCOL_TEST_ROOT/alive" ]] || exit 1
echo probe >>"$CQ_PROTOCOL_TEST_ROOT/events"
SH
chmod +x "$root/bin/"*
cat >"$root/run.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
root=$CQ_PROTOCOL_TEST_ROOT
export PATH="$root/bin:$PATH"
source "$root/functions.sh"
installed_cq="$root/bin/cq"
config_root="$root/config"
codex_root="$root/codex"
temporary_root="$root"
probe_executable="$root/bin/probe"
validation_tap=cq-validation/lifecycle
previous_version=0.48.3
current_version=0.48.4
previous_protocol=1
current_protocol=2
proxy_label=dev.jacobcx.cq.proxy
refresh_label=dev.jacobcx.cq.refresh
assert_installed() {
  echo "healthy $1 protocol $3" >>"$root/events"
  [[ "$CQ_PROTOCOL_TEST_CASE" != unhealthy ]]
}
if [[ "$CQ_PROTOCOL_TEST_CASE" == downgrade ]]; then
  assert_protocol_rejection "$current_version" "$current_protocol" unchanged "$installed_cq" service upgrade --candidate-executable=previous
else
  if [[ "$CQ_PROTOCOL_TEST_CASE" == compatible ]]; then previous_protocol=2; fi
  upgrade_cq_package
fi
SH

reset_case() {
  rm -f "$root/events" "$root/maintenance" "$root/config/state/runtime-upgrade.json"
  touch "$root/alive"
  printf '%s\n' config >"$root/config/proxy.json"
  printf '%s\n' auth >"$root/codex/auth.json"
  printf '%s\n' '{"proxy":{"pid":101,"live_executable":"previous","configured_executable":"previous"},"refresh":{"configured_executable":"previous"}}' >"$root/status.json"
}
reset_case
CQ_PROTOCOL_TEST_CASE=boundary bash "$root/run.sh"
[[ "$(cat "$root/events")" == $'upgrade\nhealthy 0.48.3 protocol 1\nprobe\nuninstall\nupgrade' ]]
reset_case
CQ_PROTOCOL_TEST_CASE=rollback-reconciled bash "$root/run.sh"
[[ "$(cat "$root/events")" == $'upgrade\nhealthy 0.48.3 protocol 1\nprobe\nuninstall\nupgrade' ]]
reset_case
CQ_PROTOCOL_TEST_CASE=compatible bash "$root/run.sh"
[[ "$(cat "$root/events")" == upgrade ]]
reset_case
CQ_PROTOCOL_TEST_CASE=downgrade bash "$root/run.sh"
[[ "$(cat "$root/events")" == $'downgrade\nhealthy 0.48.4 protocol 2\nprobe' ]]
for failure in unexpected-success unrelated pid-changed receipt-changed rollback-candidate rollback-unfinished unhealthy service-remains listener-remains auth-changed; do
  reset_case
  if CQ_PROTOCOL_TEST_CASE=$failure bash "$root/run.sh" >"$root/error" 2>&1; then
    echo "protocol lifecycle accepted $failure" >&2
    exit 1
  fi
  if [[ "$failure" != service-remains && "$failure" != listener-remains && "$failure" != auth-changed ]]; then
    ! grep -Fq uninstall "$root/events"
  fi
  [[ "$(grep -c '^upgrade$' "$root/events")" == 1 ]]
done
echo "Homebrew protocol rejection, maintenance ordering, preservation and downgrade tests passed"
