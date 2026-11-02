package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
)

type zaiPrimerTestHTTP struct {
	mu       sync.Mutex
	usage    map[string][]string
	gets     map[string]int
	posts    map[string]int
	models   []string
	postCode int
	postBody string
	postErr  error
}

func (f *zaiPrimerTestHTTP) Do(request *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := request.Header.Get("Authorization")
	if request.Method == http.MethodGet {
		queue := f.usage[key]
		index := f.gets[key]
		f.gets[key]++
		if len(queue) == 0 {
			return nil, errors.New("private-key")
		}
		if index >= len(queue) {
			index = len(queue) - 1
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(queue[index]))}, nil
	}
	key = strings.TrimPrefix(key, "Bearer ")
	f.posts[key]++
	var body struct {
		Model  string `json:"model"`
		Store  bool   `json:"store"`
		Stream bool   `json:"stream"`
		Limit  int    `json:"max_output_tokens"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		return nil, err
	}
	if request.URL.String() != zaiprov.ResponsesBaseURL+"/responses" || body.Store || body.Stream || body.Limit != 16 {
		return nil, errors.New("invalid synthetic request")
	}
	f.models = append(f.models, body.Model)
	if f.postErr != nil {
		return nil, f.postErr
	}
	code, response := f.postCode, f.postBody
	if code == 0 {
		code = 200
	}
	if response == "" {
		response = `{"id":"response","status":"completed"}`
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(response))}, nil
}

func newZAIPrimerTestHTTP() *zaiPrimerTestHTTP {
	return &zaiPrimerTestHTTP{usage: make(map[string][]string), gets: make(map[string]int), posts: make(map[string]int)}
}

func zaiPrimerTestQuota(five, weekly float64, reset int64) string {
	return fmt.Sprintf(`{"code":200,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"percentage":%g,"nextResetTime":%d},{"type":"CREDIT_LIMIT","unit":6,"percentage":%g}]}}`, 100-five, reset*1000, 100-weekly)
}

func zaiPrimerTestModels() []modelregistry.Entry {
	return []modelregistry.Entry{
		{Provider: modelregistry.ProviderZAI, Source: modelregistry.SourceNative, ID: "glm-future", Priority: 0},
		{Provider: modelregistry.ProviderZAI, Source: modelregistry.SourceNative, ID: "glm-future-flash", Priority: 1},
	}
}

func newZAIPrimerTest(t *testing.T, fsys fsutil.FileSystem, client httputil.Doer, accounts ...zaiprov.Account) *ZAIPrimer {
	t.Helper()
	primer, err := NewZAIPrimer(fsys, "/state", client, func() ([]zaiprov.Account, error) { return accounts, nil }, zaiPrimerTestModels)
	if err != nil {
		t.Fatal(err)
	}
	return primer
}

var zaiPrimerAccountA = zaiprov.Account{ID: "zai_1111111111111111", APIKey: "first-private-key"}
var zaiPrimerAccountB = zaiprov.Account{ID: "zai_2222222222222222", APIKey: "second-private-key"}

func TestZAIPrimerPrimesDormantAccountAndVerifiesWindow(t *testing.T) {
	now := time.Unix(10000, 0)
	reset := now.Add(zaiPrimerWindow).Unix()
	client := newZAIPrimerTestHTTP()
	client.usage[zaiPrimerAccountA.APIKey] = []string{zaiPrimerTestQuota(100, 100, 0), zaiPrimerTestQuota(100, 100, reset)}
	fsys := fsutil.NewMemFS()
	primer := newZAIPrimerTest(t, fsys, client, zaiPrimerAccountA)
	if _, err := primer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	record := primer.Health(true).Accounts[zaiPrimerAccountA.ID]
	if client.posts[zaiPrimerAccountA.APIKey] != 1 || len(client.models) != 1 || client.models[0] != "glm-future-flash" || record.State != "verified" || record.ResetAtUnix != reset {
		t.Fatalf("primer did not verify exact account/model: record=%+v", record)
	}
	data, err := fsys.ReadFile("/state/zai-window-primer.json")
	if err != nil || strings.Contains(string(data), "private-key") {
		t.Fatal("missing journal or credential persisted")
	}
	info, _ := fsys.Stat("/state/zai-window-primer.json")
	if info.Mode().Perm() != 0o600 {
		t.Fatal("journal is not private")
	}
	if _, err := primer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if client.posts[zaiPrimerAccountA.APIKey] != 1 {
		t.Fatal("active window primed again")
	}
}

func TestZAIPrimerSkipsActiveExhaustedUnknownAndPartiallyUsedQuota(t *testing.T) {
	now := time.Unix(10000, 0)
	for name, body := range map[string]string{
		"active":           zaiPrimerTestQuota(100, 100, now.Add(time.Minute).Unix()),
		"exhausted 5h":     zaiPrimerTestQuota(0, 100, 0),
		"exhausted weekly": zaiPrimerTestQuota(100, 0, 0),
		"partial use":      zaiPrimerTestQuota(99.99, 100, 0),
		"unknown weekly":   `{"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"percentage":0}]}}`,
		"invalid quota":    `{"error":"private-key"}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := newZAIPrimerTestHTTP()
			client.usage[zaiPrimerAccountA.APIKey] = []string{body}
			primer := newZAIPrimerTest(t, fsutil.NewMemFS(), client, zaiPrimerAccountA)
			_, err := primer.RunOnce(context.Background(), now)
			if client.posts[zaiPrimerAccountA.APIKey] != 0 {
				t.Fatal("non-dormant or unknown window consumed quota")
			}
			if err != nil && strings.Contains(err.Error(), "private-key") {
				t.Fatal("upstream payload exposed")
			}
		})
	}
}

func TestZAIPrimerSchedulesUpcomingReset(t *testing.T) {
	now := time.Unix(10000, 0)
	reset := now.Add(17 * time.Second)
	client := newZAIPrimerTestHTTP()
	client.usage[zaiPrimerAccountA.APIKey] = []string{zaiPrimerTestQuota(10, 100, reset.Unix())}
	primer := newZAIPrimerTest(t, fsutil.NewMemFS(), client, zaiPrimerAccountA)
	next, err := primer.RunOnce(context.Background(), now)
	if err != nil || !next.Equal(reset) || client.posts[zaiPrimerAccountA.APIKey] != 0 {
		t.Fatalf("next=%v error=%v", next, err)
	}
}

func TestZAIPrimerAmbiguousDispatchNotRepeatedAfterRestart(t *testing.T) {
	now := time.Unix(10000, 0)
	for name, setup := range map[string]func(*zaiPrimerTestHTTP){
		"transport":       func(client *zaiPrimerTestHTTP) { client.postErr = errors.New("first-private-key") },
		"server":          func(client *zaiPrimerTestHTTP) { client.postCode = 503 },
		"request timeout": func(client *zaiPrimerTestHTTP) { client.postCode = 408 },
		"bad body":        func(client *zaiPrimerTestHTTP) { client.postBody = "first-private-key" },
		"verification":    func(client *zaiPrimerTestHTTP) {},
	} {
		t.Run(name, func(t *testing.T) {
			client := newZAIPrimerTestHTTP()
			client.usage[zaiPrimerAccountA.APIKey] = []string{zaiPrimerTestQuota(100, 100, 0)}
			setup(client)
			fsys := fsutil.NewMemFS()
			primer := newZAIPrimerTest(t, fsys, client, zaiPrimerAccountA)
			if _, err := primer.RunOnce(context.Background(), now); err == nil || strings.Contains(err.Error(), "private-key") {
				t.Fatal("ambiguous dispatch not safely reported")
			}
			for _, elapsed := range []time.Duration{time.Minute, time.Hour, 4*time.Hour + 59*time.Minute} {
				primer = newZAIPrimerTest(t, fsys, client, zaiPrimerAccountA)
				_, _ = primer.RunOnce(context.Background(), now.Add(elapsed))
			}
			if client.posts[zaiPrimerAccountA.APIKey] != 1 {
				t.Fatal("ambiguous request replayed within five-hour window")
			}
		})
	}
}

func TestZAIPrimerRejectedDispatchUsesBoundedBackoff(t *testing.T) {
	now := time.Unix(10000, 0)
	client := newZAIPrimerTestHTTP()
	client.usage[zaiPrimerAccountA.APIKey] = []string{zaiPrimerTestQuota(100, 100, 0)}
	client.postCode = 429
	primer := newZAIPrimerTest(t, fsutil.NewMemFS(), client, zaiPrimerAccountA)
	_, _ = primer.RunOnce(context.Background(), now)
	_, _ = primer.RunOnce(context.Background(), now.Add(time.Second))
	if client.posts[zaiPrimerAccountA.APIKey] != 1 {
		t.Fatal("rejection retried without backoff")
	}
	_, _ = primer.RunOnce(context.Background(), now.Add(zaiPrimerPoll))
	if client.posts[zaiPrimerAccountA.APIKey] != 2 {
		t.Fatal("explicit rejection never eligible after backoff")
	}
}

func TestZAIPrimerRejectionDiagnosticsRemainSafeAndActionable(t *testing.T) {
	for _, test := range []struct {
		name, body, want string
		status           int
	}{
		{"coding key mismatch", `{"error":{"code":"insufficient_quota","message":"first-private-key"}}`, "Z.ai inference package unavailable; check that the API key comes from Personal Coding Plan > Plan Overview", 429},
		{"numeric package error", `{"error":{"code":1113,"message":"first-private-key"}}`, "Z.ai inference package unavailable; check that the API key comes from Personal Coding Plan > Plan Overview", 429},
		{"five hour quota", `{"error":{"code":"1308","message":"first-private-key"}}`, "Z.ai Coding Plan quota exhausted", 429},
		{"weekly quota", `{"error":{"code":"1310","message":"first-private-key"}}`, "Z.ai Coding Plan quota exhausted", 429},
		{"extra usage quota", `{"error":{"code":"1317","message":"first-private-key"}}`, "Z.ai Coding Plan quota exhausted", 429},
		{"unknown code", `{"error":{"code":"first-private-key","message":"first-private-key"}}`, "Z.ai primer request rejected (HTTP 429)", 429},
		{"malformed JSON", `{"error":"first-private-key"`, "Z.ai primer request rejected (HTTP 400)", 400},
		{"oversized error", "first-private-key" + strings.Repeat("x", httputil.MaxResponseBody), "Z.ai primer request rejected (HTTP 429)", 429},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newZAIPrimerTestHTTP()
			client.postCode, client.postBody = test.status, test.body
			client.usage[zaiPrimerAccountA.APIKey] = []string{zaiPrimerTestQuota(100, 100, 0)}
			primer := newZAIPrimerTest(t, fsutil.NewMemFS(), client, zaiPrimerAccountA)
			_, err := primer.RunOnce(context.Background(), time.Unix(10000, 0))
			if err == nil || err.Error() != test.want || primer.Health(true).LastError != test.want || strings.Contains(err.Error(), "private-key") {
				t.Fatalf("unexpected rejection diagnostic: %v", err)
			}
		})
	}
}

type zaiPrimerWriteFailureFS struct{ *fsutil.MemFS }

func (fsys zaiPrimerWriteFailureFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return zaiPrimerWriteFailureDirectory{directory}, nil
}

type zaiPrimerWriteFailureDirectory struct{ fsutil.SecureDirectory }

func (zaiPrimerWriteFailureDirectory) CreateExclusive(string, os.FileMode) (fsutil.DurableFile, error) {
	return nil, errors.New("write denied")
}

func TestZAIPrimerIntentWriteFailurePreventsDispatch(t *testing.T) {
	now := time.Unix(10000, 0)
	client := newZAIPrimerTestHTTP()
	client.usage[zaiPrimerAccountA.APIKey] = []string{zaiPrimerTestQuota(100, 100, 0)}
	primer := newZAIPrimerTest(t, zaiPrimerWriteFailureFS{fsutil.NewMemFS()}, client, zaiPrimerAccountA)
	if _, err := primer.RunOnce(context.Background(), now); err == nil || err.Error() != "save Z.ai primer journal" || client.posts[zaiPrimerAccountA.APIKey] != 0 {
		t.Fatal("request sent without persisted intent")
	}
}

func TestZAIPrimerAccountFailureDoesNotSuppressOtherAccount(t *testing.T) {
	now := time.Unix(10000, 0)
	client := newZAIPrimerTestHTTP()
	client.usage[zaiPrimerAccountB.APIKey] = []string{zaiPrimerTestQuota(100, 100, 0), zaiPrimerTestQuota(100, 100, now.Add(zaiPrimerWindow).Unix())}
	primer := newZAIPrimerTest(t, fsutil.NewMemFS(), client, zaiPrimerAccountA, zaiPrimerAccountB)
	_, err := primer.RunOnce(context.Background(), now)
	if err == nil || client.posts[zaiPrimerAccountA.APIKey] != 0 || client.posts[zaiPrimerAccountB.APIKey] != 1 {
		t.Fatal("failed account suppressed healthy account")
	}
}

func TestZAIPrimerUnsupportedCatalogueNeverDispatches(t *testing.T) {
	client := newZAIPrimerTestHTTP()
	primer := newZAIPrimerTest(t, fsutil.NewMemFS(), client, zaiPrimerAccountA)
	primer.models = func() []modelregistry.Entry {
		return []modelregistry.Entry{
			{Provider: modelregistry.ProviderCodex, Source: modelregistry.SourceNative, ID: "native-other"},
			{Provider: modelregistry.ProviderZAI, Source: modelregistry.SourceOverlay, ID: "guessed-overlay"},
			{Provider: modelregistry.ProviderZAI, Source: modelregistry.SourceNative, ID: "not-supported", Raw: json.RawMessage(`{"supported_in_api":false}`)},
		}
	}
	if _, err := primer.RunOnce(context.Background(), time.Now()); err == nil || len(client.posts) != 0 {
		t.Fatal("unsupported model used for synthetic request")
	}
}

func TestZAIPrimerCancelledRunDoesNotReadCredentials(t *testing.T) {
	primer := newZAIPrimerTest(t, fsutil.NewMemFS(), newZAIPrimerTestHTTP())
	primer.accounts = func() ([]zaiprov.Account, error) {
		t.Fatal("cancelled run read credentials")
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := primer.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error=%v", err)
	}
}

func TestZAIPrimerRefusesMalformedJournal(t *testing.T) {
	for _, body := range []string{`{`, `{"schema_version":2,"records":{}}`, `{"schema_version":1,"records":{"private-key":{"state":"claimed"}}}`} {
		fsys := fsutil.NewMemFS()
		_ = fsys.WriteFile("/state/zai-window-primer.json", []byte(body), 0o600)
		_, err := NewZAIPrimer(fsys, "/state", newZAIPrimerTestHTTP(), func() ([]zaiprov.Account, error) { return nil, nil }, zaiPrimerTestModels)
		if err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatal("invalid journal accepted or leaked")
		}
	}
}

func TestZAIPrimerReloadsJournalAcrossOverlappingWorkers(t *testing.T) {
	now := time.Unix(10000, 0)
	client := newZAIPrimerTestHTTP()
	client.usage[zaiPrimerAccountA.APIKey] = []string{zaiPrimerTestQuota(100, 100, 0)}
	fsys := fsutil.NewMemFS()
	first := newZAIPrimerTest(t, fsys, client, zaiPrimerAccountA)
	second := newZAIPrimerTest(t, fsys, client, zaiPrimerAccountA)
	_, _ = first.RunOnce(context.Background(), now)
	_, _ = second.RunOnce(context.Background(), now.Add(time.Minute))
	if client.posts[zaiPrimerAccountA.APIKey] != 1 {
		t.Fatal("overlapping worker used stale journal and repeated request")
	}
}

func TestZAIPrimerBusyLockDefersWithoutAccountOrNetworkAccess(t *testing.T) {
	fsys := fsutil.NewMemFS()
	client := newZAIPrimerTestHTTP()
	primer := newZAIPrimerTest(t, fsys, client, zaiPrimerAccountA)
	if err := fsutil.EnsureSecureDirectory(fsys, "/state"); err != nil {
		t.Fatal(err)
	}
	lock, err := fsutil.AcquireExclusiveLock(fsys, "/state/zai-window-primer.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	primer.accounts = func() ([]zaiprov.Account, error) {
		t.Fatal("busy worker read credentials")
		return nil, nil
	}
	if _, err := primer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatalf("busy worker should defer: %v", err)
	}
}

func TestNewZAIPrimerDoesNotCreateState(t *testing.T) {
	fsys := fsutil.NewMemFS()
	_ = newZAIPrimerTest(t, fsys, newZAIPrimerTestHTTP())
	if _, err := fsys.Stat("/state"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("constructor created state before running")
	}
}
