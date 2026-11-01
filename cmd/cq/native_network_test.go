package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
)

const healthyNativeNetworkStatus = `{"ready":true,"extension_connected":true,"broker_registered":true,"broker_ready":true}`

func nativeNetworkTestRunner(events *[]string) nativeNetworkRunner {
	return nativeNetworkRunner{
		validateBroker: func(context.Context) error { *events = append(*events, "validate"); return nil },
		verifyHost:     func(context.Context) error { *events = append(*events, "verify"); return nil },
		installAgent:   func(context.Context) error { *events = append(*events, "install"); return nil },
		removeAgent:    func(context.Context) error { *events = append(*events, "remove"); return nil },
		host: func(_ context.Context, arguments ...string) ([]byte, error) {
			*events = append(*events, strings.Join(arguments, " "))
			return []byte(healthyNativeNetworkStatus), nil
		},
		wait: func(context.Context, time.Duration) error { *events = append(*events, "wait"); return nil },
	}
}

func TestNativeNetworkCommandParsingAndHelp(t *testing.T) {
	for _, operation := range []string{"enable", "status", "disable"} {
		t.Run(operation, func(t *testing.T) {
			var cli CLI
			parser, err := kong.New(&cli, kong.Writers(io.Discard, io.Discard))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parser.Parse([]string{"--json", "codex", "network", operation})
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Command() != "codex network "+operation || !cli.JSON {
				t.Fatalf("command %q", parsed.Command())
			}
			if _, ok := manualHelp([]string{"codex", "network", operation}); !ok {
				t.Fatal("manual help missing")
			}
			out := &bytes.Buffer{}
			handled, code, err := runPureGlobalInspection([]string{"codex", "network", operation, "--help"}, out, io.Discard)
			if !handled || code != 0 || err != nil || !strings.Contains(out.String(), "cq codex network "+operation) {
				t.Fatalf("help = %t,%d,%v: %s", handled, code, err, out)
			}
		})
	}
}

func TestNativeNetworkEnableRequiresPreflightBeforeMutation(t *testing.T) {
	for _, stage := range []string{"validate", "verify", "install", "activate"} {
		t.Run(stage, func(t *testing.T) {
			var events []string
			runner := nativeNetworkTestRunner(&events)
			failure := errors.New("test failure")
			switch stage {
			case "validate":
				runner.validateBroker = func(context.Context) error { return failure }
			case "verify":
				runner.verifyHost = func(context.Context) error { return failure }
			case "install":
				runner.installAgent = func(context.Context) error { return failure }
			case "activate":
				runner.host = func(context.Context, ...string) ([]byte, error) { return nil, failure }
			}
			if err := runner.execute(context.Background(), "enable", false, io.Discard); !errors.Is(err, failure) {
				t.Fatalf("error %v", err)
			}
			want := map[string][]string{"validate": nil, "verify": {"validate"}, "install": {"validate", "verify"}, "activate": {"validate", "verify", "install"}}[stage]
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events %v, want %v", events, want)
			}
		})
	}
}

func TestNativeNetworkEnableWaitsForAuthenticatedReadiness(t *testing.T) {
	var events []string
	runner := nativeNetworkTestRunner(&events)
	statusCalls := 0
	runner.host = func(_ context.Context, arguments ...string) ([]byte, error) {
		events = append(events, strings.Join(arguments, " "))
		if arguments[0] == "status" {
			statusCalls++
			if statusCalls == 1 {
				return []byte(`{"ready":false,"extension_connected":true}`), errors.New("pending")
			}
		}
		return []byte(healthyNativeNetworkStatus), nil
	}
	var output bytes.Buffer
	if err := runner.execute(context.Background(), "enable", true, &output); err != nil {
		t.Fatal(err)
	}
	want := []string{"validate", "verify", "install", "activate --application /Applications/ChatGPT.app", "status", "wait", "status"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v", events)
	}
	if output.String() != healthyNativeNetworkStatus+"\n" {
		t.Fatalf("status %s", output.String())
	}
}

func TestNativeNetworkEnableWaitsForExtensionReplacement(t *testing.T) {
	var events []string
	runner := nativeNetworkTestRunner(&events)
	checks := 0
	runner.host = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "status" {
			checks++
			if checks <= 6 {
				return []byte(`{"ready":false}`), errors.New("starting")
			}
		}
		return []byte(healthyNativeNetworkStatus), nil
	}
	if err := runner.execute(context.Background(), "enable", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if checks != 7 {
		t.Fatalf("status checks %d", checks)
	}
}

func TestNativeNetworkStatusRejectsFalseReadiness(t *testing.T) {
	for _, data := range []string{
		`{}`, `null`, `{"ready":true}`, `{"ready":true,"extension_connected":true,"broker_registered":true,"broker_ready":false}`,
		healthyNativeNetworkStatus + ` {}`, `not JSON`, strings.Repeat("x", (16<<10)+1),
	} {
		t.Run(data[:min(len(data), 60)], func(t *testing.T) {
			var events []string
			runner := nativeNetworkTestRunner(&events)
			runner.host = func(context.Context, ...string) ([]byte, error) { return []byte(data), nil }
			if err := runner.execute(context.Background(), "status", true, io.Discard); err == nil {
				t.Fatal("unhealthy status accepted")
			}
		})
	}
	var events []string
	runner := nativeNetworkTestRunner(&events)
	runner.host = func(context.Context, ...string) ([]byte, error) {
		return []byte(healthyNativeNetworkStatus), errors.New("command failed")
	}
	if err := runner.execute(context.Background(), "status", true, io.Discard); err == nil {
		t.Fatal("failed command accepted")
	}
}

func TestNativeNetworkStatusPreservesSourceFreeCounters(t *testing.T) {
	var events []string
	runner := nativeNetworkTestRunner(&events)
	status := strings.TrimSuffix(healthyNativeNetworkStatus, "}") + `,"tcp_flows":18446744073709551615,"udp_flows":7,"broker_opens":8,"broker_failures":0}`
	runner.host = func(context.Context, ...string) ([]byte, error) { return []byte(status), nil }
	var output bytes.Buffer
	if err := runner.execute(context.Background(), "status", true, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != status+"\n" {
		t.Fatalf("source-free counters changed: %s", output.String())
	}
	for _, invalid := range []string{`-1`, `1.5`, `18446744073709551616`} {
		runner.host = func(context.Context, ...string) ([]byte, error) {
			return []byte(strings.TrimSuffix(healthyNativeNetworkStatus, "}") + `,"tcp_flows":` + invalid + `}`), nil
		}
		if err := runner.execute(context.Background(), "status", true, io.Discard); err == nil {
			t.Fatalf("invalid uint64 counter accepted: %s", invalid)
		}
	}
}

func TestNativeNetworkEnableStopsAfterBoundedReadinessChecks(t *testing.T) {
	var events []string
	runner := nativeNetworkTestRunner(&events)
	checks := 0
	runner.host = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "status" {
			checks++
			return []byte(`{"ready":false}`), errors.New("pending")
		}
		return nil, nil
	}
	if err := runner.execute(context.Background(), "enable", false, io.Discard); err == nil {
		t.Fatal("pending activation reported success")
	}
	if checks != 31 {
		t.Fatalf("status checks %d", checks)
	}
}

func TestNativeNetworkDisableOnlyStopsOwnBroker(t *testing.T) {
	var events []string
	runner := nativeNetworkTestRunner(&events)
	var output bytes.Buffer
	if err := runner.execute(context.Background(), "disable", true, &output); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"verify", "disable", "remove"}) {
		t.Fatalf("events %v", events)
	}
	if output.String() != "{\"enabled\":false}\n" {
		t.Fatalf("output %s", output.String())
	}
}

func TestNativeNetworkFailedDisablePreservesAgent(t *testing.T) {
	var events []string
	runner := nativeNetworkTestRunner(&events)
	runner.host = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("manager refused") }
	if err := runner.execute(context.Background(), "disable", false, io.Discard); err == nil {
		t.Fatal("failed disable accepted")
	}
	if !reflect.DeepEqual(events, []string{"verify"}) {
		t.Fatalf("events %v", events)
	}
}

func TestNativeNetworkStatusDoesNotInstallOrActivate(t *testing.T) {
	var events []string
	runner := nativeNetworkTestRunner(&events)
	if err := runner.execute(context.Background(), "status", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"verify", "status"}) {
		t.Fatalf("events %v", events)
	}
}

func TestNativeNetworkUnknownCommandDoesNotTouchHost(t *testing.T) {
	var events []string
	runner := nativeNetworkTestRunner(&events)
	if err := runner.execute(context.Background(), "restart-chatgpt", false, io.Discard); err == nil {
		t.Fatal("unknown command accepted")
	}
	if len(events) != 0 {
		t.Fatalf("events %v", events)
	}
}

func TestNativeNetworkWaitHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitNativeNetwork(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait %v", err)
	}
}
