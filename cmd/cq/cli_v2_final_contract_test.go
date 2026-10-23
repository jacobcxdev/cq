package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

func finalContractRun(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	exit := runCLIV2(context.Background(), args, &cli.Session{In: strings.NewReader(""), Out: &out, Err: &err})
	return exit, out.String(), err.String()
}

func TestCLIV2FinalEnvironmentBoundaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix XDG contract; Windows native root authority is separate")
	}
	paths := []string{"claude proxy pin show", "codex proxy pin show", "codex proxy fallback show", "codex proxy reserve status", "codex proxy policy show", "codex proxy trace", "proxy status", "codex proxy readiness show --client-build 0.146.0", "codex proxy validate websocket --client-build 0.146.0", "codex proxy canary status", "proxy rescue status", "proxy operation status", "codex proxy credential-endpoint legacy inspect", "service status"}
	for _, unavailable := range []bool{false, true} {
		name, code, message, want := "relative", "environment_path_invalid", "Environment variable XDG_CONFIG_HOME must name an absolute path.", 2
		if unavailable {
			name, code, message, want = "unavailable", "environment_root_unavailable", "Cannot resolve the user storage root.", 4
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", "sensitive-relative-value")
			if unavailable {
				t.Setenv("XDG_CONFIG_HOME", "")
				t.Setenv("HOME", "")
			}
			for _, path := range paths {
				t.Run(path, func(t *testing.T) {
					exit, stdout, stderr := finalContractRun(t, append(strings.Fields(path), "--json"))
					var envelope struct{ Errors []cli.Diagnostic }
					if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
						t.Fatal(err, stdout)
					}
					if exit != want || len(envelope.Errors) != 1 || envelope.Errors[0].Code != code || envelope.Errors[0].Message != message {
						t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
					}
				})
			}
			for _, path := range paths {
				exit, _, stderr := finalContractRun(t, append(strings.Fields(path), "--help"))
				if exit != 0 || stderr != "" {
					t.Fatalf("help %s: %d %q", path, exit, stderr)
				}
			}
		})
	}
}

func TestCLIV2FinalPersistedSelectionLabels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix XDG fixture; no ambient native roots")
	}
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	// These commands need config/state only. Invalid unused cache roots must stay lazy.
	t.Setenv("XDG_CACHE_HOME", "unused-relative-cache")
	dir := filepath.Join(root, "cq")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Équipe 東京", "before\x00\x1b[2J\x7f\u0085tail Équipe", ""} {
		cfg := map[string]any{"local_token": "synthetic-unused-token", "pinned_claude_account": label, "codex_routing_pinned_account_key": label, "codex_routing_default_account_key": label}
		raw, _ := json.Marshal(cfg)
		if err := os.WriteFile(filepath.Join(dir, "proxy.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"claude proxy pin show", "codex proxy pin show", "codex proxy fallback show"} {
			t.Run(path+"/"+label, func(t *testing.T) {
				exit, stdout, stderr := finalContractRun(t, strings.Fields(path))
				want := cli.HumanValue(label)
				if label == "" {
					want = "not configured"
				}
				if exit != 0 || !strings.Contains(stdout, ": "+want+"\n") || stderr != "" {
					t.Fatalf("human exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
				}
				exit, stdout, stderr = finalContractRun(t, append(strings.Fields(path), "--json"))
				var envelope struct {
					Data struct {
						Account    *string
						Configured bool
					}
				}
				if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
					t.Fatal(err)
				}
				if exit != 0 || stderr != "" || envelope.Data.Configured != (label != "") || (label == "" && envelope.Data.Account != nil) || (label != "" && (envelope.Data.Account == nil || *envelope.Data.Account != label)) {
					t.Fatalf("json exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
				}
			})
		}
	}
}

func TestCLIV2FinalTimestampPrecision(t *testing.T) {
	for _, nano := range []int{0, 123456789, 120000000} {
		t.Run(time.Duration(nano).String(), func(t *testing.T) {
			stamp := time.Date(2026, 9, 23, 14, 0, 0, nano, time.FixedZone("0.146.0", 3600))
			want := stamp.UTC().Format(time.RFC3339Nano)
			t.Run("candidate", func(t *testing.T) {
				if got := projectV2Candidate("/fixture", proxy.CandidateLifecycleStateV1{UpdatedAt: stamp}).UpdatedAt; got != want {
					t.Fatalf("got=%s want=%s", got, want)
				}
			})
			t.Run("canary", func(t *testing.T) {
				got := projectV2Canary(proxy.CodexCanaryState{StartedAt: stamp, EndedAt: stamp, LastObservedAt: stamp})
				if got.StartedAt != want || *got.EndedAt != want || *got.LastObservedAt != want {
					t.Fatalf("got=%+v want=%s", got, want)
				}
			})
			t.Run("service", func(t *testing.T) {
				got := projectV2ServiceComponent(serviceRefresh, componentStatus{Registered: true, Observed: &serviceObservation{LastRunAt: &stamp}}, stamp)
				if got.LastRunAt == nil || *got.LastRunAt != want {
					t.Fatalf("got=%+v want=%s", got, want)
				}
			})
			t.Run("reset", func(t *testing.T) {
				if got := v2ResetTimeText(&stamp, "none"); got != want {
					t.Fatalf("got=%s want=%s", got, want)
				}
				if got := v2ResetTimeText(nil, "none"); got != "none" {
					t.Fatal(got)
				}
			})
			t.Run("reset recommendation", func(t *testing.T) {
				a, _ := v2ResetTestApp()
				a.Clock = v2ResetClock{stamp}
				out := handleV2ResetInspectionWithApp(context.Background(), v2ResetInvocation("codex reset recommend"), nil, a)
				var data struct{ Schedule ResetSchedule }
				if err := json.Unmarshal(out.Data, &data); err != nil {
					t.Fatal(err)
				}
				horizon := data.Schedule.Horizon.UTC().Format(time.RFC3339Nano)
				if !strings.Contains(out.Human, "Horizon: "+horizon+".\n") {
					t.Fatalf("human=%q horizon=%s", out.Human, horizon)
				}
			})
			t.Run("reset preview", func(t *testing.T) {
				var output bytes.Buffer
				err := writeV2ResetPreview(&cli.Session{Err: &output}, app.CodexResetUsePlan{CurrentWindows: map[quota.WindowName]quota.Window{quota.Window5Hour: {ResetAtUnix: stamp.Unix()}, quota.Window7Day: {ResetAtUnix: stamp.Unix()}}}, false)
				if err != nil || strings.Count(output.String(), stamp.UTC().Truncate(time.Second).Format(time.RFC3339Nano)) != 2 {
					t.Fatalf("err=%v output=%q want=%s", err, output.String(), want)
				}
			})
			for _, transport := range []string{"readiness", "websocket"} {
				t.Run(transport, func(t *testing.T) {
					dir := validationTempDir(t)
					required, _ := proxy.DefaultCodexRoutingRequirements(version, "0.146.0")
					marker := completeCodexHTTPReadinessMarker(required)
					marker.ValidatedAt = stamp
					deps := v2ValidationDependencies{stateDir: func() (string, error) { return dir, nil }, loadMarker: func(string, proxy.CodexRoutingTransport) (proxy.CodexReadinessMarker, error) { return marker, nil }, websocket: func(context.Context, context.Context, string, string, string, string) (proxy.CodexReadinessMarker, error) {
						return marker, nil
					}}
					args := []string{"codex", "proxy", "readiness", "show", "--client-build", "0.146.0"}
					if transport == "websocket" {
						args = []string{"codex", "proxy", "validate", "websocket", "--client-build", "0.146.0"}
					}
					exit, stdout, _ := runValidationCLI(t, append(args, "--json"), context.Background(), validationLookup(deps))
					var envelope struct {
						Data struct {
							ValidatedAt string `json:"validated_at"`
							Marker      struct {
								ValidatedAt string `json:"validated_at"`
							}
						}
					}
					if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
						t.Fatal(err)
					}
					if exit != 0 || envelope.Data.ValidatedAt != want || envelope.Data.Marker.ValidatedAt != want {
						t.Fatalf("exit=%d stdout=%s want=%s", exit, stdout, want)
					}
					exit, stdout, _ = runValidationCLI(t, args, context.Background(), validationLookup(deps))
					if exit != 0 || !strings.Contains(stdout, "Validated: "+want+"\n") {
						t.Fatalf("human exit=%d stdout=%q", exit, stdout)
					}
				})
			}
		})
	}
}
