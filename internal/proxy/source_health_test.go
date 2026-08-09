package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestServerHealthUsesCoordinatorCodexStatus(t *testing.T) {
	srv := &Server{
		CodexDiscover: func() []codex.CodexAccount {
			return make([]codex.CodexAccount, 99)
		},
		CodexHealth: func() CodexHealth {
			return CodexHealth{
				AccountCount: 2,
				ExternalSources: []CodexSourceHealth{{
					Name: "codexbar", CandidateCount: 1, HealthCode: "ok",
				}},
			}
		},
	}

	w := httptest.NewRecorder()
	srv.handleHealth(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var response struct {
		Accounts struct {
			Codex int `json:"codex"`
		} `json:"accounts"`
		ExternalSources []CodexSourceHealth `json:"codex_external_sources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Accounts.Codex != 2 {
		t.Fatalf("Codex account count = %d, want coordinator count 2", response.Accounts.Codex)
	}
	if len(response.ExternalSources) != 1 || response.ExternalSources[0] != (CodexSourceHealth{Name: "codexbar", CandidateCount: 1, HealthCode: "ok"}) {
		t.Fatalf("external source health = %+v", response.ExternalSources)
	}
	for _, forbidden := range []string{"private@example.test", "provider-account-secret", "manifest-record-secret", "credential-revision-secret", "token-secret", "/private/managed-home"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("health JSON exposed forbidden fixture %q: %s", forbidden, w.Body.String())
		}
	}
}
