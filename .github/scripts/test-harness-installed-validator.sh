#!/usr/bin/env bash
set -euo pipefail

validator="$(cd "$(dirname "$0")" && pwd)/validate-harness-installed.sh"
root=$(mktemp -d "${TMPDIR:-/tmp}/cq-harness-validator.XXXXXX")
trap 'rm -rf "$root"' EXIT
mkdir "$root/bin"
cat >"$root/client-template" <<'SH'
#!/usr/bin/env bash
if [[ ${HARNESS_CASE:-} == invalid-version ]]; then
  echo 'unidentified client'
else
  echo 'codex-cli fixture'
fi
SH
cat >"$root/bin/uname" <<'SH'
#!/usr/bin/env bash
if [[ ${HARNESS_CASE:-} == non-macos ]]; then echo Linux; else echo Darwin; fi
SH
cat >"$root/bin/file" <<'SH'
#!/usr/bin/env bash
if [[ ${HARNESS_CASE:-} == wrapper ]]; then echo 'POSIX shell script'; else echo 'Mach-O fixture'; fi
SH
cat >"$root/bin/git" <<'SH'
#!/usr/bin/env bash
if [[ $1 == rev-parse ]]; then echo 0123456789012345678901234567890123456789; fi
SH
cat >"$root/bin/go" <<'SH'
#!/usr/bin/env bash
set -eu
if [[ $1 == version ]]; then
  echo 'go version go1.26.1 fixture'
  exit 0
fi
[[ " $* " == *' -race '* ]]
tests=(
  TestHarnessInstalledGatewayReload
  TestHarnessInstalledChatCapabilityIsolation
  TestHarnessInstalledCuratedWebSearch
  TestHarnessInstalledGoalContinuationHold
  TestHarnessInstalledGoalUserMutations
  TestHarnessInstalledInactiveMCPOverrides
  TestHarnessInstalledMultipleSubscriberRuntime
  TestHarnessInstalledNewCase
)
if [[ " $* " == *' -list '* ]]; then
  if [[ ${HARNESS_CASE:-} == empty ]]; then
    echo TestHarnessInstalledMCPHelper
    exit 0
  fi
  if [[ ${HARNESS_CASE:-} == missing-family ]]; then
    echo TestHarnessInstalledNewCase
    exit 0
  fi
  printf '%s\n' "${tests[@]}" TestHarnessInstalledMCPHelper
  exit 0
fi
[[ " $* " == *' -count=1 '* && " $* " == *' -json '* ]]
[[ "$CQ_DOTS_HARNESS_INSTALLED_ACCEPTANCE" == 1 ]]
[[ " $* " != *MCPHelper* ]]
case ${HARNESS_CASE:-} in
  failed) echo '{"Action":"fail","Test":"TestHarnessInstalledGoalContinuationHold"}'; exit 1 ;;
  invalid-json) echo 'not a native test result'; exit 0 ;;
esac
for name in "${tests[@]}"; do
  if [[ ${HARNESS_CASE:-} == missing && $name == TestHarnessInstalledGoalContinuationHold || ${HARNESS_CASE:-} == missing-new && $name == TestHarnessInstalledNewCase ]]; then continue; fi
  action=pass
  if [[ ${HARNESS_CASE:-} == skipped && $name == TestHarnessInstalledGoalContinuationHold ]]; then action=skip; fi
  printf '{"Action":"%s","Test":"%s"}\n' "$action" "$name"
done
if [[ ${HARNESS_CASE:-} == changed ]]; then
  echo '# changed native bytes' >> "$CQ_DOTS_HARNESS_INSTALLED_CODEX"
fi
SH
chmod +x "$root/bin/"* "$root/client-template"

expect() {
  local expected=$1 test_case=$2 client=${3:-$root/client}
  cp "$root/client-template" "$root/client"
  if PATH="$root/bin:$PATH" HARNESS_CASE="$test_case" bash "$validator" "$client" >"$root/result" 2>&1; then
    actual=pass
  else
    actual=fail
  fi
  if [[ "$actual" != "$expected" ]]; then
    cat "$root/result" >&2
    echo "harness validator: expected $expected for $test_case, got $actual" >&2
    exit 1
  fi
}

expect pass complete
expect fail non-macos
expect fail relative relative-codex
expect fail absent "$root/absent"
expect fail wrapper
expect fail invalid-version
expect fail empty
expect fail missing-family
expect fail failed
expect fail invalid-json
expect fail skipped
expect fail missing
expect fail missing-new
expect fail changed
echo 'Harness validator checks passed: execution, identity, required cases, skips and native replacement.'
