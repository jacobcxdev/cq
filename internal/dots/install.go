package dots

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

const (
	QualifiedAppVersion  = "26.1013.40934"
	QualifiedAppBuild    = "20770"
	QualifiedASARSHA256  = "5ddc4e48259aa6f979e04529b1a5a4ae54b0f5c4ad6db5c6e004924c10916ddb"
	QualifiedModuleAsset = "app-initial-90eb42395b7d.js"
)

// InstallOptions declares credential sources; their contents are never persisted.
type InstallOptions struct {
	Root, App, SystemAuth, CQAccounts, AccountSource string
	Mode, Executable, BaseURL                        string
}

// OperationLock serialises installation mutations without blocking status reads.
func OperationLock(ctx context.Context, root string, wait time.Duration) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := privateDirectory(root); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, filepath.Join(root, "operation.lock"))
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
			return nil, err
		}
		if wait <= 0 || !time.Now().Before(deadline) {
			return nil, fmt.Errorf("Another Dots operation is in progress; try again shortly")
		}
		timer := time.NewTimer(min(100*time.Millisecond, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func ensureInstallDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return privateDirectory(path)
}

func installSave(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.SecureAtomicWrite(fsutil.OSFileSystem{}, path, append(data, '\n'))
}

// ValidateInstallLocation rejects overlap before any installation state is made.
func ValidateInstallLocation(options InstallOptions) (string, error) {
	if !cleanAbsolute(options.Root) {
		return "", fmt.Errorf("Installation root must be an absolute non-symlink path")
	}
	if info, err := os.Lstat(options.Root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("Installation root must be an absolute non-symlink path")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	root, err := CanonicalPath(options.Root)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	home, err = CanonicalPath(home)
	if err != nil {
		return "", err
	}
	if within(root, home) {
		return "", fmt.Errorf("Installation must not contain the home directory")
	}
	protected := []string{options.App, filepath.Dir(options.SystemAuth), options.CQAccounts, filepath.Join(home, ".codex")}
	if options.AccountSource != "" {
		protected = append(protected, options.AccountSource)
	}
	for _, name := range []string{"Codex", "ChatGPT", "com.openai.codex", "com.openai.chat", "OpenAI"} {
		protected = append(protected, filepath.Join(home, "Library", "Application Support", name))
	}
	for _, path := range protected {
		if !cleanAbsolute(path) {
			return "", fmt.Errorf("Installation paths must be absolute")
		}
		boundary, err := CanonicalPath(path)
		if err != nil {
			return "", err
		}
		if within(root, boundary) || within(boundary, root) {
			return "", fmt.Errorf("Installation overlaps a protected app or credential path")
		}
	}
	return root, nil
}

// AppCheck verifies the qualified application without changing its signed bundle.
func AppCheck(settings *Settings) error {
	info, err := readAppPlist(filepath.Join(settings.AppPath, "Contents", "Info.plist"))
	if err != nil {
		return err
	}
	if info["CFBundleShortVersionString"] != settings.AppVersion || info["CFBundleVersion"] != settings.AppBuild {
		return fmt.Errorf("Native app version differs from configured pin")
	}
	file, err := os.Open(filepath.Join(settings.AppPath, "Contents", "Resources", "app.asar"))
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != settings.ASARSHA256 {
		return fmt.Errorf("Native app renderer differs from configured pin")
	}
	return nil
}

// Provision creates only private Go runtime state. Existing certificates and
// legacy Python payloads are retained unchanged for rollback.
func Provision(root, mode string) (string, error) {
	if mode != "native" && mode != "isolated" {
		return "", fmt.Errorf("invalid Dots integration mode")
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if err := ensureInstallDirectory(runtimeRoot); err != nil {
		return "", err
	}
	if mode == "isolated" {
		for _, name := range []string{"user-data", "codex-home", "null-executor", "null-executor/home", "null-executor/tmp", "null-executor/work", "null-executor/room-relay"} {
			if err := ensureInstallDirectory(filepath.Join(runtimeRoot, name)); err != nil {
				return "", err
			}
		}
	}
	names := []string{"leaf-key.pem", "leaf-cert.pem", "probe-cert.pem", "realtime-cert.pem"}
	present := 0
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(runtimeRoot, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm() != 0o600 {
			return "", fmt.Errorf("Private certificate authority changed")
		}
		present++
	}
	if present != 0 && present != len(names) {
		return "", fmt.Errorf("Private certificate set is incomplete")
	}
	if present == 0 {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return "", err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return "", err
		}
		now := time.Now()
		leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "localhost"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(365 * 24 * time.Hour), BasicConstraintsValid: true, IsCA: false, DNSNames: []string{"localhost", "realtime.chatgpt.com", "ws.chatgpt.com"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
		if err != nil {
			return "", err
		}
		cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		material := map[string][]byte{"leaf-key.pem": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), "leaf-cert.pem": cert, "probe-cert.pem": cert, "realtime-cert.pem": cert}
		for _, name := range names {
			if err := fsutil.SecureAtomicWrite(fsutil.OSFileSystem{}, filepath.Join(runtimeRoot, name), material[name]); err != nil {
				return "", err
			}
		}
	}
	port := filepath.Join(runtimeRoot, "port")
	if _, err := os.Lstat(port); os.IsNotExist(err) {
		if err := fsutil.SecureAtomicWrite(fsutil.OSFileSystem{}, port, []byte("8000")); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if _, err := readPrivate(port, "", true); err != nil {
		return "", fmt.Errorf("Proxy port must be an owned private regular file")
	}
	for _, name := range []string{"realtime-port", "celsius-port"} {
		if err := migrateLegacyProxyPort(filepath.Join(runtimeRoot, name)); err != nil {
			return "", err
		}
	}
	return runtimeRoot, nil
}

// Python wrote these non-secret ports as 0644. Preserve their values while
// adopting the private file mode required by the Go runtime.
func migrateLegacyProxyPort(path string) error {
	file, err := (fsutil.OSFileSystem{}).OpenNoFollow(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return err
	}
	mode := before.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if !before.Mode().IsRegular() || !owned(before) || before.Size() > 8 || (mode != 0o600 && mode != 0o644) {
		return fmt.Errorf("Legacy proxy port must be an owned regular file with safe permissions")
	}
	data, err := httputil.ReadBody(file)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("Legacy proxy port invalid")
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	if revision(before) != revision(after) {
		return fmt.Errorf("Legacy proxy port changed during migration")
	}
	if mode == 0o644 {
		file, ok := file.(interface{ Chmod(os.FileMode) error })
		if !ok {
			return fmt.Errorf("Legacy proxy port permission migration unavailable")
		}
		return file.Chmod(0o600)
	}
	return nil
}

// Install prepares an installation. Starting its workers remains a separate
// controller operation so an existing native app is never interrupted here.
func Install(ctx context.Context, options InstallOptions) (map[string]any, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("Native Dots installation currently requires macOS")
	}
	pin := Settings{AppPath: options.App, AppVersion: QualifiedAppVersion, AppBuild: QualifiedAppBuild, ASARSHA256: QualifiedASARSHA256, ModuleAsset: QualifiedModuleAsset}
	if err := AppCheck(&pin); err != nil {
		return nil, err
	}
	return installPrepared(ctx, options, pin)
}

func installPrepared(ctx context.Context, options InstallOptions, pin Settings) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Mode == "" {
		options.Mode = "native"
	}
	root, err := ValidateInstallLocation(options)
	if err != nil {
		return nil, err
	}
	options.Root = root
	var prior *Settings
	path := filepath.Join(root, "settings.json")
	if _, err := os.Lstat(path); err == nil {
		prior, err = LoadSettings(path)
		if err != nil {
			return nil, err
		}
		if prior.RuntimeRoot != filepath.Join(root, "runtime") {
			return nil, fmt.Errorf("Installation runtime belongs to another root")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if options.AccountSource == "" && prior != nil {
		options.AccountSource = prior.AccountSource
	}
	if _, err := ValidateInstallLocation(options); err != nil {
		return nil, err
	}
	data, err := ReadOwned(options.SystemAuth, "")
	if err != nil {
		return nil, err
	}
	var document struct {
		Tokens struct {
			AccountID string `json:"account_id"`
			IDToken   string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &document) != nil || !validText(document.Tokens.AccountID, 256) {
		return nil, fmt.Errorf("invalid system credential identity")
	}
	claims, err := Claims(document.Tokens.IDToken)
	if err != nil {
		return nil, err
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if auth["chatgpt_account_id"] != document.Tokens.AccountID {
		return nil, fmt.Errorf("System credential identity mismatch")
	}
	if prior != nil && prior.SystemAccountID != "" && prior.SystemAccountID != document.Tokens.AccountID {
		return nil, fmt.Errorf("Installation system account changed; choose a new isolated root")
	}
	settings := pin
	settings.RuntimeRoot = filepath.Join(root, "runtime")
	settings.SystemAuth = options.SystemAuth
	settings.CQAccounts = options.CQAccounts
	settings.AccountSource = options.AccountSource
	settings.IntegrationMode = options.Mode
	settings.SystemAccountID = document.Tokens.AccountID
	settings.SystemAlias = AccountAlias(document.Tokens.AccountID)
	settings.Dots = map[string]Dot{}
	if prior != nil {
		settings.Dots = prior.Dots
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if err := ensureInstallDirectory(root); err != nil {
		return nil, err
	}
	if _, err := Provision(root, options.Mode); err != nil {
		return nil, err
	}
	if err := installSave(path, &settings); err != nil {
		return nil, err
	}
	result := map[string]any{"installed": true, "system_app_modified": false, "runtime": "go"}
	if options.Mode == "native" {
		if err := InstallLauncher(root, options.App, options.Executable); err != nil {
			return nil, err
		}
		home, _ := os.UserHomeDir()
		result["dock_launcher"] = filepath.Join(home, "Applications", "ChatGPT via CQ.app")
	}
	if err := installSave(filepath.Join(root, "installation.json"), map[string]any{"schema": 1, "installed": true, "app": map[string]string{"app_version": pin.AppVersion, "app_build": pin.AppBuild, "asar_sha256": pin.ASARSHA256, "module_asset": pin.ModuleAsset}, "installed_at": float64(time.Now().UnixNano()) / 1e9, "system_app_modified": false, "runtime": "go"}); err != nil {
		return nil, err
	}
	return result, nil
}
