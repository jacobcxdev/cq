package nativeusage

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestPrepareRequiresExplicitActivationAndPreservesCertificate(t *testing.T) {
	fsys := fsutil.NewMemFS()
	root := "/usage"
	before, err := LoadTLS(root, fsys)
	if err != nil || before != nil {
		t.Fatalf("unprepared config = %v, %v", before, err)
	}
	first, err := Prepare(root, fsys)
	if err != nil {
		t.Fatal(err)
	}
	certificate, _ := fsys.ReadFile(first.CertificatePath)
	second, err := Prepare(root, fsys)
	if err != nil || second.FingerprintSHA256 != first.FingerprintSHA256 {
		t.Fatalf("certificate changed: %#v, %v", second, err)
	}
	unchanged, _ := fsys.ReadFile(first.CertificatePath)
	if !bytes.Equal(certificate, unchanged) {
		t.Fatal("preparation replaced certificate bytes")
	}
	for path, permission := range map[string]os.FileMode{root: 0o700, first.CertificatePath: 0o600, filepath.Join(root, keyName): 0o600} {
		stat, err := fsys.Stat(path)
		if err != nil || stat.Mode().Perm() != permission {
			t.Fatalf("mode for %s = %v, %v", path, stat, err)
		}
	}
	if config, err := LoadTLS(root, fsys); err != nil || config != nil {
		t.Fatalf("preparation enabled traffic: %v, %v", config, err)
	}
	if err := SetEnabled(root, fsys, true); err != nil {
		t.Fatal(err)
	}
	config, err := LoadTLS(root, fsys)
	if err != nil || config == nil {
		t.Fatalf("enabled config = %v, %v", config, err)
	}
	if config.MinVersion != tls.VersionTLS12 || !slices.Equal(config.NextProtos, []string{"http/1.1"}) {
		t.Fatalf("unexpected TLS protocol settings: %#v", config)
	}
	leaf := config.Certificates[0].Leaf
	if leaf.IsCA || leaf.KeyUsage != x509.KeyUsageDigitalSignature || !slices.Equal(leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) || !slices.Equal(leaf.DNSNames, first.Hosts) {
		t.Fatal("certificate exceeds the two-host server-leaf scope")
	}
	if err := SetEnabled(root, fsys, false); err != nil {
		t.Fatal(err)
	}
	if config, err := LoadTLS(root, fsys); err != nil || config != nil {
		t.Fatalf("disabled config = %v, %v", config, err)
	}
	if err := SetEnabled(root, fsys, false); err != nil {
		t.Fatal(err)
	}
	after, err := Prepare(root, fsys)
	if err != nil || after.FingerprintSHA256 != first.FingerprintSHA256 {
		t.Fatalf("disable lost certificate identity: %#v, %v", after, err)
	}
}

func TestInspectDoesNotCreateFilesOrReadPrivateKey(t *testing.T) {
	fsys := fsutil.NewMemFS()
	metadata, enabled, err := Inspect("/usage", fsys)
	if err != nil || enabled || metadata.CertificatePath != "" {
		t.Fatalf("absent status = %#v, %v, %v", metadata, enabled, err)
	}
	if _, err := fsys.Stat("/usage"); !os.IsNotExist(err) {
		t.Fatal("inspection created a directory")
	}
	prepared, err := Prepare("/usage", fsys)
	if err != nil {
		t.Fatal(err)
	}
	guarded := noKeyReadFS{fsys}
	metadata, enabled, err = Inspect("/usage", guarded)
	if err != nil || enabled || metadata.FingerprintSHA256 != prepared.FingerprintSHA256 {
		t.Fatalf("prepared status = %#v, %v, %v", metadata, enabled, err)
	}
	if err := SetEnabled("/usage", fsys, true); err != nil {
		t.Fatal(err)
	}
	metadata, enabled, err = Inspect("/usage", guarded)
	if err != nil || !enabled || metadata.FingerprintSHA256 != prepared.FingerprintSHA256 {
		t.Fatalf("enabled status = %#v, %v, %v", metadata, enabled, err)
	}
	if err := fsys.Remove("/usage/" + keyName); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inspect("/usage", guarded); err == nil {
		t.Fatal("partial configured state was reported as absent")
	}
}

type noKeyReadFS struct{ *fsutil.MemFS }

func (fsys noKeyReadFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return noKeyReadDirectory{directory}, nil
}

type noKeyReadDirectory struct{ fsutil.SecureDirectory }

func (directory noKeyReadDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	if name == keyName {
		return nil, os.ErrPermission
	}
	return directory.SecureDirectory.OpenNoFollow(name)
}

func TestCertificateServesOnlyDeclaredHostnames(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := Prepare("/usage", fsys); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("/usage", fsys, true); err != nil {
		t.Fatal(err)
	}
	config, err := LoadTLS("/usage", fsys)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("unexpected HTTP protocol %s", r.Proto)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	server.TLS = config
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(config.Certificates[0].Leaf)
	for _, host := range []string{"chatgpt.com", "ws.chatgpt.com", "other.chatgpt.com", "example.org"} {
		t.Run(host, func(t *testing.T) {
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			response, err := client.Get(server.URL)
			if !IsHost(host) {
				if err == nil {
					response.Body.Close()
					t.Fatal("certificate accepted unrelated hostname")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d", response.StatusCode)
			}
		})
	}
}

func TestTrustedLeafCannotIssueAdditionalCertificates(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := Prepare("/usage", fsys); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("/usage", fsys, true); err != nil {
		t.Fatal(err)
	}
	config, err := LoadTLS("/usage", fsys)
	if err != nil {
		t.Fatal(err)
	}
	pair := config.Certificates[0]
	childKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	child := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"example.org"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, child, pair.Leaf, &childKey.PublicKey, pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(pair.Leaf)
	if _, err := parsed.Verify(x509.VerifyOptions{Roots: roots, DNSName: "example.org"}); err == nil {
		t.Fatal("trusted server leaf was accepted as an issuer")
	}
}

func TestPrepareRejectsIncompletePairWithoutReplacingIt(t *testing.T) {
	for _, missing := range []string{certificateName, keyName} {
		t.Run(missing, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if _, err := Prepare("/usage", fsys); err != nil {
				t.Fatal(err)
			}
			other := keyName
			if missing == keyName {
				other = certificateName
			}
			previous, _ := fsys.ReadFile(filepath.Join("/usage", other))
			if err := fsys.Remove(filepath.Join("/usage", missing)); err != nil {
				t.Fatal(err)
			}
			if _, err := Prepare("/usage", fsys); err == nil {
				t.Fatal("incomplete pair was accepted")
			}
			now, _ := fsys.ReadFile(filepath.Join("/usage", other))
			if !bytes.Equal(now, previous) {
				t.Fatal("incomplete pair was silently replaced")
			}
			if _, err := fsys.Stat(filepath.Join("/usage", missing)); !os.IsNotExist(err) {
				t.Fatal("missing half was regenerated")
			}
		})
	}
}

func TestLoadRejectsChangedCertificateAndInvalidMarker(t *testing.T) {
	fsys := fsutil.NewMemFS()
	first, err := Prepare("/usage", fsys)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("/usage", fsys, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare("/replacement", fsys); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{certificateName, keyName} {
		data, _ := fsys.ReadFile(filepath.Join("/replacement", name))
		if err := fsys.WriteFile(filepath.Join("/usage", name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadTLS("/usage", fsys); err == nil {
		t.Fatal("marker accepted a replacement certificate")
	}
	for _, data := range []string{`{`, `null`, `{}`, `{"fingerprint_sha256":"` + first.FingerprintSHA256 + `","other":true}`, `{}` + `{}`, string(bytes.Repeat([]byte("x"), 1025))} {
		if err := fsys.WriteFile("/usage/"+enabledName, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTLS("/usage", fsys); err == nil {
			t.Fatalf("invalid marker accepted: %.80s", data)
		}
		if _, _, err := Inspect("/usage", fsys); err == nil {
			t.Fatalf("invalid marker accepted by inspection: %.80s", data)
		}
	}
}

func TestPrepareRejectsMalformedMaterialWithoutRotation(t *testing.T) {
	for _, name := range []string{certificateName, keyName} {
		for _, corrupt := range []string{"invalid PEM", "oversized", "multiple PEM blocks"} {
			t.Run(name+"/"+corrupt, func(t *testing.T) {
				fsys := fsutil.NewMemFS()
				if _, err := Prepare("/usage", fsys); err != nil {
					t.Fatal(err)
				}
				path := "/usage/" + name
				original, _ := fsys.ReadFile(path)
				var data []byte
				switch corrupt {
				case "invalid PEM":
					data = []byte("invalid fixture")
				case "oversized":
					data = bytes.Repeat([]byte("x"), materialLimit+1)
				case "multiple PEM blocks":
					data = append(append([]byte(nil), original...), original...)
				}
				if err := fsys.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := Prepare("/usage", fsys); err == nil {
					t.Fatal("malformed material accepted")
				}
				after, _ := fsys.ReadFile(path)
				if !bytes.Equal(after, data) {
					t.Fatal("malformed material was replaced")
				}
			})
		}
	}
}

func TestPrepareDoesNotRegenerateEnabledMissingPair(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := Prepare("/usage", fsys); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("/usage", fsys, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{certificateName, keyName} {
		if err := fsys.Remove("/usage/" + name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Prepare("/usage", fsys); err == nil {
		t.Fatal("enabled missing certificate was regenerated")
	}
	if _, err := fsys.Stat("/usage/" + certificateName); !os.IsNotExist(err) {
		t.Fatal("missing certificate was recreated")
	}
}

func TestPrepareAndLoadRejectInvalidCertificateMaterial(t *testing.T) {
	cases := map[string]func(*x509.Certificate){
		"expired":       func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Hour) },
		"future":        func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Hour) },
		"CA":            func(c *x509.Certificate) { c.IsCA = true },
		"signing usage": func(c *x509.Certificate) { c.KeyUsage |= x509.KeyUsageCertSign },
		"extra purpose": func(c *x509.Certificate) { c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageClientAuth) },
		"extra host":    func(c *x509.Certificate) { c.DNSNames = append(c.DNSNames, "example.org") },
		"wildcard":      func(c *x509.Certificate) { c.DNSNames[1] = "*.chatgpt.com" },
		"duplicate host": func(c *x509.Certificate) {
			c.DNSNames[1] = c.DNSNames[0]
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if err := fsys.MkdirAll("/usage", 0o700); err != nil {
				t.Fatal(err)
			}
			der := writeTestPair(t, fsys, mutate)
			fingerprint := sha256.Sum256(der)
			marker, _ := json.Marshal(enabledState{FingerprintSHA256: hex.EncodeToString(fingerprint[:])})
			if err := fsys.WriteFile("/usage/"+enabledName, marker, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Prepare("/usage", fsys); err == nil {
				t.Fatal("invalid certificate accepted by preparation")
			}
			if _, err := LoadTLS("/usage", fsys); err == nil {
				t.Fatal("invalid certificate accepted by loading")
			}
			if _, _, err := Inspect("/usage", fsys); err == nil {
				t.Fatal("invalid certificate accepted by inspection")
			}
		})
	}
}

func TestPrivateMaterialBoundaries(t *testing.T) {
	for _, badFile := range []string{certificateName, keyName, enabledName} {
		t.Run(badFile, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if _, err := Prepare("/usage", fsys); err != nil {
				t.Fatal(err)
			}
			if err := SetEnabled("/usage", fsys, true); err != nil {
				t.Fatal(err)
			}
			if err := fsys.Chmod("/usage/"+badFile, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadTLS("/usage", fsys); err == nil {
				t.Fatal("publicly readable material accepted")
			}
			if _, _, err := Inspect("/usage", fsys); err == nil {
				t.Fatal("publicly readable material accepted by inspection")
			}
		})
	}
	fsys := fsutil.NewMemFS()
	if err := SetEnabled("/usage", fsys, false); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("/usage", fsys, true); err == nil {
		t.Fatal("unprepared directory was enabled")
	}
	for _, root := range []string{"", ".", "usage", "/", "/usage/../usage"} {
		if _, err := Prepare(root, fsys); err == nil {
			t.Fatalf("invalid root accepted: %q", root)
		}
	}
	if _, err := Prepare("/usage", nil); err == nil {
		t.Fatal("nil filesystem accepted")
	}
}

func TestPrepareRejectsCertificateSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires Unix file permissions")
	}
	root := filepath.Join(t.TempDir(), "usage")
	fsys := fsutil.OSFileSystem{}
	if _, err := Prepare(root, fsys); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, certificateName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, certificateName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(root, fsys); err == nil {
		t.Fatal("certificate symlink accepted")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "outside fixture" {
		t.Fatal("outside file changed")
	}
}

func writeTestPair(t *testing.T, fsys fsutil.FileSystem, mutate func(*x509.Certificate)) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test usage"}, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: time.Now().Add(2 * time.Hour), BasicConstraintsValid: true, DNSNames: []string{"chatgpt.com", "ws.chatgpt.com"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	mutate(certificate)
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{certificateName: {Type: "CERTIFICATE", Bytes: der}, keyName: {Type: "PRIVATE KEY", Bytes: private}} {
		if err := fsys.WriteFile("/usage/"+name, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return der
}
