package dots

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProbeComputerRemovalUsesListedOwner(t *testing.T) {
	p, _, owner := probeFixtureForTest(t)
	var mu sync.Mutex
	deletes := 0
	do := probeDoer(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			if r.URL.Path == "/backend-api/tbo/profile-owner/computers" && r.Header.Get("ChatGPT-Account-Id") == owner.AccountID {
				return probeSuccess(`{"computers":[{"kind":"user","environment_id":"ccarenv_b64_research","can_delete":true}]}`), nil
			}
			return probeSuccess(`{"computers":[]}`), nil
		}
		mu.Lock()
		deletes++
		mu.Unlock()
		if r.Method != "DELETE" || r.URL.Path != "/backend-api/computers/ccarenv_b64_research" || r.Header.Get("Authorization") != "Bearer owner-token" || r.Header.Get("ChatGPT-Account-Id") != owner.AccountID {
			t.Error("removal used the wrong account or endpoint")
		}
		for _, key := range []string{"Cookie", "X-OpenAI-Account-Routing-Override", "X-OpenAI-Expected-Account-Id", "X-OpenAI-Orbit-Selection"} {
			if r.Header.Get(key) != "" {
				t.Errorf("foreign %s leaked", key)
			}
		}
		return probeSuccess(`{"removed":true}`), nil
	})
	p.options.HTTP, p.options.Catalogue.HTTP = do, do
	r := probeRequest("DELETE", "/backend-api/computers/ccarenv_b64_research", "")
	r.Header.Set("Cookie", "foreign")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"removed":true`) || deletes != 1 {
		t.Fatalf("code=%d deletes=%d", w.Code, deletes)
	}
}

func TestProbeComputerRemovalRejectsUnverifiedTarget(t *testing.T) {
	for _, scenario := range []string{"absent", "not-deletable", "cloud", "duplicate", "owner-collision", "unavailable", "malformed", "stale-owner", "unauthenticated", "body", "query", "encoded-path", "missing-fixture"} {
		t.Run(scenario, func(t *testing.T) {
			p, _, _ := probeFixtureForTest(t)
			var mu sync.Mutex
			deletes := 0
			do := probeDoer(func(r *http.Request) (*http.Response, error) {
				if r.Method == "DELETE" {
					mu.Lock()
					deletes++
					mu.Unlock()
					return probeSuccess(`{}`), nil
				}
				if scenario == "unavailable" {
					return nil, fmt.Errorf("offline")
				}
				if scenario == "malformed" {
					return probeSuccess(`{"computers":{}}`), nil
				}
				row := `{"kind":"user","environment_id":"ccarenv_b64_research","can_delete":true}`
				if scenario == "not-deletable" {
					row = strings.ReplaceAll(row, "true", "false")
				}
				if scenario == "cloud" {
					row = strings.ReplaceAll(row, `"user"`, `"openai_cloud"`)
				}
				if scenario == "stale-owner" {
					p.mu.Lock()
					p.fixture = nil
					p.mu.Unlock()
				}
				if scenario == "absent" || r.URL.Path != "/backend-api/tbo/profile-owner/computers" && scenario != "owner-collision" {
					return probeSuccess(`{"computers":[]}`), nil
				}
				if scenario == "duplicate" {
					row += "," + row
				}
				return probeSuccess(`{"computers":[` + row + `]}`), nil
			})
			p.options.HTTP, p.options.Catalogue.HTTP = do, do
			path, body := "/backend-api/computers/ccarenv_b64_research", ""
			if scenario == "body" {
				body = `{}`
			}
			if scenario == "query" {
				path += "?owner=other"
			}
			if scenario == "encoded-path" {
				path = "/backend-api/computers/%2Fforeign"
			}
			if scenario == "missing-fixture" {
				p.fixture = nil
			}
			r := probeRequest("DELETE", path, body)
			if scenario == "unauthenticated" {
				r.Header.Set("Authorization", "Bearer foreign")
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code < 400 || deletes != 0 {
				t.Fatalf("code=%d deletes=%d", w.Code, deletes)
			}
		})
	}
}

func TestProbeComputerRemovalNeverRetriesWrite(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	var mu sync.Mutex
	deletes := 0
	do := probeDoer(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			if strings.Contains(r.URL.Path, "profile-owner") {
				return probeSuccess(`{"computers":[{"kind":"user","environment_id":"research","can_delete":true}]}`), nil
			}
			return probeSuccess(`{"computers":[]}`), nil
		}
		mu.Lock()
		deletes++
		mu.Unlock()
		return nil, context.DeadlineExceeded
	})
	p.options.HTTP, p.options.Catalogue.HTTP = do, do
	w := httptest.NewRecorder()
	p.ServeHTTP(w, probeRequest("DELETE", "/backend-api/computers/research", ""))
	if w.Code != 502 || deletes != 1 {
		t.Fatalf("code=%d deletes=%d", w.Code, deletes)
	}
}

func TestProbeComputerRemovalKeepsPrimaryAndRefreshesOwnerCredentials(t *testing.T) {
	for _, primary := range []bool{false, true} {
		t.Run(fmt.Sprint(primary), func(t *testing.T) {
			p, system, owner := probeFixtureForTest(t)
			selected, profile := owner, "profile-owner"
			if primary {
				selected, profile = system, "profile-system"
			} else {
				selected.Token = "fresh-owner-token"
				for key, account := range p.fixture.Owners {
					if account.Alias == selected.Alias {
						p.fixture.Owners[key] = selected
					}
				}
			}
			do := probeDoer(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" {
					if r.URL.Path == "/backend-api/tbo/"+profile+"/computers" {
						return probeSuccess(`{"computers":[{"kind":"user","environment_id":"research","can_delete":true}]}`), nil
					}
					return probeSuccess(`{"computers":[]}`), nil
				}
				if r.Header.Get("Authorization") != "Bearer "+selected.Token || r.Header.Get("ChatGPT-Account-Id") != selected.AccountID {
					t.Error("wrong current owner credential")
				}
				if primary && r.Header.Get("Cookie") != "native-cookie" {
					t.Error("primary authority changed")
				}
				return &http.Response{StatusCode: 204, Header: http.Header{}, Body: http.NoBody}, nil
			})
			p.options.HTTP, p.options.Catalogue.HTTP = do, do
			r := probeRequest("DELETE", "/backend-api/computers/research", "")
			r.Header.Set("Cookie", "native-cookie")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != 204 || w.Body.Len() != 0 {
				t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
			}
		})
	}
}

func TestProbeComputerRemovalChecksMoreThanWorkerCapacity(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	for index := 2; index < 10; index++ {
		alias, profile, root := fmt.Sprintf("%08x", index), fmt.Sprintf("profile-%d", index), fmt.Sprintf("root-%d", index)
		owner := Account{Alias: alias, AccountID: fmt.Sprintf("account-%d", index), Token: fmt.Sprintf("token-%d", index)}
		p.settings.Dots[profile] = Dot{OwnerAlias: alias, ProfileID: profile, RootThreadID: root}
		p.fixture.Profiles = append(p.fixture.Profiles, map[string]any{"id": profile, "active_root_thread_id": root})
		p.fixture.Owners[profile], p.fixture.Owners[root] = owner, owner
	}
	var mu sync.Mutex
	reads, deletes := 0, 0
	firstBatch := make(chan struct{})
	do := probeDoer(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			mu.Lock()
			reads++
			if reads == cap(p.workers) {
				close(firstBatch)
			}
			mu.Unlock()
			select {
			case <-firstBatch:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			if r.URL.Path == "/backend-api/tbo/profile-9/computers" {
				return probeSuccess(`{"computers":[{"kind":"user","environment_id":"research","can_delete":true}]}`), nil
			}
			return probeSuccess(`{"computers":[]}`), nil
		}
		mu.Lock()
		deletes++
		mu.Unlock()
		if r.Header.Get("ChatGPT-Account-Id") != "account-9" {
			t.Error("wrong owner beyond first batch")
		}
		return probeSuccess(`{}`), nil
	})
	p.options.HTTP, p.options.Catalogue.HTTP = do, do
	w := httptest.NewRecorder()
	p.ServeHTTP(w, probeRequest("DELETE", "/backend-api/computers/research", ""))
	if w.Code != 200 || reads != 10 || deletes != 1 {
		t.Fatalf("code=%d reads=%d deletes=%d", w.Code, reads, deletes)
	}
}

func TestProbeComputerRemovalWaitsForOccupiedWorkers(t *testing.T) {
	p, _, owner := probeFixtureForTest(t)
	p.workers = make(chan struct{}, 1)
	p.workers <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	finished := make(chan map[string]probeOwnerResult, 1)
	go func() {
		defer func() { _ = recover() }()
		finished <- p.collectOwnerResults(ctx, []Account{owner}, func(context.Context, Account) (any, error) { return 1, nil }, true)
	}()
	select {
	case <-finished:
		t.Fatal("occupied worker was rejected instead of queued")
	case <-time.After(20 * time.Millisecond):
	}
	<-p.workers
	select {
	case results := <-finished:
		if results[owner.Alias].err != nil || results[owner.Alias].value != 1 {
			t.Fatal("queued lookup did not run")
		}
	case <-ctx.Done():
		t.Fatal("queued lookup did not finish")
	}
	// A cancelled waiter never invokes its lookup or extends the shared budget.
	select {
	case p.workers <- struct{}{}:
	case <-ctx.Done():
		t.Fatal("worker capacity was not released")
	}
	cancel()
	results := p.collectOwnerResults(ctx, []Account{owner}, func(context.Context, Account) (any, error) { t.Error("cancelled lookup ran"); return nil, nil }, true)
	if results[owner.Alias].err == nil {
		t.Fatal("cancelled lookup accepted")
	}
	<-p.workers
}
