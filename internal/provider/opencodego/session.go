package opencodego

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/jacobcxdev/cq/internal/credentials"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	oauthSessionFile = "opencode-go-session.enc"
	oauthKeyFile     = "opencode-go-session.key"
	oauthLockFile    = "opencode-go-session.lock"
	oauthFileVersion = 1
	oauthFileLimit   = 32 << 10
)

// SessionAuthority owns CQ's OpenCode OAuth login and serialises refreshes
// across the CLI and any running proxy. The API-key file is separate.
type SessionAuthority struct {
	FS        fsutil.FileSystem
	ConfigDir string
	Exchange  func(context.Context, OAuthSession) (OAuthSession, error)
	Now       func() time.Time
}

type refreshStatus string

const (
	refreshPending  refreshStatus = "pending"
	refreshRejected refreshStatus = "rejected"
)

type sessionRecord struct {
	Session OAuthSession  `json:"session"`
	Refresh refreshStatus `json:"refresh,omitempty"`
}

func (a SessionAuthority) Read() (OAuthSession, error) {
	record, err := a.read()
	return record.Session, err
}

func (a SessionAuthority) Save(ctx context.Context, session OAuthSession) error {
	if !validSession(session) || !session.ExpiresAt.After(a.now()) {
		return errors.New("invalid OpenCode OAuth session")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fsutil.EnsureSecureDirectory(a.FS, a.ConfigDir); err != nil {
		return errors.New("prepare OpenCode OAuth store")
	}
	store, unlock, err := a.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return store.write(sessionRecord{Session: session})
}

func (a SessionAuthority) Fresh(ctx context.Context) (OAuthSession, error) {
	if err := ctx.Err(); err != nil {
		return OAuthSession{}, err
	}
	record, err := a.read()
	if err != nil {
		return OAuthSession{}, err
	}
	if record.Refresh == refreshRejected {
		return OAuthSession{}, record.refreshError()
	}
	if record.Refresh == "" && record.Session.ExpiresAt.After(a.now().Add(time.Minute)) {
		return record.Session, nil
	}

	store, unlock, err := a.lock(ctx)
	if err != nil {
		return OAuthSession{}, err
	}
	defer unlock()

	record, err = store.read()
	if err != nil {
		return OAuthSession{}, err
	}
	if err := record.refreshError(); err != nil {
		return OAuthSession{}, err
	}
	if record.Session.ExpiresAt.After(a.now().Add(time.Minute)) {
		return record.Session, nil
	}
	if a.Exchange == nil {
		return OAuthSession{}, errors.New("OpenCode OAuth refresh unavailable")
	}
	if err := ctx.Err(); err != nil {
		return OAuthSession{}, err
	}

	// Record the attempt before spending the one-use refresh token. A lost
	// response or interrupted commit requires a new login.
	record.Refresh = refreshPending
	if err := store.write(record); err != nil {
		return OAuthSession{}, err
	}
	next, err := a.Exchange(ctx, record.Session)
	if errors.Is(err, ErrOAuthRejected) {
		record.Refresh = refreshRejected
		if writeErr := store.write(record); writeErr != nil {
			return OAuthSession{}, recordIndeterminate()
		}
		return OAuthSession{}, record.refreshError()
	}
	if err != nil || !validSession(next) || !next.ExpiresAt.After(a.now()) ||
		next.MemberID != record.Session.MemberID || next.WorkspaceID != record.Session.WorkspaceID ||
		next.RefreshToken == record.Session.RefreshToken {
		return OAuthSession{}, recordIndeterminate()
	}
	if err := store.write(sessionRecord{Session: next}); err != nil {
		return OAuthSession{}, recordIndeterminate()
	}
	return next, nil
}

func (r sessionRecord) refreshError() error {
	switch r.Refresh {
	case refreshPending:
		return recordIndeterminate()
	case refreshRejected:
		return fmt.Errorf("OpenCode login expired. Run cq opencode login: %w", ErrOAuthRejected)
	default:
		return nil
	}
}

func recordIndeterminate() error {
	return fmt.Errorf("OpenCode OAuth refresh outcome uncertain. Run cq opencode login: %w", credentials.ErrOutcomeIndeterminate)
}

func (a SessionAuthority) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func validSession(s OAuthSession) bool {
	return credentialText(s.AccessToken, 8192) && credentialText(s.RefreshToken, 8192) &&
		credentialText(s.MemberID, 256) && credentialText(s.WorkspaceID, 256) &&
		s.Email != "" && len(s.Email) <= 320 && !strings.ContainsFunc(s.Email, unicode.IsControl) &&
		!s.ExpiresAt.IsZero()
}

type sessionStore struct {
	fsutil.SecureDirectory
	inspector fsutil.SecurePathInspector
	fence     func() error
}

func (a SessionAuthority) open() (sessionStore, error) {
	inspector, inspectOK := a.FS.(fsutil.SecurePathInspector)
	opener, openOK := a.FS.(fsutil.SecureDirectoryOpener)
	if !inspectOK || !openOK || a.ConfigDir == "" {
		return sessionStore{}, errors.New("OpenCode OAuth store unavailable")
	}
	directory, err := opener.OpenSecureDirectory(a.ConfigDir)
	if errors.Is(err, os.ErrNotExist) {
		return sessionStore{}, os.ErrNotExist
	}
	if err != nil {
		return sessionStore{}, errors.New("OpenCode OAuth store unavailable")
	}
	fence := func() error {
		return fsutil.ValidateSecureDirectoryHandle(inspector, directory, a.ConfigDir)
	}
	return sessionStore{SecureDirectory: directory, inspector: inspector, fence: fence}, nil
}

func (a SessionAuthority) read() (sessionRecord, error) {
	s, err := a.open()
	if err != nil {
		return sessionRecord{}, err
	}
	defer s.Close()
	return s.read()
}

func (a SessionAuthority) lock(ctx context.Context) (sessionStore, func(), error) {
	s, err := a.open()
	if err != nil {
		return sessionStore{}, nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = s.Close()
			return sessionStore{}, nil, err
		}
		lock, err := fsutil.AcquireExclusiveLockInDirectory(s.inspector, s.SecureDirectory, oauthLockFile)
		if err == nil {
			info, err := lock.Stat()
			var identity fsutil.SecureFileIdentity
			var ok bool
			if err == nil {
				identity, ok = s.inspector.FileIdentity(info)
			}
			unlock := func() {
				_ = lock.Close()
				_ = s.Close()
			}
			if !ok {
				unlock()
				return sessionStore{}, nil, errors.New("verify OpenCode OAuth store lock")
			}
			checkDirectory := s.fence
			directory, inspector := s.SecureDirectory, s.inspector
			s.fence = func() error {
				if err := checkDirectory(); err != nil {
					return err
				}
				return fsutil.ValidateExclusiveLockHeldInDirectory(inspector, directory, oauthLockFile, identity)
			}
			return s, unlock, nil
		}
		if !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
			_ = s.Close()
			return sessionStore{}, nil, errors.New("lock OpenCode OAuth store")
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}
}

func (s sessionStore) read() (sessionRecord, error) {
	var record sessionRecord
	if err := s.fence(); err != nil {
		return record, errors.New("verify OpenCode OAuth store")
	}
	data, err := fsutil.ReadSecureFileInDirectory(s.inspector, s.SecureDirectory, oauthSessionFile, oauthFileLimit)
	if errors.Is(err, os.ErrNotExist) {
		return record, os.ErrNotExist
	}
	if err != nil || len(data) < 2 || data[0] != oauthFileVersion {
		return record, errors.New("read OpenCode OAuth session")
	}
	aead, err := s.cipher()
	if err != nil {
		return record, errors.New("read OpenCode OAuth storage key")
	}
	plain, err := aead.Open(nil, nil, data[1:], []byte(oauthSessionFile))
	if err != nil {
		return record, errors.New("decrypt OpenCode OAuth session")
	}
	defer clear(plain)
	if json.Unmarshal(plain, &record) != nil || !validSession(record.Session) ||
		(record.Refresh != "" && record.Refresh != refreshPending && record.Refresh != refreshRejected) {
		return sessionRecord{}, errors.New("invalid OpenCode OAuth session")
	}
	if err := s.fence(); err != nil {
		return sessionRecord{}, errors.New("verify OpenCode OAuth store")
	}
	return record, nil
}

func (s sessionStore) cipher() (cipher.AEAD, error) {
	rawKey, err := fsutil.ReadSecureFileInDirectory(s.inspector, s.SecureDirectory, oauthKeyFile, 32)
	if err != nil {
		return nil, err
	}
	defer clear(rawKey)
	if len(rawKey) != 32 {
		return nil, errors.New("invalid OpenCode OAuth storage key")
	}
	block, err := aes.NewCipher(rawKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func (s sessionStore) write(record sessionRecord) error {
	if err := s.fence(); err != nil {
		return errors.New("verify OpenCode OAuth store")
	}
	aead, err := s.cipher()
	if errors.Is(err, os.ErrNotExist) {
		// An existing encrypted record must never be re-keyed implicitly.
		_, priorErr := fsutil.ReadSecureFileInDirectory(s.inspector, s.SecureDirectory, oauthSessionFile, oauthFileLimit)
		if !errors.Is(priorErr, os.ErrNotExist) {
			return errors.New("OpenCode OAuth storage key unavailable")
		}
		var key [32]byte
		defer clear(key[:])
		if _, err := rand.Read(key[:]); err != nil {
			return errors.New("create OpenCode OAuth storage key")
		}
		if err := fsutil.SecureAtomicCreateInDirectoryChecked(s.inspector, s.SecureDirectory, oauthKeyFile, key[:], s.fence); err != nil {
			return errors.New("save OpenCode OAuth storage key")
		}
		aead, err = s.cipher()
	}
	if err != nil {
		return errors.New("read OpenCode OAuth storage key")
	}
	plain, err := json.Marshal(record)
	if err != nil {
		return errors.New("encode OpenCode OAuth session")
	}
	defer clear(plain)
	sealed := aead.Seal([]byte{oauthFileVersion}, nil, plain, []byte(oauthSessionFile))
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(s.inspector, s.SecureDirectory, oauthSessionFile, sealed, s.fence); err != nil {
		return errors.New("save OpenCode OAuth session")
	}
	return nil
}
