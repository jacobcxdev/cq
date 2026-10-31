package dots

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestHelperProxyConfigFailureRecoveredDesktopBeforeWatchdog(t *testing.T) {
	for _, recoveryFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovered", true: "recovery-failed"}[recoveryFails], func(t *testing.T) {
			settings := &Settings{IntegrationMode: "native"}
			calls := 0
			load := func() (*proxy.Config, error) { return nil, errors.New("private config details") }
			recoverApp := func(ctx context.Context, got *Settings) error {
				calls++
				if ctx.Err() != nil || got != settings {
					t.Fatal("recovery did not receive independent context and settings")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 15*time.Second {
					t.Fatal("recovery lacks bounded deadline")
				}
				if recoveryFails {
					return errors.New("private recovery details")
				}
				return nil
			}
			base, err := helperBaseURL("desktop-launch", settings, "http://stale", load, recoverApp)
			if err == nil || base != "" || calls != 1 || strings.Contains(err.Error(), "private") {
				t.Fatalf("base=%q, err=%v, recovery calls=%d", base, err, calls)
			}
		})
	}
}

func TestHelperProxyConfigurationWatcherFailedClosedAndOtherRolesUnchanged(t *testing.T) {
	for _, test := range []struct {
		role, mode string
		loads      int
		wantError  bool
	}{
		{"account-watch", "native", 1, true},
		{"repair", "native", 1, true},
		{"desktop-launch", "isolated", 1, true},
		{"desktop-recover", "native", 0, false},
		{"relay", "native", 0, false},
	} {
		t.Run(test.role+"/"+test.mode, func(t *testing.T) {
			loads := 0
			base, err := helperBaseURL(test.role, &Settings{IntegrationMode: test.mode}, "http://current", func() (*proxy.Config, error) {
				loads++
				return nil, errors.New("missing config")
			}, func(context.Context, *Settings) error {
				t.Fatal("non-native launcher attempted app recovery")
				return nil
			})
			if (err != nil) != test.wantError || loads != test.loads || !test.wantError && base != "http://current" {
				t.Fatalf("base=%q, err=%v, loads=%d", base, err, loads)
			}
		})
	}
}

func TestHelperProxyConfigurationOverridesInheritedBaseURL(t *testing.T) {
	for _, role := range []string{"desktop-launch", "account-watch", "repair"} {
		base, err := helperBaseURL(role, &Settings{IntegrationMode: "native"}, "https://stale.invalid", func() (*proxy.Config, error) {
			return &proxy.Config{Port: 19321}, nil
		}, func(context.Context, *Settings) error {
			t.Fatal("valid proxy configuration attempted app recovery")
			return nil
		})
		if err != nil || base != "http://127.0.0.1:19321" {
			t.Fatalf("%s base=%q, err=%v", role, base, err)
		}
	}
}
