package zcode

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const catalogueFixture = `{"code":0,"data":{"builtinModels":[{"modelId":"GLM-5.3","name":"Flagship"},{"modelId":"GLM-5.3-Flash","name":"Fast"}]}}`

func TestModelCatalogueSnapshotSharedFactsWithoutMutableCache(t *testing.T) {
	var calls atomic.Int32
	c := ModelCatalogue{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(`{"code":0,"data":{"builtinModels":[{"modelId":"GLM-5.3-Flash","name":"GLM-5.3-Flash","contextWindow":1000000,"maxCompletionTokens":128000,"priority":100,"capabilities":{"vision":true},"reasoning":{"defaultLevel":"max","levels":{"max":{},"low":{},"high":{}}},"modalities":{"input":["text","image","video"],"output":["text"]}}]}}`), nil
	})}
	models, err := c.Snapshot(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("Snapshot: %v,%v", models, err)
	}
	model := models[0]
	if model.ID != "GLM-5.3-Flash" || model.Name != model.ID || model.ContextWindow != 1000000 ||
		model.MaxOutputTokens != 128000 || model.Priority != 100 || model.DefaultReasoningLevel != "max" ||
		!model.Vision || strings.Join(model.ReasoningLevels, ",") != "high,low,max" ||
		strings.Join(model.InputModalities, ",") != "text,image,video" || strings.Join(model.OutputModalities, ",") != "text" {
		t.Fatalf("advertised facts changed: %+v", model)
	}
	models[0].ID = "mutated"
	models[0].ReasoningLevels[0] = "mutated"
	models[0].InputModalities[0] = "mutated"
	models[0].OutputModalities[0] = "mutated"
	again, err := c.Snapshot(context.Background())
	if err != nil || again[0].ID != "GLM-5.3-Flash" || again[0].ReasoningLevels[0] != "high" ||
		again[0].InputModalities[0] != "text" || again[0].OutputModalities[0] != "text" {
		t.Fatalf("caller mutated cached metadata: %+v,%v", again, err)
	}
	if got, err := c.Resolve(context.Background(), "glm-5.3-flash"); err != nil || got != "GLM-5.3-Flash" || calls.Load() != 1 {
		t.Fatalf("resolver did not share snapshot: %q,%v,calls=%d", got, err, calls.Load())
	}
}

func TestModelCatalogueRecoveredFetchPanicWithoutPoisonedWaiters(t *testing.T) {
	var calls atomic.Int32
	c := ModelCatalogue{Version: "3.17.2", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 10*time.Second {
			return nil, errors.New("missing independent fetch bound")
		}
		if calls.Add(1) == 1 {
			panic("private-upstream-value")
		}
		return jsonResponse(catalogueFixture), nil
	})}
	if _, err := c.Snapshot(context.Background()); err == nil || strings.Contains(err.Error(), "private-upstream-value") {
		t.Fatalf("unsafe panic result: %v", err)
	}
	if models, err := c.Snapshot(context.Background()); err != nil || len(models) != 2 || calls.Load() != 2 {
		t.Fatalf("pending fetch stuck after panic: %+v,%v,calls=%d", models, err, calls.Load())
	}
}

func TestModelCatalogueResolvedAdvertisedCaseAndPreservedUnknown(t *testing.T) {
	var calls int
	c := ModelCatalogue{Version: "3.17.2", Platform: "darwin-arm64", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "zcode.z.ai" || r.URL.Path != "/api/v1/client/configs" || r.URL.Query().Get("app_version") != "3.17.2" || r.URL.Query().Get("platform") != "darwin-arm64" || r.Body != nil {
			t.Fatal("wrong public catalogue request")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-ZCode-Authorization") != "" {
			t.Fatal("account credentials reached public metadata")
		}
		return jsonResponse(catalogueFixture), nil
	})}
	for input, want := range map[string]string{"glm-5.3": "GLM-5.3", "GlM-5.3-FlAsH": "GLM-5.3-Flash", "future-unknown": "future-unknown", " GLM-5.3 ": " GLM-5.3 "} {
		got, err := c.Resolve(context.Background(), input)
		if err != nil || got != want {
			t.Fatalf("Resolve(%q)=%q,%v; want %q", input, got, err, want)
		}
	}
	if calls != 1 {
		t.Fatalf("metadata fetched %d times", calls)
	}
}

func TestModelCatalogueRefreshPickedUpFutureModels(t *testing.T) {
	now := time.Unix(1800000000, 0)
	calls := 0
	c := ModelCatalogue{Version: "3.17.2", Now: func() time.Time { return now }, HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return jsonResponse(catalogueFixture), nil
		}
		return jsonResponse(`{"code":0,"data":{"builtinModels":[{"modelId":"GLM-5.4-Future"}]}}`), nil
	})}
	if got, err := c.Resolve(context.Background(), "glm-5.4-future"); err != nil || got != "glm-5.4-future" {
		t.Fatalf("unadvertised future: %q,%v", got, err)
	}
	now = now.Add(5 * time.Minute)
	if got, err := c.Resolve(context.Background(), "glm-5.4-future"); err != nil || got != "GLM-5.4-Future" {
		t.Fatalf("new advertised model: %q,%v", got, err)
	}
	if got, err := c.Resolve(context.Background(), "glm-5.3"); err != nil || got != "glm-5.3" {
		t.Fatalf("removed model invented from stale data: %q,%v", got, err)
	}
	if calls != 2 {
		t.Fatalf("refresh calls %d", calls)
	}
}

func TestModelCatalogueRejectedInvalidResponsesWithoutLeaking(t *testing.T) {
	for name, body := range map[string]string{
		"bad JSON":              "private-upstream-value",
		"missing code":          `{"data":{"builtinModels":[]}}`,
		"string code":           `{"code":"0","data":{"builtinModels":[]}}`,
		"API rejection":         `{"code":2007,"msg":"private-upstream-value","data":{"builtinModels":[]}}`,
		"false success":         `{"code":0,"success":false,"data":{"builtinModels":[]}}`,
		"missing array":         `{"code":0,"data":{}}`,
		"null array":            `{"code":0,"data":{"builtinModels":null}}`,
		"missing ID":            `{"code":0,"data":{"builtinModels":[{"name":"GLM-5.3"}]}}`,
		"wrong ID type":         `{"code":0,"data":{"builtinModels":[{"modelId":42}]}}`,
		"wrong context type":    `{"code":0,"data":{"builtinModels":[{"modelId":"GLM-5.3","contextWindow":"1000000"}]}}`,
		"wrong modalities type": `{"code":0,"data":{"builtinModels":[{"modelId":"GLM-5.3","modalities":{"input":"text"}}]}}`,
		"empty ID":              `{"code":0,"data":{"builtinModels":[{"modelId":""}]}}`,
		"ambiguous case":        `{"code":0,"data":{"builtinModels":[{"modelId":"GLM-5.3"},{"modelId":"glm-5.3"}]}}`,
		"oversized":             strings.Repeat("x", httputil.MaxResponseBody+1),
	} {
		t.Run(name, func(t *testing.T) {
			c := ModelCatalogue{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(body), nil })}
			got, err := c.Resolve(context.Background(), "original-model")
			if err == nil || got != "original-model" || strings.Contains(err.Error(), "private-upstream-value") {
				t.Fatalf("unsafe invalid result: %q,%v", got, err)
			}
		})
	}
}

func TestModelCatalogueCoalescedConcurrentColdReads(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	c := ModelCatalogue{Version: "3.17.2", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return jsonResponse(catalogueFixture), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	var group sync.WaitGroup
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			defer func() {
				if recover() != nil {
					results <- errors.New("catalogue goroutine panicked")
				}
			}()
			got, err := c.Resolve(context.Background(), "glm-5.3")
			if err == nil && got != "GLM-5.3" {
				err = errors.New("incorrect wire model")
			}
			results <- err
		}()
	}
	<-entered
	close(release)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent cold requests made %d fetches", calls.Load())
	}
}

func TestModelCatalogueCancelledWaiterDidNotCancelOwner(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	c := ModelCatalogue{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return jsonResponse(catalogueFixture), nil
	})}
	owner := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				owner <- errors.New("owner panicked")
			}
		}()
		_, err := c.Resolve(context.Background(), "glm-5.3")
		owner <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	follower := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				follower <- errors.New("waiter panicked")
			}
		}()
		_, err := c.Resolve(&catalogueWaitContext{Context: ctx, waiting: waiting}, "glm-5.3")
		follower <- err
	}()
	<-waiting
	cancel()
	select {
	case err := <-follower:
		if err == nil {
			t.Fatal("cancelled waiter succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter ignored context")
	}
	close(release)
	if err := <-owner; err != nil {
		t.Fatalf("waiter cancelled owner: %v", err)
	}
	if got, err := c.Resolve(context.Background(), "glm-5.3"); err != nil || got != "GLM-5.3" {
		t.Fatalf("owner cache lost: %q,%v", got, err)
	}
}

func TestModelCatalogueCancelledOwnerDidNotCancelWaiter(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := ModelCatalogue{Version: "3.17.2", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return jsonResponse(catalogueFixture), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				owner <- errors.New("owner panicked")
			}
		}()
		_, err := c.Resolve(ctx, "glm-5.3")
		owner <- err
	}()
	<-entered
	waiting := make(chan struct{})
	follower := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				follower <- errors.New("waiter panicked")
			}
		}()
		got, err := c.Resolve(&catalogueWaitContext{Context: context.Background(), waiting: waiting}, "glm-5.3")
		if err == nil && got != "GLM-5.3" {
			err = errors.New("incorrect wire model")
		}
		follower <- err
	}()
	<-waiting
	cancel()
	select {
	case err := <-owner:
		if err == nil {
			t.Fatal("cancelled owner succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("owner ignored cancellation")
	}
	close(release)
	if err := <-follower; err != nil {
		t.Fatalf("owner cancellation failed surviving waiter: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("shared request fetched %d times", calls.Load())
	}
}

type catalogueWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *catalogueWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestModelCatalogueBlockedRedirectsAndCookies(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse("https://zcode.z.ai")
	jar.SetCookies(u, []*http.Cookie{{Name: "private-cookie", Value: "private-value"}})
	var calls int
	client := &http.Client{Jar: jar, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Cookie") != "" {
			t.Fatal("cookies reached catalogue")
		}
		rsp := jsonResponse("")
		rsp.StatusCode = 302
		rsp.Header.Set("Location", "https://example.invalid")
		return rsp, nil
	})}
	c := ModelCatalogue{Version: "3.17.2", HTTP: client}
	if got, err := c.Resolve(context.Background(), "glm-5.3"); err == nil || got != "glm-5.3" {
		t.Fatal("redirect accepted")
	}
	if calls != 1 || client.Jar != jar || client.CheckRedirect != nil {
		t.Fatal("HTTP policy mutated or redirect followed")
	}
}
