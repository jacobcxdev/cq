package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testManifest() manifest {
	return manifest{1, "0.41.0", strings.Repeat("a", 40), 123, strings.Repeat("b", 64), "13.0"}
}

func TestNativeManifestPinAndClosedSchema(t *testing.T) {
	m := testManifest()
	var output bytes.Buffer
	if err := run([]string{"pin", m.Version, m.SourceCommit, "123", m.SHA256, m.MinimumMacOS}, &output); err != nil {
		t.Fatal(err)
	}
	var got manifest
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != m {
		t.Fatalf("pin %+v", got)
	}
	file := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(file, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var info bytes.Buffer
	if err := run([]string{"info", file}, &info); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(info.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["source_commit"] != m.SourceCommit || fields["asset_id"] != float64(m.AssetID) {
		t.Fatalf("pin identity lost in info: %s", info.String())
	}
	for _, change := range []func(*manifest){
		func(m *manifest) { m.SchemaVersion = 2 }, func(m *manifest) { m.Version = "0.41.0/../bad" }, func(m *manifest) { m.SourceCommit = "main" },
		func(m *manifest) { m.AssetID = 0 }, func(m *manifest) { m.SHA256 = "TODO" }, func(m *manifest) { m.MinimumMacOS = `13.0"; system("bad")` },
		func(m *manifest) { m.MinimumMacOS = "14.0" }, func(m *manifest) { m.MinimumMacOS = "12.0" },
	} {
		bad := m
		change(&bad)
		if err := bad.validate(); err == nil {
			t.Fatalf("invalid manifest accepted %+v", bad)
		}
	}
	for _, data := range []string{strings.TrimSpace(output.String()) + ` {}`, strings.Replace(output.String(), `"schema_version": 1`, `"unknown": true, "schema_version": 1`, 1)} {
		file := filepath.Join(t.TempDir(), "manifest.json")
		if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := run([]string{"info", file}, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid manifest JSON accepted")
		}
	}
}

func releaseFixture(t *testing.T, m manifest, change func(map[string]any)) string {
	t.Helper()
	value := map[string]any{"tag_name": m.tag(), "target_commitish": m.SourceCommit, "draft": false, "prerelease": true, "assets": []any{map[string]any{"id": m.AssetID, "name": m.asset(), "digest": "sha256:" + m.SHA256, "state": "uploaded", "browser_download_url": m.url()}}, "additional_github_field": "ignored"}
	if change != nil {
		change(value)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "release.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestNativeReleaseRequiresExactPublishedAsset(t *testing.T) {
	m := testManifest()
	if err := verifyRelease(m, releaseFixture(t, m, nil), false); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]any){
		func(r map[string]any) { r["target_commitish"] = "main" }, func(r map[string]any) { r["tag_name"] = "v0.41.0" }, func(r map[string]any) { r["draft"] = true }, func(r map[string]any) { r["prerelease"] = false },
		func(r map[string]any) { r["assets"] = []any{} }, func(r map[string]any) { r["assets"] = append(r["assets"].([]any), r["assets"].([]any)[0]) },
		func(r map[string]any) { r["assets"].([]any)[0].(map[string]any)["id"] = 124 }, func(r map[string]any) {
			r["assets"].([]any)[0].(map[string]any)["digest"] = "sha256:" + strings.Repeat("c", 64)
		},
		func(r map[string]any) {
			r["assets"].([]any)[0].(map[string]any)["browser_download_url"] = "https://example.invalid/asset.zip"
		},
	} {
		if err := verifyRelease(m, releaseFixture(t, m, change), false); err == nil {
			t.Fatal("changed native release accepted")
		}
	}
}

func TestNativeReleaseDraftAllowedOnlyForStaging(t *testing.T) {
	m := testManifest()
	draftURL := "https://github.com/" + repository + "/releases/download/untagged-de565141bccfa7e0646a/" + m.asset()
	file := releaseFixture(t, m, func(release map[string]any) {
		release["draft"] = true
		release["assets"].([]any)[0].(map[string]any)["browser_download_url"] = draftURL
	})
	if err := verifyRelease(m, file, true); err != nil {
		t.Fatal(err)
	}
	if err := verifyRelease(m, file, false); err == nil {
		t.Fatal("draft native asset passed publication gate")
	}
	file = releaseFixture(t, m, func(release map[string]any) {
		release["assets"].([]any)[0].(map[string]any)["browser_download_url"] = draftURL
	})
	if err := verifyRelease(m, file, true); err == nil {
		t.Fatal("untagged URL accepted for a published release")
	}
	for _, change := range []func(map[string]any){
		func(release map[string]any) { release["target_commitish"] = "main" },
		func(release map[string]any) { release["tag_name"] = "another-tag" },
		func(release map[string]any) { release["assets"].([]any)[0].(map[string]any)["id"] = 124 },
		func(release map[string]any) {
			release["assets"].([]any)[0].(map[string]any)["digest"] = "sha256:" + strings.Repeat("c", 64)
		},
		func(release map[string]any) {
			release["assets"].([]any)[0].(map[string]any)["browser_download_url"] = "https://example.invalid/" + m.asset()
		},
	} {
		file = releaseFixture(t, m, func(release map[string]any) {
			release["draft"] = true
			release["assets"].([]any)[0].(map[string]any)["browser_download_url"] = draftURL
			change(release)
		})
		if err := verifyRelease(m, file, true); err == nil {
			t.Fatal("changed draft asset identity accepted")
		}
	}
}

type archiveEntry struct {
	name, body string
	mode       os.FileMode
}

func archiveFixture(t *testing.T, extra ...archiveEntry) (manifest, string) {
	t.Helper()
	return archiveFixtureWithProvider(t, "dev.jacobcx.cq.network.proxy", extra...)
}

func archiveFixtureWithProvider(t *testing.T, provider string, extra ...archiveEntry) (manifest, string) {
	t.Helper()
	providerRoot := "CQNetwork.app/Contents/Library/SystemExtensions/" + provider + ".systemextension"
	entries := []archiveEntry{
		{"CQNetwork.app/Contents/Info.plist", "plist", 0o644},
		{"CQNetwork.app/Contents/MacOS/CQNetwork", "host", 0o755},
		{providerRoot + "/Contents/Info.plist", "plist", 0o644},
		{providerRoot + "/Contents/MacOS/CQNetworkProxy", "provider", 0o755},
	}
	entries = append(entries, extra...)
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name}
		header.SetMode(entry.mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "native.zip")
	if err := os.WriteFile(file, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testManifest()
	digest := sha256.Sum256(data.Bytes())
	m.SHA256 = hex.EncodeToString(digest[:])
	return m, file
}

func TestNativeArchiveRejectsUnmatchedProviderFilename(t *testing.T) {
	m, file := archiveFixtureWithProvider(t, "CQNetworkProxy")
	if err := verifyArchive(m, file); err == nil {
		t.Fatal("provider filename that differs from its identifier was accepted")
	}
}

func TestNativeArchiveValidatesDigestAndExtractionBoundary(t *testing.T) {
	m, file := archiveFixture(t, archiveEntry{"CQNetwork.app/Contents/host-link", "MacOS/CQNetwork", os.ModeSymlink | 0o777})
	if err := verifyArchive(m, file); err != nil {
		t.Fatal(err)
	}
	m.SHA256 = strings.Repeat("d", 64)
	if err := verifyArchive(m, file); err == nil {
		t.Fatal("wrong digest accepted")
	}
	for _, entry := range []archiveEntry{
		{"../outside", "bad", 0o644}, {"CQNetwork.app/../../outside", "bad", 0o644}, {"/outside", "bad", 0o644}, {"other.app/Contents/Info.plist", "bad", 0o644},
		{"CQNetwork.app/Contents/Info.plist", "duplicate", 0o644}, {"CQNetwork.app/Contents/escape", "../../../outside", os.ModeSymlink | 0o777},
		{"CQNetwork.app/Contents/escape", "/tmp/outside", os.ModeSymlink | 0o777}, {"CQNetwork.app/Contents/escape", strings.Repeat("x", 1025), os.ModeSymlink | 0o777},
	} {
		m, file := archiveFixture(t, entry)
		if err := verifyArchive(m, file); err == nil {
			t.Fatalf("unsafe entry accepted %s", entry.name)
		}
	}
}

func TestNativeArchiveRejectsEntriesBeneathSymlinks(t *testing.T) {
	entries := []archiveEntry{
		{"CQNetwork.app/Contents/subdir/x", "../../A", os.ModeSymlink | 0o777},
		{"CQNetwork.app/Contents/subdir/y", "x/../../../outside", os.ModeSymlink | 0o777},
		{"CQNetwork.app/Contents/subdir/y/file", "must not escape", 0o644},
	}
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "ancestor first", true: "descendant first"}[reverse], func(t *testing.T) {
			ordered := append([]archiveEntry(nil), entries...)
			if reverse {
				ordered[0], ordered[2] = ordered[2], ordered[0]
			}
			m, file := archiveFixture(t, ordered...)
			if err := verifyArchive(m, file); err == nil {
				t.Fatal("entry beneath composed symlink accepted")
			}
		})
	}
}

func TestNativeCaskPinsIndependentVersionWithoutActivation(t *testing.T) {
	m := testManifest()
	text := renderCask(m)
	for _, value := range []string{`cask "cq-network" do`, `version "0.41.0"`, m.SHA256, m.url(), `app "CQNetwork.app"`, `depends_on macos: :ventura`, `args:       ["disable"]`, `launchctl: "dev.jacobcx.cq.network"`} {
		if !strings.Contains(text, value) {
			t.Fatalf("cask missing %q", value)
		}
	}
	for _, value := range []string{"installer script:", "postflight", "codesign --sign", "notarytool submit"} {
		if strings.Contains(text, value) {
			t.Fatalf("cask activates or signs during installation: %s", value)
		}
	}
}

func TestNativeCaskParsesWithHomebrew(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native companion cask is validated on macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is not installed")
	}
	file := filepath.Join(t.TempDir(), "cq-network.rb")
	if err := os.WriteFile(file, []byte(renderCask(testManifest())), 0o600); err != nil {
		t.Fatal(err)
	}
	// Load the real Homebrew DSL from content. This neither creates a tap nor
	// installs the companion or asks Homebrew to trust an external cask path.
	script := `cask = Cask::CaskLoader::FromContentLoader.new(File.read(ARGV.fetch(0))).load(config: nil)
requirement = cask.depends_on.macos
abort "wrong macOS requirement" unless requirement.comparator == ">=" && requirement.minimum_version == MacOSVersion.new("13.0")
uninstall = cask.artifacts.grep(Cask::Artifact::Uninstall).fetch(0)
abort "wrong uninstall directive order" unless uninstall.directives.keys.take(3) == [:launchctl, :script, :delete]`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, brew, "ruby", "-r", "cask/cask_loader", "-e", script, file)
	command.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Homebrew rejected generated cask: %v\n%s", err, output)
	}
}

func TestNativeArchiveRequiresHostAndProvider(t *testing.T) {
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "empty.zip")
	if err := os.WriteFile(file, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testManifest()
	digest := sha256.Sum256(data.Bytes())
	m.SHA256 = hex.EncodeToString(digest[:])
	if err := verifyArchive(m, file); err == nil {
		t.Fatal("archive without host and provider accepted")
	}
}
