package auth

import (
	"testing"
)

// --- parseRegValue ---

func TestParseRegValue(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   string
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
		name   string
		input  string
		want   string
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
