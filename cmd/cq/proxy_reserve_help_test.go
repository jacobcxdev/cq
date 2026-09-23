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

func TestProxyReserveHelpAndGrammar(t *testing.T) {
	for _, action := range []string{"", "set", "disable", "enable", "clear", "status", "windows"} {
		args := []string{"proxy", "reserve"}
		if action != "" {
			args = append(args, action)
		}
		args = append(args, "--help")
		var out bytes.Buffer
		handled, code, err := runPureGlobalInspection(args, &out, io.Discard)
		if !handled || code != 0 || err != nil || !strings.Contains(out.String(), "cq proxy reserve") {
			t.Fatalf("%v: handled=%v code=%d err=%v output=%s", args, handled, code, err, out.String())
		}
	}
	for _, args := range [][]string{{"proxy", "reserve", "set", "--window", "7d", "--percent", "2"}, {"proxy", "reserve", "status", "--json"}, {"proxy", "reserve", "disable"}} {
		got := classifyInterceptedInspection(args)
		if got.handled || got.err != nil {
			t.Fatalf("valid command blocked: %v: %+v", args, got)
		}
	}
	for _, args := range [][]string{{"proxy", "reserve", "set"}, {"proxy", "reserve", "bad"}, {"proxy", "reserve", "status", "--window", "7d"}} {
		if got := classifyInterceptedInspection(args); !got.handled || got.err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
}

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
