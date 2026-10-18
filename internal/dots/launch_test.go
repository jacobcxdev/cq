package dots

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func preparedInstallationFixture(t *testing.T) (InstallOptions, *Settings) {
	t.Helper()
	options, pin := installationFixture(t)
	options.Root = filepath.Join(filepath.Dir(options.Root), "dots 'literal; $unexpanded")
	if err := PrivateDirectory(options.Root); err != nil {
		t.Fatal(err)
	}
	root, err := Provision(options.Root, "native")
	if err != nil {
		t.Fatal(err)
	}
	pin.RuntimeRoot, pin.SystemAuth, pin.CQAccounts = root, options.SystemAuth, options.CQAccounts
	pin.SystemAccountID, pin.SystemAlias, pin.IntegrationMode = "fixture-owner", AccountAlias("fixture-owner"), "native"
	pin.Dots = map[string]Dot{}
	if err := pin.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(filepath.Join(filepath.Dir(pin.RuntimeRoot), "settings.json"), &pin); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"port": "32123", "realtime-port": "32124"} {
		if err := os.WriteFile(filepath.Join(pin.RuntimeRoot, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	options.Root = filepath.Dir(pin.RuntimeRoot)
	return options, &pin
}

func launchFixtureEnvironment(t *testing.T, s *Settings) ([]string, []byte, []byte) {
	t.Helper()
	certificate := func(name string) []byte {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
		der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	public, extra := certificate("configured public roots"), certificate("configured node roots")
	publicPath, extraPath := filepath.Join(filepath.Dir(s.RuntimeRoot), "public.pem"), filepath.Join(filepath.Dir(s.RuntimeRoot), "extra.pem")
	for path, value := range map[string][]byte{publicPath: public, extraPath: extra} {
		if err := os.WriteFile(path, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"CODEX_HOME=" + filepath.Dir(s.SystemAuth), "SSL_CERT_FILE=" + publicPath, "NODE_EXTRA_CA_CERTS=" + extraPath, "OPENAI_BASE_URL=https://wrong.invalid", "OPENAI_BASE_URL=https://duplicate.invalid", "NO_PROXY=internal.example,localhost", "UNCHANGED=value with spaces"}, public, extra
}

func TestPrepareLaunchTrustBundlesStayedStableAcrossRelaunches(t *testing.T) {
	_, s := preparedInstallationFixture(t)
	env, public, extra := launchFixtureEnvironment(t, s)
	leaf, err := os.ReadFile(filepath.Join(s.RuntimeRoot, "leaf-cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	first := map[string][]byte{}
	for attempt := 0; attempt < 5; attempt++ {
		_, env, err = prepareLaunch(s, "http://127.0.0.1:12345", "/stable/cq", env)
		if err != nil {
			t.Fatal(err)
		}
		for key, certificates := range map[string][][]byte{"SSL_CERT_FILE": {public, extra, leaf}, "NODE_EXTRA_CA_CERTS": {extra, leaf}} {
			data, err := os.ReadFile(environmentValue(env, key))
			if err != nil {
				t.Fatal(err)
			}
			for _, certificate := range certificates {
				if bytes.Count(data, certificate) != 1 {
					t.Fatalf("%s attempt %d lost or duplicated a configured certificate", key, attempt+1)
				}
			}
			if attempt == 0 {
				first[key] = data
			} else if !bytes.Equal(first[key], data) {
				t.Fatalf("%s grew or changed across relaunch: %d -> %d bytes", key, len(first[key]), len(data))
			}
		}
	}
}

func TestPrepareLaunchPreservedNormalProfileTrustAndCQRouting(t *testing.T) {
	_, s := preparedInstallationFixture(t)
	input, public, extra := launchFixtureEnvironment(t, s)
	before := slices.Clone(input)
	args, env, err := prepareLaunch(s, "http://127.0.0.1:12345", "/stable/cq link", input)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, input) {
		t.Fatal("input environment mutated")
	}
	for key, want := range map[string]string{"CODEX_HOME": filepath.Dir(s.SystemAuth), "OPENAI_BASE_URL": "http://127.0.0.1:12345", "CQ_DOTS_CQ_PATH": "/stable/cq link", "CODEX_API_BASE_URL": "https://localhost:32123/backend-api", "CODEX_APP_SERVER_CHATGPT_BASE_URL": "https://localhost:32123/backend-api", "NO_PROXY": "internal.example,localhost,127.0.0.1", "UNCHANGED": "value with spaces", "CODEX_APP_SERVER_FORCE_CLI": "1"} {
		if got := environmentValue(env, key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
		count := 0
		for _, entry := range env {
			if strings.HasPrefix(entry, key+"=") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%s appeared %d times", key, count)
		}
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "CODEX_ELECTRON_USER_DATA_PATH=") || strings.HasPrefix(entry, "CODEX_SPARKLE_ENABLED=") {
			t.Fatalf("normal desktop profile overridden: %q", entry)
		}
	}
	leaf, _ := os.ReadFile(filepath.Join(s.RuntimeRoot, "leaf-cert.pem"))
	block, _ := pem.Decode(leaf)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	wantArgs := []string{"--cq-dots-runtime=" + s.RuntimeRoot, "--ignore-certificate-errors-spki-list=" + base64.StdEncoding.EncodeToString(digest[:]), "--proxy-server=http://127.0.0.1:32124", "--proxy-bypass-list=localhost;127.0.0.1"}
	if !slices.Equal(args, wantArgs) {
		t.Fatalf("arguments %#v, want %#v", args, wantArgs)
	}
	for key, want := range map[string][]byte{"SSL_CERT_FILE": bytes.Join([][]byte{public, extra, leaf}, nil), "NODE_EXTRA_CA_CERTS": bytes.Join([][]byte{extra, leaf}, nil)} {
		path := environmentValue(env, key)
		got, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if err != nil || statErr != nil || !bytes.Equal(got, want) || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s did not preserve roots privately: %v, %v", key, err, statErr)
		}
	}
	var launch map[string]any
	data, err := ReadOwned(filepath.Join(s.RuntimeRoot, "launch.json"), s.RuntimeRoot)
	if err != nil || json.Unmarshal(data, &launch) != nil || launch["debug_transport"] != "pipe" || launch["debug_port"] != nil || launch["proxy_port"] != float64(32123) || launch["catalogue_revision"] != s.CatalogueRevision() {
		t.Fatalf("launch state %#v, %v", launch, err)
	}
	for _, name := range []string{"user-data", "codex-home", "venv"} {
		if _, err := os.Stat(filepath.Join(s.RuntimeRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected native profile or Python state %s: %v", name, err)
		}
	}
}

func TestPrepareLaunchCloudHelperPreservedLiteralArgumentsWithoutPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper script")
	}
	_, s := preparedInstallationFixture(t)
	input, _, _ := launchFixtureEnvironment(t, s)
	_, env, err := prepareLaunch(s, "http://127.0.0.1:12345", "/stable/cq", input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helperExecutable(s), []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"space value", "'quoted'", "$(touch should-not-exist)", "; literal", ""}
	launcher := environmentValue(env, "CODEX_CLI_PATH")
	data, err := exec.Command(launcher, arguments...).Output()
	want := append([]string{"__dots", "cloud-stdio", filepath.Dir(s.RuntimeRoot)}, arguments...)
	if err != nil || !bytes.Equal(data, []byte(strings.Join(want, "\x00")+"\x00")) {
		t.Fatalf("helper arguments %q, %v", data, err)
	}
	script, err := os.ReadFile(launcher)
	if err != nil || bytes.Contains(script, []byte("python")) || !bytes.Contains(script, []byte("__dots cloud-stdio")) {
		t.Fatalf("launcher %q, %v", script, err)
	}
}

func TestPrepareLaunchRejectedNativeProfileOverrides(t *testing.T) {
	for _, variant := range []string{"foreign home", "relative home", "empty home", "profile", "empty profile", "missing proxy"} {
		t.Run(variant, func(t *testing.T) {
			_, s := preparedInstallationFixture(t)
			env, _, _ := launchFixtureEnvironment(t, s)
			baseURL := "http://127.0.0.1:12345"
			switch variant {
			case "foreign home":
				env = setEnvironment(env, "CODEX_HOME", t.TempDir())
			case "relative home":
				env = setEnvironment(env, "CODEX_HOME", "relative")
			case "empty home":
				env = setEnvironment(env, "CODEX_HOME", "")
			case "profile":
				env = append(env, "CODEX_ELECTRON_USER_DATA_PATH="+t.TempDir())
			case "empty profile":
				env = append(env, "CODEX_ELECTRON_USER_DATA_PATH=")
			case "missing proxy":
				baseURL = ""
			}
			if _, _, err := prepareLaunch(s, baseURL, "/stable/cq", env); err == nil {
				t.Fatal("unsafe native launch accepted")
			}
			if _, err := os.Stat(filepath.Join(s.RuntimeRoot, "launch.json")); !os.IsNotExist(err) {
				t.Fatalf("rejected launch wrote state: %v", err)
			}
		})
	}
}

func TestPrepareLaunchRejectedInvalidPortsCertificatesAndTrust(t *testing.T) {
	for _, variant := range []string{"port", "realtime-port", "leaf-cert.pem", "public trust", "node trust"} {
		t.Run(variant, func(t *testing.T) {
			_, s := preparedInstallationFixture(t)
			env, _, _ := launchFixtureEnvironment(t, s)
			path := filepath.Join(s.RuntimeRoot, variant)
			if variant == "public trust" {
				path = environmentValue(env, "SSL_CERT_FILE")
			} else if variant == "node trust" {
				path = environmentValue(env, "NODE_EXTRA_CA_CERTS")
			}
			if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := prepareLaunch(s, "http://127.0.0.1:12345", "/stable/cq", env); err == nil {
				t.Fatal("invalid launch input accepted")
			}
		})
	}
}
