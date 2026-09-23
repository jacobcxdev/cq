package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is a publication-boundary regression, not release qualification.
func TestCandidateWorkflowBuildsWithoutPublication(t *testing.T) {
	root := validateCodexReleaseRepositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	_, candidate, found := strings.Cut(workflow, "  candidate-build:\n")
	if !found {
		t.Fatal("candidate job is missing")
	}
	for _, required := range []string{
		"on:\n  workflow_dispatch:\n    inputs:\n",
		"if: ${{ inputs.candidate_only }}",
		"permissions:\n  contents: read\n",
		"persist-credentials: false",
		"install-only: true",
		"version_template: '1.0.0'",
		"test -n \"$GEMINI_ANTIGRAVITY_CLIENT_SECRET\"",
		"goreleaser build --snapshot --clean --skip=before,pre-hooks,post-hooks",
		"version[\"data\"][\"version\"] == \"1.0.0\"",
		"version[\"schema_version\"] == 2",
		"\"vcs.revision\": os.environ[\"GITHUB_SHA\"]",
		"\"vcs.modified\": \"false\"",
		"path: ${{ runner.temp }}/candidate-artifact/",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("candidate build boundary missing %q", required)
		}
	}
	for _, job := range []string{"cli-contract", "packages", "validate-codex", "homebrew-lifecycle", "linux", "windows"} {
		if !strings.Contains(workflow, "  "+job+":\n    if: ${{ !inputs.candidate_only }}\n") {
			t.Errorf("existing job %s must not run during candidate-only build", job)
		}
	}
	for _, forbidden := range []string{
		"contents: write", "actions: write", "statuses: write", "pull-requests:",
		"goreleaser release", "gh release", "gh api", "git push", "git tag",
		"HOMEBREW_TAP_TOKEN", "GITHUB_TOKEN", "--verbose",
		"path: candidate-build", "path: dist", "push:", "pull_request:", "schedule:",
	} {
		if strings.Contains(candidate, forbidden) {
			t.Errorf("candidate workflow crossed publication or secret boundary: %q", forbidden)
		}
	}
	if strings.Count(candidate, "${{ secrets.") != 1 || !strings.Contains(workflow, "${{ secrets.GEMINI_ANTIGRAVITY_CLIENT_SECRET }}") {
		t.Error("candidate workflow must expose only Gemini build secret")
	}
	_, build, found := strings.Cut(workflow, "      - name: Build all six targets")
	if !found {
		t.Fatal("candidate build step is missing")
	}
	build, _, _ = strings.Cut(build, "      - name: Collect")
	if !strings.Contains(build, "${{ secrets.GEMINI_ANTIGRAVITY_CLIENT_SECRET }}") || strings.Index(build, "test -n") > strings.Index(build, "goreleaser build") {
		t.Error("secret preflight must occur in build step before building")
	}
}
