package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/cli"
)

var terminalDotsFixtures = []terminalFixtureRegistration{
	{"cq codex dots install", "Success", "stdout", "text/cq-codex-dots-install/02-success.txt"},
	{"cq codex dots install", "Failure", "stderr", "text/cq-codex-dots-install/03-failure.txt"},
	{"cq codex dots start", "Success", "stdout", "text/cq-codex-dots-start/02-success.txt"},
	{"cq codex dots start", "Failure", "stderr", "text/cq-codex-dots-start/03-failure.txt"},
	{"cq codex dots stop", "Success", "stdout", "text/cq-codex-dots-stop/02-success.txt"},
	{"cq codex dots stop", "Failure", "stderr", "text/cq-codex-dots-stop/03-failure.txt"},
	{"cq codex dots status", "Success", "stdout", "text/cq-codex-dots-status/02-success.txt"},
	{"cq codex dots status", "Failure", "stderr", "text/cq-codex-dots-status/03-failure.txt"},
	{"cq codex dots sync", "Success", "stdout", "text/cq-codex-dots-sync/02-success.txt"},
	{"cq codex dots sync", "Failure", "stderr", "text/cq-codex-dots-sync/03-failure.txt"},
	{"cq codex dots list", "Success", "stdout", "text/cq-codex-dots-list/02-success.txt"},
	{"cq codex dots list", "Failure", "stderr", "text/cq-codex-dots-list/03-failure.txt"},
	{"cq codex dots prime", "Success", "stdout", "text/cq-codex-dots-prime/02-success.txt"},
	{"cq codex dots prime", "Failure", "stderr", "text/cq-codex-dots-prime/03-failure.txt"},
	{"cq codex dots broadcast", "Success", "stdout", "text/cq-codex-dots-broadcast/02-success.txt"},
	{"cq codex dots broadcast", "Failure", "stderr", "text/cq-codex-dots-broadcast/03-failure.txt"},
	{"cq codex dots board", "Success", "stdout", "text/cq-codex-dots-board/02-success.txt"},
	{"cq codex dots board", "Failure", "stderr", "text/cq-codex-dots-board/03-failure.txt"},
	{"cq codex dots start", "Pending · restart required", "stdout", "text/cq-codex-dots-start/04-pending-restart-required.txt"},
	{"cq codex dots start", "Pending · restart required", "stderr", "text/cq-codex-dots-start/05-pending-restart-required.txt"},
	{"cq codex dots broadcast", "Partial · delivery unknown", "stdout", "text/cq-codex-dots-broadcast/04-partial-delivery-unknown.txt"},
	{"cq codex dots broadcast", "Partial · delivery unknown", "stderr", "text/cq-codex-dots-broadcast/05-partial-delivery-unknown.txt"},
}

func TestCLIV2DotsAuthoredTerminalFixtures(t *testing.T) {
	cases := []struct {
		file, operation, stream, receipt string
		failed                           bool
	}{
		{"text/cq-codex-dots-install/02-success.txt", "install", "stdout", `{"installed":true,"native_catalog_ready":true,"profiles":2,"system_app_modified":false}`, false},
		{"text/cq-codex-dots-install/03-failure.txt", "install", "stderr", `null`, true},
		{"text/cq-codex-dots-start/02-success.txt", "start", "stdout", `{"native_catalog_ready":true,"profiles":2,"account_watcher_started":true,"relay_started":true,"system_app_modified":false}`, false},
		{"text/cq-codex-dots-start/03-failure.txt", "start", "stderr", `null`, true},
		{"text/cq-codex-dots-stop/02-success.txt", "stop", "stdout", `{"stopped":true,"system_app_modified":false}`, false},
		{"text/cq-codex-dots-stop/03-failure.txt", "stop", "stderr", `null`, true},
		{"text/cq-codex-dots-status/02-success.txt", "status", "stdout", `{"native_catalog_ready":true,"profiles":2,"system_app_modified":false}`, false},
		{"text/cq-codex-dots-status/03-failure.txt", "status", "stderr", `null`, true},
		{"text/cq-codex-dots-sync/02-success.txt", "sync", "stdout", `{"native_catalog_ready":true,"profiles":2,"catalogue_changed":false,"discovery_complete":true}`, false},
		{"text/cq-codex-dots-sync/03-failure.txt", "sync", "stderr", `null`, true},
		{"text/cq-codex-dots-list/02-success.txt", "list", "stdout", `{"dots":[{"id":"dot-one","name":"One","account":"primary","setup":"accepted"},{"id":"dot-two","name":"Two","account":"secondary","setup":"not_attempted"}]}`, false},
		{"text/cq-codex-dots-list/03-failure.txt", "list", "stderr", `null`, true},
		{"text/cq-codex-dots-prime/02-success.txt", "prime", "stdout", `{"priming":[{"target":"dot-one","outcome":"accepted"}]}`, false},
		{"text/cq-codex-dots-prime/03-failure.txt", "prime", "stderr", `null`, true},
		{"text/cq-codex-dots-broadcast/02-success.txt", "broadcast", "stdout", `{"broadcast":{"id":"broadcast-example","deliveries":[{"target":"dot-one","display_name":"One","outcome":"accepted"}]}}`, false},
		{"text/cq-codex-dots-broadcast/03-failure.txt", "broadcast", "stderr", `null`, true},
		{"text/cq-codex-dots-board/02-success.txt", "board", "stdout", `{"board":{"id":"board-example","thread_id":"01a14c1f-6e3c-7622-b4a3-b279ecb2a822"}}`, false},
		{"text/cq-codex-dots-board/03-failure.txt", "board", "stderr", `null`, true},
		{"text/cq-codex-dots-start/04-pending-restart-required.txt", "start", "stdout", `{"started":false,"restart_required":true,"reason":"Quit the app, then run cq codex dots start."}`, false},
		{"text/cq-codex-dots-start/05-pending-restart-required.txt", "start", "stderr", `{"started":false,"restart_required":true,"reason":"Quit the app, then run cq codex dots start."}`, false},
		{"text/cq-codex-dots-broadcast/04-partial-delivery-unknown.txt", "broadcast", "stdout", `{"broadcast":{"id":"broadcast-example","deliveries":[{"target":"dot-one","display_name":"One","outcome":"accepted"},{"target":"dot-two","display_name":"Two","outcome":"unknown","failure_type":"TimeoutError"}]}}`, false},
		{"text/cq-codex-dots-broadcast/05-partial-delivery-unknown.txt", "broadcast", "stderr", `{"broadcast":{"id":"broadcast-example","deliveries":[{"target":"dot-one","display_name":"One","outcome":"accepted"},{"target":"dot-two","display_name":"Two","outcome":"unknown","failure_type":"TimeoutError"}]}}`, false},
	}
	for _, test := range cases {
		t.Run(test.file, func(t *testing.T) {
			var result *v2DotsResult
			if err := json.Unmarshal([]byte(test.receipt), &result); err != nil {
				t.Fatal(err)
			}
			data := v2DotsData{Operation: test.operation, Result: result}
			outcome := cli.Outcome{}
			if test.failed {
				outcome = v2DotsFailure(errors.New("synthetic failure"))
			}
			if result != nil {
				outcome.Terminal = func(options cli.TerminalOptions) string { return terminalDots(data, options) }
				if result.incomplete() {
					outcome.Warnings = []cli.Diagnostic{{Code: "dots_incomplete", Message: "Dots has pending or incomplete work; inspect the receipt before retrying."}}
				}
			}
			args := []string{"codex", "dots", test.operation}
			switch test.operation {
			case "prime":
				args = append(args, "--all")
			case "broadcast":
				args = append(args, "Hello, team!")
			}
			var stdout, stderr bytes.Buffer
			session := &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: cli.TerminalOptions{Width: 100, Colour: true}, ErrTerminal: cli.TerminalOptions{Width: 100, Colour: true}}
			exit := cli.Run(context.Background(), args, session, func(path string) (cli.Handler, bool) {
				if path != strings.Join(args[:3], " ") {
					t.Fatalf("unexpected path %q", path)
				}
				return func(context.Context, cli.Invocation, *cli.Session) cli.Outcome { return outcome }, true
			})
			if exit != outcome.ExitCode {
				t.Fatalf("exit %d, want %d; stderr %s", exit, outcome.ExitCode, stderr.String())
			}
			actual := stdout.String()
			if test.stream == "stderr" {
				actual = stderr.String()
			}
			assertTerminalFixture(t, test.file, actual)
		})
	}
}
