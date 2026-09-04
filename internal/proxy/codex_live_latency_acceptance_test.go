//go:build darwin

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

const codexLiveLatencyAcceptanceEnvironment = "CQ_RUN_CODEX_LIVE_LATENCY_ACCEPTANCE"

func TestCodexLiveProxyLatencyMatchesDirect(t *testing.T) {
	if os.Getenv(codexLiveLatencyAcceptanceEnvironment) != "1" {
		t.Skip("live Codex latency acceptance requires explicit opt-in")
	}
	credential, err := readCodexLiveAcceptanceCredential(os.Getenv("CQ_CODEX_LIVE_AUTH_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatal(err)
	}
	client, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatal(err)
	}
	listener, supervisor, traffic := newCodexLiveNormalAcceptanceServer(t, credential)
	if listener.URL == "http://127.0.0.1:19280" || supervisor.TrafficMode() != TrafficModeNormal || !supervisor.AdmissionReady() {
		t.Fatal("live Codex latency acceptance did not use isolated normal proxy")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	for _, webSocket := range []bool{false, true} {
		direct := make([]time.Duration, 0, 3)
		proxied := make([]time.Duration, 0, 3)
		for sample := range 3 {
			output := fmt.Sprintf("LIVE-LATENCY-%t-%d", webSocket, sample)
			directIsolation := newCodexTaskAffinityAcceptanceIsolationDirectories(t)
			if err := writeCodexLiveLatencyAuth(filepath.Join(directIsolation.codexHome, "auth.json"), credential); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if err := runCodexLiveDirectLatencyTurn(ctx, client, directIsolation, webSocket, output); err != nil {
				t.Fatalf("direct Codex latency sample: %v", err)
			}
			direct = append(direct, time.Since(started))

			proxyIsolation := newCodexTaskAffinityAcceptanceIsolation(t, credential.localToken)
			beforeWebSockets, beforeResponses, _ := traffic.snapshot()
			started = time.Now()
			if err := runCodexTaskAffinityAcceptanceTurnForTransport(ctx, codexTaskAffinityAcceptanceRunner{}, client, listener.URL, proxyIsolation, false, webSocket, output); err != nil {
				t.Fatalf("proxied Codex latency sample: %v", err)
			}
			proxied = append(proxied, time.Since(started))
			afterWebSockets, afterResponses, _ := traffic.snapshot()
			if webSocket {
				if afterWebSockets-beforeWebSockets != 1 || afterResponses != beforeResponses {
					t.Fatalf("proxied WebSocket sample traffic = %d handshakes/%d HTTP requests, want 1/0", afterWebSockets-beforeWebSockets, afterResponses-beforeResponses)
				}
			} else if afterResponses-beforeResponses != 1 || afterWebSockets != beforeWebSockets {
				t.Fatalf("proxied HTTP sample traffic = %d handshakes/%d HTTP requests, want 0/1", afterWebSockets-beforeWebSockets, afterResponses-beforeResponses)
			}
		}
		directMedian := medianCodexLiveLatency(direct)
		proxyMedian := medianCodexLiveLatency(proxied)
		maximumProxyMedian := directMedian + max(2*time.Second, directMedian/2)
		t.Logf("Codex live latency websocket=%t direct=%s proxied=%s overhead=%s limit=%s", webSocket, directMedian, proxyMedian, proxyMedian-directMedian, maximumProxyMedian-directMedian)
		if proxyMedian > maximumProxyMedian {
			t.Fatalf("proxied median %s exceeds direct median %s by more than %s", proxyMedian, directMedian, maximumProxyMedian-directMedian)
		}
	}
}

func runCodexLiveDirectLatencyTurn(ctx context.Context, client codexInstalledExecutableProof, isolation codexTaskAffinityAcceptanceIsolation, webSocket bool, output string) error {
	before, err := captureCodexInstalledExecutable(client.path)
	if err != nil || before != client {
		return fmt.Errorf("direct Codex executable changed")
	}
	outputPath := filepath.Join(isolation.root, strings.ToLower(output)+".txt")
	args := codexAcceptanceExecArgumentsForTransport(DefaultCodexUpstream, isolation.work, outputPath, webSocket)
	args = slices.DeleteFunc(args, func(value string) bool { return value == "--ephemeral" })
	args[len(args)-1] = "Reply with exactly " + output + " and no other text."
	environment := append(codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config),
		"XDG_DATA_HOME="+isolation.data,
		"OPENAI_BASE_URL="+DefaultCodexUpstream,
	)
	if _, err := (osCodexAcceptanceRunner{}).Run(ctx, codexAcceptanceCommand{
		executable: client.path,
		args:       args,
		env:        environment,
		dir:        isolation.work,
		endpoint:   DefaultCodexUpstream + legacyCodexResponsesPath,
		outputPath: outputPath,
	}); err != nil {
		return err
	}
	after, err := captureCodexInstalledExecutable(client.path)
	if err != nil || after != client {
		return fmt.Errorf("direct Codex executable changed")
	}
	result, err := os.ReadFile(outputPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(result)) != output {
		return fmt.Errorf("direct Codex output mismatch")
	}
	return nil
}

func writeCodexLiveLatencyAuth(path string, credential codexLiveAcceptanceCredential) error {
	material := credential.material
	if material.AccessToken == "" || material.IDToken == "" || material.AccountID == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("live Codex latency credential is incomplete")
	}
	encoded, err := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"last_refresh":   time.Now().UTC().Format(time.RFC3339Nano),
		"tokens": map[string]any{
			"access_token":  material.AccessToken,
			"refresh_token": "",
			"id_token":      material.IDToken,
			"account_id":    material.AccountID,
		},
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func medianCodexLiveLatency(samples []time.Duration) time.Duration {
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	return ordered[len(ordered)/2]
}
