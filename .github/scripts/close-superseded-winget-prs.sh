#!/usr/bin/env bash
set -euo pipefail

: "${GH_TOKEN:?GH_TOKEN is required}"
[[ "${RELEASE_TAG:-}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo 'a stable RELEASE_TAG is required' >&2
  exit 1
}
version=${RELEASE_TAG#v}
pending=$(mktemp "${TMPDIR:-/tmp}/cq-winget-pending.XXXXXX")
trap 'rm -f "$pending"' EXIT
gh pr list --repo microsoft/winget-pkgs --state open --author jacobcxdev \
  --limit 1000 --json number,title,headRefName,headRepositoryOwner,author |
  jq '[.[] | select(.author.login == "jacobcxdev" and .headRepositoryOwner.login == "jacobcxdev")
    | select((.headRefName // "") | test("^cq-[0-9]+\\.[0-9]+\\.[0-9]+$"))
    | . + {version:(.headRefName | ltrimstr("cq-"))}
    | select(.title == ("New version: jacobcxdev.cq version " + .version)
        or .title == ("Update: jacobcxdev.cq to " + .version))]' >"$pending"

if ! jq -e --arg version "$version" 'any(.[]; .version == $version)' "$pending" >/dev/null &&
   ! gh api "repos/microsoft/winget-pkgs/contents/manifests/j/jacobcxdev/cq/$version" >/dev/null 2>&1; then
  echo "refusing cleanup: CQ $version has no current PR or published manifest" >&2
  exit 1
fi

jq -r --arg version "$version" '
  ($version | split(".") | map(tonumber)) as $current
  | .[] | select((.version | split(".") | map(tonumber)) < $current) | .number
' "$pending" | while IFS= read -r number; do
  gh pr close "$number" --repo microsoft/winget-pkgs
done
