package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const extraCreditsPolicyMaxBytes = 64 << 10

// ExtraCreditsPolicy stores CQ's spending permission by logical account, not
// credential source. Every read observes the latest atomic policy publication.
type ExtraCreditsPolicy struct {
	FS   fsutil.FileSystem
	Path string
}

type extraCreditsPolicyDocument struct {
	Version          int           `json:"version"`
	DisabledAccounts *[]AccountKey `json:"disabled_accounts"`
}

// ExtraCreditsIdentityKey remains stable when a managed login replaces an
// external or legacy inventory key. Workspace and user must both be known;
// email and credential metadata are not spending authorities.
func ExtraCreditsIdentityKey(identity AccountIdentity) (AccountKey, error) {
	if strings.TrimSpace(identity.AccountID) == "" || strings.TrimSpace(identity.UserID) == "" {
		return "", errors.New("extra credits require a strong account identity")
	}
	return generationAccountKey(identity, SourceSystem, ""), nil
}

func DefaultExtraCreditsPolicy() (*ExtraCreditsPolicy, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return nil, err
	}
	return &ExtraCreditsPolicy{FS: fsutil.OSFileSystem{}, Path: filepath.Join(roots.State, "codex-extra-credits.json")}, nil
}

// Allowed defaults to true only for absent policy/account entries. Invalid or
// unreadable persisted policy never grants spending permission.
func (p *ExtraCreditsPolicy) Allowed(account AccountKey) (bool, error) {
	if !validExtraCreditsAccountKey(account) {
		return false, errors.New("extra credits require a valid account key")
	}
	disabled, err := p.read()
	if err != nil {
		return false, err
	}
	return !disabled[account], nil
}

// Restricted reports whether unidentifiable rescue traffic must be refused.
func (p *ExtraCreditsPolicy) Restricted() (bool, error) {
	disabled, err := p.read()
	return len(disabled) != 0, err
}

func (p *ExtraCreditsPolicy) read() (map[AccountKey]bool, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	data, err := fsutil.ReadSecureFile(p.FS, p.Path, extraCreditsPolicyMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return map[AccountKey]bool{}, nil
	}
	if err != nil {
		return nil, errors.New("extra credits policy unavailable")
	}
	return decodeExtraCreditsPolicy(data)
}

func decodeExtraCreditsPolicy(data []byte) (map[AccountKey]bool, error) {
	if _, err := decodeCredentialMaintenanceObject(data); err != nil {
		return nil, errors.New("invalid extra credits policy")
	}
	var document extraCreditsPolicyDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) || document.Version != 1 || document.DisabledAccounts == nil {
		return nil, errors.New("invalid extra credits policy")
	}
	disabled := make(map[AccountKey]bool, len(*document.DisabledAccounts))
	for _, account := range *document.DisabledAccounts {
		if !validExtraCreditsAccountKey(account) || disabled[account] {
			return nil, errors.New("invalid extra credits policy account")
		}
		disabled[account] = true
	}
	return disabled, nil
}

// Set serialises mutations with the existing secure file lock and publishes
// through one retained directory, preserving other accounts and prior state on
// rejected writes. A competing writer receives a retryable lock error.
func (p *ExtraCreditsPolicy) Set(account AccountKey, allowed bool) error {
	if !validExtraCreditsAccountKey(account) {
		return errors.New("extra credits require a valid account key")
	}
	if err := p.validate(); err != nil {
		return err
	}
	inspector, ok := p.FS.(fsutil.SecurePathInspector)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	opener, ok := p.FS.(fsutil.SecureDirectoryOpener)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	dir := filepath.Dir(p.Path)
	if err := fsutil.EnsureSecureDirectory(p.FS, dir); err != nil {
		return err
	}
	directory, err := opener.OpenSecureDirectory(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := fsutil.ValidateSecureDirectoryHandle(inspector, directory, dir); err != nil {
		return err
	}
	lock, err := fsutil.AcquireExclusiveLockInDirectory(inspector, directory, filepath.Base(p.Path)+".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	disabled := map[AccountKey]bool{}
	data, err := fsutil.ReadSecureFileInDirectory(inspector, directory, filepath.Base(p.Path), extraCreditsPolicyMaxBytes)
	if err == nil {
		disabled, err = decodeExtraCreditsPolicy(data)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if allowed {
		delete(disabled, account)
	} else {
		disabled[account] = true
	}
	accounts := make([]AccountKey, 0, len(disabled))
	for key := range disabled {
		accounts = append(accounts, key)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })
	data, err = json.Marshal(extraCreditsPolicyDocument{Version: 1, DisabledAccounts: &accounts})
	if err != nil {
		return err
	}
	if len(data) > extraCreditsPolicyMaxBytes {
		return errors.New("extra credits policy exceeds size limit")
	}
	return fsutil.SecureAtomicWriteInDirectoryChecked(inspector, directory, filepath.Base(p.Path), data, func() error {
		return fsutil.ValidateSecureDirectoryHandle(inspector, directory, dir)
	})
}

func (p *ExtraCreditsPolicy) validate() error {
	if p == nil || p.FS == nil || p.Path == "" || !filepath.IsAbs(p.Path) || filepath.Clean(p.Path) != p.Path {
		return errors.New("extra credits policy unavailable")
	}
	return nil
}

func validExtraCreditsAccountKey(account AccountKey) bool {
	return account != "" && len(account) <= 1024 && strings.IndexFunc(string(account), func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) == -1
}
