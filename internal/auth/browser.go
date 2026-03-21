package auth

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var validBrowserName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func openBrowser(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("invalid URL scheme for browser open: %q", rawURL)
	}
	switch runtime.GOOS {
	case "darwin":
		return openBrowserDarwin(rawURL)
	case "linux":
		return openBrowserLinux(rawURL)
	case "windows":
		return openBrowserWindows(rawURL)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// macOS: detect default browser via Launch Services plist, open in private mode.
// --args only works on fresh launch; when the browser is already running, macOS
// ignores extra args and just sends the URL via Apple Events.
func openBrowserDarwin(rawURL string) error {
	bundleID := defaultBrowserBundleID()
	if bundleID == "" || bundleID == "com.apple.safari" || !validBrowserName.MatchString(bundleID) {
		return startAndReap(exec.Command("open", rawURL))
	}

	flag := "--incognito"
	if strings.Contains(bundleID, "firefox") {
		flag = "--private-window"
	}

	// Check if browser is already running — --args is only effective on fresh launch.
	// SECURITY: bundleID is validated by validBrowserName regex above — safe for AppleScript interpolation.
	out, _ := exec.Command("osascript", "-e",
		fmt.Sprintf(`application id "%s" is running`, bundleID)).Output()
	running := strings.TrimSpace(string(out)) == "true"

	if !running {
		if startAndReap(exec.Command("open", "-b", bundleID, "--args", flag, rawURL)) == nil {
			return nil
		}
	}

	// Browser already running: copy URL to clipboard for manual paste into private window
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(rawURL)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Browser already running \u2014 open this URL in a private window:\n  %s\n", rawURL)
	} else {
		fmt.Println("Browser already running \u2014 URL copied to clipboard. Paste in a private window.")
	}
	return nil
}

func defaultBrowserBundleID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	plistPath := filepath.Join(home, "Library", "Preferences",
		"com.apple.LaunchServices", "com.apple.launchservices.secure.plist")
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", plistPath).Output()
	if err != nil {
		return ""
	}
	var plist struct {
		LSHandlers []struct {
			LSHandlerURLScheme string `json:"LSHandlerURLScheme"`
			LSHandlerRoleAll   string `json:"LSHandlerRoleAll"`
		} `json:"LSHandlers"`
	}
	if json.Unmarshal(out, &plist) != nil {
		return ""
	}
	for _, h := range plist.LSHandlers {
		if strings.EqualFold(h.LSHandlerURLScheme, "https") {
			return h.LSHandlerRoleAll
		}
	}
	return ""
}

// Linux: detect default browser via xdg-settings, open in private mode.
func openBrowserLinux(rawURL string) error {
	out, _ := exec.Command("xdg-settings", "get", "default-web-browser").Output()
	name := strings.TrimSuffix(strings.TrimSpace(string(out)), ".desktop")
	if name != "" && !validBrowserName.MatchString(name) {
		name = "" // reject suspicious names
	}
	switch {
	case strings.Contains(name, "firefox"):
		if startAndReap(exec.Command(name, "--private-window", rawURL)) == nil {
			return nil
		}
	case name != "":
		// Chromium-based: binary name typically matches .desktop file stem
		if startAndReap(exec.Command(name, "--incognito", rawURL)) == nil {
			return nil
		}
	}
	return startAndReap(exec.Command("xdg-open", rawURL))
}

// Windows: detect default browser via registry, open in private mode.
func openBrowserWindows(rawURL string) error {
	out, _ := exec.Command("reg", "query",
		`HKEY_CURRENT_USER\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice`,
		"/v", "ProgId").Output()
	progID := parseRegValue(string(out))
	if !validBrowserName.MatchString(progID) {
		progID = ""
	}
	if progID != "" {
		cmdOut, _ := exec.Command("reg", "query",
			fmt.Sprintf(`HKEY_CLASSES_ROOT\%s\shell\open\command`, progID),
			"/ve").Output()
		if browserPath := parseBrowserPath(string(cmdOut)); browserPath != "" {
			flag := "--incognito"
			if strings.Contains(strings.ToLower(progID), "firefox") {
				flag = "--private-window"
			}
			if startAndReap(exec.Command(browserPath, flag, rawURL)) == nil {
				return nil
			}
		}
	}
	return startAndReap(exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL))
}

// startAndReap starts a command and spawns a goroutine to reap the child process,
// preventing zombie entries in the process table while the CLI waits for OAuth.
func startAndReap(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func parseRegValue(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "REG_SZ") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				return parts[len(parts)-1]
			}
		}
	}
	return ""
}

func parseBrowserPath(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "REG_SZ") {
			continue
		}
		idx := strings.Index(line, "REG_SZ")
		if idx == -1 {
			continue
		}
		rest := strings.TrimSpace(line[idx+len("REG_SZ"):])
		if strings.HasPrefix(rest, `"`) {
			if end := strings.Index(rest[1:], `"`); end != -1 {
				return rest[1 : end+1]
			}
		}
		if parts := strings.Fields(rest); len(parts) > 0 {
			return parts[0]
		}
	}
	return ""
}
