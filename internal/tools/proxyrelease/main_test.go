package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseReleaseBuildManifestAcceptsClosedRequestAndRejectsUnknown(t *testing.T) {
	valid := `{"approved_authority_path":"release-authority.json","approved_ed25519_public_key":"4444444444444444444444444444444444444444444444444444444444444444","approved_release_build_authority_digest":"3333333333333333333333333333333333333333333333333333333333333333","bundle_path":"release-bundle","kind":"proxy_release_build_manifest_v1","purpose":"floor","repository_identity_digest":"1111111111111111111111111111111111111111111111111111111111111111","schema_version":1,"source_commit":"86518eaa0edd580413dad750b31f1bfcea46f3c9","source_tree_digest":"2222222222222222222222222222222222222222222222222222222222222222"}`
	manifest, err := parseReleaseBuildManifestV1(strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Purpose != "floor" || manifest.BundlePath != "release-bundle" {
		t.Fatalf("manifest = %#v", manifest)
	}
	unknown := strings.Replace(valid, `"bundle_path"`, `"extra":true,"bundle_path"`, 1)
	if _, err := parseReleaseBuildManifestV1(strings.NewReader(unknown)); err == nil {
		t.Fatal("accepted unknown release-build manifest member")
	}
}

func TestRunRequiresExactSourceBeforeReleaseWork(t *testing.T) {
	base := `{"approved_authority_path":"release-authority.json","approved_ed25519_public_key":"4444444444444444444444444444444444444444444444444444444444444444","approved_release_build_authority_digest":"3333333333333333333333333333333333333333333333333333333333333333","bundle_path":"release-bundle","kind":"proxy_release_build_manifest_v1","purpose":"floor","repository_identity_digest":"1111111111111111111111111111111111111111111111111111111111111111","schema_version":1,"source_commit":"86518eaa0edd580413dad750b31f1bfcea46f3c9","source_tree_digest":"2222222222222222222222222222222222222222222222222222222222222222"}`
	for _, purpose := range []string{"floor", "target"} {
		t.Run(purpose, func(t *testing.T) {
			manifest := []byte(strings.Replace(base, `"purpose":"floor"`, `"purpose":"`+purpose+`"`, 1))
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, manifest, 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err := run(path, &output)
			if err == nil || strings.Contains(err.Error(), "feature inactive") {
				t.Fatalf("run error = %v", err)
			}
			if output.Len() != 0 {
				t.Fatalf("inactive production emitted output %q", output.Bytes())
			}
		})
	}
}

func TestReadReleaseBuildManifestRejectsSymlink(t *testing.T) {
	valid := []byte(`{"approved_authority_path":"release-authority.json","approved_ed25519_public_key":"4444444444444444444444444444444444444444444444444444444444444444","approved_release_build_authority_digest":"3333333333333333333333333333333333333333333333333333333333333333","bundle_path":"release-bundle","kind":"proxy_release_build_manifest_v1","purpose":"floor","repository_identity_digest":"1111111111111111111111111111111111111111111111111111111111111111","schema_version":1,"source_commit":"86518eaa0edd580413dad750b31f1bfcea46f3c9","source_tree_digest":"2222222222222222222222222222222222222222222222222222222222222222"}`)
	directory := t.TempDir()
	target := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(target, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "manifest-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readReleaseBuildManifestV1(link); err == nil {
		t.Fatal("accepted symlink release-build manifest")
	}
}

func TestBuildProxyReleaseShellEntryRejectsMissingManifest(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(repositoryRoot, "scripts", "build-proxy-release"))
	command.Dir = repositoryRoot
	root := t.TempDir()
	paths := []string{filepath.Join(root, "home"), filepath.Join(root, "gocache"), filepath.Join(root, "gomodcache"), filepath.Join(root, "gopath")}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "hostile-dirname-ran")
	if err := os.WriteFile(filepath.Join(bin, "dirname"), []byte("#!/bin/sh\n/usr/bin/touch \""+marker+"\"\nexit 97\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command.Env = append(os.Environ(), "HOME="+paths[0], "GOCACHE="+paths[1], "GOMODCACHE="+paths[2], "GOPATH="+paths[3], "GOFLAGS=-toolexec=/tmp/attacker", "GOWORK=/tmp/attacker.work")
	command.Env = append(command.Env, "PATH="+bin)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("build-proxy-release accepted missing manifest")
	}
	if !bytes.Contains(output, []byte("exactly one release-build manifest")) {
		t.Fatalf("unexpected error output: %s", output)
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("hostile path %q was used: %v", path, err)
		}
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hostile dirname ran: %v", err)
	}
}

func TestBuildProxyReleaseShellEntryParsesThenRequiresExactSource(t *testing.T) {
	repositoryRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"approved_authority_path":"release-authority.json","approved_ed25519_public_key":"4444444444444444444444444444444444444444444444444444444444444444","approved_release_build_authority_digest":"3333333333333333333333333333333333333333333333333333333333333333","bundle_path":"release-bundle","kind":"proxy_release_build_manifest_v1","purpose":"target","repository_identity_digest":"1111111111111111111111111111111111111111111111111111111111111111","schema_version":1,"source_commit":"86518eaa0edd580413dad750b31f1bfcea46f3c9","source_tree_digest":"2222222222222222222222222222222222222222222222222222222222222222"}`)
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(repositoryRoot, "scripts", "build-proxy-release"), path)
	command.Dir = repositoryRoot
	hostileRoot := t.TempDir()
	hostilePaths := []string{
		filepath.Join(hostileRoot, "home"), filepath.Join(hostileRoot, "gocache"),
		filepath.Join(hostileRoot, "gomodcache"), filepath.Join(hostileRoot, "gopath"),
	}
	command.Env = append(os.Environ(),
		"HOME="+hostilePaths[0], "GOCACHE="+hostilePaths[1],
		"GOMODCACHE="+hostilePaths[2], "GOPATH="+hostilePaths[3],
		"GOFLAGS=-definitely-invalid", "GOENV=/definitely/missing/goenv",
		"GOWORK=/definitely/missing/go.work", "GOTOOLCHAIN=definitely-invalid",
		"GOPROXY=http://127.0.0.1:1", "PATH=/definitely/missing",
	)
	output, err := command.CombinedOutput()
	if err == nil || bytes.Contains(output, []byte("feature inactive")) {
		t.Fatalf("build wrapper = %v\n%s", err, output)
	}
	if bytes.Contains(output, []byte("all modules verified")) {
		t.Fatalf("build wrapper performed module-cache work before inactive refusal: %s", output)
	}
	for _, hostilePath := range hostilePaths {
		if _, err := os.Lstat(hostilePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("build wrapper used hostile path %q: %v", hostilePath, err)
		}
	}
}
