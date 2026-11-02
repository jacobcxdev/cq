package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestServiceStatusTextExposesIncompleteInstallation(t *testing.T) {
	for _, condition := range []string{"missing executable", "old refresh"} {
		t.Run(condition, func(t *testing.T) {
			status := serviceStatus{Proxy: componentStatus{ID: "proxy", Healthy: true}, Refresh: componentStatus{ID: "refresh", Registered: true, Healthy: true}}
			problem := "package executable is unavailable"
			if condition == "missing executable" {
				status.Conflict = problem
			} else {
				problem = "refresh service uses a different runtime"
				status.Refresh.Healthy = false
				status.Refresh.Error = problem
			}
			var output bytes.Buffer
			if err := writeServiceStatus(&output, status); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(output.String(), "CQ services: degraded\n") || !strings.Contains(output.String(), problem) {
				t.Fatalf("incomplete installation hidden: %s", output.String())
			}
		})
	}
}

func TestHomebrewStatusExposesIncompletePackageReconciliation(t *testing.T) {
	for _, condition := range []string{"healthy", "missing executable", "old refresh"} {
		t.Run(condition, func(t *testing.T) {
			lifecycle, platform, _, previous, candidate := homebrewUpgradeServiceFixture(t)
			platform.live = candidate.Path
			platform.refresh = candidate.Path
			receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "interrupted-install", Generation: 2, Phase: "committed", Previous: previous, Candidate: candidate, ListenerIdentity: "tcp|127.0.0.1:12345", SupervisorPID: 41}
			lifecycle.RuntimeReceipt = func() (proxy.RuntimeUpgradeReceiptV1, error) { return receipt, nil }
			if condition == "missing executable" {
				lifecycle.DigestExecutable = func(path string) (string, error) {
					if path != lifecycle.Executable {
						t.Fatalf("checked executable %q, want package path", path)
					}
					return "", os.ErrNotExist
				}
			}
			if condition == "old refresh" {
				platform.refresh = previous.Path
			}
			platform.calls = nil
			status, err := lifecycle.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status.ActiveRuntimeVersion != candidate.Version || !status.Proxy.Healthy {
				t.Fatalf("lost live runtime status: %+v", status)
			}
			switch condition {
			case "healthy":
				if status.Conflict != "" || !status.Refresh.Healthy {
					t.Fatalf("healthy package reported incomplete: %+v", status)
				}
			case "missing executable":
				if !strings.Contains(status.Conflict, "package executable") {
					t.Fatalf("missing package executable hidden: %+v", status)
				}
			case "old refresh":
				if status.Refresh.Healthy || !strings.Contains(status.Refresh.Error, "runtime") {
					t.Fatalf("stale refresh target reported healthy: %+v", status)
				}
			}
			if len(platform.calls) != 1 || platform.calls[0] != "inspect" {
				t.Fatalf("status mutated services: %v", platform.calls)
			}
		})
	}
}
