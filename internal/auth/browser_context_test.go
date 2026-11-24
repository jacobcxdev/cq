package auth

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

func TestWindowsBrowserLaunchAndClipboardPolicy(t *testing.T) {
	const target = "https://example.com/device/α🚀?user_code=ABCD-1234&state=a%26b"
	const browser = `C:\Program Files\Browser\browser.exe`
	for _, tc := range []struct {
		name, selection, probe, wantOutput string
		clipboardFailures, wantCopies      int
		wantLaunch                         bool
	}{
		{name: "closed", selection: "ChromeHTML", wantLaunch: true},
		{name: "running", selection: "ChromeHTML", probe: "true", wantCopies: 1, wantOutput: "URL copied to clipboard"},
		{name: "busy clipboard", selection: "ChromeHTML", probe: "true", clipboardFailures: 2, wantCopies: 3, wantOutput: "URL copied to clipboard"},
		{name: "clipboard unavailable", selection: "FirefoxURL", probe: "true", clipboardFailures: 4, wantCopies: 4, wantOutput: target},
		{name: "window probe unavailable", selection: "MSEdgeHTM", probe: "fail", wantLaunch: true},
		{name: "unknown handler", selection: "invalid/name", wantLaunch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diagnostics bytes.Buffer
			clipboardFile := filepath.Join(t.TempDir(), "clipboard")
			launched, copies := false, 0
			command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
				switch name {
				case "reg":
					if strings.HasPrefix(args[1], "HKEY_CLASSES_ROOT") {
						return browserHelperCommand(ctx, "print", `Default REG_SZ "`+browser+`" "%1"`)
					}
					return browserHelperCommand(ctx, "print", "ProgId REG_SZ "+tc.selection)
				case "powershell.exe":
					if !strings.Contains(args[len(args)-1], "MainWindowHandle") || tc.selection == "invalid/name" {
						t.Errorf("unexpected window probe: %q", args)
					}
					if tc.probe == "fail" {
						return browserHelperCommand(ctx, "fail", "")
					}
					cmd := browserHelperCommand(ctx, "probe", tc.probe)
					cmd.Env = append(cmd.Env, "CQ_BROWSER_EXPECTED_PATH="+browser)
					return cmd
				case "clip.exe":
					copies++
					if len(args) != 0 {
						t.Errorf("clip.exe received the URL as arguments: %q", args)
					}
					if copies <= tc.clipboardFailures {
						return browserHelperCommand(ctx, "fail", "")
					}
					return browserHelperCommand(ctx, "copy", clipboardFile)
				case "rundll32":
					launched = true
					if len(args) != 2 || args[0] != "url.dll,FileProtocolHandler" || args[1] != target || ctx.Done() != nil {
						t.Errorf("default shell launch = %q, ctx = %v", args, ctx)
					}
				default:
					t.Errorf("unexpected browser command %q %q", name, args)
				}
				return browserHelperCommand(ctx, "print", "")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := openBrowserContext(ctx, target, "windows", command, &diagnostics); err != nil {
				t.Fatal(err)
			}
			if launched != tc.wantLaunch || copies != tc.wantCopies {
				t.Fatalf("launched=%t, copy attempts=%d", launched, copies)
			}
			if !strings.Contains(diagnostics.String(), tc.wantOutput) {
				t.Fatalf("diagnostics = %q, want %q", &diagnostics, tc.wantOutput)
			}
			if copies > tc.clipboardFailures {
				got, err := os.ReadFile(clipboardFile)
				if err != nil || len(got)%2 != 0 {
					t.Fatalf("invalid clipboard input %x, %v", got, err)
				}
				units := make([]uint16, len(got)/2)
				if _, err := binary.Decode(got, binary.LittleEndian, units); err != nil {
					t.Fatal(err)
				}
				if text := string(utf16.Decode(units)); text != target {
					t.Fatalf("clipboard text = %q, want %q", text, target)
				}
			}
		})
	}
}

func TestBrowserContextCancelsDetectionWithoutLaunching(t *testing.T) {
	for _, tc := range []struct{ platform, blocked string }{
		{"linux", "xdg-settings"}, {"windows", "user-registry"}, {"windows", "browser-registry"},
		{"windows", "window-probe"}, {"windows", "clipboard"},
		{"darwin", "plutil"}, {"darwin", "osascript"}, {"darwin", "pbcopy"},
	} {
		t.Run(tc.platform+"/"+tc.blocked, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{})
			var once sync.Once
			var blockedCommand *exec.Cmd
			commands := func(commandCtx context.Context, name string, args ...string) *exec.Cmd {
				stage, output := name, "firefox.desktop"
				if name == "reg" {
					stage, output = "user-registry", "ProgId REG_SZ FirefoxURL"
					if strings.Contains(strings.Join(args, " "), "HKEY_CLASSES_ROOT") {
						stage, output = "browser-registry", `Default REG_SZ "firefox.exe" "%1"`
					}
				} else if name == "powershell.exe" {
					stage, output = "window-probe", "true"
				} else if name == "clip.exe" {
					stage, output = "clipboard", ""
				} else if name == "plutil" {
					output = `{"LSHandlers":[{"LSHandlerURLScheme":"https","LSHandlerRoleAll":"org.mozilla.firefox"}]}`
				} else if name == "osascript" {
					output = "true"
				} else if name != "xdg-settings" && name != "pbcopy" {
					t.Errorf("browser launch after cancelled detection: %s", name)
				}
				mode := "print"
				if stage == tc.blocked {
					mode = "block"
				}
				cmd := browserHelperCommand(commandCtx, mode, output)
				if mode == "block" {
					blockedCommand = cmd
					cmd.Stderr = browserReadyWriter(func() { once.Do(func() { close(ready) }) })
				}
				return cmd
			}
			done := make(chan error, 1)
			go func() {
				defer func() {
					if recover() != nil {
						done <- errors.New("opener panic")
					}
				}()
				done <- openBrowserContext(ctx, "https://example.com/login", tc.platform, commands, io.Discard)
			}()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("detection helper did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation = %v", err)
				}
				if blockedCommand.ProcessState == nil {
					t.Fatal("cancelled detection child was not reaped")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancelled detection did not return")
			}
		})
	}
}

func TestLinuxBrowserContextPreservesPrivateFlagsAndBrowserLifetime(t *testing.T) {
	for _, tc := range []struct{ selection, executable, flag string }{
		{"firefox.desktop", "firefox", "--private-window"},
		{"chromium.desktop", "chromium", "--incognito"},
		{"invalid/name.desktop", "xdg-open", "https://example.com/login"},
	} {
		t.Run(tc.selection, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			launched := false
			commands := func(commandCtx context.Context, name string, args ...string) *exec.Cmd {
				output := tc.selection
				if name != "xdg-settings" {
					launched = true
					if name != tc.executable || len(args) == 0 || args[0] != tc.flag || commandCtx.Done() != nil {
						t.Errorf("launch changed browser policy or inherited cancellation: %s %q", name, args)
					}
					output = ""
				}
				return browserHelperCommand(commandCtx, "print", output)
			}
			if err := openBrowserContext(ctx, "https://example.com/login", "linux", commands, io.Discard); err != nil || !launched {
				t.Fatalf("launch = %v, launched=%v", err, launched)
			}
			cancel()
		})
	}
}

func TestBrowserContextRejectsCancelledOrInvalidInputBeforeCommands(t *testing.T) {
	command := func(context.Context, string, ...string) *exec.Cmd { t.Fatal("started a command"); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := OpenBrowserContext(ctx, "https://example.com"); !errors.Is(err, context.Canceled) {
		t.Fatalf("exported cancelled opener = %v", err)
	}
	if err := openBrowserContext(ctx, "https://example.com", "linux", command, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled opener = %v", err)
	}
	if err := openBrowserContext(context.Background(), "file:///private", "linux", command, io.Discard); err == nil {
		t.Fatal("accepted non-web URL")
	}
}

func TestBrowserContextDeadlineBoundsDetection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	commands := func(commandCtx context.Context, _ string, _ ...string) *exec.Cmd {
		return browserHelperCommand(commandCtx, "block", "")
	}
	if err := openBrowserContext(ctx, "https://example.com/login", "linux", commands, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("detection deadline = %v", err)
	}
}

type browserReadyWriter func()

func (w browserReadyWriter) Write(p []byte) (int, error) { w(); return len(p), nil }

func browserHelperCommand(ctx context.Context, mode, output string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrowserContextHelperProcess$", "--", mode, output)
	cmd.Env = append(os.Environ(), "CQ_BROWSER_CONTEXT_HELPER=1")
	return cmd
}

func TestBrowserContextHelperProcess(t *testing.T) {
	if os.Getenv("CQ_BROWSER_CONTEXT_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg != "--" || i+2 >= len(os.Args) {
			continue
		}
		if os.Args[i+1] == "block" {
			_, _ = io.WriteString(os.Stderr, "ready")
			time.Sleep(30 * time.Second)
		} else if os.Args[i+1] == "probe" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil || string(data) != os.Getenv("CQ_BROWSER_EXPECTED_PATH") {
				os.Exit(1)
			}
			_, _ = io.WriteString(os.Stdout, os.Args[i+2])
		} else if os.Args[i+1] == "copy" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil || os.WriteFile(os.Args[i+2], data, 0o600) != nil {
				os.Exit(1)
			}
		} else if os.Args[i+1] == "fail" {
			os.Exit(1)
		} else {
			_, _ = io.WriteString(os.Stdout, os.Args[i+2])
		}
		os.Exit(0)
	}
	os.Exit(2)
}
