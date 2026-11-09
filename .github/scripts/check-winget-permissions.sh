#!/usr/bin/env bash
set -euo pipefail
umask 077

# Run with the actual publication token. Checking another local credential does
# not establish the workflow actor's repository or assignment permissions.
actor=$(gh api user --jq .login)
repository=$(gh api repos/microsoft/winget-pkgs)
temporary=$(mktemp -d /tmp/cq-winget-permissions.XXXXXX)
trap 'rm -rf "$temporary"' EXIT
gh api repos/microsoft/winget-pkgs/assignees/jacobcxdev --include >"$temporary/status" 2>/dev/null || true
status=$(awk 'NR == 1 {print $2}' "$temporary/status")
[[ "$status" =~ ^[0-9]{3}$ ]] || status=0
jq -n --arg actor "$actor" --argjson repository "$repository" --argjson status "$status" '
  {actor:$actor,push:($repository.permissions.push == true),assignee_status:$status}
  | .eligible = (.actor == "jacobcxdev" and .push and .assignee_status == 204)
' >"$temporary/result.json"
cat "$temporary/result.json"
jq -e '.eligible' "$temporary/result.json" >/dev/null || {
  echo 'refusing WinGet PR: actual actor, push access and assignee eligibility must all be verified' >&2
  exit 1
}
