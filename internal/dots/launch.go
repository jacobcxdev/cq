package dots

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func setEnvironment(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func environmentValue(env []string, key string) string {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return strings.TrimPrefix(entry, key+"=")
		}
	}
	return ""
}

func launchWrite(path string, data []byte, mode os.FileMode) error {
	if mode == 0o600 {
		return fsutil.SecureAtomicWrite(fsutil.OSFileSystem{}, path, data)
	}
	return launcherWrite(path, data, mode)
}

func publicCertificate(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("Public certificate bundle unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8*1024*1024 {
		return nil, errors.New("Public certificate bundle invalid")
	}
	data, err := io.ReadAll(io.LimitReader(file, 8*1024*1024+1))
	if err != nil || len(data) > 8*1024*1024 {
		return nil, errors.New("Public certificate bundle invalid")
	}
	if block, _ := pem.Decode(data); block == nil {
		return nil, errors.New("Public certificate bundle invalid")
	}
	return data, nil
}

// uniquePEMBundle preserves distinct trust material when a relaunch inherits
// the bundles generated for its previous launch.
func uniquePEMBundle(parts ...[]byte) []byte {
	var bundle []byte
	seen := map[string]bool{}
	for _, part := range parts {
		for {
			block, rest := pem.Decode(part)
			if block == nil {
				break
			}
			encoded := pem.EncodeToMemory(block)
			key := string(encoded)
			if !seen[key] {
				bundle = append(bundle, encoded...)
				seen[key] = true
			}
			part = rest
		}
	}
	return bundle
}

// PrepareLaunch preserves the normal profile and public TLS roots while adding
// account-owned transports. It writes only this installation's private state.
func PrepareLaunch(s *Settings, baseURL, executable string) ([]string, []string, error) {
	return prepareLaunch(s, baseURL, executable, os.Environ())
}
func prepareLaunch(s *Settings, baseURL, executable string, env []string) ([]string, []string, error) {
	if err := ValidateRuntimePaths(s.RuntimeRoot); err != nil {
		return nil, nil, err
	}
	native := s.IntegrationMode == "native"
	if native {
		if baseURL == "" {
			return nil, nil, errors.New("CQ proxy base URL unavailable")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, err
		}
		normalHome := environmentValue(env, "CODEX_HOME")
		if normalHome == "" {
			normalHome = filepath.Join(home, ".codex")
		}
		resolved, err := CanonicalPath(normalHome)
		expected, e := CanonicalPath(filepath.Dir(s.SystemAuth))
		if err != nil || e != nil || !filepath.IsAbs(normalHome) || resolved != expected {
			return nil, nil, errors.New("Native Dots launch requires the configured normal CODEX_HOME")
		}
		for _, entry := range env {
			if strings.HasPrefix(entry, "CODEX_ELECTRON_USER_DATA_PATH=") {
				return nil, nil, errors.New("Native Dots launch requires the normal desktop profile")
			}
			if entry == "CODEX_HOME=" {
				return nil, nil, errors.New("Native Dots launch requires the configured normal CODEX_HOME")
			}
		}
	}
	portData, err := ReadOwned(filepath.Join(s.RuntimeRoot, "port"), s.RuntimeRoot)
	if err != nil {
		return nil, nil, err
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(portData)))
	if err != nil || port < 1 || port > 65535 {
		return nil, nil, errors.New("Dot proxy port invalid")
	}
	realtimeData, err := ReadOwned(filepath.Join(s.RuntimeRoot, "realtime-port"), s.RuntimeRoot)
	if err != nil {
		return nil, nil, err
	}
	realtime, err := strconv.Atoi(strings.TrimSpace(string(realtimeData)))
	if err != nil || realtime < 1 || realtime > 65535 {
		return nil, nil, errors.New("Dot realtime port invalid")
	}
	certificate, err := ReadOwned(filepath.Join(s.RuntimeRoot, "leaf-cert.pem"), s.RuntimeRoot)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(certificate)
	if block == nil {
		return nil, nil, errors.New("Dot certificate invalid")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, errors.New("Dot certificate invalid")
	}
	digest := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	spki := base64.StdEncoding.EncodeToString(digest[:])
	args := []string{}
	var debug any
	if native {
		args = append(args, "--cq-dots-runtime="+s.RuntimeRoot)
	} else {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.DebugPort)))
		if err != nil {
			return nil, nil, err
		}
		debugPort := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		debug = debugPort
		args = append(args, "--user-data-dir="+filepath.Join(s.RuntimeRoot, "user-data"), "--remote-debugging-port="+strconv.Itoa(debugPort), "--remote-debugging-address=127.0.0.1")
		env = setEnvironment(env, "CODEX_HOME", filepath.Join(s.RuntimeRoot, "codex-home"))
		env = setEnvironment(env, "CODEX_ELECTRON_USER_DATA_PATH", filepath.Join(s.RuntimeRoot, "user-data"))
		env = setEnvironment(env, "CODEX_SPARKLE_ENABLED", "false")
	}
	transport := "tcp"
	if native {
		transport = "pipe"
	}
	if err = SaveJSON(filepath.Join(s.RuntimeRoot, "launch.json"), map[string]any{"debug_transport": transport, "debug_port": debug, "proxy_port": port, "catalogue_revision": s.CatalogueRevision()}); err != nil {
		return nil, nil, err
	}
	args = append(args, "--ignore-certificate-errors-spki-list="+spki, "--proxy-server=http://127.0.0.1:"+strconv.Itoa(realtime), "--proxy-bypass-list=localhost;127.0.0.1")
	env = setEnvironment(env, "CQ_DOTS_CONFIG", filepath.Join(filepath.Dir(s.RuntimeRoot), "settings.json"))
	env = setEnvironment(env, "CQ_DOTS_CQ_PATH", executable)
	env = setEnvironment(env, "OPENAI_BASE_URL", baseURL)
	for _, key := range []string{"CODEX_API_BASE_URL", "CODEX_APP_SERVER_CHATGPT_BASE_URL"} {
		env = setEnvironment(env, key, "https://localhost:"+strconv.Itoa(port)+"/backend-api")
	}
	publicPath := environmentValue(env, "SSL_CERT_FILE")
	if publicPath == "" {
		publicPath = "/etc/ssl/cert.pem"
	}
	public, err := publicCertificate(publicPath)
	if err != nil {
		return nil, nil, err
	}
	var extra []byte
	if extraPath := environmentValue(env, "NODE_EXTRA_CA_CERTS"); extraPath != "" {
		extra, err = publicCertificate(extraPath)
		if err != nil {
			return nil, nil, err
		}
	}
	trust := uniquePEMBundle(public, extra, certificate)
	additional := uniquePEMBundle(extra, certificate)
	if err = launchWrite(filepath.Join(s.RuntimeRoot, "native-trust.pem"), trust, 0o600); err != nil {
		return nil, nil, err
	}
	if err = launchWrite(filepath.Join(s.RuntimeRoot, "native-extra-ca.pem"), additional, 0o600); err != nil {
		return nil, nil, err
	}
	env = setEnvironment(env, "SSL_CERT_FILE", filepath.Join(s.RuntimeRoot, "native-trust.pem"))
	env = setEnvironment(env, "NODE_EXTRA_CA_CERTS", filepath.Join(s.RuntimeRoot, "native-extra-ca.pem"))
	bypass, seen := []string{}, map[string]bool{}
	for _, host := range append(strings.Split(environmentValue(env, "NO_PROXY"), ","), "localhost", "127.0.0.1") {
		if host != "" && !seen[host] {
			bypass = append(bypass, host)
			seen[host] = true
		}
	}
	env = setEnvironment(env, "NO_PROXY", strings.Join(bypass, ","))
	script := "#!/bin/sh\nexec " + shellQuote(helperExecutable(s)) + " __dots cloud-stdio " + shellQuote(filepath.Dir(s.RuntimeRoot)) + " \"$@\"\n"
	launcher := filepath.Join(s.RuntimeRoot, "cloud-stdio-launch")
	if err = launchWrite(launcher, []byte(script), 0o700); err != nil {
		return nil, nil, err
	}
	env = setEnvironment(env, "CODEX_APP_SERVER_FORCE_CLI", "1")
	env = setEnvironment(env, "CODEX_CLI_PATH", launcher)
	return args, env, nil
}
