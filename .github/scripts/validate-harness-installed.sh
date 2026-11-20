#!/usr/bin/env bash
set -euo pipefail

fail() {
  echo "harness qualification: $*" >&2
  exit 1
}

[[ $(uname -s) == Darwin ]] || fail 'macOS is required for the isolated native sandbox'
[[ $# == 1 && "$1" == /* && -f "$1" && -x "$1" ]] || fail 'provide one absolute native Codex executable'
client=$1
[[ $(file -Lb "$client") == *Mach-O* ]] || fail 'Codex must be a native executable, not a shell wrapper'
version=$("$client" --version)
[[ "$version" == 'codex-cli '* ]] || fail 'cannot identify native Codex version'
before=$(shasum -a 256 "$client" | awk '{print $1}')
source=$(git rev-parse HEAD)
echo "Harness qualification source: $source"
if [[ -n $(git status --porcelain=v1 --untracked-files=all) ]]; then
  echo 'Harness qualification worktree: dirty (local result; CI requires its checked-out source)'
fi
echo "Harness qualification client: $version"
echo "Harness qualification executable: $client"
echo "Harness qualification SHA-256: $before"
go version

root=$(mktemp -d "${TMPDIR:-/tmp}/cq-harness-qualification.XXXXXX")
trap 'rm -rf "$root"' EXIT
go test -race -list '^TestHarnessInstalled' ./internal/dots >"$root/tests"
sed -n '/^TestHarnessInstalled/{ /MCPHelper/d; p; }' "$root/tests" >"$root/required"
[[ -s "$root/required" ]] || fail 'no installed harness tests were discovered'
for required in \
  TestHarnessInstalledGatewayReload \
  TestHarnessInstalledChatCapabilityIsolation \
  TestHarnessInstalledCuratedWebSearch \
  TestHarnessInstalledGoalContinuationHold \
  TestHarnessInstalledGoalUserMutations \
  TestHarnessInstalledInactiveMCPOverrides \
  TestHarnessInstalledMultipleSubscriberRuntime; do
  grep -Fx "$required" "$root/required" >/dev/null || fail "$required is missing from the conformance suite"
done
pattern=$(paste -sd '|' "$root/required")
echo 'Harness qualification cases:'
cat "$root/required"
if ! CQ_DOTS_HARNESS_INSTALLED_ACCEPTANCE=1 CQ_DOTS_HARNESS_INSTALLED_CODEX="$client" \
  go test -race -count=1 -timeout=10m -json ./internal/dots -run "^($pattern)$" >"$root/results" 2>&1; then
  cat "$root/results" >&2
  fail 'isolated native conformance failed'
fi
# Require real execution. A renamed, skipped or empty fixture cannot qualify a
# new native client merely because go test returned success.
jq -s -e 'all(.[]; .Action != "skip")' "$root/results" >/dev/null || {
  cat "$root/results" >&2
  fail 'a required native case was skipped or its result was invalid'
}
while IFS= read -r test_name; do
  jq -s -e --arg name "$test_name" 'any(.[]; .Action == "pass" and .Test == $name)' "$root/results" >/dev/null || {
    cat "$root/results" >&2
    fail "$test_name did not pass"
  }
done <"$root/required"
jq -r 'select(.Action == "pass" and .Test != null) | "PASS " + .Test' "$root/results"
[[ $(shasum -a 256 "$client" | awk '{print $1}') == "$before" ]] || fail 'native executable changed during qualification'
echo "Harness qualification passed: $version, source $source, SHA-256 $before"
