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
	var runnerErr error
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
		header := make(http.Header)
		header.Set("Authorization", "Bearer "+auth.Tokens.AccessToken)
		address := "ws" + strings.TrimPrefix(command.endpoint, "http")
		connection, _, err := websocket.DefaultDialer.DialContext(ctx, address, header)
		if err != nil {
			runnerErr = err
			return nil, err
		}
		defer connection.Close()
		frame := codexTerminatingWSFrame("installed-turn", "")
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
		t.Fatalf("validation error = %v; runner error = %v", err, runnerErr)
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
