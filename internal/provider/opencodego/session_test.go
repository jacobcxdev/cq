package opencodego

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/credentials"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestSessionAuthoritySavesPrivateEncryptedSession(t *testing.T) {
	ctx := context.Background()
	a, fsys, session, _ := sessionAuthorityFixture(t)
	if _, err := a.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read absent session: %v", err)
	}
	if err := a.Save(ctx, session); err != nil {
		t.Fatal(err)
	}
	if got, err := a.Fresh(ctx); err != nil || !sameSession(got, session) {
		t.Fatalf("read saved session: %v", err)
	}
	for _, filename := range []string{oauthSessionFile, oauthKeyFile, oauthLockFile} {
		if err := fsutil.ValidateSecureRegularFile(fsys, filepath.Join(a.ConfigDir, filename)); err != nil {
			t.Fatalf("%s is not private: %v", filename, err)
		}
	}
	path := filepath.Join(a.ConfigDir, oauthSessionFile)
	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{session.AccessToken, session.RefreshToken, session.Email} {
		if bytes.Contains(data, []byte(value)) {
			t.Fatal("stored session contains plaintext")
		}
	}
	data[len(data)-1] ^= 1
	if err := fsys.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt OAuth file must fail closed: %v", err)
	}
}

func TestSessionAuthoritySerialisesConcurrentRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, _, session, now := sessionAuthorityFixture(t)
	if err := a.Save(ctx, session); err != nil {
		t.Fatal(err)
	}
	*now = session.ExpiresAt.Add(-time.Second)
	refreshed := session
	refreshed.AccessToken = "new-access-to-open-code"
	refreshed.RefreshToken = "new-refresh-to-open-code"
	refreshed.ExpiresAt = now.Add(time.Hour)
	entered, release := make(chan struct{}), make(chan struct{})
	var attempts atomic.Int32
	a.Exchange = func(ctx context.Context, prior OAuthSession) (OAuthSession, error) {
		if attempts.Add(1) == 1 {
			close(entered)
		}
		if !sameSession(prior, session) {
			t.Error("exchange received a different session")
		}
		select {
		case <-ctx.Done():
			return OAuthSession{}, ctx.Err()
		case <-release:
			return refreshed, nil
		}
	}
	results := make(chan error, 2)
	run := func(authority SessionAuthority) {
		defer func() {
			if recover() != nil {
				results <- errors.New("session fetch panicked")
			}
		}()
		if got, err := authority.Fresh(ctx); err != nil {
			results <- err
		} else if !sameSession(got, refreshed) {
			results <- errors.New("unexpected session after refresh")
		} else {
			results <- nil
		}
	}
	go run(a)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("refresh exchange did not start")
	}

	// The first exchange has marked the record pending, but still owns the
	// lock. A second process must wait for the committed result.
	go run(a)
	select {
	case err := <-results:
		close(release)
		t.Fatalf("returned while another refresh owned the store: %v", err)
	case <-time.After(35 * time.Millisecond):
		close(release)
	}
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("refresh readers did not complete")
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("refresh attempts = %d, want 1", got)
	}
}

func TestSessionAuthorityRequiresLoginAfterFailedRotation(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{"uncertain", errors.New("refresh failed for secret-access-token"), credentials.ErrOutcomeIndeterminate},
		{"rejected", ErrOAuthRejected, ErrOAuthRejected},
		{"changed workspace", nil, credentials.ErrOutcomeIndeterminate},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			a, _, session, now := sessionAuthorityFixture(t)
			if err := a.Save(ctx, session); err != nil {
				t.Fatal(err)
			}
			*now = session.ExpiresAt
			var attempts int
			a.Exchange = func(context.Context, OAuthSession) (OAuthSession, error) {
				attempts++
				other := session
				other.RefreshToken = "new-refresh"
				other.WorkspaceID = "another-workspace"
				other.ExpiresAt = now.Add(time.Hour)
				return other, test.err
			}
			for range 2 {
				if _, err := a.Fresh(ctx); !errors.Is(err, test.want) || strings.Contains(err.Error(), session.AccessToken) {
					t.Fatalf("failed rotation classification: %v", err)
				}
			}
			if attempts != 1 {
				t.Fatalf("refresh attempts = %d, want 1", attempts)
			}
			if got, err := a.Read(); err != nil || !sameSession(got, session) {
				t.Fatalf("failed rotation changed saved identity: %v", err)
			}
			newLogin := session
			newLogin.WorkspaceID = "new-approved-workspace"
			newLogin.MemberID = "different-verified-member"
			newLogin.RefreshToken = "fresh-login-refresh"
			newLogin.ExpiresAt = now.Add(time.Hour)
			if err := a.Save(ctx, newLogin); err != nil {
				t.Fatal(err)
			}
			if got, err := a.Fresh(ctx); err != nil || !sameSession(got, newLogin) {
				t.Fatalf("new login did not replace expired session: %v", err)
			}
		})
	}
}

func TestSessionAuthorityRejectsRefreshCommitAfterLockReplacement(t *testing.T) {
	ctx := context.Background()
	a, fsys, session, now := sessionAuthorityFixture(t)
	if err := a.Save(ctx, session); err != nil {
		t.Fatal(err)
	}
	*now = session.ExpiresAt
	a.Exchange = func(context.Context, OAuthSession) (OAuthSession, error) {
		lock := filepath.Join(a.ConfigDir, oauthLockFile)
		if err := fsys.Remove(lock); err != nil {
			t.Fatal(err)
		}
		if err := fsys.WriteFile(lock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		next := session
		next.AccessToken = "new-access"
		next.RefreshToken = "new-refresh"
		next.ExpiresAt = now.Add(time.Hour)
		return next, nil
	}
	if _, err := a.Fresh(ctx); !errors.Is(err, credentials.ErrOutcomeIndeterminate) {
		t.Fatalf("refresh after owner-lock replacement: %v", err)
	}
	if got, err := a.Read(); err != nil || !sameSession(got, session) {
		t.Fatalf("replaced owner lock admitted refresh commit: %v", err)
	}
}

func TestSessionAuthorityRejectsInsecureExistingFile(t *testing.T) {
	ctx := context.Background()
	a, fsys, session, _ := sessionAuthorityFixture(t)
	if err := a.Save(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chmod(filepath.Join(a.ConfigDir, oauthSessionFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("insecure OAuth file must fail closed: %v", err)
	}
	if err := a.Save(ctx, session); err == nil {
		t.Fatal("save accepted an insecure OAuth file")
	}
}

func sessionAuthorityFixture(t *testing.T) (SessionAuthority, *fsutil.MemFS, OAuthSession, *time.Time) {
	t.Helper()
	fsys := fsutil.NewMemFS()
	dir := filepath.Join(string(filepath.Separator), "cq")
	if err := fsys.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.November, 18, 15, 30, 0, 0, time.UTC)
	a := SessionAuthority{FS: fsys, ConfigDir: dir, Now: func() time.Time { return now }}
	session := OAuthSession{
		AccessToken: "secret-access-token", RefreshToken: "secret-refresh-token",
		MemberID: "verified-member", WorkspaceID: "approved-workspace",
		Email: "go-member@example.test", ExpiresAt: now.Add(time.Hour),
	}
	return a, fsys, session, &now
}

func sameSession(a, b OAuthSession) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken &&
		a.MemberID == b.MemberID && a.WorkspaceID == b.WorkspaceID && a.Email == b.Email &&
		a.ExpiresAt.Equal(b.ExpiresAt)
}
