package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var validBrowserName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// OpenBrowser opens a validated login URL using CQ's existing browser policy.
func OpenBrowser(rawURL string) error { return openBrowser(rawURL) }

func openBrowser(rawURL string) error {
	return OpenBrowserContext(context.Background(), rawURL)
}

// OpenBrowserContext bounds browser detection and launch preparation. Once a
// browser starts, its lifetime is independent of the login or verification.
func OpenBrowserContext(ctx context.Context, rawURL string) error {
	return openBrowserContext(ctx, rawURL, runtime.GOOS, exec.CommandContext)
}

type browserCommandFactory func(context.Context, string, ...string) *exec.Cmd

func openBrowserContext(ctx context.Context, rawURL, platform string, command browserCommandFactory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("invalid URL scheme for browser open: %q", rawURL)
	}
	switch platform {
	case "darwin":
		return openBrowserDarwin(ctx, rawURL, command)
	case "linux":
		return openBrowserLinux(ctx, rawURL, command)
	case "windows":
		return openBrowserWindows(ctx, rawURL, command)
	default:
		return fmt.Errorf("unsupported OS: %s", platform)
	}
}

// macOS: detect default browser via Launch Services plist, open in private mode.
// --args only works on fresh launch; when the browser is already running, macOS
// ignores extra args and just sends the URL via Apple Events.
func openBrowserDarwin(ctx context.Context, rawURL string, command browserCommandFactory) error {
	bundleID := defaultBrowserBundleID(ctx, command)
	if bundleID == "" || bundleID == "com.apple.safari" || !validBrowserName.MatchString(bundleID) {
		return startBrowser(ctx, command, "open", rawURL)
	}

	flag := "--incognito"
	if strings.Contains(bundleID, "firefox") {
		flag = "--private-window"
	}

	// Check if browser is already running — --args is only effective on fresh launch.
	// SECURITY: bundleID is validated by validBrowserName regex above — safe for AppleScript interpolation.
	out, _ := browserCommandOutput(ctx, command, "osascript", "-e",
		fmt.Sprintf(`application id "%s" is running`, bundleID))
	if err := ctx.Err(); err != nil {
		return err
	}
	running := strings.TrimSpace(string(out)) == "true"

	if !running {
		if startBrowser(ctx, command, "open", "-b", bundleID, "--args", flag, rawURL) == nil {
			return nil
		}
	}

	// Browser already running: copy URL to clipboard for manual paste into private window
	cmd := command(ctx, "pbcopy")
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader(rawURL)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Printf("Browser already running \u2014 open this URL in a private window:\n  %s\n", rawURL)
	} else {
		fmt.Println("Browser already running \u2014 URL copied to clipboard. Paste in a private window.")
	}
	return nil
}

func defaultBrowserBundleID(ctx context.Context, command browserCommandFactory) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	plistPath := filepath.Join(home, "Library", "Preferences",
		"com.apple.LaunchServices", "com.apple.launchservices.secure.plist")
	out, err := browserCommandOutput(ctx, command, "plutil", "-convert", "json", "-o", "-", plistPath)
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
func openBrowserLinux(ctx context.Context, rawURL string, command browserCommandFactory) error {
	out, _ := browserCommandOutput(ctx, command, "xdg-settings", "get", "default-web-browser")
	if err := ctx.Err(); err != nil {
		return err
	}
	name := strings.TrimSuffix(strings.TrimSpace(string(out)), ".desktop")
	if name != "" && !validBrowserName.MatchString(name) {
		name = "" // reject suspicious names
	}
	switch {
	case strings.Contains(name, "firefox"):
		if startBrowser(ctx, command, name, "--private-window", rawURL) == nil {
			return nil
		}
	case name != "":
		// Chromium-based: binary name typically matches .desktop file stem
		if startBrowser(ctx, command, name, "--incognito", rawURL) == nil {
			return nil
		}
	}
	return startBrowser(ctx, command, "xdg-open", rawURL)
}

// Windows: detect default browser via registry, open in private mode.
func openBrowserWindows(ctx context.Context, rawURL string, command browserCommandFactory) error {
	out, _ := browserCommandOutput(ctx, command, "reg", "query",
		`HKEY_CURRENT_USER\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice`,
		"/v", "ProgId")
	if err := ctx.Err(); err != nil {
		return err
	}
	progID := parseRegValue(string(out))
	if !validBrowserName.MatchString(progID) {
		progID = ""
	}
	if progID != "" {
		cmdOut, _ := browserCommandOutput(ctx, command, "reg", "query",
			fmt.Sprintf(`HKEY_CLASSES_ROOT\%s\shell\open\command`, progID),
			"/ve")
		if err := ctx.Err(); err != nil {
			return err
		}
		if browserPath := parseBrowserPath(string(cmdOut)); browserPath != "" {
			flag := "--incognito"
			if strings.Contains(strings.ToLower(progID), "firefox") {
				flag = "--private-window"
			}
			if startBrowser(ctx, command, browserPath, flag, rawURL) == nil {
				return nil
			}
		}
	}
	return startBrowser(ctx, command, "rundll32", "url.dll,FileProtocolHandler", rawURL)
}

func browserCommandOutput(ctx context.Context, command browserCommandFactory, name string, args ...string) ([]byte, error) {
	cmd := command(ctx, name, args...)
	// A detection helper's descendant may retain an output pipe after cancellation.
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

func startBrowser(ctx context.Context, command browserCommandFactory, name string, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return startAndReap(command(context.Background(), name, args...))
}

// startAndReap starts a command and spawns a goroutine to reap the child process,
// preventing zombie entries in the process table while the CLI waits for OAuth.
func startAndReap(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		defer func() { _ = recover() }()
		_ = cmd.Wait()
	}()
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
