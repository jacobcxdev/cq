//go:build windows

package dots

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jacobcxdev/cq/internal/winpackage"
	"golang.org/x/sys/windows"
)

const windowsDotsPublisher = "CN=50BDFD77-8903-4850-9FFE-6E8522F64D5B"

var windowsDotsVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)

// A Store package can be traversable even when WindowsApps cannot be listed.
// Protect its verified parent, including when settings pin a retired version.
func windowsRegisteredAppBoundary(path string, canonicalErr error) (string, error) {
	if !errors.Is(canonicalErr, windows.ERROR_ACCESS_DENIED) || !winpackage.OfficialCodexPackage(filepath.Base(path)) {
		return "", canonicalErr
	}
	registered, err := winpackage.RegisteredCodexRoot()
	parent := filepath.Dir(path)
	if err != nil || !strings.EqualFold(filepath.Dir(registered), parent) {
		return "", canonicalErr
	}
	if err := winpackage.CheckPath(filepath.Join(registered, "AppxManifest.xml")); err != nil {
		return "", fmt.Errorf("validate registered Codex package path: %w", err)
	}
	return parent, nil
}

type windowsAppIdentity struct {
	version, build, executable, archive, logo string
}

// windowsAppIdentityAt validates the package layout supplied by Windows package
// registration. The caller must resolve that registration and reject reparse
// points before using the executable for a launch.
func windowsAppIdentityAt(root string) (windowsAppIdentity, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return windowsAppIdentity{}, errors.New("Windows Dots package root must be clean and absolute")
	}
	path := filepath.Join(root, "AppxManifest.xml")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1<<20 {
		return windowsAppIdentity{}, errors.New("Windows Dots package manifest unavailable or invalid")
	}
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) > 1<<20 {
		return windowsAppIdentity{}, errors.New("Windows Dots package manifest unavailable or invalid")
	}
	var manifest struct {
		Identity struct {
			Name         string `xml:"Name,attr"`
			Architecture string `xml:"ProcessorArchitecture,attr"`
			Version      string `xml:"Version,attr"`
			Publisher    string `xml:"Publisher,attr"`
		} `xml:"Identity"`
		Applications []struct {
			ID             string `xml:"Id,attr"`
			Executable     string `xml:"Executable,attr"`
			EntryPoint     string `xml:"EntryPoint,attr"`
			VisualElements struct {
				Logo string `xml:"Square44x44Logo,attr"`
			} `xml:"VisualElements"`
		} `xml:"Applications>Application"`
	}
	if xml.Unmarshal(data, &manifest) != nil || manifest.Identity.Name != "OpenAI.Codex" || manifest.Identity.Publisher != windowsDotsPublisher || !windowsDotsVersion.MatchString(manifest.Identity.Version) {
		return windowsAppIdentity{}, errors.New("Windows Dots package identity invalid")
	}
	arch := manifest.Identity.Architecture
	if arch != "x64" && arch != "arm64" {
		return windowsAppIdentity{}, errors.New("Windows Dots package architecture unsupported")
	}
	fullName := fmt.Sprintf("OpenAI.Codex_%s_%s__2p2nqsd0c76g0", manifest.Identity.Version, arch)
	if !strings.EqualFold(filepath.Base(root), fullName) {
		return windowsAppIdentity{}, errors.New("Windows Dots package name does not match its manifest")
	}
	var appCount int
	var logo string
	for _, app := range manifest.Applications {
		if app.ID == "App" {
			appCount++
			if app.Executable != "app/ChatGPT.exe" || app.EntryPoint != "Windows.FullTrustApplication" {
				return windowsAppIdentity{}, errors.New("Windows Dots desktop entry point changed")
			}
			logo = app.VisualElements.Logo
		}
	}
	if appCount != 1 {
		return windowsAppIdentity{}, errors.New("Windows Dots desktop entry point missing or ambiguous")
	}
	executable := filepath.Join(root, "app", "ChatGPT.exe")
	archive := filepath.Join(root, "app", "resources", "app.asar")
	for _, path := range []string{executable, archive} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return windowsAppIdentity{}, errors.New("Windows Dots package executable or renderer unavailable")
		}
	}
	parts := strings.Split(manifest.Identity.Version, ".")
	return windowsAppIdentity{version: manifest.Identity.Version, build: parts[2], executable: executable, archive: archive, logo: logo}, nil
}

// QualifyWindowsApp pins the installed MSIX renderer by digest and discovers
// its semantic bindings. This read-only check does not start Dots or the app.
func QualifyWindowsApp(root string) (Settings, string, error) {
	app, err := windowsAppIdentityAt(root)
	if err != nil {
		return Settings{}, "", err
	}
	digest, err := nativeArchiveDigest(app.archive)
	if err != nil {
		return Settings{}, "", err
	}
	asset, bindings, err := discoverNativeBindings(app.archive)
	if err != nil {
		return Settings{}, "", fmt.Errorf("Windows Dots renderer compatibility check failed: %w", err)
	}
	return Settings{AppPath: root, AppVersion: app.version, AppBuild: app.build, ASARSHA256: digest, ModuleAsset: asset, NativeBindings: bindings}, app.executable, nil
}

// CheckWindowsApp confirms that the package manifest and renderer still match
// a previously qualified snapshot.
func CheckWindowsApp(settings *Settings) error {
	if settings == nil {
		return errors.New("Windows Dots app pin unavailable")
	}
	app, err := windowsAppIdentityAt(settings.AppPath)
	if err != nil {
		return err
	}
	if app.version != settings.AppVersion || app.build != settings.AppBuild {
		return errors.New("Windows Dots package version differs from configured pin")
	}
	digest, err := nativeArchiveDigest(app.archive)
	if err != nil {
		return err
	}
	if digest != settings.ASARSHA256 {
		return errors.New("Windows Dots renderer differs from configured pin")
	}
	return nil
}

// ResolveRegisteredWindowsApp qualifies the current user's official package
// without opening a Desktop profile or launching a process.
func ResolveRegisteredWindowsApp() (Settings, string, error) {
	root, err := winpackage.RegisteredCodexRoot()
	if err != nil {
		return Settings{}, "", err
	}
	for _, path := range []string{
		filepath.Join(root, "AppxManifest.xml"),
		filepath.Join(root, "app", "ChatGPT.exe"),
		filepath.Join(root, "app", "resources", "app.asar"),
	} {
		if err := winpackage.CheckPath(path); err != nil {
			return Settings{}, "", err
		}
	}
	return QualifyWindowsApp(root)
}
