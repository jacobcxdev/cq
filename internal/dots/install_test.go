package dots

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func installationFixture(t *testing.T) (InstallOptions, Settings) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	options := InstallOptions{Root: filepath.Join(base, "dots"), App: filepath.Join(base, "Original.app"), SystemAuth: filepath.Join(base, "credentials", "auth.json"), CQAccounts: filepath.Join(base, "accounts"), Mode: "native", Executable: filepath.Join(base, "cq")}
	if err := os.MkdirAll(filepath.Dir(options.SystemAuth), 0o700); err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "fixture-owner"}})
	document, _ := json.Marshal(map[string]any{"tokens": map[string]string{"account_id": "fixture-owner", "id_token": "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"}})
	if err := os.WriteFile(options.SystemAuth, document, 0o600); err != nil {
		t.Fatal(err)
	}
	resources := filepath.Join(options.App, "Contents", "Resources")
	if err := os.MkdirAll(resources, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resources, "original.icns"), []byte("synthetic-icon"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(options.App, "Contents", "Info.plist"), launcherPlist(map[string]string{"CFBundleShortVersionString": "fixture", "CFBundleVersion": "42", "CFBundleIconFile": "original.icns"}), 0o600); err != nil {
		t.Fatal(err)
	}
	asar := []byte("original signed application fixture")
	digest := sha256.Sum256(asar)
	if err := os.WriteFile(filepath.Join(resources, "app.asar"), asar, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.Executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return options, Settings{AppPath: options.App, AppVersion: "fixture", AppBuild: "42", ASARSHA256: hex.EncodeToString(digest[:]), ModuleAsset: QualifiedModuleAsset}
}

func TestProvisionFreshNativeUsesGoCertificatesOnly(t *testing.T) {
	root := t.TempDir()
	runtimeRoot, err := Provision(root, "native")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"venv", "user-data", "codex-home", "null-executor"} {
		if _, err := os.Stat(filepath.Join(runtimeRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected secondary profile/dependency %s: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(runtimeRoot, "leaf-cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("missing certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.IsCA || !cert.BasicConstraintsValid {
		t.Fatal("certificate must be server leaf")
	}
	for _, host := range []string{"localhost", "127.0.0.1", "realtime.chatgpt.com", "ws.chatgpt.com"} {
		if err := cert.VerifyHostname(host); err != nil {
			t.Fatal(err)
		}
	}
	key, err := os.ReadFile(filepath.Join(runtimeRoot, "leaf-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(data, key); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"leaf-key.pem", "leaf-cert.pem", "probe-cert.pem", "realtime-cert.pem", "port"} {
		info, err := os.Stat(filepath.Join(runtimeRoot, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private file %s: %v", name, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(runtimeRoot, "port")); string(got) != "8000" {
		t.Fatalf("port %q", got)
	}
}

func TestProvisionPreservesCompleteLegacyStateAndRejectsPartialSet(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, name := range []string{"leaf-key.pem", "leaf-cert.pem", "probe-cert.pem", "realtime-cert.pem", "port", "venv-marker", "settings.py"} {
		data := []byte("retained " + name)
		before[name] = data
		if err := os.WriteFile(filepath.Join(runtimeRoot, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Provision(root, "native"); err != nil {
		t.Fatal(err)
	}
	for name, data := range before {
		got, err := os.ReadFile(filepath.Join(runtimeRoot, name))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("legacy state changed: %s", name)
		}
	}
	if err := os.Remove(filepath.Join(runtimeRoot, "leaf-cert.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := Provision(root, "native"); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("partial certificates accepted: %v", err)
	}
}

func TestProvisionRejectsSymlinkAndSharedCertificates(t *testing.T) {
	for _, variant := range []string{"directory", "certificate", "port"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			runtimeRoot := filepath.Join(root, "runtime")
			if variant == "directory" {
				if err := os.Symlink(t.TempDir(), runtimeRoot); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"leaf-key.pem", "leaf-cert.pem", "probe-cert.pem", "realtime-cert.pem"} {
					if err := os.WriteFile(filepath.Join(runtimeRoot, name), []byte("retained"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if variant == "certificate" {
					if err := os.Chmod(filepath.Join(runtimeRoot, "leaf-key.pem"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Symlink(filepath.Join(runtimeRoot, "leaf-cert.pem"), filepath.Join(runtimeRoot, "port")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := Provision(root, "native"); err == nil {
				t.Fatal("unsafe state accepted")
			}
		})
	}
}

func TestFreshInstallAndLegacyReinstallPreserveIdentityAndAwareness(t *testing.T) {
	options, pin := installationFixture(t)
	if err := AppCheck(&pin); err != nil {
		t.Fatal(err)
	}
	result, err := installPrepared(context.Background(), options, pin)
	if err != nil {
		t.Fatal(err)
	}
	if result["installed"] != true || result["system_app_modified"] != false {
		t.Fatalf("result %#v", result)
	}
	settingsPath := filepath.Join(options.Root, "settings.json")
	settings, err := LoadSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	settings.AccountSource = filepath.Join(filepath.Dir(options.Root), "declared-accounts")
	handle := StableHandle(settings.SystemAccountID, "fixture-profile")
	settings.Dots[handle] = Dot{OwnerAlias: settings.SystemAlias, OwnerAccountID: settings.SystemAccountID, ProfileID: "fixture-profile", RootThreadID: "fixture-root", DisplayName: "Same name"}
	if err := installSave(settingsPath, settings); err != nil {
		t.Fatal(err)
	}
	legacy := map[string][]byte{}
	for _, name := range []string{"runtime/leaf-key.pem", "runtime/leaf-cert.pem", "runtime/probe-cert.pem", "runtime/realtime-cert.pem"} {
		data, err := os.ReadFile(filepath.Join(options.Root, name))
		if err != nil {
			t.Fatal(err)
		}
		legacy[name] = data
	}
	for _, name := range []string{"runtime/awareness/setup.json", "runtime/board.json", "payload/cli.py", "candidate/manifest.json"} {
		path := filepath.Join(options.Root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		data := []byte("legacy " + name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		legacy[name] = data
	}
	if _, err := installPrepared(context.Background(), options, pin); err != nil {
		t.Fatal(err)
	}
	updated, err := LoadSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccountSource != settings.AccountSource || updated.Dots[handle] != settings.Dots[handle] {
		t.Fatal("declared source or stable profile changed")
	}
	for name, data := range legacy {
		got, err := os.ReadFile(filepath.Join(options.Root, name))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("legacy state changed: %s", name)
		}
	}
	if err := AppCheck(&pin); err != nil {
		t.Fatal("original app changed", err)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("signature")) {
		t.Fatal("credentials persisted")
	}
}

func TestInstallationRejectsProtectedRootsAndAccountReplacementBeforeWrites(t *testing.T) {
	options, pin := installationFixture(t)
	for _, root := range []string{filepath.Dir(options.SystemAuth), filepath.Join(filepath.Dir(options.SystemAuth), "nested"), filepath.Dir(options.Root)} {
		options.Root = root
		if _, err := ValidateInstallLocation(options); err == nil {
			t.Fatalf("protected root accepted: %s", root)
		}
	}
	options.Root = filepath.Join(filepath.Dir(options.App), "install")
	if _, err := installPrepared(context.Background(), options, pin); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(options.Root, "settings.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(before, &document); err != nil {
		t.Fatal(err)
	}
	document["system_account_id"] = "another"
	document["system_alias"] = AccountAlias("another")
	if err := installSave(path, document); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(path)
	if _, err := installPrepared(context.Background(), options, pin); err == nil {
		t.Fatal("system account replacement accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed reinstall changed settings")
	}
}

func TestAppCheckRejectsVersionAndRendererChanges(t *testing.T) {
	options, pin := installationFixture(t)
	pin.AppBuild = "another"
	if err := AppCheck(&pin); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("version accepted: %v", err)
	}
	pin.AppBuild = "42"
	if err := os.WriteFile(filepath.Join(options.App, "Contents", "Resources", "app.asar"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppCheck(&pin); err == nil || !strings.Contains(err.Error(), "renderer") {
		t.Fatalf("renderer accepted: %v", err)
	}
}

func TestOperationLockExclusionWaitAndCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := OperationLock(context.Background(), root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OperationLock(context.Background(), root, 0); err == nil || !strings.Contains(err.Error(), "progress") {
		t.Fatalf("concurrent operation accepted: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := OperationLock(ctx, root, time.Second); err != context.DeadlineExceeded {
		t.Fatalf("cancelled lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := OperationLock(context.Background(), root, 0)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
