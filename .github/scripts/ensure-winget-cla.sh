#!/usr/bin/env bash
set -euo pipefail

test -n "$GH_TOKEN"
[[ "$RELEASE_TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo 'a stable RELEASE_TAG is required' >&2
  exit 1
}
version=$(printf '%s' "$RELEASE_TAG" | cut -c2-)
repository=microsoft/winget-pkgs
agreement='@microsoft-github-policy-service agree company="JacobCX Development Ltd."'
[[ "$(gh api user --jq .login)" == jacobcxdev ]] || {
  echo 'refusing CLA comment: authenticated author must be jacobcxdev' >&2
  exit 1
}
matches=$(gh pr list --repo "$repository" --state open --head "cq-$version" \
  --json number,title,headRefName,headRepositoryOwner,author,url |
  jq --arg version "$version" '[.[] | select(
    .author.login == "jacobcxdev" and .headRepositoryOwner.login == "jacobcxdev"
    and .headRefName == ("cq-" + $version)
    and (.title == ("New version: jacobcxdev.cq version " + $version)
      or .title == ("Update: jacobcxdev.cq to " + $version)))]')
count=$(jq length <<<"$matches")
if [[ "$count" == 0 ]] && gh api "repos/$repository/contents/manifests/j/jacobcxdev/cq/$version" >/dev/null 2>&1; then
  echo "WinGet $version is already published; no open CLA target"
  exit 0
fi
[[ "$count" == 1 ]] || {
  echo 'refusing CLA comment: current owned CQ PR is absent or ambiguous' >&2
  exit 1
}
number=$(jq -r '.[0].number' <<<"$matches")
[[ "$number" =~ ^[0-9]+$ ]]
pr=$(gh pr view "$number" --repo "$repository" \
  --json number,state,title,headRefName,headRefOid,headRepository,headRepositoryOwner,author,url,files)
jq -e --arg version "$version" --argjson number "$number" '
  .number == $number and .state == "OPEN"
  and .url == ("https://github.com/microsoft/winget-pkgs/pull/" + ($number|tostring))
  and .author.login == "jacobcxdev" and .headRepositoryOwner.login == "jacobcxdev"
  and .headRepository.name == "winget-pkgs" and (.headRefOid|test("^[a-fA-F0-9]{40}$"))
  and .headRefName == ("cq-" + $version)
  and (.title == ("New version: jacobcxdev.cq version " + $version)
    or .title == ("Update: jacobcxdev.cq to " + $version))
  and ((.files|map(.path)|sort) == ([
    ("manifests/j/jacobcxdev/cq/" + $version + "/jacobcxdev.cq.installer.yaml"),
    ("manifests/j/jacobcxdev/cq/" + $version + "/jacobcxdev.cq.locale.en-US.yaml"),
    ("manifests/j/jacobcxdev/cq/" + $version + "/jacobcxdev.cq.yaml")]|sort))
' <<<"$pr" >/dev/null || {
  echo 'refusing CLA comment: CQ PR identity or version files changed' >&2
  exit 1
}
comments=$(gh api --paginate --slurp "repos/$repository/issues/$number/comments")
jq -e 'type == "array" and all(.[]; type == "array" and all(.[];
  type == "object" and (.user.login|type) == "string" and (.body|type) == "string"))' \
  <<<"$comments" >/dev/null || {
  echo 'refusing CLA comment: existing comments could not be verified' >&2
  exit 1
}
if jq -e --arg agreement "$agreement" 'flatten | any(.[];
  .user.login == "jacobcxdev" and ((.body // "" | sub("[\r\n]+$"; "")) == $agreement))' \
  <<<"$comments" >/dev/null; then
  echo "CLA agreement already posted on CQ $version PR $number"
  exit 0
fi
head=$(jq -r .headRefOid <<<"$pr")
current=$(gh pr view "$number" --repo "$repository" \
  --json number,state,headRefName,headRefOid,headRepository,headRepositoryOwner,author,url)
jq -e --arg version "$version" --arg head "$head" --argjson number "$number" '
  .number == $number and .state == "OPEN" and .headRefOid == $head
  and .url == ("https://github.com/microsoft/winget-pkgs/pull/" + ($number|tostring))
  and .headRefName == ("cq-" + $version) and .headRepository.name == "winget-pkgs"
  and .headRepositoryOwner.login == "jacobcxdev" and .author.login == "jacobcxdev"
' <<<"$current" >/dev/null || {
  echo 'refusing CLA comment: current PR head or identity changed' >&2
  exit 1
}
[[ "$(gh api "repos/jacobcxdev/winget-pkgs/git/ref/heads/cq-$version" --jq .object.sha)" == "$head" ]] || {
  echo 'refusing CLA comment: owned fork branch no longer matches reviewed PR head' >&2
  exit 1
}
body=$(mktemp /tmp/cq-winget-cla.XXXXXX)
trap 'rm -f "$body"' EXIT
printf '%s\n' "$agreement" >"$body"
gh pr comment "$number" --repo "$repository" --body-file "$body"
