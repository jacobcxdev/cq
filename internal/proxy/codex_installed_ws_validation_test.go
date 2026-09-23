package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRunCodexInstalledWebSocketValidationUsesIsolatedCandidate(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(executable, []byte("exact client"), 0o500); err != nil {
		t.Fatal(err)
	}
	clientBuild := "0.146.0"
	markerDir := t.TempDir()
	if err := os.Chmod(markerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := validationWebSocketRunner()

	marker, err := runCodexInstalledWebSocketValidationWithDependencies(
		context.Background(), "cq-build", clientBuild, markerDir,
		codexInstalledWebSocketValidationDependencies{
			resolveExecutable: func() (string, error) { return executable, nil },
			captureExecutable: captureCodexInstalledExecutable,
			runVersion: func(context.Context, string, codexInstalledExecutableProof) ([]byte, error) {
				return []byte("codex-cli " + clientBuild + "\n"), nil
			},
			runner: runner,
			now:    func() time.Time { return time.Unix(40_000, 0).UTC() },
		},
	)
	if err != nil {
		t.Fatalf("validation error = %v", err)
	}
	if marker.Transport != CodexRoutingWebSocket || marker.InstalledResult != "passed" {
		t.Fatalf("marker = %#v", marker)
	}
	loaded, err := LoadCodexReadinessMarker(markerDir, CodexRoutingWebSocket)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, marker) {
		t.Fatalf("loaded marker = %#v, want %#v", loaded, marker)
	}
}

func TestRunCodexInstalledWebSocketAcceptanceRejectsClientWithoutPrewarm(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex")
	material := "exact client"
	if err := os.WriteFile(executable, []byte(material), 0o500); err != nil {
		t.Fatal(err)
	}
	runner := testCodexAcceptanceRunner(func(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
		authBytes, err := os.ReadFile(filepath.Join(commandEnv(command.env, "CODEX_HOME"), "auth.json"))
		if err != nil {
			return nil, err
		}
		var auth struct {
			Tokens struct {
				AccessToken string `json:"access_token"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(authBytes, &auth); err != nil {
			return nil, err
		}
		if err := requestCodexAcceptanceUserSettings(command, auth.Tokens.AccessToken); err != nil {
			return nil, err
		}
		header := make(http.Header)
		header.Set("Authorization", "Bearer "+auth.Tokens.AccessToken)
		address := "ws" + strings.TrimPrefix(command.endpoint, "http")
		connection, _, err := websocket.DefaultDialer.DialContext(ctx, address, header)
		if err != nil {
			return nil, err
		}
		defer connection.Close()
		if err := connection.WriteMessage(websocket.TextMessage, codexTerminatingWSFrame("installed-turn", "")); err != nil {
			return nil, err
		}
		for {
			_, reply, err := connection.ReadMessage()
			if err != nil {
				return nil, err
			}
			if strings.Contains(string(reply), `"type":"response.completed"`) {
				break
			}
		}
		return nil, os.WriteFile(command.outputPath, []byte("PONG\n"), 0o600)
	})
	_, err := runCodexInstalledWebSocketAcceptance(
		context.Background(), "cq-build", "0.146.0", testCodexInstalledExecutableProof(executable, material), runner,
	)
	if !errors.Is(err, errCodexInstalledListenerAcceptance) {
		t.Fatalf("acceptance error = %v, want listener acceptance failure", err)
	}
}

func TestRunCodexInstalledWebSocketValidationLeavesMarkerAbsentOnFailure(t *testing.T) {
	markerDir := t.TempDir()
	if err := os.Chmod(markerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, required := DefaultCodexRoutingRequirements("cq-build", "0.146.0")
	prior := testCodexMarker(required)
	prior.CQExecutableSHA256 = ""
	prior.ClientExecutableSHA256 = ""
	prior.ServiceKind = ""
	prior.ServiceIdentitySHA256 = ""
	if err := saveCodexWebSocketReadinessMarkerDurably(markerDir, prior); err != nil {
		t.Fatal(err)
	}
	_, err := runCodexInstalledWebSocketValidationWithDependencies(
		context.Background(), "cq-build", required.ClientBuild, markerDir,
		codexInstalledWebSocketValidationDependencies{
			resolveExecutable: func() (string, error) { return "", errors.New("unavailable") },
		},
	)
	if err == nil {
		t.Fatal("validation failure accepted")
	}
	if _, err := os.Stat(codexReadinessPath(markerDir, CodexRoutingWebSocket)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed validation retained marker: %v", err)
	}
}

func validationWebSocketRunner() testCodexAcceptanceRunner {
	return testCodexAcceptanceRunner(func(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
		authBytes, err := os.ReadFile(filepath.Join(commandEnv(command.env, "CODEX_HOME"), "auth.json"))
		if err != nil {
			return nil, err
		}
		var auth struct {
			Tokens struct {
				AccessToken string `json:"access_token"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(authBytes, &auth); err != nil {
			return nil, err
		}
		if err := requestCodexAcceptanceUserSettings(command, auth.Tokens.AccessToken); err != nil {
			return nil, err
		}
		header := make(http.Header)
		header.Set("Authorization", "Bearer "+auth.Tokens.AccessToken)
		address := "ws" + strings.TrimPrefix(command.endpoint, "http")
		connection, _, err := websocket.DefaultDialer.DialContext(ctx, address, header)
		if err != nil {
			return nil, err
		}
		defer connection.Close()
		prewarm := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session-a\",\"thread_id\":\"thread-a\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
		if err := connection.WriteMessage(websocket.TextMessage, prewarm); err != nil {
			return nil, err
		}
		for {
			_, reply, err := connection.ReadMessage()
			if err != nil {
				return nil, err
			}
			if strings.Contains(string(reply), `"type":"response.completed"`) {
				break
			}
		}
		frame := codexTerminatingWSFrame("installed-turn", `,"previous_response_id":"acceptance-prewarm"`)
		if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
			return nil, err
		}
		for {
			_, reply, err := connection.ReadMessage()
			if err != nil {
				return nil, err
			}
			if strings.Contains(string(reply), `"type":"response.completed"`) {
				break
			}
		}
		return nil, os.WriteFile(command.outputPath, []byte("PONG\n"), 0o600)
	})

}

func TestCodexInstalledWebSocketValidationBudgetAndBinding(t *testing.T) {
	for _, scenario := range []string{"success", "version-failed", "version-mismatch", "changed-executable", "preparation-timeout", "exercise-timeout", "interrupted", "cleanup-exhausted"} {
		t.Run(scenario, func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), "codex")
			if err := os.WriteFile(executable, []byte("synthetic executable"), 0500); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			_, required := DefaultCodexRoutingRequirements("cq-build", "0.146.0")
			prior := testCodexMarker(required)
			prior.CQExecutableSHA256 = ""
			prior.ClientExecutableSHA256 = ""
			prior.ServiceKind = ""
			prior.ServiceIdentitySHA256 = ""
			if err := saveCodexWebSocketReadinessMarkerDurably(dir, prior); err != nil {
				t.Fatal(err)
			}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			cleanup, cancelCleanup := context.WithTimeout(parent, 2*time.Second)
			defer cancelCleanup()
			work, cancelWork := context.WithTimeout(cleanup, time.Second)
			defer cancelWork()
			if scenario == "preparation-timeout" || scenario == "exercise-timeout" {
				cancelWork()
				work, cancelWork = context.WithTimeout(cleanup, 50*time.Millisecond)
				defer cancelWork()
			}
			deps := codexInstalledWebSocketValidationDependencies{cleanupContext: cleanup, resolveExecutable: func() (string, error) { return executable, nil }, captureExecutable: captureCodexInstalledExecutable, runVersion: func(context.Context, string, codexInstalledExecutableProof) ([]byte, error) {
				return []byte("codex-cli 0.146.0\n"), nil
			}, runner: validationWebSocketRunner(), now: time.Now}
			ran := false
			runner := deps.runner
			deps.runner = testCodexAcceptanceRunner(func(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
				ran = true
				switch scenario {
				case "exercise-timeout":
					<-ctx.Done()
					return nil, ctx.Err()
				case "interrupted":
					cancel()
					return nil, ctx.Err()
				}
				output, err := runner.Run(ctx, command)
				if scenario == "changed-executable" {
					if e := os.Chmod(executable, 0700); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(executable, []byte("changed executable"), 0500); e != nil {
						t.Fatal(e)
					}
				}
				if scenario == "cleanup-exhausted" {
					cancelCleanup()
				}
				return output, err
			})
			switch scenario {
			case "version-failed":
				deps.runVersion = func(context.Context, string, codexInstalledExecutableProof) ([]byte, error) {
					return nil, errors.New("synthetic failure")
				}
			case "version-mismatch":
				deps.runVersion = func(context.Context, string, codexInstalledExecutableProof) ([]byte, error) {
					return []byte("codex-cli 0.145.0\n"), nil
				}
			case "preparation-timeout":
				deps.resolveExecutable = func() (string, error) { <-work.Done(); return executable, nil }
			}
			started := time.Now()
			marker, err := runCodexInstalledWebSocketValidationWithDependencies(work, "cq-build", "0.146.0", dir, deps)
			if time.Since(started) > 3*time.Second {
				t.Fatal("operation exceeded original allowance")
			}
			if scenario == "success" {
				if err != nil || marker.ClientBuild != "0.146.0" {
					t.Fatalf("err=%v marker=%+v", err, marker)
				}
				return
			}
			if err == nil || marker.ClientBuild != "" {
				t.Fatal("failed exercise returned success evidence")
			}
			if scenario == "version-mismatch" && !errors.Is(err, ErrCodexValidationBuildMismatch) {
				t.Fatalf("wrong mismatch error: %v", err)
			}
			if scenario == "version-failed" && !errors.Is(err, ErrCodexValidationClientUnavailable) {
				t.Fatalf("wrong version error: %v", err)
			}
			if (scenario == "version-failed" || scenario == "version-mismatch" || scenario == "preparation-timeout") && ran {
				t.Fatal("exercise started after failed preflight")
			}
			if _, err := os.Stat(codexReadinessPath(dir, CodexRoutingWebSocket)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale marker retained: %v", err)
			}
		})
	}
}

func TestCodexInstalledWebSocketValidationCleanupDeadlineClosesServer(t *testing.T) {
	entered := make(chan struct{})
	released := make(chan struct{})
	listener, server, serverErrors, err := startCodexAcceptanceHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(released) }))
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		defer func() { _ = recover() }()
		response, _ := http.Get("http://" + listener.Addr().String())
		if response != nil {
			response.Body.Close()
		}
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := shutdownCodexAcceptanceServerContext(ctx, server); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup err=%v", err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("owned request remained live")
	}
	<-requestDone
	if err := <-serverErrors; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("server err=%v", err)
	}
}

func TestCodexInstalledWebSocketValidationRequiresExplicitCleanup(t *testing.T) {
	if _, err := RunCodexInstalledWebSocketValidationWithCleanup(context.Background(), nil, "test", "0.1.0", "", filepath.Join(t.TempDir(), "state")); err == nil {
		t.Fatal("canonical entry accepted absent cleanup")
	}
}
