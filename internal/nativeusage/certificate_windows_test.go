//go:build windows

package nativeusage

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

func TestWindowsPrivateCertificateLifecycle(t *testing.T) {
	root := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	cqRoot := filepath.Join(root, "cq")
	fsys, err := fsutil.NewPrivateDestinationFileSystem(cqRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.EnsureSecureDirectory(fsys, cqRoot); err != nil {
		t.Fatal(err)
	}
	usageRoot := filepath.Join(cqRoot, "usage")
	first, err := Prepare(usageRoot, fsys)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first.CertificatePath, filepath.Join(usageRoot, keyName)} {
		if err := fsutil.ValidateSecureRegularFile(fsys, path); err != nil {
			t.Fatalf("certificate material ACL: %v", err)
		}
	}
	if err := fsutil.ValidateSecureDirectory(fsys, usageRoot); err != nil {
		t.Fatalf("certificate directory ACL: %v", err)
	}
	if config, err := LoadTLS(usageRoot, fsys); err != nil || config != nil {
		t.Fatalf("preparation activated TLS: config=%t err=%v", config != nil, err)
	}
	if err := SetEnabled(usageRoot, fsys, true); err != nil {
		t.Fatal(err)
	}
	config, err := LoadTLS(usageRoot, fsys)
	if err != nil || config == nil {
		t.Fatalf("enabled TLS unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.TLS = config
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(config.Certificates[0].Leaf)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "chatgpt.com", MinVersion: tls.VersionTLS12}}}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("TLS status = %d", response.StatusCode)
	}
	if err := SetEnabled(usageRoot, fsys, false); err != nil {
		t.Fatal(err)
	}
	if config, err := LoadTLS(usageRoot, fsys); err != nil || config != nil {
		t.Fatalf("disabled TLS remains active: config=%t err=%v", config != nil, err)
	}
	second, err := Prepare(usageRoot, fsys)
	if err != nil || second.FingerprintSHA256 != first.FingerprintSHA256 {
		t.Fatalf("certificate changed after disable: %v", err)
	}
}
