package keyring

import (
	"encoding/json"
	"os"
	"testing"
)

// ── mergeAnonymousFresh ───────────────────────────────────────────────────────

func TestMergeAnonymousFresh(t *testing.T) {
	tests := []struct {
		name     string
		input    []ClaudeOAuth
		wantLen  int
		wantAt0  ClaudeOAuth // only checked when wantLen > 0
		checkAt0 bool
	}{
		{
			name:    "empty slice",
			input:   []ClaudeOAuth{},
			wantLen: 0,
		},
		{
			name: "single entry no merge possible",
			input: []ClaudeOAuth{
				{Email: "a@example.com", AccessToken: "tok", ExpiresAt: 100},
			},
			wantLen:  1,
			checkAt0: true,
			wantAt0:  ClaudeOAuth{Email: "a@example.com", AccessToken: "tok", ExpiresAt: 100},
		},
		{
			name: "anonymous fresher than identified — merges token",
			input: []ClaudeOAuth{
				{Email: "a@example.com", AccountUUID: "uuid1", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 100},
				{AccessToken: "new-at", RefreshToken: "new-rt", ExpiresAt: 200}, // anonymous, fresher
			},
			wantLen:  1,
			checkAt0: true,
			wantAt0: ClaudeOAuth{
				Email: "a@example.com", AccountUUID: "uuid1",
				AccessToken: "new-at", RefreshToken: "new-rt", ExpiresAt: 200,
			},
		},
		{
			name: "anonymous staler than identified — no merge",
			input: []ClaudeOAuth{
				{Email: "a@example.com", AccountUUID: "uuid1", AccessToken: "new-at", RefreshToken: "new-rt", ExpiresAt: 300},
				{AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 100}, // anonymous, staler
			},
			wantLen: 2, // anonymous entry kept, no merge
		},
		{
			name: "multiple anonymous entries — only fresher one merges",
			input: []ClaudeOAuth{
				{Email: "a@example.com", AccountUUID: "uuid1", AccessToken: "base-at", RefreshToken: "base-rt", ExpiresAt: 100},
				{AccessToken: "anon1-at", RefreshToken: "anon1-rt", ExpiresAt: 50},  // staler, no merge
				{AccessToken: "anon2-at", RefreshToken: "anon2-rt", ExpiresAt: 200}, // fresher, merges
			},
			// anon2 merges into identified; anon1 is kept (staler, no merge target)
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeAnonymousFresh(tt.input)
			if len(got) != tt.wantLen {
				t.Fatalf("len = %d, want %d; got %+v", len(got), tt.wantLen, got)
			}
			if tt.checkAt0 && tt.wantLen == 1 {
				a := got[0]
				w := tt.wantAt0
				if a.Email != w.Email || a.AccountUUID != w.AccountUUID ||
					a.AccessToken != w.AccessToken || a.RefreshToken != w.RefreshToken ||
					a.ExpiresAt != w.ExpiresAt {
					t.Errorf("got[0] = %+v, want %+v", a, w)
				}
			}
		})
	}
}

func TestMergeAnonymousFreshScopesCarried(t *testing.T) {
	// Scopes from anonymous entry should propagate when non-empty.
	input := []ClaudeOAuth{
		{Email: "a@example.com", AccountUUID: "uuid1", AccessToken: "old", ExpiresAt: 10},
		{AccessToken: "new", ExpiresAt: 20, Scopes: []string{"scope1", "scope2"}},
	}
	got := mergeAnonymousFresh(input)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if len(got[0].Scopes) != 2 || got[0].Scopes[0] != "scope1" {
		t.Errorf("scopes = %v, want [scope1 scope2]", got[0].Scopes)
	}
}

// ── dedupByEmail ─────────────────────────────────────────────────────────────

func TestDedupByEmail(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := dedupByEmail(nil); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("single entry passthrough", func(t *testing.T) {
		input := []ClaudeOAuth{{Email: "a@example.com", AccessToken: "tok"}}
		got := dedupByEmail(input)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
	})

	t.Run("same email prefers fresher ExpiresAt", func(t *testing.T) {
		input := []ClaudeOAuth{
			{Email: "a@example.com", AccessToken: "old", ExpiresAt: 100},
			{Email: "a@example.com", AccessToken: "new", ExpiresAt: 200},
		}
		got := dedupByEmail(input)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if got[0].AccessToken != "new" {
			t.Errorf("AccessToken = %q, want %q", got[0].AccessToken, "new")
		}
	})

	t.Run("no-email entries preserved when no email entries exist", func(t *testing.T) {
		input := []ClaudeOAuth{
			{AccessToken: "tok1", AccountUUID: ""},
			{AccessToken: "tok2", AccountUUID: ""},
		}
		got := dedupByEmail(input)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
	})

	t.Run("no-email entries without UUID dropped when email entries exist", func(t *testing.T) {
		input := []ClaudeOAuth{
			{Email: "a@example.com", AccessToken: "tok1"},
			{AccessToken: "tok2"}, // no email, no UUID — stale
		}
		got := dedupByEmail(input)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1; got %+v", len(got), got)
		}
		if got[0].Email != "a@example.com" {
			t.Errorf("Email = %q, want %q", got[0].Email, "a@example.com")
		}
	})

	t.Run("no-email entry with UUID preserved alongside email entry", func(t *testing.T) {
		input := []ClaudeOAuth{
			{Email: "a@example.com", AccessToken: "tok1"},
			{AccountUUID: "uuid2", AccessToken: "tok2"}, // no email but has UUID
		}
		got := dedupByEmail(input)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2; got %+v", len(got), got)
		}
	})

	t.Run("mixed entries with and without email", func(t *testing.T) {
		input := []ClaudeOAuth{
			{Email: "a@example.com", AccessToken: "tokA1", ExpiresAt: 100},
			{Email: "b@example.com", AccessToken: "tokB"},
			{Email: "a@example.com", AccessToken: "tokA2", ExpiresAt: 200},
			{AccessToken: "anon"}, // anonymous, dropped because email entries exist
		}
		got := dedupByEmail(input)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2; got %+v", len(got), got)
		}
		// find a@example.com entry
		var aEntry ClaudeOAuth
		for _, g := range got {
			if g.Email == "a@example.com" {
				aEntry = g
			}
		}
		if aEntry.AccessToken != "tokA2" {
			t.Errorf("a@example.com AccessToken = %q, want %q", aEntry.AccessToken, "tokA2")
		}
	})
}

// ── sameStoredAccount ─────────────────────────────────────────────────────────

func TestSameStoredAccount(t *testing.T) {
	t.Run("nil stored", func(t *testing.T) {
		if sameStoredAccount(nil, &ClaudeOAuth{Email: "a@example.com"}) {
			t.Error("expected false for nil stored")
		}
	})

	t.Run("nil acct", func(t *testing.T) {
		if sameStoredAccount(&ClaudeOAuth{Email: "a@example.com"}, nil) {
			t.Error("expected false for nil acct")
		}
	})

	t.Run("both nil", func(t *testing.T) {
		if sameStoredAccount(nil, nil) {
			t.Error("expected false for both nil")
		}
	})

	t.Run("email match", func(t *testing.T) {
		a := &ClaudeOAuth{Email: "a@example.com", AccessToken: "tok1"}
		b := &ClaudeOAuth{Email: "a@example.com", AccessToken: "tok2"}
		if !sameStoredAccount(a, b) {
			t.Error("expected true for matching email")
		}
	})

	t.Run("UUID match", func(t *testing.T) {
		a := &ClaudeOAuth{AccountUUID: "uuid-abc", AccessToken: "tok1"}
		b := &ClaudeOAuth{AccountUUID: "uuid-abc", AccessToken: "tok2"}
		if !sameStoredAccount(a, b) {
			t.Error("expected true for matching UUID")
		}
	})

	t.Run("RefreshToken match", func(t *testing.T) {
		a := &ClaudeOAuth{RefreshToken: "rt-xyz"}
		b := &ClaudeOAuth{RefreshToken: "rt-xyz"}
		if !sameStoredAccount(a, b) {
			t.Error("expected true for matching RefreshToken")
		}
	})

	t.Run("AccessToken match", func(t *testing.T) {
		a := &ClaudeOAuth{AccessToken: "at-xyz"}
		b := &ClaudeOAuth{AccessToken: "at-xyz"}
		if !sameStoredAccount(a, b) {
			t.Error("expected true for matching AccessToken")
		}
	})

	t.Run("no match", func(t *testing.T) {
		a := &ClaudeOAuth{Email: "a@example.com", AccountUUID: "uuid1", RefreshToken: "rt1", AccessToken: "at1"}
		b := &ClaudeOAuth{Email: "b@example.com", AccountUUID: "uuid2", RefreshToken: "rt2", AccessToken: "at2"}
		if sameStoredAccount(a, b) {
			t.Error("expected false for completely different accounts")
		}
	})

	t.Run("empty fields do not match", func(t *testing.T) {
		// Both have empty email/uuid/rt/at — should not match on empty strings.
		a := &ClaudeOAuth{}
		b := &ClaudeOAuth{}
		if sameStoredAccount(a, b) {
			t.Error("expected false when all fields are empty")
		}
	})
}

// ── accountKey ────────────────────────────────────────────────────────────────

func TestAccountKey(t *testing.T) {
	tests := []struct {
		name string
		acct ClaudeOAuth
		want string
	}{
		{
			name: "with AccountUUID returns uuid prefix",
			acct: ClaudeOAuth{AccountUUID: "abc-123", RefreshToken: "rt", AccessToken: "at"},
			want: "uuid:abc-123",
		},
		{
			name: "no UUID but has RefreshToken returns rt prefix",
			acct: ClaudeOAuth{RefreshToken: "rt-tok", AccessToken: "at-tok"},
			want: "rt:" + Hash8("rt-tok"),
		},
		{
			name: "fallback to AccessToken returns at prefix",
			acct: ClaudeOAuth{AccessToken: "at-only"},
			want: "at:" + Hash8("at-only"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := accountKey(&tt.acct)
			if got != tt.want {
				t.Errorf("accountKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ── loadManifest ──────────────────────────────────────────────────────────────

func TestLoadManifest(t *testing.T) {
	t.Run("valid file returns entries", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/accounts.json"
		content := `[{"uuid":"u1","email":"a@example.com"},{"uuid":"u2","email":"b@example.com"}]`
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := loadManifest(path)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if got[0].UUID != "u1" || got[0].Email != "a@example.com" {
			t.Errorf("got[0] = %+v, want {u1, a@example.com}", got[0])
		}
		if got[1].UUID != "u2" || got[1].Email != "b@example.com" {
			t.Errorf("got[1] = %+v, want {u2, b@example.com}", got[1])
		}
	})

	t.Run("missing file returns nil", func(t *testing.T) {
		dir := t.TempDir()
		got := loadManifest(dir + "/nonexistent.json")
		if got != nil {
			t.Errorf("expected nil for missing file, got %+v", got)
		}
	})

	t.Run("corrupt JSON returns nil", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/accounts.json"
		if err := os.WriteFile(path, []byte("not valid json {{{"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := loadManifest(path)
		if got != nil {
			t.Errorf("expected nil for corrupt JSON, got %+v", got)
		}
	})
}

// ── saveManifest ──────────────────────────────────────────────────────────────

func TestSaveManifest(t *testing.T) {
	t.Run("round-trip save then load returns same entries", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/sub/accounts.json"
		entries := []manifestEntry{
			{UUID: "uuid-1", Email: "alice@example.com"},
			{UUID: "uuid-2", Email: "bob@example.com"},
		}
		saveManifest(path, entries)

		got := loadManifest(path)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		for i, want := range entries {
			if got[i].UUID != want.UUID || got[i].Email != want.Email {
				t.Errorf("got[%d] = %+v, want %+v", i, got[i], want)
			}
		}
	})

	t.Run("saved file is valid JSON array", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/accounts.json"
		entries := []manifestEntry{{UUID: "u1", Email: "x@example.com"}}
		saveManifest(path, entries)

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read saved file: %v", err)
		}
		// Must be parseable as a JSON array
		var out []manifestEntry
		if err := json.Unmarshal(data, &out); err != nil {
			t.Errorf("saved file is not valid JSON: %v\ncontent: %s", err, data)
		}
	})
}

// ── WriteCredentialsFile ──────────────────────────────────────────────────────

func TestWriteCredentialsFile(t *testing.T) {
	t.Run("writes valid JSON that round-trips", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)

		creds := &ClaudeCredentials{
			ClaudeAiOauth: &ClaudeOAuth{
				AccessToken:  "tok-abc",
				RefreshToken: "rt-xyz",
				ExpiresAt:    123456789,
				Email:        "user@example.com",
			},
		}
		if err := WriteCredentialsFile(creds); err != nil {
			t.Fatalf("WriteCredentialsFile() error = %v", err)
		}

		data, err := os.ReadFile(dir + "/.claude/.credentials.json")
		if err != nil {
			t.Fatalf("read back file: %v", err)
		}
		var got ClaudeCredentials
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.ClaudeAiOauth == nil {
			t.Fatal("claudeAiOauth is nil")
		}
		if got.ClaudeAiOauth.AccessToken != "tok-abc" {
			t.Errorf("AccessToken = %q, want tok-abc", got.ClaudeAiOauth.AccessToken)
		}
		if got.ClaudeAiOauth.Email != "user@example.com" {
			t.Errorf("Email = %q, want user@example.com", got.ClaudeAiOauth.Email)
		}
	})

	t.Run("creates .claude directory if missing", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)

		creds := &ClaudeCredentials{
			ClaudeAiOauth: &ClaudeOAuth{AccessToken: "tok", RefreshToken: "rt"},
		}
		if err := WriteCredentialsFile(creds); err != nil {
			t.Fatalf("WriteCredentialsFile() error = %v", err)
		}
		if _, err := os.Stat(dir + "/.claude"); err != nil {
			t.Errorf(".claude dir not created: %v", err)
		}
	})

	t.Run("enforces 0700 permissions on the directory", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)

		// Create dir with permissive mode first to verify chmod is enforced.
		if err := os.MkdirAll(dir+"/.claude", 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		creds := &ClaudeCredentials{
			ClaudeAiOauth: &ClaudeOAuth{AccessToken: "tok", RefreshToken: "rt"},
		}
		if err := WriteCredentialsFile(creds); err != nil {
			t.Fatalf("WriteCredentialsFile() error = %v", err)
		}
		info, err := os.Stat(dir + "/.claude")
		if err != nil {
			t.Fatalf("stat .claude: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf(".claude perm = %o, want 0700", perm)
		}
	})
}

// ── BackfillCredentialsFile ───────────────────────────────────────────────────

func TestBackfillCredentialsFile(t *testing.T) {
	writeInitialCreds := func(t *testing.T, dir string, acct ClaudeOAuth) {
		t.Helper()
		if err := os.MkdirAll(dir+"/.claude", 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		creds := ClaudeCredentials{ClaudeAiOauth: &acct}
		data, err := json.MarshalIndent(creds, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(dir+"/.claude/.credentials.json", data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	readCreds := func(t *testing.T, dir string) *ClaudeOAuth {
		t.Helper()
		data, err := os.ReadFile(dir + "/.claude/.credentials.json")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var creds ClaudeCredentials
		if err := json.Unmarshal(data, &creds); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return creds.ClaudeAiOauth
	}

	t.Run("updates email and UUID when account matches by access token", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)

		stored := ClaudeOAuth{AccessToken: "tok-shared", RefreshToken: "rt", ExpiresAt: 100}
		writeInitialCreds(t, dir, stored)

		update := &ClaudeOAuth{
			AccessToken: "tok-shared",
			Email:       "new@example.com",
			AccountUUID: "uuid-new",
		}
		BackfillCredentialsFile(update)

		got := readCreds(t, dir)
		if got.Email != "new@example.com" {
			t.Errorf("Email = %q, want new@example.com", got.Email)
		}
		if got.AccountUUID != "uuid-new" {
			t.Errorf("AccountUUID = %q, want uuid-new", got.AccountUUID)
		}
		// Token must not be overwritten
		if got.AccessToken != "tok-shared" {
			t.Errorf("AccessToken = %q, want tok-shared", got.AccessToken)
		}
	})

	t.Run("skips update when account does not match", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)

		stored := ClaudeOAuth{AccessToken: "tok-A", RefreshToken: "rt-A", ExpiresAt: 100}
		writeInitialCreds(t, dir, stored)

		update := &ClaudeOAuth{
			AccessToken: "tok-B", // different account
			Email:       "other@example.com",
			AccountUUID: "uuid-other",
		}
		BackfillCredentialsFile(update)

		got := readCreds(t, dir)
		if got.Email != "" {
			t.Errorf("Email should not be updated, got %q", got.Email)
		}
		if got.AccountUUID != "" {
			t.Errorf("AccountUUID should not be updated, got %q", got.AccountUUID)
		}
	})

	t.Run("no change when already current", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)

		stored := ClaudeOAuth{
			AccessToken: "tok-match",
			RefreshToken: "rt",
			ExpiresAt:   100,
			Email:       "already@example.com",
			AccountUUID: "uuid-already",
		}
		writeInitialCreds(t, dir, stored)

		// Backfill with identical data — no write should occur (file unchanged).
		update := &ClaudeOAuth{
			AccessToken: "tok-match",
			Email:       "already@example.com",
			AccountUUID: "uuid-already",
		}
		BackfillCredentialsFile(update)

		got := readCreds(t, dir)
		if got.Email != "already@example.com" {
			t.Errorf("Email = %q, want already@example.com", got.Email)
		}
		if got.AccountUUID != "uuid-already" {
			t.Errorf("AccountUUID = %q, want uuid-already", got.AccountUUID)
		}
	})
}

// ── Hash8 ─────────────────────────────────────────────────────────────────────

func TestHash8(t *testing.T) {
	t.Run("output length is 8 hex chars", func(t *testing.T) {
		h := Hash8("any input")
		if len(h) != 8 {
			t.Errorf("len(Hash8(...)) = %d, want 8", len(h))
		}
	})

	t.Run("deterministic — same input produces same output", func(t *testing.T) {
		h1 := Hash8("hello")
		h2 := Hash8("hello")
		if h1 != h2 {
			t.Errorf("Hash8 not deterministic: %q != %q", h1, h2)
		}
	})

	t.Run("different inputs produce different outputs", func(t *testing.T) {
		h1 := Hash8("hello")
		h2 := Hash8("world")
		if h1 == h2 {
			t.Errorf("Hash8(\"hello\") == Hash8(\"world\") = %q; expected distinct values", h1)
		}
	})

	t.Run("empty string is valid and 8 chars", func(t *testing.T) {
		h := Hash8("")
		if len(h) != 8 {
			t.Errorf("len(Hash8(\"\")) = %d, want 8", len(h))
		}
	})
}
