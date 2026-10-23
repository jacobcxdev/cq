package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestCLIV2RescueRecoveryHonoursExplicitPort(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		calls, restarts := 0, 0
		deps := v2RescueDependencies{
			LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: 19280, LocalToken: "fixture"}, nil },
			Doer: &http.Client{Transport: v2ProxyRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, syscall.ECONNREFUSED
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"mode":"rescue","generation":1,"active_rescue_requests":0,"draining_sessions":[]}`))}, nil
			})},
			Restart: func(context.Context) error { restarts++; return nil },
		}
		args := []string{"proxy", "rescue", "enter"}
		if explicit {
			args = append(args, "--port", "19280")
		}
		inv, err := cli.Parse(args)
		if err != nil {
			t.Fatal(err)
		}
		out := handleV2RescueWithPreparation(context.Background(), inv, &cli.Session{}, func(context.Context, cli.Invocation) (v2RescueDependencies, error) { return deps, nil })
		if explicit {
			if out.ExitCode == 0 || restarts != 0 || calls != 1 {
				t.Fatalf("explicit: %+v restarts=%d calls=%d", out, restarts, calls)
			}
		} else if out.ExitCode != 0 || restarts != 1 || calls != 2 {
			t.Fatalf("default: %+v restarts=%d calls=%d", out, restarts, calls)
		}
	}
}
