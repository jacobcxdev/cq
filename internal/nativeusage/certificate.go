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
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	certificateName = "usage-cert.pem"
	keyName         = "usage-key.pem"
	enabledName     = "enabled.json"
	materialLimit   = 16 << 10
)

var errPairMissing = errors.New("usage certificate pair is missing")

// CertificateInfo contains only public information needed to approve local trust.
type CertificateInfo struct {
	CertificatePath   string    `json:"certificate_path"`
	FingerprintSHA256 string    `json:"fingerprint_sha256"`
	Hosts             []string  `json:"hosts"`
	NotAfter          time.Time `json:"not_after"`
}

type enabledState struct {
	FingerprintSHA256 string `json:"fingerprint_sha256"`
}

// IsHost accepts only the two service hostnames used for native usage updates.
func IsHost(host string) bool {
	return host == "chatgpt.com" || host == "ws.chatgpt.com"
}

// Prepare creates private certificate material without enabling response handling.
// Existing material is validated and reused; it is never silently replaced.
func Prepare(root string, fsys fsutil.FileSystem) (CertificateInfo, error) {
	if err := validateRoot(root, fsys); err != nil {
		return CertificateInfo{}, err
	}
	if err := fsutil.EnsureSecureDirectory(fsys, root); err != nil {
		return CertificateInfo{}, err
	}
	lock, err := fsutil.AcquireExclusiveLock(fsys, filepath.Join(root, "certificate.lock"))
	if err != nil {
		return CertificateInfo{}, err
	}
	defer lock.Close()
	_, certificate, err := readPair(root, fsys)
	if errors.Is(err, errPairMissing) {
		if markerErr := fsutil.ValidateSecureRegularFile(fsys, filepath.Join(root, enabledName)); markerErr == nil {
			return CertificateInfo{}, errors.New("enabled usage certificate is missing")
		} else if !errors.Is(markerErr, os.ErrNotExist) {
			return CertificateInfo{}, markerErr
		}
		if err = generatePair(root, fsys); err != nil {
			return CertificateInfo{}, err
		}
		_, certificate, err = readPair(root, fsys)
	}
	if err != nil {
		return CertificateInfo{}, err
	}
	return info(root, certificate), nil
}

// Inspect reads public certificate and activation metadata without creating
// files or reading private-key contents. LoadTLS validates the key when used.
func Inspect(root string, fsys fsutil.FileSystem) (CertificateInfo, bool, error) {
	if err := validateRoot(root, fsys); err != nil {
		return CertificateInfo{}, false, err
	}
	if err := fsutil.ValidateSecureDirectory(fsys, root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CertificateInfo{}, false, nil
		}
		return CertificateInfo{}, false, err
	}
	data, certErr := fsutil.ReadSecureFile(fsys, filepath.Join(root, certificateName), materialLimit)
	keyErr := fsutil.ValidateSecureRegularFile(fsys, filepath.Join(root, keyName))
	state, enabled, stateErr := readState(root, fsys)
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) && !enabled && stateErr == nil {
		return CertificateInfo{}, false, nil
	}
	if certErr != nil || keyErr != nil {
		return CertificateInfo{}, false, fmt.Errorf("usage certificate pair is incomplete or unreadable: %w", errors.Join(certErr, keyErr))
	}
	certificate, err := parseCertificate(data)
	if err != nil {
		return CertificateInfo{}, false, err
	}
	if stateErr != nil {
		return CertificateInfo{}, false, stateErr
	}
	metadata := info(root, certificate)
	if enabled && state.FingerprintSHA256 != metadata.FingerprintSHA256 {
		return CertificateInfo{}, false, errors.New("usage certificate differs from enabled settings")
	}
	return metadata, enabled, nil
}

// LoadTLS returns nil when response handling is not enabled. An enabled marker
// is valid only for the exact certificate which was approved.
func LoadTLS(root string, fsys fsutil.FileSystem) (*tls.Config, error) {
	if err := validateRoot(root, fsys); err != nil {
		return nil, err
	}
	state, enabled, err := readState(root, fsys)
	if err != nil || !enabled {
		return nil, err
	}
	pair, certificate, err := readPair(root, fsys)
	if err != nil {
		return nil, err
	}
	if state.FingerprintSHA256 != info(root, certificate).FingerprintSHA256 {
		return nil, errors.New("usage certificate differs from enabled settings")
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}, nil
}

func readState(root string, fsys fsutil.FileSystem) (enabledState, bool, error) {
	data, err := fsutil.ReadSecureFile(fsys, filepath.Join(root, enabledName), 1024)
	if errors.Is(err, os.ErrNotExist) {
		return enabledState{}, false, nil
	}
	if err != nil {
		return enabledState{}, false, err
	}
	var state enabledState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&state); err != nil || !json.Valid(data) {
		return enabledState{}, false, errors.New("usage response settings are invalid")
	}
	return state, true, nil
}

// SetEnabled changes only the activation marker. Disabling preserves the
// certificate so its trust setting can be removed separately.
func SetEnabled(root string, fsys fsutil.FileSystem, enabled bool) error {
	if err := validateRoot(root, fsys); err != nil {
		return err
	}
	if err := fsutil.ValidateSecureDirectory(fsys, root); err != nil {
		if !enabled && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	lock, err := fsutil.AcquireExclusiveLock(fsys, filepath.Join(root, "certificate.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	path := filepath.Join(root, enabledName)
	if !enabled {
		if err = fsutil.ValidateSecureRegularFile(fsys, path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		durable, ok := fsys.(fsutil.DurableFileSystem)
		if !ok {
			return fsutil.ErrSecureCapabilityUnavailable
		}
		if err = fsys.Remove(path); err != nil {
			return err
		}
		return durable.SyncDir(root)
	}
	_, certificate, err := readPair(root, fsys)
	if err != nil {
		return err
	}
	data, err := json.Marshal(enabledState{FingerprintSHA256: info(root, certificate).FingerprintSHA256})
	if err != nil {
		return err
	}
	return fsutil.SecureAtomicWrite(fsys, path, data)
}

func validateRoot(root string, fsys fsutil.FileSystem) error {
	if fsys == nil || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return errors.New("usage certificate directory is invalid")
	}
	return nil
}

func info(root string, certificate *x509.Certificate) CertificateInfo {
	fingerprint := sha256.Sum256(certificate.Raw)
	return CertificateInfo{CertificatePath: filepath.Join(root, certificateName), FingerprintSHA256: hex.EncodeToString(fingerprint[:]), Hosts: []string{"chatgpt.com", "ws.chatgpt.com"}, NotAfter: certificate.NotAfter}
}

func generatePair(root string, fsys fsutil.FileSystem) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	now := time.Now()
	certificate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "CQ native usage"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(365 * 24 * time.Hour), BasicConstraintsValid: true, DNSNames: []string{"chatgpt.com", "ws.chatgpt.com"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		return err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err = fsutil.SecureAtomicWrite(fsys, filepath.Join(root, keyName), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})); err != nil {
		return err
	}
	return fsutil.SecureAtomicWrite(fsys, filepath.Join(root, certificateName), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func readPair(root string, fsys fsutil.FileSystem) (tls.Certificate, *x509.Certificate, error) {
	certData, certErr := fsutil.ReadSecureFile(fsys, filepath.Join(root, certificateName), materialLimit)
	keyData, keyErr := fsutil.ReadSecureFile(fsys, filepath.Join(root, keyName), materialLimit)
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
		return tls.Certificate{}, nil, errPairMissing
	}
	if certErr != nil || keyErr != nil {
		return tls.Certificate{}, nil, fmt.Errorf("usage certificate pair is incomplete or unreadable: %w", errors.Join(certErr, keyErr))
	}
	keyBlock, trailing := pem.Decode(keyData)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" || len(bytes.TrimSpace(trailing)) != 0 {
		return tls.Certificate{}, nil, errors.New("usage certificate key is invalid")
	}
	certificate, err := parseCertificate(certData)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pair, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		return tls.Certificate{}, nil, errors.New("usage certificate key does not match")
	}
	pair.Leaf = certificate
	return pair, certificate, nil
}

func parseCertificate(data []byte) (*x509.Certificate, error) {
	block, trailing := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(trailing)) != 0 {
		return nil, errors.New("usage certificate is invalid")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.New("usage certificate is invalid")
	}
	now := time.Now()
	validHosts := len(certificate.DNSNames) == 2 && IsHost(certificate.DNSNames[0]) && IsHost(certificate.DNSNames[1]) && certificate.DNSNames[0] != certificate.DNSNames[1]
	if !validHosts || len(certificate.IPAddresses) != 0 || len(certificate.EmailAddresses) != 0 || len(certificate.URIs) != 0 || !certificate.BasicConstraintsValid || certificate.IsCA || certificate.KeyUsage != x509.KeyUsageDigitalSignature || len(certificate.ExtKeyUsage) != 1 || certificate.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || len(certificate.UnknownExtKeyUsage) != 0 || len(certificate.UnhandledCriticalExtensions) != 0 || now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) || !bytes.Equal(certificate.RawIssuer, certificate.RawSubject) {
		return nil, errors.New("usage certificate constraints or validity are invalid")
	}
	if certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature) != nil {
		return nil, errors.New("usage certificate must be self-signed")
	}
	return certificate, nil
}
