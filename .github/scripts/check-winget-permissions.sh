#!/usr/bin/env bash
set -euo pipefail
umask 077

# Run with the actual publication token. Checking another local credential does
# not establish the workflow actor's access to the owned publication fork.
actor=$(gh api user --jq .login)
repository=$(gh api repos/microsoft/winget-pkgs)
temporary=$(mktemp -d /tmp/cq-winget-permissions.XXXXXX)
trap 'rm -rf "$temporary"' EXIT
fork_exists=true
if ! gh api repos/jacobcxdev/winget-pkgs >"$temporary/fork.json" 2>/dev/null; then
  jq -e '.status == "404"' "$temporary/fork.json" >/dev/null || {
    echo 'fork state could not be verified' >&2; exit 1;
  }
  fork_exists=false
fi
jq -n --arg actor "$actor" --argjson repository "$repository" --argjson fork_exists "$fork_exists" \
  --slurpfile fork "$temporary/fork.json" '
  {actor:$actor,upstream_readable:($repository.full_name == "microsoft/winget-pkgs"),
    upstream_push:($repository.permissions.push == true),fork_exists:$fork_exists,
    fork_writable:($fork_exists and $fork[0].full_name == "jacobcxdev/winget-pkgs"
      and $fork[0].fork == true and $fork[0].owner.login == "jacobcxdev"
      and $fork[0].parent.full_name == "microsoft/winget-pkgs" and $fork[0].permissions.push == true)}
  | .eligible = (.actor == "jacobcxdev" and .upstream_readable and (.fork_exists == false or .fork_writable))
' >"$temporary/result.json"
cat "$temporary/result.json"
jq -e '.eligible' "$temporary/result.json" >/dev/null || {
  echo 'refusing WinGet PR: actual actor, upstream identity and existing fork write access must be verified' >&2
  exit 1
}
