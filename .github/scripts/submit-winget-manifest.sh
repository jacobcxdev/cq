#!/usr/bin/env bash
set -euo pipefail
umask 077

[[ $# == 1 && "$RELEASE_TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo 'a manifest directory and stable RELEASE_TAG are required' >&2; exit 1;
}
manifest=$1
version=${RELEASE_TAG#v}
repository=microsoft/winget-pkgs
fork=jacobcxdev/winget-pkgs
branch=cq-$version
prefix=manifests/j/jacobcxdev/cq/$version
scripts="$(cd "$(dirname "$0")" && pwd)"
temporary=$(mktemp -d /tmp/cq-winget-submit.XXXXXX)
trap 'rm -rf "$temporary"' EXIT
for name in jacobcxdev.cq.installer.yaml jacobcxdev.cq.locale.en-US.yaml jacobcxdev.cq.yaml; do
  [[ -f "$manifest/$name" && ! -L "$manifest/$name" ]] || { echo 'manifest file is absent or unsafe' >&2; exit 1; }
done
jq -n --arg prefix "$prefix" --rawfile installer "$manifest/jacobcxdev.cq.installer.yaml" \
  --rawfile locale "$manifest/jacobcxdev.cq.locale.en-US.yaml" --rawfile version "$manifest/jacobcxdev.cq.yaml" \
  '[{path:($prefix+"/jacobcxdev.cq.installer.yaml"),contents:($installer|@base64)},
    {path:($prefix+"/jacobcxdev.cq.locale.en-US.yaml"),contents:($locale|@base64)},
    {path:($prefix+"/jacobcxdev.cq.yaml"),contents:($version|@base64)}]' >"$temporary/additions.json"
release=$(gh release view "$RELEASE_TAG" --repo jacobcxdev/cq --json tagName,isDraft,isPrerelease)
jq -e --arg tag "$RELEASE_TAG" '.tagName == $tag and .isDraft == false and .isPrerelease == false' <<<"$release" >/dev/null
if gh api "repos/$repository/contents/$prefix" >"$temporary/published.json" 2>/dev/null; then
  echo "WinGet $version is already published"
  exit 0
fi
jq -e '.status == "404"' "$temporary/published.json" >/dev/null || {
  echo 'upstream publication state could not be verified' >&2; exit 1;
}

verify_head() {
  local head=$1 base=${2:-}
  gh api "repos/$fork/commits/$head" >"$temporary/commit.json"
  jq -e --arg head "$head" --arg base "$base" --slurpfile additions "$temporary/additions.json" '
    .sha == $head and .commit.verification.verified == true and .commit.verification.reason == "valid"
    and ($base == "" or ((.parents|length) == 1 and .parents[0].sha == $base))
    and ((.files|map(.filename)|sort) == ($additions[0]|map(.path)|sort))
  ' "$temporary/commit.json" >/dev/null || { echo 'submission signature, parent or files could not be verified' >&2; exit 1; }
  for name in jacobcxdev.cq.installer.yaml jacobcxdev.cq.locale.en-US.yaml jacobcxdev.cq.yaml; do
    gh api "repos/$fork/contents/$prefix/$name?ref=$head" >"$temporary/content.json"
    jq -e --rawfile expected "$manifest/$name" '(.content|gsub("\\s";"")|@base64d) == $expected' "$temporary/content.json" >/dev/null || {
      echo 'submission manifest differs from the qualified public package' >&2; exit 1;
    }
  done
}
verify_submission() {
  local url=$1 pr head current
  pr=$(gh pr view "$url" --repo "$repository" --json number,state,title,headRefName,headRefOid,headRepository,headRepositoryOwner,author,url,files,assignees)
  jq -e --arg version "$version" --slurpfile additions "$temporary/additions.json" '
    .state == "OPEN" and .author.login == "jacobcxdev" and .headRepositoryOwner.login == "jacobcxdev"
    and .headRepository.name == "winget-pkgs" and .headRefName == ("cq-"+$version)
    and .url == ("https://github.com/microsoft/winget-pkgs/pull/"+(.number|tostring))
    and (.title == ("New version: jacobcxdev.cq version "+$version) or .title == ("Update: jacobcxdev.cq to "+$version))
    and (.assignees|map(.login)|index("jacobcxdev") != null)
    and (.headRefOid|test("^[0-9a-f]{40}$"))
    and ((.files|map(.path)|sort) == ($additions[0]|map(.path)|sort))
  ' <<<"$pr" >/dev/null || { echo 'submission identity, assignment or files could not be verified' >&2; exit 1; }
  head=$(jq -r .headRefOid <<<"$pr")
  current=$(gh api "repos/$fork/git/ref/heads/$branch" --jq .object.sha)
  [[ "$current" == "$head" ]] || { echo 'submission branch changed' >&2; exit 1; }
  verify_head "$head"
}

existing=$(gh pr list --repo "$repository" --head "$branch" --state open --json url,author,headRepositoryOwner |
  jq '[.[]|select(.author.login == "jacobcxdev" and .headRepositoryOwner.login == "jacobcxdev")]')
[[ "$(jq length <<<"$existing")" -le 1 ]] || { echo 'owned submission is ambiguous' >&2; exit 1; }
if [[ "$(jq length <<<"$existing")" == 1 ]]; then
  url=$(jq -r '.[0].url' <<<"$existing")
  verify_submission "$url"
  echo "$url"
  exit 0
fi
# No remote write occurs until the actual publication token can both assign the
# requested user and add assignees. GitHub otherwise silently ignores assignment.
bash "$scripts/check-winget-permissions.sh"
gh api "repos/$fork" >"$temporary/fork.json"
jq -e '.fork == true and .owner.login == "jacobcxdev" and .parent.full_name == "microsoft/winget-pkgs"' "$temporary/fork.json" >/dev/null
base=$(gh api "repos/$fork/git/ref/heads/master" --jq .object.sha)
[[ "$base" =~ ^[0-9a-f]{40}$ ]]
if gh api "repos/$fork/git/ref/heads/$branch" >"$temporary/ref.json" 2>/dev/null; then
  head=$(jq -er .object.sha "$temporary/ref.json")
  [[ "$head" =~ ^[0-9a-f]{40}$ ]]
  if [[ "$head" != "$base" ]]; then verify_head "$head" "$base"; fi
else
  jq -e '.status == "404"' "$temporary/ref.json" >/dev/null || { echo 'branch state could not be verified' >&2; exit 1; }
  jq -n --arg ref "refs/heads/$branch" --arg sha "$base" '{ref:$ref,sha:$sha}' >"$temporary/ref-input.json"
  gh api --method POST "repos/$fork/git/refs" --input "$temporary/ref-input.json" >/dev/null
  head=$base
fi
if [[ "$head" == "$base" ]]; then
  jq -n --arg repository "$fork" --arg branch "$branch" --arg base "$base" --slurpfile additions "$temporary/additions.json" \
    '{branch:{repositoryNameWithOwner:$repository,refName:$branch},expectedHeadOid:$base,
      message:{headline:"chore: updated package manifests",body:"- Recorded the published version and installer checksums"},
      fileChanges:{additions:$additions[0]}}' >"$temporary/commit-input.json"
  head=$(bash "$scripts/verified-commit.sh" "$fork" "$temporary/commit-input.json")
fi
[[ "$(gh api "repos/$fork/git/ref/heads/$branch" --jq .object.sha)" == "$head" ]] || { echo 'signed branch changed before submission' >&2; exit 1; }
gh api "repos/$repository/compare/master...jacobcxdev:$branch" >"$temporary/comparison.json"
jq -e --slurpfile additions "$temporary/additions.json" '
  ((.files|map(.filename)|sort) == ($additions[0]|map(.path)|sort))
  and all(.files[]; .status == "added")
' "$temporary/comparison.json" >/dev/null || { echo 'upstream comparison contains unexpected changes' >&2; exit 1; }
printf 'Published CQ %s MSI installers for x64 and ARM64.\n' "$version" >"$temporary/body.md"
url=$(gh pr create --repo "$repository" --base master --head "jacobcxdev:$branch" \
  --title "New version: jacobcxdev.cq version $version" --body-file "$temporary/body.md" --assignee jacobcxdev)
verify_submission "$url"
echo "$url"
