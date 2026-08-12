package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerHealthEmitsRoutingDefaultWithCodexHealth(t *testing.T) {
	srv := &Server{
		CodexHealth: func() CodexHealth { return CodexHealth{} },
	}
	w := httptest.NewRecorder()
	srv.handleHealth(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	got, ok := response["codex_routing_default"].(map[string]any)
	if !ok {
		t.Fatalf("codex_routing_default = %#v, want object", response["codex_routing_default"])
	}
	want := map[string]any{
		"configured": false,
		"resolved":   false,
		"routable":   false,
		"status":     "unconfigured",
	}
	if !healthMapsEqual(got, want) {
		t.Fatalf("codex_routing_default = %#v, want %#v", got, want)
	}
}

func TestServerHealthOmitsRoutingDefaultWithoutCodexHealth(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).handleHealth(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got, ok := response["codex_routing_default"]; ok {
		t.Fatalf("codex_routing_default = %#v, want omitted", got)
	}
}

func TestServerHealthNormalisesRoutingDefaultAndDegrades(t *testing.T) {
	unknown := CodexRoutingDefaultHealth{
		Configured: true,
		Status:     "unknown",
	}
	tests := []struct {
		name          string
		input         CodexRoutingDefaultHealth
		want          CodexRoutingDefaultHealth
		wantStatus    string
		forbiddenText string
	}{
		{
			name:       "unconfigured",
			input:      CodexRoutingDefaultHealth{Status: "unconfigured"},
			want:       CodexRoutingDefaultHealth{Status: "unconfigured"},
			wantStatus: "ok",
		},
		{
			name: "resolved",
			input: CodexRoutingDefaultHealth{
				Configured: true,
				Resolved:   true,
				Routable:   true,
				Status:     "resolved",
			},
			want: CodexRoutingDefaultHealth{
				Configured: true,
				Resolved:   true,
				Routable:   true,
				Status:     "resolved",
			},
			wantStatus: "ok",
		},
		{
			name:       "unresolved",
			input:      CodexRoutingDefaultHealth{Configured: true, Status: "unresolved"},
			want:       CodexRoutingDefaultHealth{Configured: true, Status: "unresolved"},
			wantStatus: "degraded",
		},
		{
			name: "unroutable",
			input: CodexRoutingDefaultHealth{
				Configured: true,
				Resolved:   true,
				Status:     "unroutable",
			},
			want: CodexRoutingDefaultHealth{
				Configured: true,
				Resolved:   true,
				Status:     "unroutable",
			},
			wantStatus: "degraded",
		},
		{
			name:       "unknown",
			input:      unknown,
			want:       unknown,
			wantStatus: "degraded",
		},
		{
			name:       "missing status while configured",
			input:      CodexRoutingDefaultHealth{Configured: true},
			want:       unknown,
			wantStatus: "degraded",
		},
		{
			name:       "unconfigured status while configured",
			input:      CodexRoutingDefaultHealth{Configured: true, Status: "unconfigured"},
			want:       unknown,
			wantStatus: "degraded",
		},
		{
			name:       "resolved status without routable",
			input:      CodexRoutingDefaultHealth{Configured: true, Resolved: true, Status: "resolved"},
			want:       unknown,
			wantStatus: "degraded",
		},
		{
			name:       "unresolved status after resolution",
			input:      CodexRoutingDefaultHealth{Configured: true, Resolved: true, Status: "unresolved"},
			want:       unknown,
			wantStatus: "degraded",
		},
		{
			name:       "unroutable status before resolution",
			input:      CodexRoutingDefaultHealth{Configured: true, Status: "unroutable"},
			want:       unknown,
			wantStatus: "degraded",
		},
		{
			name:       "unknown status with resolved claim",
			input:      CodexRoutingDefaultHealth{Configured: true, Resolved: true, Status: "unknown"},
			want:       unknown,
			wantStatus: "degraded",
		},
		{
			name:          "invalid status",
			input:         CodexRoutingDefaultHealth{Configured: true, Status: "secret@example.test/path"},
			want:          unknown,
			wantStatus:    "degraded",
			forbiddenText: "secret@example.test/path",
		},
		{
			name:       "routable without resolved",
			input:      CodexRoutingDefaultHealth{Configured: true, Routable: true, Status: "resolved"},
			want:       unknown,
			wantStatus: "degraded",
		},
		{
			name:       "resolved without configured",
			input:      CodexRoutingDefaultHealth{Resolved: true, Status: "unroutable"},
			want:       unknown,
			wantStatus: "degraded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{
				CodexHealth: func() CodexHealth {
					return CodexHealth{RoutingDefault: tt.input}
				},
			}
			w := httptest.NewRecorder()
			srv.handleHealth(w, httptest.NewRequest(http.MethodGet, "/health", nil))

			var response struct {
				Status         string                    `json:"status"`
				RoutingDefault CodexRoutingDefaultHealth `json:"codex_routing_default"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", response.Status, tt.wantStatus)
			}
			if response.RoutingDefault != tt.want {
				t.Errorf("codex_routing_default = %+v, want %+v", response.RoutingDefault, tt.want)
			}
			if tt.forbiddenText != "" && strings.Contains(w.Body.String(), tt.forbiddenText) {
				t.Errorf("health leaked invalid routing-default status: %s", w.Body.String())
			}
		})
	}
}

func healthMapsEqual(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}
