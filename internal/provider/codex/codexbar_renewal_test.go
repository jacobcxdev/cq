//go:build darwin || linux

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

type codexBarRenewalTestFS struct {
	fsutil.OSFileSystem
	home string
}

func (fs codexBarRenewalTestFS) UserHomeDir() (string, error) { return fs.home, nil }

func newCodexBarRenewalTest(t *testing.T) (*CredentialCoordinator, *CodexBarSource, time.Time) {
	t.Helper()
	home := t.TempDir()
	fs := codexBarRenewalTestFS{home: home}
	store, err := NewManagedStore(fs)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCredentialCoordinator(store, filepath.Join(home, "state"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	c.Now = func() time.Time { return now }
	root := filepath.Join(home, "CodexBar")
	writeCodexBarFixture(t, root, 0o600, nil)
	writeCodexBarAuthAndFingerprint(t, root, inventoryAuth(
		fakeCodexJWTWithExpiry("one@example.test", "acct-1", "user-1", "plus", now.Add(-time.Minute).Unix()),
		"acct-1", fakeCodexJWT("one@example.test", "acct-1", "user-1", "plus"), 0))
	source := NewCodexBarSource(root)
	c.ExternalSources = []ExternalCredentialSource{source}
	return c, source, now
}

func codexBarRenewalTestResponse(now time.Time) *auth.CodexTokenResponse {
	return &auth.CodexTokenResponse{
		AccessToken:  fakeCodexJWTWithExpiry("one@example.test", "acct-1", "user-1", "plus", now.Add(time.Hour).Unix()),
		RefreshToken: "rotated-synthetic-refresh", IDToken: fakeCodexJWT("one@example.test", "acct-1", "user-1", "plus"), ExpiresIn: 3600,
	}
}

func TestCodexBarRenewalExpiredCredentialBecomesRoutable(t *testing.T) {
	c, source, now := newCodexBarRenewalTest(t)
	calls := 0
	c.RefreshExchange = func(ctx context.Context, refresh string) (*auth.CodexTokenResponse, error) {
		return auth.RefreshCodexToken(ctx, providerDoerFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.Method != http.MethodPost || request.ParseForm() != nil || request.FormValue("grant_type") != "refresh_token" || request.FormValue("refresh_token") != refresh {
				t.Fatal("invalid refresh exchange request")
			}
			data, _ := json.Marshal(codexBarRenewalTestResponse(now))
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
		}), refresh)
	}
	c.enableCodexBarRenewal()
	inventory, err := listAfterCodexBarRenewal(c, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(inventory.Accounts) != 1 || CandidateAvailabilityAt(inventory.Accounts[0].Candidates[0], now) != CandidateReady {
		t.Fatalf("calls/accounts = %d/%d, want one renewal and ready account", calls, len(inventory.Accounts))
	}
	if _, err := source.List(context.Background()); err != nil {
		t.Fatalf("published fingerprint: %v", err)
	}
	data, err := os.ReadFile(codexBarAuthPath(source.root))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document["tokens"].(map[string]any)["refresh_token"] != "rotated-synthetic-refresh" {
		t.Fatal("owner did not receive rotated refresh token")
	}
}

func TestCodexBarRenewalRequiresOwnerCapability(t *testing.T) {
	c, source, _ := newCodexBarRenewalTest(t)
	before, err := os.ReadFile(codexBarAuthPath(source.root))
	if err != nil {
		t.Fatal(err)
	}
	c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		t.Fatal("read-only coordinator refreshed")
		return nil, nil
	}
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(codexBarAuthPath(source.root))
	if err != nil || string(before) != string(after) {
		t.Fatal("read-only coordinator mutated owner auth")
	}
}

func TestCodexBarRenewalCoalescesConcurrentInventory(t *testing.T) {
	c, _, now := newCodexBarRenewalTest(t)
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		calls.Add(1)
		close(started)
		<-release
		return codexBarRenewalTestResponse(now), nil
	}
	c.enableCodexBarRenewal()
	var wait sync.WaitGroup
	errorsOut := make(chan error, 12)
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			inventory, err := listAfterCodexBarRenewal(c, context.Background())
			if err == nil && (len(inventory.Accounts) != 1 || CandidateAvailabilityAt(inventory.Accounts[0].Candidates[0], now) != CandidateReady) {
				err = errors.New("concurrent inventory did not observe ready credential")
			}
			errorsOut <- err
		}()
	}
	<-started
	close(release)
	wait.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("exchanges = %d, want one", calls.Load())
	}
}

func TestCodexBarRenewalBusyOwnerDoesNotExchange(t *testing.T) {
	c, source, _ := newCodexBarRenewalTest(t)
	directory, _, err := (&codexBarRenewal{fs: c.Store.FS}).openDirectory(source.root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	lock, err := directory.OpenExclusiveLock("managed-codex-accounts.json.lock", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		t.Fatal("busy owner exchanged token")
		return nil, nil
	}
	c.enableCodexBarRenewal()
	started := time.Now()
	if _, err := listAfterCodexBarRenewal(c, context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("busy owner blocked inventory")
	}
}

func TestCodexBarRenewalWaitRespectsCancellation(t *testing.T) {
	c, _, now := newCodexBarRenewalTest(t)
	started, release := make(chan struct{}), make(chan struct{})
	c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		close(started)
		<-release
		return codexBarRenewalTestResponse(now), nil
	}
	c.enableCodexBarRenewal()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = listAfterCodexBarRenewal(c, context.Background()) }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := listAfterCodexBarRenewal(c, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	close(release)
	<-done
}

func TestCodexBarRenewalPreservesOwnerFieldsAndPermissions(t *testing.T) {
	c, source, now := newCodexBarRenewalTest(t)
	data, err := os.ReadFile(codexBarAuthPath(source.root))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["ownerUnknown"] = map[string]any{"number": json.Number("9007199254740993")}
	document["tokens"].(map[string]any)["ownerTokenMetadata"] = "retain"
	data, _ = json.Marshal(document)
	writeCodexBarAuthAndFingerprint(t, source.root, data)
	rewriteCodexBarManifest(t, source.root, func(doc map[string]any) { doc["unknownRoot"] = "retain" })
	rewriteCodexBarRecord(t, source.root, func(record map[string]any) { record["workspaceLabel"] = "retain" })
	c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		return codexBarRenewalTestResponse(now), nil
	}
	c.enableCodexBarRenewal()
	if _, err := listAfterCodexBarRenewal(c, context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(codexBarAuthPath(source.root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "9007199254740993") || !strings.Contains(string(after), "ownerTokenMetadata") {
		t.Fatal("owner auth fields changed")
	}
	manifest, err := os.ReadFile(filepath.Join(source.root, "managed-codex-accounts.json"))
	if err != nil || !strings.Contains(string(manifest), "unknownRoot") || !strings.Contains(string(manifest), "workspaceLabel") {
		t.Fatal("owner manifest fields changed")
	}
	for _, path := range []string{source.root, filepath.Dir(codexBarAuthPath(source.root))} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatal("owner directory permissions changed")
		}
	}
	for _, path := range []string{codexBarAuthPath(source.root), filepath.Join(source.root, "managed-codex-accounts.json")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("owner private file permissions changed")
		}
	}
}

func TestCodexBarRenewalNewOwnerLoginWins(t *testing.T) {
	c, source, now := newCodexBarRenewalTest(t)
	newHome := filepath.Join(source.root, "managed-codex-homes", "owner-new-home")
	if err := os.MkdirAll(newHome, 0o755); err != nil {
		t.Fatal(err)
	}
	newAuth := inventoryAuth("new-owner-access", "acct-1", fakeCodexJWT("one@example.test", "acct-1", "user-1", "plus"), now.Add(time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(newHome, "auth.json"), newAuth, 0o600); err != nil {
		t.Fatal(err)
	}
	c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		// Simulate a nonparticipating writer; participating owner login
		// commits after this operation releases the shared lock.
		rewriteCodexBarRecord(t, source.root, func(record map[string]any) {
			record["managedHomePath"] = newHome
			record["authFingerprint"] = renewalDigest(newAuth)
		})
		return codexBarRenewalTestResponse(now), nil
	}
	c.enableCodexBarRenewal()
	if _, err := listAfterCodexBarRenewal(c, context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := listAfterCodexBarRenewal(c, context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(newHome, "auth.json"))
	if err != nil || string(got) != string(newAuth) {
		t.Fatal("new owner login overwritten")
	}
	manifest, err := source.loadManifest()
	if err != nil || manifest.Accounts[0].ManagedHomePath != newHome {
		t.Fatal("new owner manifest overwritten")
	}
}

func TestCodexBarRenewalFailuresNeverReplayExchange(t *testing.T) {
	for _, failure := range []string{"network", "identity", "expired response", "empty response", "panic"} {
		t.Run(failure, func(t *testing.T) {
			c, source, now := newCodexBarRenewalTest(t)
			before, _ := os.ReadFile(codexBarAuthPath(source.root))
			calls := 0
			c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
				calls++
				response := codexBarRenewalTestResponse(now)
				switch failure {
				case "network":
					return nil, context.DeadlineExceeded
				case "identity":
					response.AccessToken = fakeCodexJWTWithExpiry("other@example.test", "acct-other", "user-other", "plus", now.Add(time.Hour).Unix())
				case "expired response":
					response.AccessToken = fakeCodexJWTWithExpiry("one@example.test", "acct-1", "user-1", "plus", now.Add(-time.Minute).Unix())
				case "empty response":
					return nil, nil
				case "panic":
					panic("synthetic private exchange error")
				}
				return response, nil
			}
			c.enableCodexBarRenewal()
			if _, err := listAfterCodexBarRenewal(c, context.Background()); err != nil {
				t.Fatal(err)
			}
			// Discard volatile state to prove persisted attempt fences replay.
			c.codexBarRenewal = nil
			c.enableCodexBarRenewal()
			if _, err := listAfterCodexBarRenewal(c, context.Background()); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(codexBarAuthPath(source.root))
			if calls != 1 || string(before) != string(after) {
				t.Fatalf("calls=%d; failed exchange replayed or auth changed", calls)
			}
		})
	}
}

type codexBarRenewalFaultFS struct {
	codexBarRenewalTestFS
	beforeRename func(string, string) error
}

func (fs *codexBarRenewalFaultFS) OpenDurableDirectory(path string) (fsutil.DurableDirectory, error) {
	opened, err := fs.OSFileSystem.OpenDurableDirectory(path)
	if err != nil {
		return nil, err
	}
	return &codexBarRenewalFaultDirectory{SecureDirectory: opened.(fsutil.SecureDirectory), path: path, fs: fs}, nil
}

func (fs *codexBarRenewalFaultFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	opened, err := fs.OSFileSystem.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &codexBarRenewalFaultDirectory{SecureDirectory: opened, path: path, fs: fs}, nil
}

type codexBarRenewalFaultDirectory struct {
	fsutil.SecureDirectory
	path string
	fs   *codexBarRenewalFaultFS
}

func (d *codexBarRenewalFaultDirectory) Mkdir(name string, perm os.FileMode) error {
	return d.SecureDirectory.(fsutil.DurableDirectory).Mkdir(name, perm)
}

func (d *codexBarRenewalFaultDirectory) OpenDirectory(name string) (fsutil.DurableDirectory, error) {
	return d.fs.OpenDurableDirectory(filepath.Join(d.path, name))
}

func (d *codexBarRenewalFaultDirectory) RenameChecked(oldName, newName string, identity fsutil.SecureFileIdentity) error {
	if d.fs.beforeRename != nil {
		if err := d.fs.beforeRename(d.path, newName); err != nil {
			return err
		}
	}
	return d.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameChecked(oldName, newName, identity)
}

func (d *codexBarRenewalFaultDirectory) RenameNoReplaceChecked(oldName, newName string, identity fsutil.SecureFileIdentity) error {
	return d.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, identity)
}

func (d *codexBarRenewalFaultDirectory) RemoveChecked(name string, identity fsutil.SecureFileIdentity) error {
	return d.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, identity)
}

func TestCodexBarRenewalPersistenceRecovery(t *testing.T) {
	for _, failure := range []string{"intent", "result", "auth", "manifest"} {
		t.Run(failure, func(t *testing.T) {
			c, source, now := newCodexBarRenewalTest(t)
			fs := &codexBarRenewalFaultFS{codexBarRenewalTestFS: c.Store.FS.(codexBarRenewalTestFS)}
			c.Store.FS = fs
			stateWrites, failed := 0, false
			fs.beforeRename = func(directory, name string) error {
				if failed {
					return nil
				}
				if strings.HasSuffix(directory, "codexbar-renewal") {
					stateWrites++
				}
				if failure == "intent" && stateWrites == 1 || failure == "result" && stateWrites == 2 || failure == "auth" && name == "auth.json" || failure == "manifest" && name == "managed-codex-accounts.json" {
					failed = true
					return os.ErrPermission
				}
				return nil
			}
			calls := 0
			c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
				calls++
				return codexBarRenewalTestResponse(now), nil
			}
			c.enableCodexBarRenewal()
			_, _ = listAfterCodexBarRenewal(c, context.Background())
			if !failed {
				t.Fatal("fault not exercised")
			}
			if failure == "intent" && calls != 0 {
				t.Fatal("exchange preceded durable intent")
			}
			if failure != "intent" && calls != 1 {
				t.Fatal("expected one exchange")
			}
			if failure != "result" {
				// Published durable results recover across coordinator restart.
				c.codexBarRenewal = nil
				c.enableCodexBarRenewal()
			}
			inventory, err := listAfterCodexBarRenewal(c, context.Background())
			if err != nil || len(inventory.Accounts) != 1 || CandidateAvailabilityAt(inventory.Accounts[0].Candidates[0], now) != CandidateReady {
				t.Fatal("publication did not recover")
			}
			if calls != 1 {
				t.Fatal("recovery repeated exchange")
			}
			if _, err := source.List(context.Background()); err != nil {
				t.Fatalf("recovered fingerprint: %v", err)
			}
		})
	}
}

func TestCodexBarRenewalMissingDurableResultBlocksAfterRestart(t *testing.T) {
	c, _, now := newCodexBarRenewalTest(t)
	fs := &codexBarRenewalFaultFS{codexBarRenewalTestFS: c.Store.FS.(codexBarRenewalTestFS)}
	c.Store.FS = fs
	writes := 0
	fs.beforeRename = func(directory, name string) error {
		if strings.HasSuffix(directory, "codexbar-renewal") {
			writes++
			if writes > 1 {
				return os.ErrPermission
			}
		}
		return nil
	}
	calls := 0
	c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		calls++
		return codexBarRenewalTestResponse(now), nil
	}
	c.enableCodexBarRenewal()
	_, _ = listAfterCodexBarRenewal(c, context.Background())
	c.codexBarRenewal = nil
	c.enableCodexBarRenewal()
	fs.beforeRename = nil
	inventory, err := listAfterCodexBarRenewal(c, context.Background())
	if err != nil || len(inventory.Accounts) != 1 || CandidateAvailabilityAt(inventory.Accounts[0].Candidates[0], now) != CandidateUnavailable || calls != 1 {
		t.Fatal("indeterminate exchange replayed after lost result")
	}
}

func listAfterCodexBarRenewal(c *CredentialCoordinator, ctx context.Context) (Inventory, error) {
	if c.codexBarRenewal != nil {
		c.codexBarRenewal.renew(ctx, c.ExternalSources, c.RefreshExchange, c.Now)
	}
	return c.List(ctx)
}

func TestCodexBarRenewalStalledExchangeKeepsOwnerRPCResponsive(t *testing.T) {
	c, source, now := newCodexBarRenewalTest(t)
	home, _ := c.Store.FS.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), inventoryAuth(fakeCodexJWTWithExpiry("two@example.test", "acct-2", "user-2", "plus", now.Add(time.Hour).Unix()), "acct-2", fakeCodexJWT("two@example.test", "acct-2", "user-2", "plus"), 0), 0o600); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c.RefreshExchange = func(ctx context.Context, _ string) (*auth.CodexTokenResponse, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		response := codexBarRenewalTestResponse(now)
		response.IDToken = "" // Retained older ID token must not govern access expiry.
		return response, nil
	}
	temporaryRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	shortSocketDir, err := os.MkdirTemp(temporaryRoot, "cq-renew-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(shortSocketDir)
	owner, err := OpenCredentialControlPrepared(context.Background(), filepath.Join(shortSocketDir, "credential.sock"), c, initialiseCredentialOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	delegate, err := OpenCredentialControlPrepared(context.Background(), filepath.Join(shortSocketDir, "credential.sock"), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer delegate.Close()
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		inventory, err := delegate.List(ctx)
		cancel()
		if err != nil {
			t.Fatalf("responsive inventory %d: %v", i, err)
		}
		healthy := false
		for _, account := range inventory.Accounts {
			for _, candidate := range account.Candidates {
				if account.Identity.AccountID == "acct-2" && CandidateAvailabilityAt(candidate, now) == CandidateReady {
					healthy = true
				}
			}
		}
		if !healthy {
			t.Fatal("healthy account was unavailable during unrelated renewal")
		}
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("renewal did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- owner.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("owner released before renewal: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner did not close after renewal")
	}
	if calls.Load() != 1 {
		t.Fatalf("exchanges = %d", calls.Load())
	}
	candidates, err := source.List(context.Background())
	if err != nil || len(candidates) != 1 || !candidates[0].AccessExpiresAt.After(now) {
		t.Fatal("new access expiry was not published")
	}
}

func TestCodexBarRenewalPartialPublicationKeepsHealthyOwnerAccount(t *testing.T) {
	c, source, now := newCodexBarRenewalTest(t)
	manifest, err := source.loadValidatedManifest()
	if err != nil {
		t.Fatal(err)
	}
	healthyData := inventoryAuth(fakeCodexJWTWithExpiry("two@example.test", "acct-2", "user-2", "plus", now.Add(time.Hour).Unix()), "acct-2", fakeCodexJWT("two@example.test", "acct-2", "user-2", "plus"), 0)
	healthyHome := filepath.Join(source.root, "managed-codex-homes", "healthy")
	if err := os.MkdirAll(healthyHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(healthyHome, "auth.json"), healthyData, 0o600); err != nil {
		t.Fatal(err)
	}
	record := manifest.value.Accounts[0]
	record.ID = "healthy"
	record.ManagedHomePath = healthyHome
	record.ProviderAccountID = "acct-2"
	record.WorkspaceAccountID = "acct-2"
	record.AuthFingerprint = renewalDigest(healthyData)
	manifest.value.Accounts = append(manifest.value.Accounts, record)
	data, _ := json.Marshal(manifest.value)
	if err := os.WriteFile(manifest.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fs := &codexBarRenewalFaultFS{codexBarRenewalTestFS: c.Store.FS.(codexBarRenewalTestFS)}
	c.Store.FS = fs
	fs.beforeRename = func(_, name string) error {
		if name == "managed-codex-accounts.json" {
			return os.ErrPermission
		}
		return nil
	}
	c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		return codexBarRenewalTestResponse(now), nil
	}
	c.enableCodexBarRenewal()
	_, _ = listAfterCodexBarRenewal(c, context.Background())
	candidates, err := source.List(context.Background())
	if err != nil || len(candidates) != 1 || candidates[0].Identity.AccountID != "acct-2" {
		t.Fatalf("healthy owner account lost during recoverable publication: %v", err)
	}
	// A fingerprint mismatch unrelated to this exact private transaction still
	// fails closed; the exception is not a general malformed-record filter.
	if err := os.WriteFile(filepath.Join(healthyHome, "auth.json"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.List(context.Background()); err == nil {
		t.Fatal("unrelated owner tampering accepted")
	}
}

func TestCodexBarRenewalMalformedJournalStaysBlocked(t *testing.T) {
	for _, failure := range []string{"missing auth", "null auth", "wrong identity", "consistent wrong user", "missing root", "incomplete record", "malformed auth"} {
		t.Run(failure, func(t *testing.T) {
			c, source, now := newCodexBarRenewalTest(t)
			manifest, _ := source.loadValidatedManifest()
			record := manifest.value.Accounts[0]
			old, err := source.readValidatedRecord(record, "")
			if err != nil {
				t.Fatal(err)
			}
			renewed, err := renewedCodexBarAuth(old, codexBarRenewalTestResponse(now), now)
			if err != nil {
				t.Fatal(err)
			}
			txn := &codexBarRenewalTransaction{Version: 1, Root: source.root, Record: record, Identity: identityFromAccount(old.account), OriginalRevision: old.revision, OriginalDigest: renewalDigest(old.data), Stage: "result", Auth: renewed}
			switch failure {
			case "missing auth":
				txn.Auth = nil
			case "null auth":
				txn.Auth = json.RawMessage("null")
			case "wrong identity":
				txn.Auth = inventoryAuth(fakeCodexJWTWithExpiry("two@example.test", "acct-2", "user-2", "plus", now.Add(time.Hour).Unix()), "acct-2", fakeCodexJWT("two@example.test", "acct-2", "user-2", "plus"), 0)
			case "consistent wrong user":
				txn.Identity.UserID = "different-user"
				txn.Auth = inventoryAuth(fakeCodexJWTWithExpiry("one@example.test", "acct-1", "different-user", "plus", now.Add(time.Hour).Unix()), "acct-1", fakeCodexJWT("one@example.test", "acct-1", "different-user", "plus"), 0)
			case "missing root":
				txn.Root = ""
			case "incomplete record":
				txn.Record.ManagedHomePath = ""
			case "malformed auth":
				txn.Auth = json.RawMessage(`{"tokens":{}}`)
			}
			c.enableCodexBarRenewal()
			path := c.codexBarRenewal.transactionPath(source.root, record.ID)
			if err := c.codexBarRenewal.persist(path, txn); err != nil {
				t.Fatal(err)
			}
			c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
				t.Fatal("invalid journal replayed refresh")
				return nil, nil
			}
			for range 2 {
				if _, err := listAfterCodexBarRenewal(c, context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			after, _ := os.ReadFile(old.path)
			if string(after) != string(old.data) {
				t.Fatal("invalid journal overwrote owner auth")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("invalid journal discarded")
			}
		})
	}
}

func TestCodexBarRenewalSafetyWindow(t *testing.T) {
	for _, lifetime := range []time.Duration{30 * time.Second, 2 * time.Minute} {
		t.Run(lifetime.String(), func(t *testing.T) {
			c, source, now := newCodexBarRenewalTest(t)
			writeCodexBarAuthAndFingerprint(t, source.root, inventoryAuth(fakeCodexJWTWithExpiry("one@example.test", "acct-1", "user-1", "plus", now.Add(lifetime).Unix()), "acct-1", fakeCodexJWT("one@example.test", "acct-1", "user-1", "plus"), 0))
			calls := 0
			c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
				calls++
				return codexBarRenewalTestResponse(now), nil
			}
			owner, err := OpenCredentialControlPrepared(context.Background(), shortControlPath(t), c, initialiseCredentialOwner)
			if err != nil {
				t.Fatal(err)
			}
			_, err = owner.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = owner.Close()
			if err != nil {
				t.Fatal(err)
			}
			expected := 0
			if lifetime < time.Minute {
				expected = 1
			}
			if calls != expected {
				t.Fatalf("calls=%d, want %d", calls, expected)
			}
		})
	}
}

type codexBarRenewalReadHookFS struct {
	codexBarReadFileSystem
	beforeOpen func(string)
}

func (fs *codexBarRenewalReadHookFS) OpenNoFollow(root, path string) (codexBarReadFile, error) {
	if fs.beforeOpen != nil {
		fs.beforeOpen(path)
	}
	return fs.codexBarReadFileSystem.OpenNoFollow(root, path)
}

func TestCodexBarRenewalCompletedPublicationRetriesOldManifest(t *testing.T) {
	c, source, now := newCodexBarRenewalTest(t)
	c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		return codexBarRenewalTestResponse(now), nil
	}
	c.enableCodexBarRenewal()
	hooked := false
	fs := &codexBarRenewalReadHookFS{codexBarReadFileSystem: source.fs}
	fs.beforeOpen = func(path string) {
		if hooked || path != codexBarAuthPath(source.root) {
			return
		}
		hooked = true
		c.codexBarRenewal.renew(context.Background(), c.ExternalSources, c.RefreshExchange, c.Now)
	}
	source.fs = fs
	candidates, err := source.List(context.Background())
	if err != nil || len(candidates) != 1 || !candidates[0].AccessExpiresAt.After(now) {
		t.Fatalf("snapshot publication race: %v", err)
	}
	manifest, _ := source.loadValidatedManifest()
	if _, err := os.Stat(c.codexBarRenewal.transactionPath(source.root, manifest.value.Accounts[0].ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("publication fixture did not remove journal")
	}
}

func (r *codexBarRenewal) renew(ctx context.Context, sources []ExternalCredentialSource, exchange RefreshExchange, clock func() time.Time) {
	// One inventory read has a bounded renewal budget. A busy owner is never
	// waited out, and a timed-out token exchange is never replayed.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case r.gate <- struct{}{}:
		defer func() { <-r.gate }()
	case <-ctx.Done():
		return
	}
	r.renewSources(ctx, sources, exchange, clock)
}

func TestCodexBarRenewalIgnoresSystemAndOtherExternalSources(t *testing.T) {
	c, source, now := newCodexBarRenewalTest(t)
	before, _ := os.ReadFile(codexBarAuthPath(source.root))
	home, _ := c.Store.FS.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	system := inventoryAuth(fakeCodexJWTWithExpiry("two@example.test", "acct-2", "user-2", "plus", now.Add(-time.Minute).Unix()), "acct-2", fakeCodexJWT("two@example.test", "acct-2", "user-2", "plus"), 0)
	systemPath := filepath.Join(home, ".codex", "auth.json")
	if err := os.WriteFile(systemPath, system, 0o600); err != nil {
		t.Fatal(err)
	}
	c.ExternalSources = []ExternalCredentialSource{&fakeExternalCredentialSource{name: "codexbar"}}
	c.RefreshExchange = func(context.Context, string) (*auth.CodexTokenResponse, error) {
		t.Fatal("non-CodexBar authority refreshed")
		return nil, nil
	}
	c.enableCodexBarRenewal()
	if _, err := listAfterCodexBarRenewal(c, context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(codexBarAuthPath(source.root))
	systemAfter, _ := os.ReadFile(systemPath)
	if string(after) != string(before) || string(systemAfter) != string(system) {
		t.Fatal("read-only auth changed")
	}
}
