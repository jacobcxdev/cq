package zai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func resetTestExecutor(fsys fsutil.FileSystem, client ResetClient) ResetExecutor {
	return ResetExecutor{FS: fsys, Client: client, StateDir: "/state/zai-resets", Now: func() time.Time { return resetNow }, RequestID: func() string { return "stable-request" }}
}

func TestResetExecutorNeverDispatchesWithoutConfirmation(t *testing.T) {
	posts := 0
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts++
		}
		return resetResponse(200, resetFixture), nil
	})}
	executor := resetTestExecutor(fsutil.NewMemFS(), client)
	_, err := executor.Use(context.Background(), Account{ID: "account", APIKey: "private-key"}, "5h", "", func(ResetCredit) (bool, error) { return false, nil })
	if err == nil || posts != 0 {
		t.Fatalf("unconfirmed operation err=%v posts=%d", err, posts)
	}
}

func TestResetExecutorAcceptedReceiptPreventsReplay(t *testing.T) {
	posts := 0
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts++
			return resetResponse(200, `{"code":200,"success":true}`), nil
		}
		return resetResponse(200, resetFixture), nil
	})}
	executor := resetTestExecutor(fsutil.NewMemFS(), client)
	account := Account{ID: "account", APIKey: "private-key"}
	confirm := func(ResetCredit) (bool, error) { return true, nil }
	result, err := executor.Use(context.Background(), account, "5h", "5h:earlier", confirm)
	if err != nil || !result.Accepted || result.AlreadyAccepted || !result.Dispatched || posts != 1 {
		t.Fatalf("first result=%+v err=%v posts=%d", result, err, posts)
	}
	result, err = executor.Use(context.Background(), account, "5h", "5h:earlier", confirm)
	if err != nil || !result.Accepted || !result.AlreadyAccepted || result.Dispatched || posts != 1 {
		t.Fatalf("repeated result=%+v err=%v posts=%d", result, err, posts)
	}
}

func TestResetExecutorAmbiguousRetryRetainsExactCardAndRequestID(t *testing.T) {
	fsys := fsutil.NewMemFS()
	var ids, cards []string
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return resetResponse(200, resetFixture), nil
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		ids, cards = append(ids, string(body["requestId"])), append(cards, string(body["recordId"]))
		if len(ids) == 1 {
			return nil, errors.New("private-key transport error")
		}
		return resetResponse(200, `{"code":200,"success":true}`), nil
	})}
	executor := resetTestExecutor(fsys, client)
	generated := 0
	executor.RequestID = func() string { generated++; return "stable-request" }
	account := Account{ID: "account", APIKey: "private-key"}
	confirm := func(ResetCredit) (bool, error) { return true, nil }
	result, err := executor.Use(context.Background(), account, "5h", "", confirm)
	if err == nil || !result.Dispatched || strings.Contains(err.Error(), "private-key") {
		t.Fatal("ambiguous operation accepted or leaked")
	}
	_, err = executor.Use(context.Background(), account, "5h", "5h:900719925474099313", confirm)
	if err == nil || len(ids) != 1 {
		t.Fatal("ambiguous attempt selected another card")
	}
	// Simulate a new process using the same journal and a different random-ID source.
	executor = resetTestExecutor(fsys, client)
	executor.RequestID = func() string { generated++; return "must-not-use-new-request" }
	if _, err = executor.Use(context.Background(), account, "5h", "", confirm); err != nil {
		t.Fatal(err)
	}
	if generated != 1 || len(ids) != 2 || ids[0] != ids[1] || cards[0] != cards[1] {
		t.Fatalf("retry identity changed: generated=%d ids=%v cards=%v", generated, ids, cards)
	}
}

func TestResetExecutorRechecksAvailabilityAfterConfirmation(t *testing.T) {
	gets := 0
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			t.Fatal("stale card consumed")
		}
		gets++
		body := resetFixture
		if gets > 1 {
			body = strings.ReplaceAll(body, `"available":true`, `"available":false`)
		}
		return resetResponse(200, body), nil
	})}
	executor := resetTestExecutor(fsutil.NewMemFS(), client)
	_, err := executor.Use(context.Background(), Account{ID: "account", APIKey: "private-key"}, "5h", "", func(ResetCredit) (bool, error) { return true, nil })
	if err == nil {
		t.Fatal("unavailable card accepted")
	}
}

func TestResetExecutorCancellationBeforeDispatchDoesNotPost(t *testing.T) {
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			t.Fatal("cancelled reset posted")
		}
		return resetResponse(200, resetFixture), nil
	})}
	executor := resetTestExecutor(fsutil.NewMemFS(), client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := executor.Use(ctx, Account{ID: "account", APIKey: "private-key"}, "5h", "", func(ResetCredit) (bool, error) {
		cancel()
		return true, nil
	})
	if err == nil {
		t.Fatal("cancelled reset accepted")
	}
}

func TestResetExecutorRequiresExclusiveAccountLock(t *testing.T) {
	fsys := fsutil.NewMemFS()
	digest := sha256.Sum256([]byte("account"))
	path := filepath.Join("/state/zai-resets", hex.EncodeToString(digest[:])+".json.lock")
	if err := fsutil.EnsureSecureDirectory(fsys, "/state/zai-resets"); err != nil {
		t.Fatal(err)
	}
	lock, err := fsutil.AcquireExclusiveLock(fsys, path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	client := ResetClient{HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("busy reset account reached upstream")
		return nil, errors.New("unexpected")
	})}
	executor := resetTestExecutor(fsys, client)
	result, err := executor.Use(context.Background(), Account{ID: "account", APIKey: "private-key"}, "5h", "", func(ResetCredit) (bool, error) { return true, nil })
	if err == nil || result.Dispatched {
		t.Fatal("account lock ignored")
	}
}

type resetWriteFailureFS struct{ *fsutil.MemFS }
type resetWriteFailureDirectory struct {
	fsutil.SecureDirectory
	fsutil.IdentityBoundRenamer
	fsutil.IdentityBoundRemover
}

func (f resetWriteFailureFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	dir, err := f.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return resetWriteFailureDirectory{SecureDirectory: dir, IdentityBoundRenamer: dir.(fsutil.IdentityBoundRenamer), IdentityBoundRemover: dir.(fsutil.IdentityBoundRemover)}, nil
}

func (resetWriteFailureDirectory) CreateExclusive(string, os.FileMode) (fsutil.DurableFile, error) {
	return nil, os.ErrPermission
}

func TestResetExecutorNeverPostsBeforeDurableIntent(t *testing.T) {
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			t.Fatal("reset sent before durable intent")
		}
		return resetResponse(200, resetFixture), nil
	})}
	executor := resetTestExecutor(resetWriteFailureFS{fsutil.NewMemFS()}, client)
	result, err := executor.Use(context.Background(), Account{ID: "account", APIKey: "private-key"}, "5h", "", func(ResetCredit) (bool, error) { return true, nil })
	if err == nil || result.Dispatched || !strings.Contains(err.Error(), "save Z.ai reset attempt") {
		t.Fatalf("expected intent write error, got %v", err)
	}
}

func TestResetExecutorInventoryFailureIsPreDispatch(t *testing.T) {
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("failed inventory reached mutation boundary")
		}
		return resetResponse(500, "private-key"), nil
	})}
	executor := resetTestExecutor(fsutil.NewMemFS(), client)
	result, err := executor.Use(context.Background(), Account{ID: "account", APIKey: "private-key"}, "5h", "", func(ResetCredit) (bool, error) { return true, nil })
	if err == nil || result.Dispatched {
		t.Fatalf("GET failure lost pre-dispatch state: result=%+v err=%v", result, err)
	}
}

type resetReceiptFailureFS struct {
	*fsutil.MemFS
	writes *int
}

type resetReceiptFailureDirectory struct {
	fsutil.SecureDirectory
	fsutil.IdentityBoundRenamer
	fsutil.IdentityBoundRemover
	writes *int
}

func (f resetReceiptFailureFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	dir, err := f.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return resetReceiptFailureDirectory{SecureDirectory: dir, IdentityBoundRenamer: dir.(fsutil.IdentityBoundRenamer), IdentityBoundRemover: dir.(fsutil.IdentityBoundRemover), writes: f.writes}, nil
}

func (d resetReceiptFailureDirectory) CreateExclusive(name string, mode os.FileMode) (fsutil.DurableFile, error) {
	*d.writes++
	if *d.writes == 2 {
		return nil, os.ErrPermission
	}
	return d.SecureDirectory.CreateExclusive(name, mode)
}

func TestResetExecutorReceiptFailureRemainsDispatched(t *testing.T) {
	writes, posts := 0, 0
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts++
			return resetResponse(200, `{"code":200,"success":true}`), nil
		}
		return resetResponse(200, resetFixture), nil
	})}
	executor := resetTestExecutor(resetReceiptFailureFS{MemFS: fsutil.NewMemFS(), writes: &writes}, client)
	result, err := executor.Use(context.Background(), Account{ID: "account", APIKey: "private-key"}, "5h", "", func(ResetCredit) (bool, error) { return true, nil })
	if err == nil || !result.Dispatched || posts != 1 || !strings.Contains(err.Error(), "local receipt") {
		t.Fatalf("accepted mutation lost dispatch state: result=%+v posts=%d writes=%d err=%v", result, posts, writes, err)
	}
}

func TestResetExecutorScheduledRunNeverReplaysPendingManualAttempt(t *testing.T) {
	posts := 0
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return resetResponse(200, resetFixture), nil
		}
		posts++
		return resetResponse(408, `{"error":"request timed out"}`), nil
	})}
	executor := resetTestExecutor(fsutil.NewMemFS(), client)
	account := Account{ID: "account", APIKey: "private-key"}
	result, err := executor.Use(context.Background(), account, "5h", "5h:earlier", func(ResetCredit) (bool, error) { return true, nil })
	var resetErr *ResetError
	if !errors.As(err, &resetErr) || !resetErr.Ambiguous || !result.Dispatched || posts != 1 {
		t.Fatalf("408 did not retain ambiguous intent: result=%+v posts=%d err=%v", result, posts, err)
	}
	result, err = executor.UseScheduled(context.Background(), account, "5h", "5h:earlier")
	if !errors.Is(err, ErrResetPending) || !result.Dispatched || posts != 1 {
		t.Fatalf("schedule replayed pending attempt: result=%+v posts=%d err=%v", result, posts, err)
	}
}

func TestResetExecutorPendingAttemptRetainsDispatchStateOnInventoryFailure(t *testing.T) {
	posts := 0
	failGet := false
	client := ResetClient{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			if failGet {
				return resetResponse(500, "private-key"), nil
			}
			return resetResponse(200, resetFixture), nil
		}
		posts++
		return resetResponse(408, `{"error":"request timed out"}`), nil
	})}
	executor := resetTestExecutor(fsutil.NewMemFS(), client)
	account := Account{ID: "account", APIKey: "private-key"}
	confirm := func(ResetCredit) (bool, error) { return true, nil }
	if result, err := executor.Use(context.Background(), account, "5h", "5h:earlier", confirm); err == nil || !result.Dispatched {
		t.Fatal("first ambiguous dispatch lost")
	}
	failGet = true
	result, err := executor.Use(context.Background(), account, "5h", "5h:earlier", confirm)
	if err == nil || !result.Dispatched || posts != 1 {
		t.Fatalf("inventory failure downgraded pending dispatch: result=%+v posts=%d err=%v", result, posts, err)
	}
}
