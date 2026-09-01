package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testX64Digest   = "1f3f918c6a83f506aaf78021bc0b0b8a5b235f2629caa6e97a1ee59f0f816adc"
	testARM64Digest = "7b8e440c1722ca8daa2f8046d48a03b785730860046e40493616e5af0b564f10"
)

func TestRenderManifestsMatchesGoldenBytes(t *testing.T) {
	manifests, err := renderManifests(testManifestConfig())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"jacobcxdev.cq.yaml": `# yaml-language-server: $schema=https://aka.ms/winget-manifest.version.1.10.0.schema.json
PackageIdentifier: jacobcxdev.cq
PackageVersion: 0.27.0
DefaultLocale: en-US
ManifestType: version
ManifestVersion: 1.10.0
`,
		"jacobcxdev.cq.installer.yaml": `# yaml-language-server: $schema=https://aka.ms/winget-manifest.installer.1.10.0.schema.json
PackageIdentifier: jacobcxdev.cq
PackageVersion: 0.27.0
InstallerLocale: en-US
InstallerType: wix
Scope: user
InstallModes:
  - interactive
  - silent
  - silentWithProgress
UpgradeBehavior: install
Commands:
  - cq
Installers:
  - Architecture: x64
    InstallerUrl: https://github.com/jacobcxdev/cq/releases/download/v0.27.0/cq_0.27.0_windows_amd64.msi
    InstallerSha256: 1f3f918c6a83f506aaf78021bc0b0b8a5b235f2629caa6e97a1ee59f0f816adc
    AppsAndFeaturesEntries:
      - DisplayName: CQ
        Publisher: jacobcxdev
        DisplayVersion: 0.27.0
        UpgradeCode: '{7B64C2EF-DF57-4C43-8C35-7D1949B09469}'
  - Architecture: arm64
    InstallerUrl: https://github.com/jacobcxdev/cq/releases/download/v0.27.0/cq_0.27.0_windows_arm64.msi
    InstallerSha256: 7b8e440c1722ca8daa2f8046d48a03b785730860046e40493616e5af0b564f10
    AppsAndFeaturesEntries:
      - DisplayName: CQ
        Publisher: jacobcxdev
        DisplayVersion: 0.27.0
        UpgradeCode: '{7B64C2EF-DF57-4C43-8C35-7D1949B09469}'
ManifestType: installer
ManifestVersion: 1.10.0
`,
		"jacobcxdev.cq.locale.en-US.yaml": `# yaml-language-server: $schema=https://aka.ms/winget-manifest.defaultLocale.1.10.0.schema.json
PackageIdentifier: jacobcxdev.cq
PackageVersion: 0.27.0
PackageLocale: en-US
Publisher: jacobcxdev
PublisherUrl: https://github.com/jacobcxdev
PublisherSupportUrl: https://github.com/jacobcxdev/cq/issues
Author: jacobcxdev
PackageName: CQ
PackageUrl: https://github.com/jacobcxdev/cq
License: MIT
LicenseUrl: https://github.com/jacobcxdev/cq/blob/v0.27.0/LICENSE
ShortDescription: Check AI provider usage limits and run CQ proxy services.
Moniker: cq
Tags:
  - ai
  - api
  - cli
  - quota
ManifestType: defaultLocale
ManifestVersion: 1.10.0
`,
	}
	if len(manifests) != len(want) {
		t.Fatalf("manifest count = %d", len(manifests))
	}
	for name, wantBody := range want {
		if got := string(manifests[name]); got != wantBody {
			t.Fatalf("%s differs\ngot:\n%s\nwant:\n%s", name, got, wantBody)
		}
	}
}

func TestManifestConfigRejectsUnpinnedInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*manifestConfig)
	}{
		{name: "bad version", mutate: func(config *manifestConfig) { config.Version = "v0.27.0" }},
		{name: "uppercase digest", mutate: func(config *manifestConfig) { config.X64SHA256 = strings.ToUpper(config.X64SHA256) }},
		{name: "wrong host", mutate: func(config *manifestConfig) {
			config.X64URL = strings.Replace(config.X64URL, "github.com", "example.com", 1)
		}},
		{name: "query", mutate: func(config *manifestConfig) { config.ARM64URL += "?download=1" }},
		{name: "wrong architecture", mutate: func(config *manifestConfig) { config.X64URL = strings.Replace(config.X64URL, "amd64", "arm64", 1) }},
		{name: "legacy executable", mutate: func(config *manifestConfig) { config.X64URL = strings.TrimSuffix(config.X64URL, ".msi") + ".exe" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testManifestConfig()
			test.mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("Validate() succeeded")
			}
		})
	}
}

func TestGenerateWritesCommunityManifestPath(t *testing.T) {
	output := t.TempDir()
	if err := generate(testManifestConfig(), output); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(output, "manifests", "j", "jacobcxdev", "cq", "0.27.0")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("generated entries = %d", len(entries))
	}
}

func TestGenerateRejectsNonemptyOutput(t *testing.T) {
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(output, "keep"), []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := generate(testManifestConfig(), output); err == nil {
		t.Fatal("generate() accepted nonempty output")
	}
	if body, err := os.ReadFile(filepath.Join(output, "keep")); err != nil || string(body) != "user" {
		t.Fatalf("existing output changed: %q, %v", body, err)
	}
}

func TestFindRepositoryRootAcceptsWindowsLineEndings(t *testing.T) {
	repositoryRoot := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(repositoryRoot, "go.mod"),
		[]byte(repositoryModule+"\r\n\r\ngo 1.26.1\r\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(repositoryRoot, "internal", "tools", "wingetmanifest")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := findRepositoryRootFrom(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got != repositoryRoot {
		t.Fatalf("findRepositoryRootFrom() = %q, want %q", got, repositoryRoot)
	}
}

func testManifestConfig() manifestConfig {
	return manifestConfig{
		Version:     "0.27.0",
		X64URL:      "https://github.com/jacobcxdev/cq/releases/download/v0.27.0/cq_0.27.0_windows_amd64.msi",
		X64SHA256:   testX64Digest,
		ARM64URL:    "https://github.com/jacobcxdev/cq/releases/download/v0.27.0/cq_0.27.0_windows_arm64.msi",
		ARM64SHA256: testARM64Digest,
	}
}
