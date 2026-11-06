//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexHarnessProfileReadsDirectoryAliasWithoutFollowingFileLinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "real")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	contents := []byte(`{"changes":{"default":"disabled"}}`)
	if err := os.WriteFile(filepath.Join(directory, "profile.json"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := defaultCodexHarnessRunner()
	data, err := runner.readHarnessProfileFile(filepath.Join(alias, "profile.json"))
	if err != nil || !bytes.Equal(data, contents) {
		t.Fatalf("directory alias profile read failed: %v", err)
	}
	if err := os.Symlink(filepath.Join(directory, "profile.json"), filepath.Join(directory, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.readHarnessProfileFile(filepath.Join(alias, "linked.json")); err == nil {
		t.Fatal("profile input followed a final file symlink")
	}
	if _, err := runner.readHarnessProfileFile(filepath.Join(alias, "missing.json")); err == nil {
		t.Fatal("missing profile input accepted")
	}
}
