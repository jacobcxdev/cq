//go:build darwin

package dots

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise Chromium's existing SPKI launch contract with a production-shaped
// leaf (no chatgpt.com SAN), isolated profile and loopback-only OS sandbox.
// This never launches or attaches to the user's ChatGPT or Chrome session.
func TestChromiumCanonicalAPIOrigin(t *testing.T) {
	executable := os.Getenv("CQ_TEST_CANONICAL_API_CHROMIUM")
	if executable == "" {
		t.Skip("set CQ_TEST_CANONICAL_API_CHROMIUM for isolated installed Chromium acceptance")
	}
	if !filepath.IsAbs(executable) {
		t.Fatal("Chromium executable must be absolute")
	}
	_, settings := preparedInstallationFixture(t)
	certificate, err := tls.LoadX509KeyPair(filepath.Join(settings.RuntimeRoot, "leaf-cert.pem"), filepath.Join(settings.RuntimeRoot, "leaf-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil || leaf.VerifyHostname("chatgpt.com") == nil {
		t.Fatal("fixture must retain production certificate's canonical hostname mismatch")
	}
	probe, _, _ := probeFixtureForTest(t)
	const challenge = `{"proof":"cq-canonical-origin-pinned","target_origin":"https://chatgpt.com"}`
	probe.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == "https://chatgpt.com/" {
			response := probeSuccess(`<html><body>waiting<script>fetch('/backend-api/codex/remote/control/client/refresh/start',{method:'POST',headers:{Authorization:'Bearer native-token'}}).then(r=>r.text()).then(body=>document.body.textContent=body)</script></body></html>`)
			response.Header.Set("Content-Type", "text/html")
			return response, nil
		}
		if r.URL.String() != "https://chatgpt.com/backend-api/codex/remote/control/client/refresh/start" {
			return nil, fmt.Errorf("unexpected synthetic upstream %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer native-token" {
			return nil, fmt.Errorf("synthetic native authorization was lost")
		}
		return probeSuccess(challenge), nil
	})
	local := httptest.NewUnstartedServer(probe)
	local.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	local.StartTLS()
	defer local.Close()
	p, _, connect := realtimeTestProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	p.settings.RuntimeRoot = settings.RuntimeRoot
	p.dependencies.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != local.Listener.Addr().String() {
			return nil, fmt.Errorf("canonical request bypassed private Probe: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	_, port, _ := net.SplitHostPort(local.Listener.Addr().String())
	proxyURL, _ := url.Parse(connect.URL)
	_, proxyPort, _ := net.SplitHostPort(proxyURL.Host)
	for name, value := range map[string]string{"port": port, "realtime-port": proxyPort} {
		if err := os.WriteFile(filepath.Join(settings.RuntimeRoot, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	environment, _, _ := launchFixtureEnvironment(t, settings)
	launchArgs, _, err := prepareLaunch(settings, "http://127.0.0.1:12345", "/synthetic/cq", environment)
	if err != nil {
		t.Fatal(err)
	}
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned-%t", pinned), func(t *testing.T) {
			temporary, err := os.MkdirTemp("/tmp", "cq-chromium-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(temporary) })
			root, err := filepath.EvalSymlinks(temporary)
			if err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{"home", "profile", "tmp"} {
				if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// The OS sandbox replaces Chrome's nested process sandbox for this
			// synthetic-only subprocess; public networking and writes are denied.
			profile := `(version 1) (allow default) (deny network*) (deny file-write*) (allow network-outbound (remote ip "localhost:*")) (allow network-bind (local ip "localhost:*")) (allow network-inbound (local ip "localhost:*")) (allow network-bind (local unix-socket (subpath ` + fmt.Sprintf("%q", root) + `))) (allow file-write* (literal "/dev/null")) (allow file-write* (subpath ` + fmt.Sprintf("%q", root) + `))`
			args := []string{"-p", profile, executable, "--headless=new", "--no-sandbox", "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--disable-extensions", "--disable-quic", "--disable-breakpad", "--disable-crashpad-for-testing", "--crash-dumps-dir=" + filepath.Join(root, "crashes"), "--password-store=basic", "--use-mock-keychain", "--dump-dom", "--virtual-time-budget=3000", "--user-data-dir=" + filepath.Join(root, "profile")}
			for _, arg := range launchArgs {
				if strings.HasPrefix(arg, "--proxy-") || pinned && strings.HasPrefix(arg, "--ignore-certificate-errors-spki-list=") {
					args = append(args, arg)
				}
			}
			args = append(args, "https://chatgpt.com/")
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", args...)
			cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + filepath.Join(root, "home"), "TMPDIR=" + filepath.Join(root, "tmp"), "MAC_CHROMIUM_TMPDIR=" + filepath.Join(root, "tmp")}
			var diagnostics bytes.Buffer
			cmd.Dir, cmd.Stderr = root, &diagnostics
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("isolated Chromium failed: %v: %s", err, diagnostics.String())
			}
			if strings.Contains(string(output), "cq-canonical-origin-pinned") != pinned {
				t.Fatalf("canonical transport ignored pin contract: pinned=%t", pinned)
			}
		})
	}
}
