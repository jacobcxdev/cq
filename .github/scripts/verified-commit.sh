#!/usr/bin/env bash
set -euo pipefail
umask 077

[[ $# == 2 ]] || { echo 'usage: verified-commit.sh REPOSITORY INPUT_JSON' >&2; exit 1; }
repository=$1
input=$2
jq -e --arg repository "$repository" '
  .branch.repositoryNameWithOwner == $repository
  and ($repository|test("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$"))
  and (.branch.refName|type == "string" and length > 0)
  and (.expectedHeadOid|test("^[0-9a-f]{40}$"))
  and (.message.headline|type == "string" and length > 0 and length < 50)
  and (.message.body|type == "string" and startswith("- "))
  and (.fileChanges|keys == ["additions"])
  and (.fileChanges.additions|type == "array" and length > 0 and length <= 10)
  and all(.fileChanges.additions[];
    (.path|type == "string" and test("^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$")
      and (split("/")|all(. != "." and . != "..")))
    and (.contents|type == "string" and test("^[A-Za-z0-9+/]*={0,2}$")))
  and ((.fileChanges.additions|map(.path)|unique|length) == (.fileChanges.additions|length))
' "$input" >/dev/null || { echo 'refusing commit: invalid repository, message or file changes' >&2; exit 1; }
temporary=$(mktemp -d /tmp/cq-verified-commit.XXXXXX)
trap 'rm -rf "$temporary"' EXIT
jq -n --slurpfile input "$input" '{query:
  "mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input: $input) { commit { oid } ref { target { oid } } } }",
  variables:{input:$input[0]}}' >"$temporary/request.json"
gh api graphql --input "$temporary/request.json" >"$temporary/response.json"
head=$(jq -er '.data.createCommitOnBranch | select(.commit.oid == .ref.target.oid)
  | .commit.oid | select(test("^[0-9a-f]{40}$"))' "$temporary/response.json")
gh api "repos/$repository/commits/$head" >"$temporary/commit.json"
jq -e --arg head "$head" --slurpfile input "$input" '
  .sha == $head and (.parents|length) == 1 and .parents[0].sha == $input[0].expectedHeadOid
  and .commit.verification.verified == true and .commit.verification.reason == "valid"
  and ((.files|map(.filename)|sort) == ($input[0].fileChanges.additions|map(.path)|sort))
  and all(.files[]; .status == "added" or .status == "modified")
' "$temporary/commit.json" >/dev/null || {
  echo 'refusing publication: commit signature, parent or changed files could not be verified' >&2
  exit 1
}
printf '%s\n' "$head"
