package proxy

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func testPrimerTarget() CodexPrimerTarget {
	return CodexPrimerTarget{
		ResetAt: time.Unix(1786834644, 0), ModelID: "gpt-5.3-codex-spark",
		Windows: []codex.WindowDescriptor{{
			RawLimitName: "backend-scope-secret", WindowName: quota.Window7Day,
			Period: 7 * 24 * time.Hour, ScopeKind: codex.WindowScopeModelFamily,
			Scope: "backend-scope-secret", ResetAt: time.Unix(1786834644, 0),
		}},
	}
}

func TestCodexPrimerStoreHashesIdentityAndClaimsOnce(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	target := testPrimerTarget()
	if err := store.Observe("account-secret", target); err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile("/state/primer.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"account-secret", "backend-scope-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("journal leaked %q", secret)
		}
	}
	claimed, err := store.Claim("account-secret", target)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	claimed, err = store.Claim("account-secret", target)
	if err != nil || claimed {
		t.Fatalf("second claim = %v, %v", claimed, err)
	}
}

func TestCodexPrimerStoreNeverReplaysAdmittedGenerationAfterRestart(t *testing.T) {
	fsys := fsutil.NewMemFS()
	store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	target := testPrimerTarget()
	if err := store.Observe("account-secret", target); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.Claim("account-secret", target); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if err := store.Mark("account-secret", target, PrimerStateAdmitted, ""); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := restarted.Claim("account-secret", target); err != nil || claimed {
		t.Fatalf("restart claim = %v, %v", claimed, err)
	}
}

func TestCodexPrimerStoreUsesPrivateFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenCodexPrimerStore(fsutil.OSFileSystem{}, dir+"/primer.json", dir+"/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Observe("account-secret", testPrimerTarget()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir + "/primer.json", dir + "/primer.key"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
}

func TestCodexPrimerStoreWriteFailureDoesNotAdvanceMemory(t *testing.T) {
	fsys := &failingDurableFS{MemFS: fsutil.NewMemFS()}
	store, err := OpenCodexPrimerStore(fsys, "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	fsys.failWrite = true
	if err := store.Observe("account-secret", testPrimerTarget()); err == nil {
		t.Fatal("Observe returned no write error")
	}
	if len(store.Records()) != 0 {
		t.Fatalf("records advanced after failed write: %+v", store.Records())
	}
	fsys.failWrite = false
	if err := store.Observe("account-secret", testPrimerTarget()); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadFile("/state/primer.json"); err != nil {
		t.Fatalf("retry did not persist journal: %v", err)
	}
}
