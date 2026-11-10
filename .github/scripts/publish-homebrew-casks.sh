#!/usr/bin/env bash
set -euo pipefail
umask 077

[[ $# == 1 && "$RELEASE_TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo 'a formatted Cask directory and stable RELEASE_TAG are required' >&2; exit 1;
}
directory=$1
version=${RELEASE_TAG#v}
escaped_version=${version//./\\.}
repository=jacobcxdev/homebrew-tap
scripts="$(cd "$(dirname "$0")" && pwd)"
temporary=$(mktemp -d /tmp/cq-homebrew-publication.XXXXXX)
trap 'rm -rf "$temporary"' EXIT
branch=$(gh api "repos/$repository" --jq .default_branch)
head=$(gh api "repos/$repository/git/ref/heads/$branch" --jq .object.sha)
[[ "$head" =~ ^[0-9a-f]{40}$ ]]
printf '[]\n' >"$temporary/additions.json"
for token in cq-network cq; do
  file=$directory/$token.rb
  [[ -f "$file" && ! -L "$file" ]] || { echo 'formatted Cask is absent or unsafe' >&2; exit 1; }
  grep -Eq "^[[:space:]]*version \"$escaped_version\"$" "$file" || { echo 'Cask version differs from release' >&2; exit 1; }
  if gh api "repos/$repository/contents/Casks/$token.rb?ref=$head" >"$temporary/current.json" 2>/dev/null; then
    jq -e '.content|type == "string"' "$temporary/current.json" >/dev/null || { echo 'invalid Cask content response' >&2; exit 1; }
    if jq -e --rawfile expected "$file" '(.content|gsub("\\s";"")) == ($expected|@base64)' "$temporary/current.json" >/dev/null; then continue; fi
  else
    jq -e '.status == "404"' "$temporary/current.json" >/dev/null || { echo 'current Cask could not be verified' >&2; exit 1; }
  fi
  jq --arg path "Casks/$token.rb" --rawfile contents "$file" '. + [{path:$path,contents:($contents|@base64)}]' \
    "$temporary/additions.json" >"$temporary/next.json"
  mv "$temporary/next.json" "$temporary/additions.json"
done
if [[ "$(jq length "$temporary/additions.json")" == 0 ]]; then exit 0; fi
jq -n --arg repository "$repository" --arg branch "$branch" --arg head "$head" --slurpfile additions "$temporary/additions.json" \
  '{branch:{repositoryNameWithOwner:$repository,branchName:$branch},expectedHeadOid:$head,
    message:{headline:"chore: updated release packages",body:"- Recorded the verified shared-version package references"},
    fileChanges:{additions:$additions[0]}}' >"$temporary/input.json"
bash "$scripts/verified-commit.sh" "$repository" "$temporary/input.json"
