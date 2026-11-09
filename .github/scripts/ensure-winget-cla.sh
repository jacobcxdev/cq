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
release=$(gh release view "$RELEASE_TAG" --repo jacobcxdev/cq --json tagName,isDraft,isPrerelease)
jq -e --arg tag "$RELEASE_TAG" '.tagName == $tag and .isDraft == false and .isPrerelease == false' \
  <<<"$release" >/dev/null || {
  echo 'refusing CLA comment: CQ release must be published and stable' >&2
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
# WinGet's opening policy adds Needs-CLA asynchronously for these associations.
# Its separate author-comment rule only removes a label which already exists.
head=$(jq -r .headRefOid <<<"$pr")
cla_check_passed() {
  local checks
  checks=$(gh api --paginate --slurp "repos/$repository/commits/$head/check-runs?check_name=license%2Fcla&filter=latest&per_page=100") || return 1
  jq -e --arg head "$head" '
    if type == "array" and all(.[]; type == "object" and (.check_runs|type) == "array")
    then [.[].check_runs[] | select(.name == "license/cla"
      and .app.id == 95686 and .app.slug == "microsoft-github-policy-service")]
    else error("invalid CLA check response") end |
    if all(.[]; (.id|type) == "number" and .id > 0 and (.id|floor) == .id and .head_sha == $head)
    then max_by(.id) | .status == "completed" and .conclusion == "success"
    else false end
  ' <<<"$checks" >/dev/null
}
read_policy() {
  policy=$(gh api "repos/$repository/pulls/$number" --jq '{number,state,author:.user.login,author_association,labels:[.labels[].name]}')
  jq -e --argjson number "$number" '
    .number == $number and .state == "open" and .author == "jacobcxdev"
    and (.author_association | IN("COLLABORATOR","CONTRIBUTOR","FIRST_TIMER","FIRST_TIME_CONTRIBUTOR","MANNEQUIN","MEMBER","NONE","OWNER"))
    and (.labels|type) == "array" and all(.labels[]; type == "string")
  ' <<<"$policy" >/dev/null || {
    echo 'refusing CLA comment: upstream author association or labels could not be verified' >&2
    exit 1
  }
}
read_label_event() {
  local events
  events=$(gh api --paginate --slurp "repos/$repository/issues/$number/events") || return 1
  jq -cs '
    if length == 1 then .[0] else error("invalid label response") end |
    def stamp: type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")
      and (try (fromdateiso8601|type == "number") catch false);
    if type == "array" and all(.[]; type == "array" and all(.[]; type == "object" and (.event|type) == "string"))
    then [flatten[] | select((.event == "labeled" or .event == "unlabeled") and .label.name == "Needs-CLA")]
      | if all(.[]; (.id|type) == "number" and .id > 0 and (.id|floor) == .id and (.created_at|stamp))
        then max_by(.id) | if . == null then null else {id,event,created_at} end
        else error("invalid label metadata") end
    else error("invalid label events") end
  ' <<<"$events"
}
for attempt in {1..24}; do
  read_policy
  if jq -e '(.author_association|test("^(NONE|FIRST_TIMER|FIRST_TIME_CONTRIBUTOR)$")|not)
    or (.labels|index("Needs-CLA") != null)' <<<"$policy" >/dev/null; then
    break
  fi
  # Policy can accept the CLA without ever adding Needs-CLA. Only trust its
  # latest check on this exact head; explicit labels still control replay.
  if cla_check_passed; then
    break
  fi
  label_event=$(read_label_event) || {
    echo 'refusing CLA comment: initial label events could not be verified' >&2
    exit 1
  }
  if jq -e '.event == "unlabeled"' <<<"$label_event" >/dev/null; then
    break
  fi
  if [[ "$attempt" == 24 ]]; then
    echo 'refusing CLA comment: timed out waiting for initial Needs-CLA policy processing' >&2
    exit 1
  fi
  sleep 5
done
comments=$(gh api --paginate --slurp "repos/$repository/issues/$number/comments")
jq -e 'type == "array" and all(.[]; type == "array" and all(.[];
  type == "object" and (.user.login|type) == "string" and (.body|type) == "string"))' \
  <<<"$comments" >/dev/null || {
  echo 'refusing CLA comment: existing comments could not be verified' >&2
  exit 1
}
existing=$(jq -c --arg agreement "$agreement" '[flatten[] | select(
  .user.login == "jacobcxdev" and ((.body|sub("[\\r\\n]+$"; "")) == $agreement))]' <<<"$comments")
jq -e --arg issue "https://api.github.com/repos/$repository/issues/$number" '
  def stamp: type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")
    and (try (fromdateiso8601|type == "number") catch false);
  all(.[]; (.id|type) == "number" and .id > 0 and (.id|floor) == .id and .issue_url == $issue
    and (.created_at|stamp) and (.updated_at|stamp) and .updated_at >= .created_at)
' <<<"$existing" >/dev/null || {
  echo 'refusing CLA comment: agreement metadata could not be verified' >&2
  exit 1
}
existing=$(jq -c 'max_by(.updated_at,.id) // null' <<<"$existing")
replay=false
if [[ "$existing" != null ]]; then
  if jq -e '.labels|index("Needs-CLA") != null' <<<"$policy" >/dev/null; then
    generation=$(read_label_event) || {
      echo 'refusing CLA replay: current Needs-CLA label event could not be verified' >&2
      exit 1
    }
    jq -e '.event == "labeled"' <<<"$generation" >/dev/null || {
      echo 'refusing CLA replay: current Needs-CLA label generation is absent' >&2
      exit 1
    }
    replay=$(jq -nr --argjson existing "$existing" --argjson generation "$generation" '$existing.updated_at < $generation.created_at')
  fi
  if [[ "$replay" != true ]]; then
    echo "CLA agreement already posted on CQ $version PR $number; no replay needed"
    exit 0
  fi
fi
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
if [[ "$replay" == true ]]; then
  read_policy
  if ! jq -e '.labels|index("Needs-CLA") != null' <<<"$policy" >/dev/null; then
    echo "Needs-CLA already cleared on CQ $version PR $number"
    exit 0
  fi
  current_generation=$(read_label_event)
  [[ "$current_generation" == "$generation" ]] || {
    echo 'refusing CLA replay: Needs-CLA label generation changed' >&2
    exit 1
  }
  comment_id=$(jq -r .id <<<"$existing")
  current_comment=$(gh api "repos/$repository/issues/comments/$comment_id")
  jq -e --argjson expected "$existing" '
    .id == $expected.id and .user.login == "jacobcxdev" and .issue_url == $expected.issue_url
    and .body == $expected.body and .created_at == $expected.created_at and .updated_at == $expected.updated_at
  ' <<<"$current_comment" >/dev/null || {
    echo 'refusing CLA replay: existing author agreement changed' >&2
    exit 1
  }
  # Edited issue comments reach the upstream rule too. Toggle only the trailing
  # blank line to emit a real edit while leaving the legal command unchanged.
  jq -n --arg agreement "$agreement" --argjson existing "$existing" \
    '{body:($agreement + (if $existing.body == ($agreement+"\n") then "\n\n" else "\n" end))}' >"$body"
  updated=$(gh api --method PATCH "repos/$repository/issues/comments/$comment_id" --input "$body")
  jq -e --argjson expected "$existing" --argjson generation "$generation" --slurpfile sent "$body" '
    .id == $expected.id and .user.login == "jacobcxdev" and .issue_url == $expected.issue_url
    and .body == $sent[0].body and .created_at == $expected.created_at
    and (.updated_at|type) == "string"
    and (.updated_at|test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
    and (try (.updated_at|fromdateiso8601|type == "number") catch false)
    and .updated_at >= $expected.updated_at and .updated_at >= $generation.created_at
  ' <<<"$updated" >/dev/null || {
    echo 'CLA replay response could not be verified; inspect the existing comment before retrying' >&2
    exit 1
  }
  echo "Replayed existing CLA agreement on CQ $version PR $number"
else
  printf '%s\n' "$agreement" >"$body"
  gh pr comment "$number" --repo "$repository" --body-file "$body"
fi
