package main

import (
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/harness"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestHarnessDisableBypassesInvalidWorkspaceForRestoration(t *testing.T) {
	fsys := newHarnessFixtureFS()
	roots := userdirs.Roots{Config: harnessFixturePath("/user/cq"), State: harnessFixturePath("/user/cq/state")}
	policyPath := filepath.Join(roots.Config, "harness.json")
	policy := harness.Policy{Version: 2, Enabled: false}
	if err := harness.Save(fsys, policyPath, policy); err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkdirAll(harnessFixturePath("/workspace/.cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(harnessFixturePath("/workspace/.cq/harness.json"), []byte("broken workspace configuration"), 0o644); err != nil {
		t.Fatal(err)
	}
	controller := newHarnessController(fsys, roots)
	resolved, err := controller.ContextPolicy(harnessFixturePath("/workspace"))
	if err != nil || resolved.Enabled {
		t.Fatalf("disabled policy could not restore baseline: %#v,%v", resolved, err)
	}
	policy.Enabled = true
	if err := harness.Save(fsys, policyPath, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ContextPolicy(harnessFixturePath("/workspace")); err == nil {
		t.Fatal("enabled runtime ignored malformed workspace")
	}
}
