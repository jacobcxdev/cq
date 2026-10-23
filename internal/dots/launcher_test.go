package dots

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLauncherRecoveryDoesNotRelaunchOrGuessUnknownAppState(t *testing.T) {
	queryError := errors.New("process query failed")
	openError := errors.New("open failed")
	for _, test := range []struct {
		name                  string
		running               bool
		queryError, openError error
		calls                 int
		want                  error
	}{
		{"running", true, nil, nil, 0, nil},
		{"unknown", false, queryError, nil, 0, queryError},
		{"missing", false, nil, nil, 1, nil},
		{"open-failed", false, nil, openError, 1, openError},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := recoverApp(test.running, test.queryError, func() error { calls++; return test.openError })
			if err != test.want || calls != test.calls {
				t.Fatalf("recovery err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestSpotlightLauncherUsesGoHelperAndModernIconAssets(t *testing.T) {
	options, _ := installationFixture(t)
	home := filepath.Dir(options.Root)
	options.Root = filepath.Join(home, "CQ with 'quoted' $(literal) paths")
	if err := os.WriteFile(options.Executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(options.App, "Contents", "Info.plist"), launcherPlist(map[string]string{"CFBundleIconFile": "original.icns", "CFBundleIconName": "Icon"}), 0o600); err != nil {
		t.Fatal(err)
	}
	asset := []byte("synthetic light and dark compiled icon assets")
	if err := os.WriteFile(filepath.Join(options.App, "Contents", "Resources", "Assets.car"), asset, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installLauncherAt(home, options.Root, options.App, options.Executable); err != nil {
		t.Fatal(err)
	}
	contents := filepath.Join(home, "Applications", "ChatGPT via CQ.app", "Contents")
	info, err := readAppPlist(filepath.Join(contents, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if info["CFBundleIdentifier"] != LauncherIdentifier || info["CQDotsRoot"] != options.Root || info["CFBundleIconName"] != "Icon" {
		t.Fatalf("launcher plist %#v", info)
	}
	data, err := os.ReadFile(filepath.Join(contents, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("<key>LSUIElement</key><true/>")) {
		t.Fatal("launcher has a visible Dock identity")
	}
	got, err := os.ReadFile(filepath.Join(contents, "Resources", "Assets.car"))
	if err != nil || !bytes.Equal(got, asset) {
		t.Fatal("modern icon assets not retained")
	}
	command := exec.Command(filepath.Join(contents, "MacOS", "launch"))
	command.Env = []string{"PATH=/usr/bin:/bin", "PYTHONHOME=/invalid"}
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "__dots\ndesktop-launch\n" + options.Root + "\n"
	if string(output) != want {
		t.Fatalf("helper argv %q want %q", output, want)
	}
	if err := installLauncherAt(home, options.Root, options.App, options.Executable); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "Preferences", "com.apple.dock.plist")); !os.IsNotExist(err) {
		t.Fatal("installer touched Dock preferences")
	}
}

func TestLauncherRejectsUnrelatedBundleAndSymlinkDestinations(t *testing.T) {
	options, _ := installationFixture(t)
	home := filepath.Dir(options.Root)
	if err := installLauncherAt(home, options.Root, options.App, options.Executable); err != nil {
		t.Fatal(err)
	}
	contents := filepath.Join(home, "Applications", "ChatGPT via CQ.app", "Contents")
	path := filepath.Join(contents, "Info.plist")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, values := range []map[string]string{{"CFBundleIdentifier": "another.app", "CQDotsRoot": options.Root}, {"CFBundleIdentifier": LauncherIdentifier, "CQDotsRoot": "/another/root"}} {
		if err := os.WriteFile(path, launcherPlist(values), 0o600); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)
		if err := installLauncherAt(home, options.Root, options.App, options.Executable); err == nil || !strings.Contains(err.Error(), "another app") {
			t.Fatalf("unrelated launcher accepted: %v", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("unrelated launcher overwritten")
		}
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(contents, "MacOS", "launch")
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(home, "victim")
	if err := os.WriteFile(victim, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, executable); err != nil {
		t.Fatal(err)
	}
	if err := installLauncherAt(home, options.Root, options.App, options.Executable); err == nil {
		t.Fatal("symlink destination accepted")
	}
	data, _ := os.ReadFile(victim)
	if string(data) != "unchanged" {
		t.Fatal("symlink target changed")
	}
}

func TestLauncherRejectsAliasDirectoryUnsafeIconAndSharedFiles(t *testing.T) {
	for _, variant := range []string{"alias", "icon", "shared"} {
		t.Run(variant, func(t *testing.T) {
			options, _ := installationFixture(t)
			home := filepath.Dir(options.Root)
			switch variant {
			case "alias":
				if err := os.Symlink(t.TempDir(), filepath.Join(home, "Applications")); err != nil {
					t.Fatal(err)
				}
			case "icon":
				if err := os.WriteFile(filepath.Join(options.App, "Contents", "Info.plist"), launcherPlist(map[string]string{"CFBundleIconFile": "../victim"}), 0o600); err != nil {
					t.Fatal(err)
				}
			case "shared":
				if err := installLauncherAt(home, options.Root, options.App, options.Executable); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(home, "Applications", "ChatGPT via CQ.app", "Contents", "MacOS", "launch"), 0o777); err != nil {
					t.Fatal(err)
				}
			}
			if err := installLauncherAt(home, options.Root, options.App, options.Executable); err == nil {
				t.Fatal("unsafe launcher accepted")
			}
		})
	}
}

func TestReadAppPlistKeepsTopLevelScalarsAndEscaping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Info.plist")
	data := []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>Nested</key><dict><key>CFBundleVersion</key><string>wrong</string></dict><key>CFBundleVersion</key><integer>42</integer><key>Name</key><string>Tom &amp; Jerry</string></dict></plist>`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := readAppPlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["CFBundleVersion"] != "42" || values["Name"] != "Tom & Jerry" {
		t.Fatalf("plist values %#v", values)
	}
	if err := os.WriteFile(path, launcherPlist(values), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err = readAppPlist(path)
	if err != nil || values["Name"] != "Tom & Jerry" {
		t.Fatalf("plist round trip %#v %v", values, err)
	}
}
