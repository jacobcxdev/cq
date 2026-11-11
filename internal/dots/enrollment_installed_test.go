package dots

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This uses the installed app-server with synthetic file credentials, private
// state and a loopback-only sandbox. No real account or model request is made.
func TestInstalledEnrollmentWorkspaceOrigin(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("CQ_DOTS_HARNESS_INSTALLED_ACCEPTANCE") != "1" {
		t.Skip("requires opt-in installed macOS acceptance")
	}
	client := os.Getenv("CQ_DOTS_HARNESS_INSTALLED_CODEX")
	if !filepath.IsAbs(client) {
		t.Fatal("CQ_DOTS_HARNESS_INSTALLED_CODEX must name the native executable")
	}
	for _, origin := range []string{"https://chatgpt.com", "NO_CONSTRAINT", ""} {
		t.Run("origin-"+origin, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/backend-api/wham/accounts/check" {
					_, _ = io.WriteString(w, `{}`)
					return
				}
				account := map[string]any{"id": "fixture-account", "account_routing_override": "NO_CONSTRAINT"}
				if origin != "" {
					account["workspace_backend_origin"] = origin
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []any{account}, "default_account_id": "fixture-account"})
			}))
			defer backend.Close()
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "NO_PROXY=127.0.0.1,localhost", "CI=1"}
			for key, name := range map[string]string{"HOME": "home", "CODEX_HOME": "codex", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_DATA_HOME": "data", "TMPDIR": "tmp"} {
				path := filepath.Join(root, name)
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				env = append(env, key+"="+path)
			}
			config := fmt.Sprintf("cli_auth_credentials_store = \"file\"\nchatgpt_base_url = %q\ncheck_for_update_on_startup = false\n", backend.URL+"/backend-api")
			if err := os.WriteFile(filepath.Join(root, "codex", "config.toml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"fixture-user","email":"fixture@example.invalid","exp":4102444800,"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account","chatgpt_user_id":"fixture-user","chatgpt_plan_type":"pro"}}`)) + ".fixture"
			auth, err := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"id_token": jwt, "access_token": jwt, "refresh_token": "fixture-nonsecret-refresh", "account_id": "fixture-account"}, "last_refresh": time.Now().UTC().Format(time.RFC3339)})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "codex", "auth.json"), auth, 0o600); err != nil {
				t.Fatal(err)
			}
			profile := `(version 1) (allow default) (deny network*) (deny file-write*) (allow network-outbound (remote ip "localhost:*")) (allow network-bind (local ip "localhost:*")) (allow file-write* (literal "/dev/null")) (allow file-write* (subpath ` + fmt.Sprintf("%q", root) + `))`
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, client, "app-server", "--listen", "stdio://")
			var diagnostics bytes.Buffer
			cmd.Dir, cmd.Env, cmd.Stderr = root, env, &diagnostics
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("synthetic installed app-server: %s", diagnostics.String())
				}
			})
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			rpc := &harnessInstalledRPC{t: t, encoder: json.NewEncoder(stdin), decoder: json.NewDecoder(stdout)}
			rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cq_enrollment_fixture", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}})
			rpc.id++
			if err := rpc.encoder.Encode(map[string]any{"id": rpc.id, "method": "account/read", "params": map[string]bool{"refreshToken": false}}); err != nil {
				t.Fatal(err)
			}
			var message harnessInstalledMessage
			for message.ID != rpc.id {
				message = rpc.next()
			}
			if origin == "" {
				if !strings.Contains(string(message.Error), "workspace routing discovery missing backend origin") {
					t.Fatalf("missing origin did not fail closed: %s", message.Error)
				}
				return
			}
			if origin == "NO_CONSTRAINT" {
				// Native routing validates its effective bootstrap origin for this
				// sentinel. Our HTTP-only fixture must fail HTTPS validation rather
				// than silently using the public origin or omitting routing.
				if !strings.Contains(string(message.Error), "workspace backend must use an HTTPS origin without credentials") {
					t.Fatalf("unconstrained workspace ignored effective transport origin: %s", message.Error)
				}
				return
			}
			var result struct {
				WorkspaceRouting struct{ BackendOrigin, ChatgptAccountID, AccountRoutingOverride string }
			}
			if len(message.Error) != 0 || json.Unmarshal(message.Result, &result) != nil || result.WorkspaceRouting.BackendOrigin != "https://chatgpt.com" || result.WorkspaceRouting.ChatgptAccountID != "fixture-account" || result.WorkspaceRouting.AccountRoutingOverride != "NO_CONSTRAINT" {
				t.Fatalf("loopback transport changed public workspace identity: %s error %s", message.Result, message.Error)
			}
		})
	}
}
