//go:build darwin

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestZCodeBrowserUsedSharedRunningBrowserPolicy(t *testing.T) {
	const target = "https://chat.z.ai/fixture-login"
	if os.Getenv("CQ_ZAI_BROWSER_HELPER") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := openZCodeBrowser(ctx, target); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	for _, clipboardWorks := range []bool{true, false} {
		t.Run(fmt.Sprintf("clipboard=%t", clipboardWorks), func(t *testing.T) {
			dir := t.TempDir()
			clipboard := filepath.Join(dir, "clipboard")
			opened := filepath.Join(dir, "opened")
			scripts := map[string]string{
				"plutil":    `printf '%s' '{"LSHandlers":[{"LSHandlerURLScheme":"https","LSHandlerRoleAll":"org.mozilla.firefox"}]}'`,
				"osascript": `printf true`,
				"pbcopy":    `cat > "$CQ_ZAI_BROWSER_CLIPBOARD"`,
				"open":      `touch "$CQ_ZAI_BROWSER_OPENED"`,
			}
			if !clipboardWorks {
				scripts["pbcopy"] = "exit 1"
			}
			for name, script := range scripts {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestZCodeBrowserUsedSharedRunningBrowserPolicy$")
			command.Env = append(os.Environ(), "CQ_ZAI_BROWSER_HELPER=1", "CQ_ZAI_BROWSER_CLIPBOARD="+clipboard, "CQ_ZAI_BROWSER_OPENED="+opened, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			var output, diagnostics bytes.Buffer
			command.Stdout, command.Stderr = &output, &diagnostics
			if err := command.Run(); err != nil {
				t.Fatalf("browser policy: %v: %s", err, diagnostics.String())
			}
			if output.Len() != 0 {
				t.Fatal("browser guidance contaminated stdout")
			}
			if !strings.Contains(diagnostics.String(), "Browser already running") {
				t.Fatal("login skipped shared running-browser policy")
			}
			if _, err := os.Stat(opened); !os.IsNotExist(err) {
				t.Fatal("login opened existing browser session")
			}
			if clipboardWorks {
				copied, err := os.ReadFile(clipboard)
				if err != nil || string(copied) != target || !strings.Contains(diagnostics.String(), "URL copied to clipboard") {
					t.Fatal("login did not copy original URL with private-window guidance")
				}
			} else if !strings.Contains(diagnostics.String(), target) {
				t.Fatal("clipboard failure omitted manual URL")
			}
		})
	}
}
