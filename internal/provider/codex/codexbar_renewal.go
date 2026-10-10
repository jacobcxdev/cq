package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

const codexBarRenewalReadLimit = 3 << 20

// Renewal is an owner-only capability. Ordinary CodexBarSource reads and all
// other external sources remain read-only. CodexBar's participating writers
// share the manifest's .lock flock; unrelated Codex processes do not.
type codexBarRenewal struct {
	gate                chan struct{}
	fs                  fsutil.DurableFileSystem
	stateDir            string
	retained            map[string]*codexBarRenewalTransaction
	beginOwnerOperation func() (*CredentialOwnerOperation, error)
	runMu               sync.Mutex
	done                chan struct{}
}

func (r *codexBarRenewal) start(ctx context.Context, sources []ExternalCredentialSource, exchange RefreshExchange, clock func() time.Time) <-chan struct{} {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if ctx.Err() != nil || r.beginOwnerOperation == nil {
		return nil
	}
	select {
	case r.gate <- struct{}{}:
	default:
		return r.done
	}
	operation, err := r.beginOwnerOperation()
	if err != nil {
		<-r.gate
		return nil
	}
	done := make(chan struct{})
	r.done = done
	// The guard keeps the endpoint owner alive after the inventory request
	// finishes. Reads never wait for a token endpoint or another renewal.
	go func() {
		defer operation.Release()
		defer func() {
			r.runMu.Lock()
			close(done)
			r.done = nil
			<-r.gate
			r.runMu.Unlock()
			_ = recover()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.renewSources(ctx, sources, exchange, clock)
	}()
	return done
}

type codexBarRenewalTransaction struct {
	Version          int                    `json:"version"`
	Root             string                 `json:"root"`
	Record           codexBarManifestRecord `json:"record"`
	Identity         AccountIdentity        `json:"identity"`
	OriginalRevision Revision               `json:"original_revision"`
	OriginalDigest   string                 `json:"original_digest"`
	Stage            string                 `json:"stage"`
	Auth             json.RawMessage        `json:"auth,omitempty"`
}

func (c *CredentialCoordinator) enableCodexBarRenewal() {
	// CodexBar's documented managed-account lock is a Unix flock contract.
	if c.codexBarRenewal != nil || runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return
	}
	c.codexBarRenewal = &codexBarRenewal{
		fs: c.Store.FS, stateDir: filepath.Join(c.StateDir, "codexbar-renewal"),
		gate:     make(chan struct{}, 1),
		retained: make(map[string]*codexBarRenewalTransaction),
	}
	for _, external := range c.ExternalSources {
		if source, ok := external.(*CodexBarSource); ok && source != nil {
			source.renewalPending = func(record codexBarManifestRecord) bool {
				return c.codexBarRenewal.pendingPublication(source, record)
			}
		}
	}
}

func (r *codexBarRenewal) pendingPublication(source *CodexBarSource, record codexBarManifestRecord) bool {
	// Auth and manifest are separate owner files. Exclude only an exact
	// journalled intermediate record, preserving other healthy owner accounts.
	data, err := fsutil.ReadSecureFile(r.fs, r.transactionPath(source.root, record.ID), codexBarRenewalReadLimit)
	if err != nil {
		return false
	}
	var txn codexBarRenewalTransaction
	if json.Unmarshal(data, &txn) != nil || !validCodexBarRenewalTransaction(&txn, source.root, record.ID) || txn.Record != record || txn.Stage != "result" {
		return false
	}
	record.AuthFingerprint = renewalDigest(txn.Auth)
	_, _, err = source.readRecord(record, "")
	return err == nil
}

func (r *codexBarRenewal) renewSources(ctx context.Context, sources []ExternalCredentialSource, exchange RefreshExchange, clock func() time.Time) {
	_, counts := snapshotExternalSourceNames(sources)
	for _, external := range sources {
		source, ok := external.(*CodexBarSource)
		if !ok || source == nil || counts[codexBarSourceName] != 1 || ctx.Err() != nil {
			continue
		}
		now := time.Now()
		if clock != nil {
			now = clock()
		}
		_ = r.renewSource(ctx, source, exchange, now)
	}
}

func (r *codexBarRenewal) renewSource(ctx context.Context, source *CodexBarSource, exchange RefreshExchange, now time.Time) (result error) {
	defer func() {
		if recover() != nil {
			result = ErrExternalUnavailable
		}
	}()
	manifest, err := source.loadValidatedManifest()
	if err != nil {
		return err
	}
	directory, inspector, err := r.openDirectory(source.root)
	if err != nil {
		return err
	}
	defer directory.Close()
	lock, err := directory.OpenExclusiveLock("managed-codex-accounts.json.lock", 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil {
		return err
	}
	lockIdentity, ok := inspector.FileIdentity(lockInfo)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	checkOwner := func() error {
		if err := fsutil.ValidateOwnerControlledDirectoryHandle(inspector, directory, source.root); err != nil {
			return err
		}
		// Our descriptor retains the flock until this operation returns.
		// Prove the canonical name still refers to that descriptor's file.
		file, err := directory.OpenNoFollow("managed-codex-accounts.json.lock")
		if err != nil {
			return err
		}
		info, err := file.Stat()
		_ = file.Close()
		if err != nil {
			return err
		}
		if err := fsutil.ValidateExternalCredentialFile(inspector, info); err != nil {
			return err
		}
		identity, ok := inspector.FileIdentity(info)
		if !ok || identity != lockIdentity {
			return ErrExternalUnsafePath
		}
		return nil
	}
	if err := checkOwner(); err != nil {
		return err
	}
	manifest, err = source.loadValidatedManifest()
	if err != nil {
		return err
	}
	for _, record := range manifest.value.Accounts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		path := r.transactionPath(source.root, record.ID)
		txn, err := r.load(path, source.root, record.ID)
		if err != nil {
			continue
		}
		if txn != nil {
			if txn.Root != source.root || txn.Record != record && !renewalPublishedRecord(txn, record) {
				// A newer owner login, workspace change or removal supersedes
				// the old operation. It must never be overwritten.
				_ = r.remove(path)
				continue
			}
			if txn.Stage == "result" {
				_ = r.publish(source, directory, inspector, checkOwner, path, txn)
			}
			continue
		}
		validated, err := source.readValidatedRecord(record, "")
		if err != nil || validated.account.RefreshToken == "" || validated.account.ExpiresAt == 0 || validated.account.ExpiresAt > now.Add(time.Minute).UnixMilli() {
			continue
		}
		if err := checkOwner(); err != nil {
			return err
		}
		txn = &codexBarRenewalTransaction{
			Version: 1, Root: source.root, Record: record, Identity: identityFromAccount(validated.account), OriginalRevision: validated.revision,
			OriginalDigest: renewalDigest(validated.data), Stage: "attempted",
		}
		// Persist before consuming a potentially rotating refresh token.
		if err := r.persist(path, txn); err != nil {
			continue
		}
		r.retained[path] = txn
		// The owner lock serialises CodexBar writers. Also fence a writer
		// that does not participate in that lock immediately before HTTP.
		if err := checkOwner(); err != nil {
			continue
		}
		if _, err := source.readValidatedRecord(record, validated.revision); err != nil {
			continue
		}
		tokens, err := exchange(ctx, validated.account.RefreshToken)
		if err != nil {
			txn.Stage = "blocked"
			_ = r.persist(path, txn)
			continue
		}
		txn.Auth, err = renewedCodexBarAuth(validated, tokens, now)
		if err != nil {
			txn.Stage = "blocked"
			_ = r.persist(path, txn)
			continue
		}
		txn.Stage = "result"
		// Retain the result in memory even when persistence fails, so a
		// retry can publish it without another token exchange. After a
		// crash an attempted intent without a result remains blocked.
		if err := r.persist(path, txn); err != nil {
			continue
		}
		_ = r.publish(source, directory, inspector, checkOwner, path, txn)
	}
	return nil
}

func (r *codexBarRenewal) transactionPath(root, recordID string) string {
	digest := sha256.Sum256([]byte(root + "\x00" + recordID))
	return filepath.Join(r.stateDir, hex.EncodeToString(digest[:])+".json")
}

func (r *codexBarRenewal) load(path, root, recordID string) (*codexBarRenewalTransaction, error) {
	if txn := r.retained[path]; txn != nil {
		return txn, nil
	}
	data, err := fsutil.ReadSecureFile(r.fs, path, codexBarRenewalReadLimit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var txn codexBarRenewalTransaction
	if json.Unmarshal(data, &txn) != nil || !validCodexBarRenewalTransaction(&txn, root, recordID) {
		return nil, ErrExternalInvalid
	}
	r.retained[path] = &txn
	return &txn, nil
}

func validCodexBarRenewalTransaction(txn *codexBarRenewalTransaction, root, recordID string) bool {
	digest, err := hex.DecodeString(txn.OriginalDigest)
	if txn.Version != 1 || txn.Root != root || txn.Record.ID != recordID || txn.OriginalRevision == "" || err != nil || len(digest) != sha256.Size || !filepath.IsAbs(txn.Record.ManagedHomePath) || !pathContained(root, txn.Record.ManagedHomePath) || !completeStrongIdentity(txn.Identity) || txn.Record.ProviderAccountID != txn.Identity.AccountID || txn.Record.WorkspaceAccountID != txn.Identity.AccountID || txn.Record.AuthFingerprint != txn.OriginalDigest {
		return false
	}
	if txn.Stage == "attempted" || txn.Stage == "blocked" {
		return len(txn.Auth) == 0
	}
	if txn.Stage != "result" || len(txn.Auth) == 0 || len(txn.Auth) > int(codexBarMaximumDeclaredSize) {
		return false
	}
	account, ok := parseAccountData(txn.Auth, "")
	claims := auth.DecodeCodexClaims(account.IDToken)
	access := auth.DecodeCodexClaims(account.AccessToken)
	return ok && account.ExpiresAt > 0 && account.AccessToken != "" && account.RefreshToken != "" && account.AccountID == txn.Identity.AccountID && account.UserID == txn.Identity.UserID && claims.AccountID == txn.Identity.AccountID && claims.UserID == txn.Identity.UserID && claims.RecordKey() != "" && (access.AccountID == "" || access.AccountID == txn.Identity.AccountID) && (access.UserID == "" || access.UserID == txn.Identity.UserID)
}

func (r *codexBarRenewal) persist(path string, txn *codexBarRenewalTransaction) error {
	data, err := json.Marshal(txn)
	if err != nil || len(data) > codexBarRenewalReadLimit {
		return ErrExternalInvalid
	}
	return fsutil.SecureAtomicWrite(r.fs, path, data)
}

func (r *codexBarRenewal) remove(path string) error {
	if err := r.fs.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := r.fs.SyncDir(r.stateDir); err != nil {
		return err
	}
	delete(r.retained, path)
	return nil
}

func (r *codexBarRenewal) openDirectory(path string) (fsutil.SecureDirectory, fsutil.SecurePathInspector, error) {
	opener, ok := r.fs.(fsutil.DurableDirectoryOpener)
	if !ok {
		return nil, nil, fsutil.ErrSecureCapabilityUnavailable
	}
	inspector, ok := r.fs.(fsutil.SecurePathInspector)
	if !ok {
		return nil, nil, fsutil.ErrSecureCapabilityUnavailable
	}
	opened, err := opener.OpenDurableDirectory(path)
	if err != nil {
		return nil, nil, err
	}
	directory, ok := opened.(fsutil.SecureDirectory)
	if !ok {
		_ = opened.Close()
		return nil, nil, fsutil.ErrSecureCapabilityUnavailable
	}
	if err := fsutil.ValidateOwnerControlledDirectoryHandle(inspector, directory, path); err != nil {
		_ = directory.Close()
		return nil, nil, err
	}
	return directory, inspector, nil
}

func renewedCodexBarAuth(old validatedCodexBarRecord, tokens *auth.CodexTokenResponse, now time.Time) (json.RawMessage, error) {
	if tokens == nil || tokens.AccessToken == "" {
		return nil, ErrExternalInvalid
	}
	material := CredentialMaterial{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, IDToken: tokens.IDToken, AccountID: old.account.AccountID}
	if material.RefreshToken == "" {
		material.RefreshToken = old.account.RefreshToken
	}
	if material.IDToken == "" {
		material.IDToken = old.account.IDToken
	}
	if !credentialMaterialMatchesIdentity(material, identityFromAccount(old.account)) {
		return nil, ErrExternalIdentityMismatch
	}
	idClaims := auth.DecodeCodexClaims(material.IDToken)
	if idClaims.AccountID != old.account.AccountID || idClaims.UserID != old.account.UserID || idClaims.RecordKey() == "" {
		return nil, ErrExternalIdentityMismatch
	}
	claims := auth.DecodeCodexClaims(tokens.AccessToken)
	if claims.AccountID != "" && claims.AccountID != old.account.AccountID || claims.UserID != "" && claims.UserID != old.account.UserID {
		return nil, ErrExternalIdentityMismatch
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(old.data, &document) != nil {
		return nil, ErrExternalInvalid
	}
	var tokenDocument map[string]json.RawMessage
	if json.Unmarshal(document["tokens"], &tokenDocument) != nil || tokenDocument == nil {
		return nil, ErrExternalInvalid
	}
	for key, value := range map[string]string{"access_token": material.AccessToken, "refresh_token": material.RefreshToken, "id_token": material.IDToken} {
		tokenDocument[key], _ = json.Marshal(value)
	}
	document["tokens"], _ = json.Marshal(tokenDocument)
	document["last_refresh"], _ = json.Marshal(now.UTC().Format(time.RFC3339Nano))
	// Prefer the new access JWT's expiry; opaque responses require a
	// positive provider lifetime rather than retaining the old expiry.
	expiresAt := claims.ExpiresAt * 1000
	if expiresAt == 0 && tokens.ExpiresIn > 0 && tokens.ExpiresIn <= int64((365*24*time.Hour)/time.Second) {
		expiresAt = now.Add(time.Duration(tokens.ExpiresIn) * time.Second).UnixMilli()
	}
	if expiresAt <= now.UnixMilli() {
		return nil, ErrExternalInvalid
	}
	document["cq_expires_at"], _ = json.Marshal(expiresAt)
	data, err := json.Marshal(document)
	if err != nil || int64(len(data)) > codexBarMaximumDeclaredSize {
		return nil, ErrExternalInvalid
	}
	return data, nil
}

func renewalDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func renewalPublishedRecord(txn *codexBarRenewalTransaction, record codexBarManifestRecord) bool {
	if txn.Stage != "result" || len(txn.Auth) == 0 {
		return false
	}
	expected := txn.Record
	expected.AuthFingerprint = renewalDigest(txn.Auth)
	return expected == record
}

func renewalManifestRecord(manifest validatedCodexBarManifest, id string) (codexBarManifestRecord, bool) {
	for _, record := range manifest.value.Accounts {
		if record.ID == id {
			return record, true
		}
	}
	return codexBarManifestRecord{}, false
}

func (r *codexBarRenewal) publish(source *CodexBarSource, root fsutil.SecureDirectory, inspector fsutil.SecurePathInspector, checkOwner func() error, path string, txn *codexBarRenewalTransaction) error {
	if !validCodexBarRenewalTransaction(txn, source.root, txn.Record.ID) || txn.Stage != "result" {
		return ErrExternalInvalid
	}
	if err := r.persist(path, txn); err != nil {
		return err
	}
	manifest, err := source.loadValidatedManifest()
	if err != nil {
		return err
	}
	record, ok := renewalManifestRecord(manifest, txn.Record.ID)
	if !ok || record != txn.Record && !renewalPublishedRecord(txn, record) {
		return ErrStaleRevision
	}
	authPath, err := source.authPath(record.ManagedHomePath, ErrStaleRevision)
	if err != nil {
		return err
	}
	read, err := source.readValidatedFile(authPath, ErrStaleRevision, ErrStaleRevision)
	if err != nil {
		return err
	}
	if !bytes.Equal(read.data, txn.Auth) {
		if renewalDigest(read.data) != txn.OriginalDigest || externalCredentialRevision(read.data, authPath, read.generation) != txn.OriginalRevision || record != txn.Record {
			return ErrStaleRevision
		}
		home, _, err := r.openDirectory(filepath.Dir(authPath))
		if err != nil {
			return err
		}
		defer home.Close()
		if err := fsutil.SecureAtomicWriteInOwnerControlledDirectoryChecked(inspector, home, filepath.Dir(authPath), "auth.json", txn.Auth, func() error {
			if err := checkOwner(); err != nil {
				return err
			}
			current, err := source.loadValidatedManifest()
			if err != nil {
				return err
			}
			currentRecord, ok := renewalManifestRecord(current, record.ID)
			if !ok || currentRecord != record {
				return ErrStaleRevision
			}
			original, err := source.readValidatedRecord(record, txn.OriginalRevision)
			if err != nil {
				return err
			}
			if !sameStrongIdentity(identityFromAccount(original.account), txn.Identity) {
				return ErrExternalIdentityMismatch
			}
			return nil
		}); err != nil {
			return err
		}
	}
	// The rotated result is already durable. A crash between these two
	// publications resumes here; it can never repeat the exchange.
	if !renewalPublishedRecord(txn, record) {
		updated, err := renewalManifestData(manifest.data, record.ID, renewalDigest(txn.Auth))
		if err != nil {
			return err
		}
		if err := fsutil.SecureAtomicWriteInOwnerControlledDirectoryChecked(inspector, root, source.root, filepath.Base(manifest.path), updated, func() error {
			if err := checkOwner(); err != nil {
				return err
			}
			current, err := source.loadValidatedManifest()
			if err != nil {
				return err
			}
			if !bytes.Equal(current.data, manifest.data) {
				return ErrStaleRevision
			}
			currentAuth, err := source.readValidatedFile(authPath, ErrStaleRevision, ErrStaleRevision)
			if err != nil {
				return err
			}
			if !bytes.Equal(currentAuth.data, txn.Auth) {
				return ErrStaleRevision
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return r.remove(path)
}

func renewalManifestData(data []byte, id, fingerprint string) ([]byte, error) {
	var document map[string]json.RawMessage
	if json.Unmarshal(data, &document) != nil {
		return nil, ErrExternalInvalid
	}
	var records []json.RawMessage
	if json.Unmarshal(document["accounts"], &records) != nil {
		return nil, ErrExternalInvalid
	}
	found := false
	for i, raw := range records {
		var record map[string]json.RawMessage
		var recordID string
		if json.Unmarshal(raw, &record) != nil || json.Unmarshal(record["id"], &recordID) != nil {
			return nil, ErrExternalInvalid
		}
		if recordID == id {
			record["authFingerprint"], _ = json.Marshal(fingerprint)
			records[i], _ = json.Marshal(record)
			found = true
		}
	}
	if !found {
		return nil, ErrStaleRevision
	}
	document["accounts"], _ = json.Marshal(records)
	updated, err := json.Marshal(document)
	if err != nil || int64(len(updated)) > codexBarMaximumDeclaredSize {
		return nil, ErrExternalInvalid
	}
	return updated, nil
}
