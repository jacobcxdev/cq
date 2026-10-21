//go:build !windows

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

type codexLiveAcceptanceModelSelection struct {
	model    string
	snapshot modelregistry.Snapshot
}

// Freeze the native recommendation once per test process. Direct and proxied
// traffic must use the same model even if the upstream catalogue changes.
var codexLiveAcceptanceSelectedModel = sync.OnceValues(discoverCodexLiveAcceptanceModel)

func codexLiveAcceptanceModel() (string, error) {
	// Synthetic acceptance tests keep their reviewed fixture model and never
	// consult installed clients, credentials or the network.
	live := false
	for _, key := range []string{
		"CQ_RUN_CODEX_LIVE_UPSTREAM_ACCEPTANCE", "CQ_RUN_CODEX_LIVE_LATENCY_ACCEPTANCE",
		"CQ_RUN_CODEX_RELEASE_EXECUTABLE_ACCEPTANCE", "CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE",
	} {
		live = live || os.Getenv(key) == "1"
	}
	if !live {
		return "gpt-5.6-sol", nil
	}
	selection, err := codexLiveAcceptanceSelectedModel()
	return selection.model, err
}

func codexLiveAcceptanceExecArguments(baseURL, work, outputPath string, webSocket bool) ([]string, error) {
	model, err := codexLiveAcceptanceModel()
	if err != nil {
		return nil, err
	}
	args := codexAcceptanceExecArgumentsForTransport(baseURL, work, outputPath, webSocket)
	for index := range args {
		if args[index] == "--model" && index+1 < len(args) {
			args[index+1] = model
			return args, nil
		}
	}
	return nil, errors.New("live Codex acceptance model argument is missing")
}

func discoverCodexLiveAcceptanceModel() (codexLiveAcceptanceModelSelection, error) {
	var selection codexLiveAcceptanceModelSelection
	credential, err := readCodexLiveAcceptanceCredential(os.Getenv("CQ_CODEX_LIVE_AUTH_FILE"))
	if err != nil {
		return selection, err
	}
	client, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		return selection, err
	}
	root, err := os.MkdirTemp("/tmp", "cq-codex-live-model-")
	if err != nil {
		return selection, errors.New("create live Codex model discovery isolation")
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return selection, errors.New("resolve live Codex model discovery isolation")
	}
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(root, "codex-home")
	tmp := filepath.Join(root, "tmp")
	cache := filepath.Join(root, "cache")
	config := filepath.Join(root, "config")
	for _, directory := range []string{home, codexHome, tmp, cache, config} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return selection, errors.New("create live Codex model discovery directory")
		}
	}
	material := credential.material
	encoded, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt", "OPENAI_API_KEY": nil,
		"tokens": map[string]string{"access_token": material.AccessToken, "id_token": material.IDToken, "account_id": material.AccountID},
	})
	if err != nil {
		return selection, errors.New("encode live Codex model discovery credential")
	}
	err = os.WriteFile(filepath.Join(codexHome, "auth.json"), encoded, 0o600)
	clearBytes(encoded)
	if err != nil {
		return selection, errors.New("write live Codex model discovery credential")
	}
	catalogue, err := readCodexLiveAcceptanceModelCache(os.Getenv("CQ_CODEX_LIVE_AUTH_FILE"))
	if err != nil {
		return selection, err
	}
	err = os.WriteFile(filepath.Join(codexHome, "models_cache.json"), catalogue, 0o600)
	clearBytes(catalogue)
	if err != nil {
		return selection, errors.New("write live Codex model discovery catalogue")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// The native client interprets a copy of its existing catalogue. This
	// metadata-only process cannot access the network or write the real profile.
	profile := codexAcceptanceSandboxBase +
		` (allow file-write* (literal "/dev/null"))` +
		` (allow file-write* (subpath ` + strconv.Quote(root) + `))`
	child := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, client,
		"app-server", "--stdio", "--strict-config",
		"-c", "openai_base_url="+strconv.Quote(DefaultCodexUpstream),
		"-c", `cli_auth_credentials_store="file"`,
		"-c", `analytics.enabled=false`, "-c", `features.plugins=false`,
		"-c", `check_for_update_on_startup=false`,
	)
	child.Env = codexAcceptanceBaseEnvironment(home, codexHome, tmp, cache, config)
	child.Dir = root
	child.WaitDelay = 2 * time.Second
	stdin, err := child.StdinPipe()
	if err != nil {
		return selection, errors.New("open live Codex model discovery input")
	}
	defer stdin.Close()
	stdout, err := child.StdoutPipe()
	if err != nil {
		return selection, errors.New("open live Codex model discovery output")
	}
	if err := child.Start(); err != nil {
		return selection, errors.New("start live Codex model discovery")
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
	if err := encoder.Encode(map[string]any{
		"id": 1, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]string{"name": "cq-release-model-discovery", "version": "1"}},
	}); err != nil {
		return selection, errors.New("initialise live Codex model discovery")
	}
	if _, err := awaitCodexAppServerResponse(decoder, 1); err != nil {
		return selection, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized"}); err != nil {
		return selection, errors.New("acknowledge live Codex model discovery")
	}
	if err := encoder.Encode(map[string]any{
		"id": 2, "method": "model/list", "params": map[string]any{"limit": 100, "includeHidden": false},
	}); err != nil {
		return selection, errors.New("request live Codex default model")
	}
	response, err := awaitCodexAppServerResponse(decoder, 2)
	if err != nil {
		return selection, err
	}
	selection.model, err = decodeCodexLiveAcceptanceDefaultModel(response)
	if err != nil {
		return selection, err
	}
	selection.snapshot, err = codexLiveAcceptanceModelSnapshot(filepath.Join(codexHome, "auth.json"), selection.model)
	if err != nil {
		return selection, err
	}
	fmt.Fprintf(os.Stderr, "Codex live acceptance model: %s (native recommended default; fixed for this run)\n", selection.model)
	return selection, nil
}

func decodeCodexLiveAcceptanceDefaultModel(body json.RawMessage) (string, error) {
	var response struct {
		Data []struct {
			Model     string `json:"model"`
			IsDefault bool   `json:"isDefault"`
			Hidden    bool   `json:"hidden"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", errors.New("decode live Codex default model")
	}
	selected := ""
	for _, model := range response.Data {
		if !model.IsDefault {
			continue
		}
		if selected != "" || model.Hidden || model.Model == "" || strings.TrimSpace(model.Model) != model.Model {
			return "", errors.New("live Codex default model is invalid or ambiguous")
		}
		selected = model.Model
	}
	if selected == "" {
		return "", errors.New("live Codex default model is unavailable")
	}
	return selected, nil
}

func TestCodexLiveAcceptanceDefaultModel(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "native default", body: `{"data":[{"model":"older-sol","isDefault":false},{"model":"newer-sol","isDefault":true}]}`, want: "newer-sol"},
		{name: "order independent", body: `{"data":[{"model":"newer-sol","isDefault":true},{"model":"older-sol","isDefault":false}]}`, want: "newer-sol"},
		{name: "no default", body: `{"data":[{"model":"older-sol","isDefault":false}]}`},
		{name: "ambiguous default", body: `{"data":[{"model":"first","isDefault":true},{"model":"second","isDefault":true}]}`},
		{name: "hidden default", body: `{"data":[{"model":"hidden","isDefault":true,"hidden":true}]}`},
		{name: "empty model", body: `{"data":[{"model":"","isDefault":true}]}`},
		{name: "padded model", body: `{"data":[{"model":" model ","isDefault":true}]}`},
		{name: "invalid response", body: `{"data":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			model, err := decodeCodexLiveAcceptanceDefaultModel(json.RawMessage(test.body))
			if test.want == "" {
				if err == nil {
					t.Fatalf("selected %q from invalid catalogue", model)
				}
				return
			}
			if err != nil || model != test.want {
				t.Fatalf("selected model = %q, %v; want %q", model, err, test.want)
			}
		})
	}
}

func TestCodexLiveAcceptanceModelDiscovery(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_LIVE_UPSTREAM_ACCEPTANCE") != "1" {
		t.Skip("live Codex model discovery requires explicit opt-in")
	}
	selection, err := codexLiveAcceptanceSelectedModel()
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.snapshot.Entries) != 1 || selection.snapshot.Entries[0].ID != selection.model {
		t.Fatal("native default and frozen model catalogue disagree")
	}
	t.Logf("selected installed Codex default: %s", selection.model)
}
