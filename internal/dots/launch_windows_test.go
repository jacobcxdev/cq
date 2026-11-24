//go:build windows

package dots

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWindowsDotsCloudWrapperAndStockInvocation(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	root := filepath.Join(anchor, "Dots 'literal; $unexpanded")
	runtime := filepath.Join(root, "runtime")
	s := &Settings{RuntimeRoot: runtime, AppPath: filepath.Join(root, "package")}
	executable := WindowsCloudWrapperPath(root)
	if _, _, err := prepareCloudLauncher(s, nil); err == nil {
		t.Fatal("accepted a missing Windows runtime executable")
	}
	if err := PrivateDirectory(runtime); err != nil {
		t.Fatal(err)
	}
	if err := launchWrite(executable, []byte("launcher fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"codex_cli_path=old", "codex_app_runtime_cli_path=old", "cq_dots_windows_cloud_root=old", "OPENAI_BASE_URL=http://127.0.0.1:4242", "USER_SETTINGS=keep"}
	wrapper, prepared, err := prepareCloudLauncher(s, env)
	if err != nil || wrapper != executable || environmentValue(prepared, WindowsCloudRootEnv) != root {
		t.Fatalf("launcher %q, %q, %v", wrapper, prepared, err)
	}
	if _, err := os.Stat(filepath.Join(runtime, "cloud-stdio-launch")); !os.IsNotExist(err) {
		t.Fatalf("unexpected POSIX script: %v", err)
	}
	stock := filepath.Join(s.AppPath, "app", "resources", "codex.exe")
	for _, test := range []struct {
		args   []string
		bridge bool
	}{
		{[]string{"--version"}, false},
		{[]string{"app-server", "--print-startup-config"}, false},
		{[]string{"app-server", "generate-json-schema"}, false},
		{[]string{"app-server", "--listen", "ws://127.0.0.1:5000"}, false},
		{[]string{"app-server"}, true},
		{[]string{"-c", "literal=\"user; $(setting)\"", "app-server", "--listen", "stdio://"}, true},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			path, args, child, bridge, err := cloudInvocation(s, test.args, prepared)
			if err != nil || path != stock || bridge != test.bridge || len(args) < len(test.args) || !slices.Equal(args[:len(test.args)], test.args) {
				t.Fatalf("invocation %q, %q, bridge %v, %v", path, args, bridge, err)
			}
			for _, key := range []string{"CODEX_CLI_PATH", "CODEX_APP_RUNTIME_CLI_PATH"} {
				if environmentValue(child, key) != stock {
					t.Fatalf("stock child %s not pinned", key)
				}
			}
			for _, entry := range child {
				if matchesEnvironmentKey(entry, WindowsCloudRootEnv) || entry == "codex_cli_path=old" {
					t.Fatalf("stock child inherited the wrapper or stale environment: %q", entry)
				}
			}
		})
	}
}

func TestWindowsDotsPrepareLaunchUsesPrivateRuntimeAndNativeProfile(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	root, home := filepath.Join(anchor, "dots"), filepath.Join(anchor, "codex home")
	runtime := filepath.Join(root, "runtime")
	for _, dir := range []string{root, runtime, home} {
		if err := PrivateDirectory(dir); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	leaf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	for name, data := range map[string][]byte{
		"port": []byte("32123"), "realtime-port": []byte("32124"), "leaf-cert.pem": leaf,
		"cq-dots-runtime.exe": []byte("launcher fixture"),
	} {
		if err := launchWrite(filepath.Join(runtime, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings := &Settings{
		RuntimeRoot: runtime, SystemAuth: filepath.Join(home, "auth.json"), IntegrationMode: "native",
	}
	base := "http://127.0.0.1:4242"
	args, env, err := prepareLaunch(settings, base, filepath.Join(anchor, "cq.exe"), []string{"codeX_home=" + home})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"CODEX_HOME": home, "CODEX_CLI_PATH": WindowsCloudWrapperPath(root), "OPENAI_BASE_URL": base,
		WindowsCloudRootEnv: root, "CODEX_API_BASE_URL": "https://chatgpt.com/backend-api",
		"CODEX_APP_SERVER_CHATGPT_BASE_URL": "https://localhost:32123/backend-api",
	} {
		if got := environmentValue(env, key); got != want {
			t.Fatalf("%s: %q, want %q", key, got, want)
		}
	}
	if !slices.Contains(args, "--proxy-server=http://127.0.0.1:32124") || environmentValue(env, "SSL_CERT_FILE") != "" {
		t.Fatalf("missing private CONNECT route or system trust overridden: %q", args)
	}
	for _, key := range []string{"CODEX_CA_CERTIFICATE", "NODE_EXTRA_CA_CERTS"} {
		data, err := ReadOwned(environmentValue(env, key), runtime)
		if err != nil || !bytes.Equal(data, leaf) {
			t.Fatalf("private launch certificate for %s: %v", key, err)
		}
	}
	var state struct {
		DebugTransport string `json:"debug_transport"`
	}
	data, err := ReadOwned(filepath.Join(runtime, "launch.json"), runtime)
	if err != nil || json.Unmarshal(data, &state) != nil || state.DebugTransport != "pipe" {
		t.Fatalf("private Windows launch state: %s, %v", data, err)
	}
}

func TestWindowsDotsCustomTrustDoesNotReplacePlatformRoots(t *testing.T) {
	if data, err := launchPublicTrust(nil, "CODEX_CA_CERTIFICATE"); err != nil || len(data) != 0 {
		t.Fatalf("Windows launch required an external system PEM: %d bytes, %v", len(data), err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	additional := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "extra.pem")
	if err := os.WriteFile(path, additional, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_CA_CERTIFICATE", path)
	t.Setenv("SSL_CERT_FILE", path+".unused")
	if data, err := launchPublicTrust(os.Environ(), "CODEX_CA_CERTIFICATE"); err != nil || !bytes.Equal(data, additional) {
		t.Fatalf("Codex custom PEM changed: %d bytes, %v", len(data), err)
	}
	config, err := cloudPublicTLS()
	if err != nil || config.RootCAs != nil || config.MinVersion != tls.VersionTLS12 {
		t.Fatalf("Windows cloud trust did not select native verification: %+v, %v", config, err)
	}
	transport := &http.Transport{TLSClientConfig: config}
	defer transport.CloseIdleConnections()
	if response, err := (&http.Client{Transport: transport}).Get(server.URL); err == nil {
		_ = response.Body.Close()
		t.Fatal("private application roots leaked into the upstream transport")
	}
}
