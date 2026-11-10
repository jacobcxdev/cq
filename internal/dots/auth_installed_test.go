package dots

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Characterize the installed service's surprising distinction between a valid
// credential file and its cached auth after terminal refresh rejection. The
// fixture has no real credentials and cannot reach non-loopback networks.
func TestInstalledNativeTerminalRefreshInvalidatedMemoryOnly(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("CQ_DOTS_HARNESS_INSTALLED_ACCEPTANCE") != "1" {
		t.Skip("requires opt-in installed macOS acceptance")
	}
	client := os.Getenv("CQ_DOTS_HARNESS_INSTALLED_CODEX")
	if !filepath.IsAbs(client) {
		t.Fatal("CQ_DOTS_HARNESS_INSTALLED_CODEX must name the native executable")
	}
	var calls atomic.Int32
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/oauth/token" {
			t.Error("unexpected synthetic refresh request")
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"refresh_token_reused","message":"synthetic rejection"}}`)
	}))
	defer refresh.Close()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "NO_PROXY=localhost,127.0.0.1", "RUST_LOG=off", "CODEX_REFRESH_TOKEN_URL_OVERRIDE=" + refresh.URL + "/oauth/token"}
	for key, name := range map[string]string{"HOME": "home", "CODEX_HOME": "codex", "TMPDIR": "tmp", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_DATA_HOME": "data"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		env = append(env, key+"="+path)
	}
	if err := os.WriteFile(filepath.Join(root, "codex", "config.toml"), []byte("cli_auth_credentials_store=\"file\"\ncheck_for_update_on_startup=false\n[analytics]\nenabled=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"fixture-user","email":"fixture@example.invalid","exp":4102444800,"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account","chatgpt_user_id":"fixture-user","chatgpt_plan_type":"pro"}}`)) + ".fixture"
	auth, err := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"id_token": jwt, "access_token": jwt, "refresh_token": "fixture-refresh", "account_id": "fixture-account"}, "last_refresh": time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(root, "codex", "auth.json")
	if err := os.WriteFile(authPath, auth, 0o600); err != nil {
		t.Fatal(err)
	}
	profile := `(version 1) (allow default) (deny network*) (deny file-write*) (allow network-outbound (remote ip "localhost:*")) (allow file-write* (literal "/dev/null")) (allow file-write* (subpath ` + strconv.Quote(root) + `))`
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, client, "app-server", "--listen", "stdio://")
	cmd.Dir, cmd.Env, cmd.Stderr = root, env, io.Discard
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
	rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cq_terminal_auth_fixture", "version": "1"}})
	for index, refreshToken := range []bool{false, true, false} {
		raw := rpc.call("getAuthStatus", map[string]bool{"includeToken": true, "refreshToken": refreshToken})
		var result struct {
			AuthMethod string
			AuthToken  *string
		}
		if json.Unmarshal(raw, &result) != nil || result.AuthMethod != "chatgpt" || (result.AuthToken != nil && *result.AuthToken != "") != (index == 0) {
			t.Fatalf("unexpected installed authentication state at step %d", index)
		}
	}
	after, err := os.ReadFile(authPath)
	if err != nil || !bytes.Equal(after, auth) || calls.Load() != 1 {
		t.Fatalf("terminal refresh changed credential file or repeated refresh: read=%v calls=%d", err, calls.Load())
	}
}
