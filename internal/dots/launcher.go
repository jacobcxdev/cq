package dots

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const LauncherIdentifier = "dev.jacobcx.cq.chatgpt-launcher"

const desktopFallbackMessage = "CQ could not enable Dots. ChatGPT may be running without Dots or CQ routing. Your existing chats have not been restarted. Run 'cq codex dots status' for details. When ready, quit ChatGPT and reopen ChatGPT via CQ."

const desktopFallbackAlert = `display alert "CQ Dots unavailable" message "` + desktopFallbackMessage + `" as warning buttons {"OK"} default button "OK"`

// Spotlight launches have no terminal for stderr. Show a native warning without
// delaying recovery, tying it to a cancelled startup context, or opening a shell.
func notifyDesktopFallback() {
	command := exec.Command("/usr/bin/osascript", "-e", desktopFallbackAlert)
	if err := command.Start(); err != nil {
		fmt.Fprintln(os.Stderr, desktopFallbackMessage)
		return
	}
	go func() {
		defer func() { _ = recover() }()
		if err := command.Wait(); err != nil {
			fmt.Fprintln(os.Stderr, desktopFallbackMessage)
		}
	}()
}

// readAppPlist extracts top-level scalar values. macOS plutil only converts an
// existing binary plist to stdout; no application files are rewritten.
func readAppPlist(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := httputil.ReadBody(file)
	file.Close()
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(data, []byte("bplist")) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		data, err = exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "xml1", "-o", "-", path).Output()
		if err != nil || len(data) > 1<<20 {
			return nil, fmt.Errorf("Native app property list unavailable")
		}
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	result := map[string]string{}
	depth := 0
	key := ""
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Native app property list invalid")
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if depth == 3 && value.Name.Local == "key" {
				if err := decoder.DecodeElement(&key, &value); err != nil {
					return nil, err
				}
				depth--
				continue
			}
			if depth == 3 && key != "" && (value.Name.Local == "string" || value.Name.Local == "integer") {
				var scalar string
				if err := decoder.DecodeElement(&scalar, &value); err != nil {
					return nil, err
				}
				depth--
				result[key] = scalar
				key = ""
			}
		case xml.EndElement:
			depth--
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("Native app property list invalid")
	}
	return result, nil
}

func launcherPlist(values map[string]string) []byte {
	var output bytes.Buffer
	output.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n")
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		output.WriteString("<key>")
		xml.EscapeText(&output, []byte(key))
		output.WriteString("</key><string>")
		xml.EscapeText(&output, []byte(values[key]))
		output.WriteString("</string>\n")
	}
	output.WriteString("<key>LSUIElement</key><true/>\n</dict></plist>\n")
	return output.Bytes()
}

func launcherOwned(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !owned(info) || info.Mode().Perm()&0o022 != 0 || directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("Launcher paths must be owned, without symlinks or shared writes")
	}
	return nil
}

func launcherWrite(path string, data []byte, mode os.FileMode) error {
	if _, err := os.Lstat(path); err == nil {
		if err := launcherOwned(path, false); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".cq-launcher-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func launcherResource(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return nil, fmt.Errorf("Native app icon is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return httputil.ReadBodyLimit(file, 64<<20)
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// InstallLauncher registers a quiet Spotlight/Raycast entry point. It never
// changes Dock preferences or copies/patches the signed native executable.
func InstallLauncher(root, app, executable string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return installLauncherAt(home, root, app, executable)
}

func installLauncherAt(home, root, app, executable string) error {
	if !cleanAbsolute(root) || !cleanAbsolute(app) || !cleanAbsolute(executable) {
		return fmt.Errorf("Launcher requires absolute installation and executable paths")
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("Launcher requires the installed CQ executable")
	}
	applications := filepath.Join(home, "Applications")
	bundle := filepath.Join(applications, "ChatGPT via CQ.app")
	contents := filepath.Join(bundle, "Contents")
	directories := []string{applications, bundle, contents, filepath.Join(contents, "MacOS"), filepath.Join(contents, "Resources")}
	files := []string{filepath.Join(contents, "Info.plist"), filepath.Join(contents, "MacOS", "launch"), filepath.Join(contents, "Resources", "ChatGPT.icns"), filepath.Join(contents, "Resources", "Assets.car")}
	for _, path := range directories {
		if _, err := os.Lstat(path); err == nil {
			if err := launcherOwned(path, true); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	for _, path := range files {
		if _, err := os.Lstat(path); err == nil {
			if err := launcherOwned(path, false); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if _, err := os.Lstat(bundle); err == nil {
		previous, err := readAppPlist(files[0])
		if err != nil {
			return err
		}
		if previous["CFBundleIdentifier"] != LauncherIdentifier || previous["CQDotsRoot"] != root {
			return fmt.Errorf("Launcher destination belongs to another app or installation")
		}
	}
	original, err := readAppPlist(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return err
	}
	icon := original["CFBundleIconFile"]
	if icon == "" || filepath.Base(icon) != icon || strings.ContainsAny(icon, "/\\") {
		return fmt.Errorf("Native app icon path is invalid")
	}
	if !strings.HasSuffix(icon, ".icns") {
		icon += ".icns"
	}
	iconData, err := launcherResource(filepath.Join(app, "Contents", "Resources", icon))
	if err != nil {
		return err
	}
	values := map[string]string{"CFBundleIdentifier": LauncherIdentifier, "CFBundleName": "ChatGPT via CQ", "CFBundleDisplayName": "ChatGPT via CQ", "CFBundlePackageType": "APPL", "CFBundleExecutable": "launch", "CFBundleIconFile": "ChatGPT.icns", "CFBundleVersion": "2", "CFBundleShortVersionString": "2.0", "CQDotsRoot": root}
	var assets []byte
	if iconName := original["CFBundleIconName"]; iconName != "" {
		assets, err = launcherResource(filepath.Join(app, "Contents", "Resources", "Assets.car"))
		if err != nil {
			return err
		}
		values["CFBundleIconName"] = iconName
	}
	for _, path := range directories {
		if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	if err := launcherWrite(files[2], iconData, 0o600); err != nil {
		return err
	}
	if assets != nil {
		if err := launcherWrite(files[3], assets, 0o600); err != nil {
			return err
		}
	}
	script := "#!/bin/sh\nexec " + shellQuote(executable) + " __dots desktop-launch " + shellQuote(root) + "\n"
	if err := launcherWrite(files[1], []byte(script), 0o700); err != nil {
		return err
	}
	return launcherWrite(files[0], launcherPlist(values), 0o600)
}

// RecoverApp reopens the official app only when no ordinary-profile process is
// running. It never kills an app or touches its profile/configuration.
func RecoverApp(ctx context.Context, settings *Settings) error {
	running, err := NormalAppRunning(ctx, settings)
	return recoverApp(running, err, func() error {
		query, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := exec.CommandContext(query, "/usr/bin/open", "-a", settings.AppPath).Run(); err != nil {
			return fmt.Errorf("Native app recovery failed")
		}
		return nil
	}, notifyDesktopFallback)
}

func recoverApp(running bool, err error, open func() error, notify func()) error {
	// This boundary covers immediate failure, missing configuration before the
	// watchdog starts, and detached recovery after the launcher itself exits.
	defer notify()
	if err != nil {
		return err
	}
	if running {
		return nil
	}
	return open()
}
