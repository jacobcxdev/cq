package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/cli"
)

func TestCLIV2ReserveExactHelpWithoutAccess(t *testing.T) {
	for _, action := range []string{"", "set", "disable", "enable", "clear", "status", "windows"} {
		path := "codex proxy reserve"
		if action != "" {
			path += " " + action
		}
		want, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "help", strings.ReplaceAll(path, " ", "-")+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		for _, prefix := range []string{"codex proxy reserve", "proxy reserve"} {
			for _, help := range []string{"-h", "--help"} {
				args := strings.Fields(prefix)
				if action != "" {
					args = append(args, action)
				}
				args = append(args, help, "--json")
				var stdout, stderr bytes.Buffer
				if exit := cli.Run(context.Background(), args, &cli.Session{Out: &stdout, Err: &stderr}, noAccessV2Fixture(t).Lookup); exit != 0 || !bytes.Equal(stdout.Bytes(), want) {
					t.Fatalf("help %v exit=%d differs from frozen help", args, exit)
				}
			}
		}
		if action == "" {
			var stdout bytes.Buffer
			if exit := cli.Run(context.Background(), strings.Fields(path), &cli.Session{Out: &stdout, Err: io.Discard}, noAccessV2Fixture(t).Lookup); exit != 0 || !bytes.Equal(stdout.Bytes(), want) {
				t.Fatal("bare group read state or differed from exact help")
			}
		}
	}
}
