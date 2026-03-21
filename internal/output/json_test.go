package output

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/provider"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestJSONRendererCompact(t *testing.T) {
	var buf bytes.Buffer
	r := &JSONRenderer{W: &buf}
	report := app.Report{
		GeneratedAt: time.Unix(1000, 0),
		Providers: []app.ProviderReport{
			{ID: provider.Codex, Name: "codex", Results: []quota.Result{
				{Status: quota.StatusOK},
			}},
		},
	}
	if err := r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render error: %v", err)
	}
	// Compact JSON should be a single line
	if strings.Count(buf.String(), "\n") > 1 {
		t.Fatal("compact JSON should not have extra newlines")
	}
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
}

func TestJSONRendererPretty(t *testing.T) {
	var buf bytes.Buffer
	r := &JSONRenderer{W: &buf, Pretty: true}
	report := app.Report{
		GeneratedAt: time.Unix(1000, 0),
		Providers:   []app.ProviderReport{},
	}
	if err := r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if !strings.Contains(buf.String(), "\n") {
		t.Fatal("expected pretty-printed output with newlines")
	}
}

func TestJSONRendererColorise(t *testing.T) {
	var buf bytes.Buffer
	r := &JSONRenderer{W: &buf, Pretty: true, Colorise: true}
	report := app.Report{
		GeneratedAt: time.Unix(1000, 0),
		Providers:   []app.ProviderReport{},
	}
	if err := r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if !strings.Contains(buf.String(), "\033[") {
		t.Fatal("expected ANSI colour codes in colourised output")
	}
}

func TestColoriseJSONKeys(t *testing.T) {
	src := []byte(`{"key": "value", "num": 42, "bool": true, "nil": null}`)
	result := string(coloriseJSON(src))
	if !strings.Contains(result, "\033[1;34m") {
		t.Error("expected bold blue for keys")
	}
	if !strings.Contains(result, "\033[32m") {
		t.Error("expected green for string values")
	}
	if !strings.Contains(result, "\033[33m") {
		t.Error("expected yellow for numbers/booleans")
	}
	if !strings.Contains(result, "\033[2m") {
		t.Error("expected dim for null")
	}
}
