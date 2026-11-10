#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/check-winget-permissions.sh"
root=$(mktemp -d /tmp/cq-winget-permissions-test.XXXXXX)
trap 'rm -rf "$root"' EXIT
mkdir "$root/bin"
cat >"$root/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  'api user') cat "$CQ_PERMISSION_TEST_ROOT/actor" ;;
  'api repos/microsoft/winget-pkgs')
    if [[ "$(cat "$CQ_PERMISSION_TEST_ROOT/upstream-status")" == 200 ]]; then
      cat "$CQ_PERMISSION_TEST_ROOT/repository.json"
    else
      jq -n --arg status "$(cat "$CQ_PERMISSION_TEST_ROOT/upstream-status")" '{status:$status}'
      exit 1
    fi ;;
  'api repos/jacobcxdev/winget-pkgs')
    if [[ "$(cat "$CQ_PERMISSION_TEST_ROOT/fork-status")" == 200 ]]; then
      cat "$CQ_PERMISSION_TEST_ROOT/fork.json"
    else
      jq -n --arg status "$(cat "$CQ_PERMISSION_TEST_ROOT/fork-status")" '{status:$status}'
      exit 1
    fi ;;
  *) echo 'unexpected non-read permission call' >&2; exit 2 ;;
esac
SH
chmod +x "$root/bin/gh"
export CQ_PERMISSION_TEST_ROOT="$root" PATH="$root/bin:$PATH" GH_TOKEN=fixture
printf 'jacobcxdev\n' >"$root/actor"
printf '{"full_name":"microsoft/winget-pkgs","permissions":{"push":false}}\n' >"$root/repository.json"
printf '200\n' >"$root/upstream-status"
printf '200\n' >"$root/fork-status"
printf '{"full_name":"jacobcxdev/winget-pkgs","fork":true,"owner":{"login":"jacobcxdev"},"parent":{"full_name":"microsoft/winget-pkgs"},"permissions":{"push":true}}\n' >"$root/fork.json"
# An ordinary contributor does not need upstream push or assignee permissions.
if ! bash "$script" >"$root/output" 2>"$root/error"; then
  cat "$root/output" "$root/error" >&2
  echo 'ordinary contributor permission check was rejected' >&2
  exit 1
fi
jq -e '.actor == "jacobcxdev" and .upstream_readable == true and .upstream_push == false
  and .fork_exists == true and .fork_writable == true and .eligible == true' "$root/output" >/dev/null
printf '{"full_name":"microsoft/winget-pkgs","permissions":{"push":true}}\n' >"$root/repository.json"
bash "$script" >"$root/output"
jq -e '.upstream_push == true and .eligible == true' "$root/output" >/dev/null

# Only an exact missing-fork response permits first-time fork creation.
printf '404\n' >"$root/fork-status"
bash "$script" >"$root/output"
jq -e '.fork_exists == false and .fork_writable == false and .eligible == true' "$root/output" >/dev/null
for status in 403 503; do
  printf '%s\n' "$status" >"$root/fork-status"
  if bash "$script" >"$root/output" 2>"$root/error"; then exit 1; fi
done
printf '200\n' >"$root/fork-status"
for status in 404 403 503; do
  printf '%s\n' "$status" >"$root/upstream-status"
  if bash "$script" >"$root/output" 2>"$root/error"; then exit 1; fi
done
printf '200\n' >"$root/upstream-status"

# Credentials, upstream identity and the writable fork remain mandatory.
printf 'other\n' >"$root/actor"
if bash "$script" >"$root/output" 2>"$root/error"; then exit 1; fi
jq -e '.actor == "other" and .eligible == false' "$root/output" >/dev/null
printf 'jacobcxdev\n' >"$root/actor"
printf '{"full_name":"other/winget-pkgs","permissions":{"push":true}}\n' >"$root/repository.json"
if bash "$script" >"$root/output" 2>"$root/error"; then exit 1; fi
jq -e '.upstream_readable == false and .eligible == false' "$root/output" >/dev/null
printf '{"full_name":"microsoft/winget-pkgs","permissions":{"push":false}}\n' >"$root/repository.json"
cp "$root/fork.json" "$root/valid-fork.json"
for change in '.full_name="other/winget-pkgs"' '.owner.login="other"' '.parent.full_name="other/winget-pkgs"' '.fork=false' '.permissions.push=false'; do
  jq "$change" "$root/valid-fork.json" >"$root/fork.json"
  if bash "$script" >"$root/output" 2>"$root/error"; then exit 1; fi
  jq -e '.fork_exists == true and .fork_writable == false and .eligible == false' "$root/output" >/dev/null
done
echo 'WinGet permission regression checks passed.'
