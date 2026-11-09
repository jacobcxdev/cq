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
  'api repos/microsoft/winget-pkgs') cat "$CQ_PERMISSION_TEST_ROOT/repository.json" ;;
  'api repos/microsoft/winget-pkgs/assignees/jacobcxdev')
    [[ "$3" == --include && $# == 3 ]]
    printf 'HTTP/2.0 %s response\r\n\r\n' "$(cat "$CQ_PERMISSION_TEST_ROOT/status")"
    [[ "$(cat "$CQ_PERMISSION_TEST_ROOT/status")" == 204 ]] ;;
  *) echo 'unexpected non-read permission call' >&2; exit 2 ;;
esac
SH
chmod +x "$root/bin/gh"
export CQ_PERMISSION_TEST_ROOT="$root" PATH="$root/bin:$PATH" GH_TOKEN=fixture
printf 'jacobcxdev\n' >"$root/actor"
printf '{"permissions":{"push":true}}\n' >"$root/repository.json"
printf '204\n' >"$root/status"
bash "$script" >"$root/output"
jq -e '.actor == "jacobcxdev" and .push == true and .assignee_status == 204 and .eligible == true' "$root/output" >/dev/null
for status in 404 403 503; do
  printf '%s\n' "$status" >"$root/status"
  if bash "$script" >"$root/output" 2>"$root/error"; then exit 1; fi
  jq -e --argjson status "$status" '.eligible == false and .assignee_status == $status' "$root/output" >/dev/null
done
printf '204\n' >"$root/status"
printf '{"permissions":{"push":false}}\n' >"$root/repository.json"
if bash "$script" >"$root/output" 2>"$root/error"; then exit 1; fi
jq -e '.push == false and .eligible == false' "$root/output" >/dev/null
printf '{"permissions":{"push":true}}\n' >"$root/repository.json"
printf 'other\n' >"$root/actor"
if bash "$script" >"$root/output" 2>"$root/error"; then exit 1; fi
jq -e '.actor == "other" and .eligible == false' "$root/output" >/dev/null
echo 'WinGet permission regression checks passed.'
