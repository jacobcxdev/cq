package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const modelCatalogueURL = "https://zcode.z.ai/api/v1/client/configs"
const modelCatalogueTTL = 5 * time.Minute

// ModelCatalogue resolves wire IDs from ZCode's published built-in model list.
// Configure it before first use; successful snapshots are shared for five
// minutes. A caller cancellation stops only that caller's wait. Shared fetches
// have an independent ten-second bound.
type ModelCatalogue struct {
	HTTP    httputil.Doer
	Version string
	// Platform uses ZCode's Node-style platform-architecture value. An empty
	// value uses the current runtime's platform and architecture.
	Platform string
	Now      func() time.Time

	mu        sync.Mutex
	snapshot  *modelCatalogueSnapshot
	expiresAt time.Time
	pending   *modelCatalogueFetch
}

type modelCatalogueFetch struct {
	done     chan struct{}
	snapshot *modelCatalogueSnapshot
	err      error
}

// ModelDefinition contains published ZCode model facts, without CQ's harness
// defaults. Missing metadata remains zero-valued rather than being invented.
type ModelDefinition struct {
	ID                    string
	Name                  string
	Description           string
	ContextWindow         int
	MaxOutputTokens       int
	Priority              int
	ReasoningLevels       []string
	DefaultReasoningLevel string
	InputModalities       []string
	OutputModalities      []string
	Vision                bool
}

type modelCatalogueSnapshot struct {
	models      map[string]string
	definitions []ModelDefinition
}

// Resolve returns the exact advertised ID for a case-insensitive match.
// Unknown IDs, including future models not advertised yet, remain unchanged.
// Failures also retain the input ID and return a fixed, secret-free error.
func (c *ModelCatalogue) Resolve(ctx context.Context, modelID string) (string, error) {
	snapshot, err := c.load(ctx)
	if err != nil {
		return modelID, err
	}
	return advertisedModelID(snapshot.models, modelID), nil
}

// Snapshot returns independent copies of the currently advertised definitions.
// It shares the same cached public configuration and in-flight fetch as Resolve.
func (c *ModelCatalogue) Snapshot(ctx context.Context) ([]ModelDefinition, error) {
	snapshot, err := c.load(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]ModelDefinition, len(snapshot.definitions))
	for i, model := range snapshot.definitions {
		model.ReasoningLevels = append([]string(nil), model.ReasoningLevels...)
		model.InputModalities = append([]string(nil), model.InputModalities...)
		model.OutputModalities = append([]string(nil), model.OutputModalities...)
		models[i] = model
	}
	return models, nil
}

func (c *ModelCatalogue) load(ctx context.Context) (*modelCatalogueSnapshot, error) {
	if c == nil || c.HTTP == nil || !validHeaderValue(c.Version, 80) || c.Platform != "" && !validHeaderValue(c.Platform, 80) {
		return nil, &Error{Code: "model_catalogue_unavailable"}
	}
	if ctx.Err() != nil {
		return nil, &Error{Code: "model_catalogue_cancelled"}
	}
	now := c.now()
	c.mu.Lock()
	if c.snapshot != nil && now.Before(c.expiresAt) {
		snapshot := c.snapshot
		c.mu.Unlock()
		return snapshot, nil
	}
	pending := c.pending
	if pending == nil {
		pending = &modelCatalogueFetch{done: make(chan struct{})}
		c.pending = pending
		go c.completeFetch(context.WithoutCancel(ctx), pending)
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, &Error{Code: "model_catalogue_cancelled"}
	case <-pending.done:
		if ctx.Err() != nil {
			return nil, &Error{Code: "model_catalogue_cancelled"}
		}
		return pending.snapshot, pending.err
	}
}

func (c *ModelCatalogue) completeFetch(ctx context.Context, pending *modelCatalogueFetch) {
	var snapshot *modelCatalogueSnapshot
	var err error
	var completed time.Time
	defer func() {
		if recover() != nil {
			snapshot = nil
			err = &Error{Code: "model_catalogue_request_failed"}
		}
		c.mu.Lock()
		pending.snapshot, pending.err = snapshot, err
		if err == nil {
			c.snapshot = snapshot
			c.expiresAt = completed.Add(modelCatalogueTTL)
		}
		c.pending = nil
		close(pending.done)
		c.mu.Unlock()
	}()
	snapshot, err = c.fetch(ctx)
	if err == nil {
		completed = c.now()
	}
}

func (c *ModelCatalogue) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func advertisedModelID(models map[string]string, input string) string {
	if model, ok := models[strings.ToLower(input)]; ok {
		return model
	}
	return input
}

func (c *ModelCatalogue) fetch(ctx context.Context) (snapshot *modelCatalogueSnapshot, err error) {
	defer func() {
		if recover() != nil {
			snapshot = nil
			err = &Error{Code: "model_catalogue_request_failed"}
		}
	}()
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	platform := c.Platform
	if platform == "" {
		os, arch := runtime.GOOS, runtime.GOARCH
		if os == "windows" {
			os = "win32"
		}
		if arch == "amd64" {
			arch = "x64"
		}
		platform = os + "-" + arch
	}
	query := url.Values{"app_version": {c.Version}, "platform": {platform}}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, modelCatalogueURL+"?"+query.Encode(), nil)
	if err != nil {
		return nil, &Error{Code: "model_catalogue_invalid_request"}
	}
	SetSourceHeaders(req.Header, c.Version)
	req.Header.Set("Accept", "application/json")
	doer := c.HTTP
	if client, ok := doer.(*http.Client); ok {
		private := *client
		private.Jar = nil
		private.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		doer = &private
	}
	response, err := doer.Do(req)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		code := "model_catalogue_request_failed"
		if requestCtx.Err() != nil {
			code = "model_catalogue_cancelled"
		}
		return nil, &Error{Code: code}
	}
	if response == nil || response.Body == nil {
		return nil, &Error{Code: "model_catalogue_invalid_response"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &Error{Code: "model_catalogue_http_error", HTTPStatus: response.StatusCode}
	}
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return nil, &Error{Code: "model_catalogue_read_failed"}
	}
	var envelope struct {
		Code    *int  `json:"code"`
		Success *bool `json:"success"`
		Data    *struct {
			Models *[]struct {
				ID                  *string `json:"modelId"`
				Name                string  `json:"name"`
				Description         string  `json:"description"`
				ContextWindow       int     `json:"contextWindow"`
				MaxCompletionTokens int     `json:"maxCompletionTokens"`
				Priority            int     `json:"priority"`
				Capabilities        struct {
					Vision bool `json:"vision"`
				} `json:"capabilities"`
				Reasoning struct {
					DefaultLevel string                     `json:"defaultLevel"`
					Levels       map[string]json.RawMessage `json:"levels"`
				} `json:"reasoning"`
				Modalities struct {
					Input  []string `json:"input"`
					Output []string `json:"output"`
				} `json:"modalities"`
			} `json:"builtinModels"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code == nil {
		return nil, &Error{Code: "model_catalogue_invalid_response"}
	}
	if *envelope.Code != 0 || envelope.Success != nil && !*envelope.Success {
		return nil, &Error{Code: "model_catalogue_api_error", VendorCode: *envelope.Code}
	}
	if envelope.Data == nil || envelope.Data.Models == nil {
		return nil, &Error{Code: "model_catalogue_invalid_response"}
	}
	snapshot = &modelCatalogueSnapshot{models: make(map[string]string, len(*envelope.Data.Models))}
	for _, model := range *envelope.Data.Models {
		if model.ID == nil || *model.ID == "" || strings.TrimSpace(*model.ID) != *model.ID {
			return nil, &Error{Code: "model_catalogue_invalid_model"}
		}
		key := strings.ToLower(*model.ID)
		if previous, ok := snapshot.models[key]; ok {
			if previous != *model.ID {
				return nil, &Error{Code: "model_catalogue_ambiguous_model"}
			}
			continue
		}
		snapshot.models[key] = *model.ID
		levels := make([]string, 0, len(model.Reasoning.Levels))
		for level := range model.Reasoning.Levels {
			levels = append(levels, level)
		}
		sort.Strings(levels)
		snapshot.definitions = append(snapshot.definitions, ModelDefinition{
			ID: *model.ID, Name: model.Name, Description: model.Description,
			ContextWindow: model.ContextWindow, MaxOutputTokens: model.MaxCompletionTokens,
			Priority: model.Priority, ReasoningLevels: levels,
			DefaultReasoningLevel: model.Reasoning.DefaultLevel,
			InputModalities:       model.Modalities.Input, OutputModalities: model.Modalities.Output,
			Vision: model.Capabilities.Vision,
		})
	}
	return snapshot, nil
}
