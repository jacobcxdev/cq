package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
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
