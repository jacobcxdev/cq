package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- parseRegValue ---

func TestParseRegValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "typical registry output with REG_SZ",
			input: "HKEY_CURRENT_USER\\Software\\...\\UserChoice\r\n" +
				"    ProgId    REG_SZ    ChromeHTML\r\n",
			want: "ChromeHTML",
		},
		{
			name:  "no REG_SZ line",
			input: "HKEY_CURRENT_USER\\Software\\...\\UserChoice\r\n    ProgId    REG_DWORD    0x1\r\n",
			want:  "",
		},
		{
			name: "multiple lines, only one with REG_SZ",
			input: "HKEY_CURRENT_USER\\Software\\...\r\n" +
				"    OtherKey    REG_DWORD    0x0\r\n" +
				"    ProgId    REG_SZ    FirefoxURL-308046B0AF4A39CB\r\n" +
				"    AnotherKey    REG_DWORD    0x1\r\n",
			want: "FirefoxURL-308046B0AF4A39CB",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRegValue(tc.input)
			if got != tc.want {
				t.Errorf("parseRegValue() = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- parseBrowserPath ---

func TestParseBrowserPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "quoted path",
			input: "HKEY_CLASSES_ROOT\\ChromeHTML\\shell\\open\\command\r\n" +
				`    (Default)    REG_SZ    "C:\Program Files\Google\Chrome\Application\chrome.exe" -- "%1"` + "\r\n",
			want: `C:\Program Files\Google\Chrome\Application\chrome.exe`,
		},
		{
			name: "unquoted path",
			input: "HKEY_CLASSES_ROOT\\SomeBrowser\\shell\\open\\command\r\n" +
				`    (Default)    REG_SZ    C:\browser.exe --flag "%1"` + "\r\n",
			want: `C:\browser.exe`,
		},
		{
			name:  "no REG_SZ line",
			input: "HKEY_CLASSES_ROOT\\SomeBrowser\\shell\\open\\command\r\n    (Default)    REG_DWORD    0x0\r\n",
			want:  "",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBrowserPath(tc.input)
			if got != tc.want {
				t.Errorf("parseBrowserPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCanonicalBrowserClipboardPresentation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin browser dispatch")
	}
	for _, mode := range []string{"success", "failure", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			t.Setenv("HOME", dir)
			scripts := map[string]string{
				"plutil":    `printf '%s' '{"LSHandlers":[{"LSHandlerURLScheme":"https","LSHandlerRoleAll":"com.google.chrome"}]}'`,
				"osascript": "printf true",
				"pbcopy":    "while IFS= read -r line; do :; done; exit 0",
				"open":      "exit 99",
			}
			if mode == "failure" {
				scripts["pbcopy"] = "exit 1"
			}
			for name, body := range scripts {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			var out bytes.Buffer
			err := OpenBrowserContextTo(ctx, "https://example.test/oauth?state=private-state", &out)
			if strings.Contains(out.String(), "private-state") || strings.Contains(out.String(), "https:") {
				t.Fatal("OAuth URL exposed")
			}
			switch mode {
			case "success":
				if err != nil || !strings.Contains(out.String(), "Paste in a private window") {
					t.Fatalf("manual browser step missing: %q error=%v", out.String(), err)
				}
			case "failure":
				if err == nil {
					t.Fatal("clipboard failure silently succeeded")
				}
			case "cancelled":
				if !errors.Is(err, context.Canceled) || out.Len() != 0 {
					t.Fatal("cancelled launch continued")
				}
			}
		})
	}
}
