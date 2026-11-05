package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBrowserContextCancelsDetectionWithoutLaunching(t *testing.T) {
	for _, tc := range []struct{ platform, blocked string }{
		{"linux", "xdg-settings"}, {"windows", "user-registry"}, {"windows", "browser-registry"},
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

func TestBrowserContextPreservesPrivateFlagsAndBrowserLifetime(t *testing.T) {
	for _, tc := range []struct{ platform, selection, executable, flag string }{
		{"linux", "firefox.desktop", "firefox", "--private-window"},
		{"linux", "chromium.desktop", "chromium", "--incognito"},
		{"linux", "invalid/name.desktop", "xdg-open", "https://example.com/login"},
		{"windows", "FirefoxURL", "firefox.exe", "--private-window"},
		{"windows", "ChromeHTML", "chrome.exe", "--incognito"},
		{"windows", "invalid/name", "rundll32", "url.dll,FileProtocolHandler"},
	} {
		t.Run(tc.platform+"/"+tc.selection, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			launched := false
			commands := func(commandCtx context.Context, name string, args ...string) *exec.Cmd {
				output := tc.selection
				if name == "reg" && strings.Contains(strings.Join(args, " "), "HKEY_CLASSES_ROOT") {
					output = fmt.Sprintf(`Default REG_SZ "%s" "%%1"`, tc.executable)
				} else if name == "reg" {
					output = "ProgId REG_SZ " + tc.selection
				} else if name != "xdg-settings" {
					launched = true
					if name != tc.executable || len(args) == 0 || args[0] != tc.flag || commandCtx.Done() != nil {
						t.Errorf("launch changed browser policy or inherited cancellation: %s %q", name, args)
					}
					output = ""
				}
				return browserHelperCommand(commandCtx, "print", output)
			}
			if err := openBrowserContext(ctx, "https://example.com/login", tc.platform, commands, io.Discard); err != nil || !launched {
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
		} else {
			_, _ = io.WriteString(os.Stdout, os.Args[i+2])
		}
		os.Exit(0)
	}
	os.Exit(2)
}
