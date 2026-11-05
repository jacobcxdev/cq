package main

import (
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestHarnessDisableBypassesInvalidWorkspaceForRestoration(t *testing.T) {
	fsys := fsutil.NewMemFS()
	roots := userdirs.Roots{Config: "/user/cq", State: "/user/cq/state"}
	policyPath := filepath.Join(roots.Config, "harness.json")
	policy := harness.Policy{Version: 2, Enabled: false}
	if err := harness.Save(fsys, policyPath, policy); err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkdirAll("/workspace/.cq", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/workspace/.cq/harness.json", []byte("broken workspace configuration"), 0o644); err != nil {
		t.Fatal(err)
	}
	controller := newHarnessController(fsys, roots)
	resolved, err := controller.ContextPolicy("/workspace")
	if err != nil || resolved.Enabled {
		t.Fatalf("disabled policy could not restore baseline: %#v,%v", resolved, err)
	}
	policy.Enabled = true
	if err := harness.Save(fsys, policyPath, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ContextPolicy("/workspace"); err == nil {
		t.Fatal("enabled runtime ignored malformed workspace")
	}
}
