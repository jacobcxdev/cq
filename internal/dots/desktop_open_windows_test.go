//go:build windows

package dots

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsDesktopLauncherReopensExistingTraySession(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		openErr                       error
		cancelled                     bool
		wantActivations, wantRecovery int
	}{
		{name: "restored", wantActivations: 1},
		{name: "activation refused", openErr: errors.New("activation refused"), wantActivations: 1, wantRecovery: 1},
		{name: "cancelled", cancelled: true, wantRecovery: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, controller, deps, watchdog := desktopFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			deps.start = func(context.Context, Controller) (map[string]any, error) {
				if tc.cancelled {
					cancel()
				}
				return map[string]any{"restart_required": true}, nil
			}
			activations, recoveries := 0, 0
			deps.open = func(_ context.Context, app string) error {
				if app != s.AppPath {
					t.Fatalf("opened wrong app: %q", app)
				}
				activations++
				return tc.openErr
			}
			deps.recover = func(context.Context, *Settings) error {
				recoveries++
				return nil
			}
			err := runDesktopLaunch(ctx, s, controller, deps)
			if activations != tc.wantActivations || recoveries != tc.wantRecovery || watchdog.kills != 1 || err == nil || !strings.Contains(err.Error(), "system tray") {
				t.Fatalf("tray reactivation: activations=%d recoveries=%d watchdog stops=%d result=%v", activations, recoveries, watchdog.kills, err)
			}
		})
	}
}

func TestWindowsDesktopActivationRejectsInvalidOrCancelledPackage(t *testing.T) {
	if err := openDesktopApp(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing package was eligible for Desktop activation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := openDesktopApp(ctx, windowsAppFixture(t)); err == nil {
		t.Fatal("cancelled Desktop activation created a process")
	}
}
