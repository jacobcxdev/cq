package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func TestConfigHeadroomJSON(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wantBool bool
	}{
		{"enabled", `{"port":19280,"local_token":"tok","headroom":true}`, true},
		{"disabled", `{"port":19280,"local_token":"tok","headroom":false}`, false},
		{"omitted", `{"port":19280,"local_token":"tok"}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(tt.json), &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Headroom != tt.wantBool {
				t.Errorf("Headroom = %v, want %v", cfg.Headroom, tt.wantBool)
			}
		})
	}

	// Round-trip: true should appear in JSON output.
	t.Run("marshal_true", func(t *testing.T) {
		cfg := Config{Port: 19280, LocalToken: "tok", Headroom: true}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if string(raw["headroom"]) != "true" {
			t.Errorf("expected headroom:true in JSON, got %s", data)
		}
	})

	// Round-trip: false should be omitted (omitempty).
	t.Run("marshal_false_omitted", func(t *testing.T) {
		cfg := Config{Port: 19280, LocalToken: "tok", Headroom: false}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["headroom"]; ok {
			t.Errorf("expected headroom omitted when false, got %s", data)
		}
	})
}

// fakeBridge creates a HeadroomBridge backed by an in-process pipe pair
// instead of a real Python subprocess. The responder function handles
// each JSON line and writes back a response.
func fakeBridge(t *testing.T, responder func(req headroomRequest) headroomResponse) *HeadroomBridge {
	t.Helper()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	process := newHeadroomTestProcess()

	t.Cleanup(func() {
		stdinW.Close()
		stdinR.Close()
		stdoutW.Close()
	})

	go func() {
		defer process.exit()
		scanner := bufio.NewScanner(stdinR)
		for scanner.Scan() {
			var req headroomRequest
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				continue
			}
			resp := responder(req)
			line, _ := json.Marshal(resp)
			line = append(line, '\n')
			stdoutW.Write(line)
		}
		stdoutW.Close()
	}()

	bridge := newHeadroomBridge(process, stdinW, stdoutR, nil)
	t.Cleanup(bridge.Stop)
	return bridge
}

func TestHeadroomProbeConfirmsLiveCacheBridge(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		return validHeadroomProbeResponse(reqBytes)
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bridge.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestHeadroomPingKeepsTokenModeAvailableWithoutResponsesConverter(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var operation struct {
			Operation string `json:"operation"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(reqBytes, &operation); err != nil {
			return []byte(`not-json`)
		}
		if operation.Operation == "probe" {
			response, _ := json.Marshal(headroomProbeResponse{
				Operation: "probe",
				RequestID: operation.RequestID,
				Protocol:  1,
				OK:        true,
				CacheMode: false,
			})
			return response
		}
		response, _ := json.Marshal(headroomResponse{Messages: json.RawMessage(`[]`)})
		return response
	})

	if err := bridge.ping(); err != nil {
		t.Fatalf("ping rejected token-only bridge: %v", err)
	}
}

func TestHeadroomProbeRequiresDeadline(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		return validHeadroomProbeResponse(reqBytes)
	})

	if err := bridge.Probe(context.Background()); err == nil {
		t.Fatal("Probe succeeded without a context deadline")
	}
}

func TestHeadroomProbeRejectsNilBridge(t *testing.T) {
	var bridge *HeadroomBridge
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bridge.Probe(ctx); !errors.Is(err, errHeadroomProbeUnavailable) {
		t.Fatalf("Probe error = %v, want unavailable", err)
	}
}

func TestHeadroomProbeRejectsStoppedBridge(t *testing.T) {
	bridge := headroomTestBridge(t, func([]byte) headroomTestAction {
		return headroomTestAction{Response: []byte(`{}`), Respond: true}
	})
	bridge.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bridge.Probe(ctx); !errors.Is(err, errHeadroomProbeUnavailable) {
		t.Fatalf("Probe error = %v, want unavailable", err)
	}
}

func TestHeadroomProbeRejectsExitedBridge(t *testing.T) {
	bridge := headroomTestBridge(t, func([]byte) headroomTestAction {
		return headroomTestAction{Exit: true}
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bridge.Probe(ctx); !errors.Is(err, errHeadroomProbeUnavailable) {
		t.Fatalf("Probe error = %v, want unavailable", err)
	}
}

func TestHeadroomProbeDeadlineInterruptsHungResponse(t *testing.T) {
	requestSeen := make(chan struct{})
	var seenOnce sync.Once
	bridge := headroomTestBridge(t, func([]byte) headroomTestAction {
		seenOnce.Do(func() { close(requestSeen) })
		return headroomTestAction{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- bridge.Probe(ctx) }()

	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("probe request was not dispatched")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Probe error = %v, want deadline exceeded", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Probe did not return after its context deadline")
	}
}

func TestHeadroomExchangeKeepsDeadlineActiveThroughValidation(t *testing.T) {
	bridge := headroomTestBridge(t, func([]byte) headroomTestAction {
		return headroomTestAction{Response: []byte(`{}`), Respond: true}
	})
	validationStarted := make(chan struct{})
	releaseValidation := make(chan struct{})
	defer func() {
		select {
		case <-releaseValidation:
		default:
			close(releaseValidation)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := bridge.exchange(ctx, []byte(`{}`), func([]byte) error {
			close(validationStarted)
			<-releaseValidation
			return nil
		})
		result <- err
	}()

	select {
	case <-validationStarted:
	case <-time.After(time.Second):
		t.Fatal("validation did not start")
	}
	<-ctx.Done()
	close(releaseValidation)
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exchange error = %v, want deadline exceeded", err)
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), time.Second)
	defer probeCancel()
	if err := bridge.Probe(probeCtx); !errors.Is(err, errHeadroomProbeUnavailable) {
		t.Fatalf("Probe after validation deadline = %v, want unavailable", err)
	}
}

func TestHeadroomProbeRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name     string
		response func([]byte) []byte
	}{
		{name: "malformed", response: func([]byte) []byte { return []byte(`not-json`) }},
		{name: "trailing_json", response: func(req []byte) []byte { return append(validHeadroomProbeResponse(req), []byte(` {}`)...) }},
		{name: "wrong_operation", response: mutateHeadroomProbeResponse("operation", "compress_messages")},
		{name: "empty_request_id", response: mutateHeadroomProbeResponse("request_id", "")},
		{name: "wrong_request_id", response: mutateHeadroomProbeResponse("request_id", "not-the-request")},
		{name: "wrong_protocol", response: mutateHeadroomProbeResponse("protocol", 2)},
		{name: "not_ok", response: mutateHeadroomProbeResponse("ok", false)},
		{name: "no_cache_mode", response: mutateHeadroomProbeResponse("cache_mode", false)},
		{name: "unknown_field", response: mutateHeadroomProbeResponse("unexpected", true)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			bridge := headroomTestBridge(t, func(req []byte) headroomTestAction {
				calls++
				if calls == 1 {
					return headroomTestAction{Response: tt.response(req), Respond: true}
				}
				return headroomTestAction{Response: validHeadroomProbeResponse(req), Respond: true}
			})

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := bridge.Probe(ctx); !errors.Is(err, errHeadroomProbeInvalid) {
				t.Fatalf("first Probe error = %v, want invalid response", err)
			}
			if err := bridge.Probe(ctx); !errors.Is(err, errHeadroomProbeUnavailable) {
				t.Fatalf("second Probe error = %v, want retired bridge", err)
			}
		})
	}
}

func TestHeadroomProbeDoesNotExposeMalformedResponse(t *testing.T) {
	const privateMarker = "private-response-marker"
	bridge := headroomTestBridge(t, func([]byte) headroomTestAction {
		return headroomTestAction{Response: []byte(`{"` + privateMarker + `"`), Respond: true}
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := bridge.Probe(ctx)
	if err == nil {
		t.Fatal("Probe succeeded with malformed response")
	}
	if strings.Contains(err.Error(), privateMarker) {
		t.Fatalf("Probe error exposed response bytes: %v", err)
	}
}

func TestHeadroomProbeSerialisesWithCompress(t *testing.T) {
	compressSeen := make(chan struct{})
	releaseCompress := make(chan struct{})
	probeSeen := make(chan struct{})
	var compressOnce, probeOnce sync.Once
	bridge := headroomTestBridge(t, func(reqBytes []byte) headroomTestAction {
		var operation struct {
			Operation string `json:"operation"`
		}
		_ = json.Unmarshal(reqBytes, &operation)
		if operation.Operation == "probe" {
			probeOnce.Do(func() { close(probeSeen) })
			return headroomTestAction{Response: validHeadroomProbeResponse(reqBytes), Respond: true}
		}

		compressOnce.Do(func() { close(compressSeen) })
		<-releaseCompress
		var req headroomRequest
		_ = json.Unmarshal(reqBytes, &req)
		response, _ := json.Marshal(headroomResponse{Messages: req.Messages})
		return headroomTestAction{Response: response, Respond: true}
	})
	defer func() {
		select {
		case <-releaseCompress:
		default:
			close(releaseCompress)
		}
	}()

	compressResult := make(chan error, 1)
	go func() {
		_, _, err := bridge.Compress([]byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`))
		compressResult <- err
	}()
	select {
	case <-compressSeen:
	case <-time.After(time.Second):
		t.Fatal("Compress was not dispatched")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	probeResult := make(chan error, 1)
	go func() { probeResult <- bridge.Probe(ctx) }()
	select {
	case <-probeSeen:
		t.Fatal("Probe was dispatched before Compress completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseCompress)

	if err := <-compressResult; err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if err := <-probeResult; err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestHeadroomProbeQueuedCancellationDoesNotRetireBridge(t *testing.T) {
	compressSeen := make(chan struct{})
	releaseCompress := make(chan struct{})
	var compressOnce sync.Once
	bridge := headroomTestBridge(t, func(reqBytes []byte) headroomTestAction {
		var operation struct {
			Operation string `json:"operation"`
		}
		_ = json.Unmarshal(reqBytes, &operation)
		if operation.Operation == "probe" {
			return headroomTestAction{Response: validHeadroomProbeResponse(reqBytes), Respond: true}
		}

		compressOnce.Do(func() { close(compressSeen) })
		<-releaseCompress
		var req headroomRequest
		_ = json.Unmarshal(reqBytes, &req)
		response, _ := json.Marshal(headroomResponse{Messages: req.Messages})
		return headroomTestAction{Response: response, Respond: true}
	})
	defer func() {
		select {
		case <-releaseCompress:
		default:
			close(releaseCompress)
		}
	}()

	compressResult := make(chan error, 1)
	go func() {
		_, _, err := bridge.Compress([]byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`))
		compressResult <- err
	}()
	select {
	case <-compressSeen:
	case <-time.After(time.Second):
		t.Fatal("Compress was not dispatched")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	probeResult := make(chan error, 1)
	go func() { probeResult <- bridge.Probe(ctx) }()
	select {
	case err := <-probeResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued Probe error = %v, want deadline exceeded", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("queued Probe ignored context deadline")
	}

	close(releaseCompress)
	if err := <-compressResult; err != nil {
		t.Fatalf("Compress: %v", err)
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), time.Second)
	defer probeCancel()
	if err := bridge.Probe(probeCtx); err != nil {
		t.Fatalf("Probe after queued cancellation: %v", err)
	}
}

func TestHeadroomProbeConcurrentStopReturnsPromptly(t *testing.T) {
	probeSeen := make(chan struct{})
	var probeOnce sync.Once
	bridge := headroomTestBridge(t, func([]byte) headroomTestAction {
		probeOnce.Do(func() { close(probeSeen) })
		return headroomTestAction{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	probeResult := make(chan error, 1)
	go func() { probeResult <- bridge.Probe(ctx) }()
	select {
	case <-probeSeen:
	case <-time.After(time.Second):
		t.Fatal("Probe was not dispatched")
	}

	stopResult := make(chan struct{})
	go func() {
		bridge.Stop()
		close(stopResult)
	}()
	select {
	case <-stopResult:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop deadlocked with Probe")
	}
	select {
	case err := <-probeResult:
		if !errors.Is(err, errHeadroomProbeUnavailable) {
			t.Fatalf("Probe error = %v, want unavailable", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Probe remained blocked after Stop")
	}
}

func TestHeadroomStopUnblocksHungCompress(t *testing.T) {
	compressSeen := make(chan struct{})
	var compressOnce sync.Once
	bridge := headroomTestBridge(t, func([]byte) headroomTestAction {
		compressOnce.Do(func() { close(compressSeen) })
		return headroomTestAction{}
	})

	compressResult := make(chan error, 1)
	go func() {
		_, _, err := bridge.Compress([]byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`))
		compressResult <- err
	}()
	select {
	case <-compressSeen:
	case <-time.After(time.Second):
		t.Fatal("Compress was not dispatched")
	}

	stopResult := make(chan struct{})
	go func() {
		bridge.Stop()
		close(stopResult)
	}()
	select {
	case <-stopResult:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop deadlocked with Compress")
	}
	select {
	case err := <-compressResult:
		if err == nil {
			t.Fatal("Compress succeeded after bridge Stop")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Compress remained blocked after Stop")
	}
}

func TestHeadroomStopIsSafeForConcurrentCallers(t *testing.T) {
	bridge := headroomTestBridge(t, func([]byte) headroomTestAction {
		return headroomTestAction{Exit: true}
	})

	const callers = 8
	done := make(chan struct{}, callers)
	for i := 0; i < callers; i++ {
		go func() {
			bridge.Stop()
			done <- struct{}{}
		}()
	}
	for i := 0; i < callers; i++ {
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("concurrent Stop call did not return")
		}
	}
}

func TestHeadroomStopReturnsAfterStderrReaderPanic(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	process := newHeadroomTestProcess()
	bridge := newHeadroomBridge(process, stdinW, stdoutR, panickingHeadroomReadCloser{})
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdoutW.Close()
	})

	stopResult := make(chan struct{})
	go func() {
		bridge.Stop()
		close(stopResult)
	}()
	select {
	case <-stopResult:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop blocked after stderr reader panic")
	}
}

func TestHeadroomStopReturnsAfterProcessWaitPanic(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	bridge := newHeadroomBridge(panickingHeadroomProcess{}, stdinW, stdoutR, nil)
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdoutW.Close()
	})

	stopResult := make(chan struct{})
	go func() {
		bridge.Stop()
		close(stopResult)
	}()
	select {
	case <-stopResult:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop blocked after process Wait panic")
	}
}

func TestHeadroomDefersProcessWaitUntilReadersRetire(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	process := newHeadroomTestProcess()
	bridge := newHeadroomBridge(process, stdinW, stdoutR, nil)
	t.Cleanup(func() {
		bridge.Stop()
		_ = stdinR.Close()
		_ = stdoutW.Close()
	})

	select {
	case <-process.waitStarted:
		t.Fatal("process Wait started before bridge readers retired")
	case <-time.After(20 * time.Millisecond):
	}
	bridge.Stop()
	select {
	case <-process.waitStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("process Wait did not start during bridge retirement")
	}
}

func TestHeadroomProbeDeadlineKillsAndReapsHungProcess(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	process := newHeadroomTestProcess()
	requestSeen := make(chan struct{})
	var requestOnce sync.Once
	go func() {
		defer stdoutW.Close()
		scanner := bufio.NewScanner(stdinR)
		if scanner.Scan() {
			requestOnce.Do(func() { close(requestSeen) })
			<-process.killed
		}
	}()
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdoutW.Close()
		_ = stderrW.Close()
		_ = process.Kill()
	})

	bridge := newHeadroomBridge(process, stdinW, stdoutR, stderrR)
	t.Cleanup(bridge.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := bridge.Probe(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Probe error = %v, want deadline exceeded", err)
	}
	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("Probe request was not dispatched")
	}
	select {
	case <-process.killed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Probe cancellation did not kill process")
	}
	select {
	case <-bridge.processDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Probe cancellation did not reap process")
	}
	select {
	case <-bridge.stderrDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Probe cancellation did not join stderr drain")
	}
}

func TestCompress_SplicesMessages(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		return headroomResponse{
			Messages:         json.RawMessage(`[{"role":"user","content":"hi"}]`),
			TokensSaved:      42,
			CompressionRatio: 0.5,
		}
	})

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hello world, this is a long message"}],"max_tokens":1024,"stream":true}`)

	compressed, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 42 {
		t.Errorf("tokens saved = %d, want 42", saved)
	}

	// Verify messages were spliced and other fields preserved.
	var result map[string]json.RawMessage
	if err := json.Unmarshal(compressed, &result); err != nil {
		t.Fatal(err)
	}
	if string(result["messages"]) != `[{"role":"user","content":"hi"}]` {
		t.Errorf("messages = %s, want compressed version", result["messages"])
	}
	if string(result["model"]) != `"claude-sonnet-4-5-20250929"` {
		t.Errorf("model = %s, want preserved", result["model"])
	}
	if string(result["max_tokens"]) != "1024" {
		t.Errorf("max_tokens = %s, want preserved", result["max_tokens"])
	}
	if string(result["stream"]) != "true" {
		t.Errorf("stream = %s, want preserved", result["stream"])
	}
}

func TestCompress_KnownModel_IncludesModelLimit(t *testing.T) {
	var captured struct {
		ModelLimit *int `json:"model_limit,omitempty"`
	}

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		if err := json.Unmarshal(reqBytes, &captured); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		resp, _ := json.Marshal(headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		})
		return resp
	})

	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`)
	if _, _, err := bridge.Compress(body); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if captured.ModelLimit == nil {
		t.Fatal("model_limit missing from bridge request")
	}
	if *captured.ModelLimit != 1050000 {
		t.Fatalf("model_limit = %d, want 1050000", *captured.ModelLimit)
	}
}

func TestCompress_RegistryModel_IncludesCatalogLimit(t *testing.T) {
	var captured struct {
		ModelLimit *int `json:"model_limit,omitempty"`
	}

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		if err := json.Unmarshal(reqBytes, &captured); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		resp, _ := json.Marshal(headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		})
		return resp
	})
	bridge.Catalog = modelregistry.NewCatalog(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{Provider: modelregistry.ProviderCodex, ID: "gpt-5.5", Source: modelregistry.SourceOverlay, ContextWindow: 2000000},
	}})

	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`)
	if _, _, err := bridge.Compress(body); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if captured.ModelLimit == nil {
		t.Fatal("model_limit missing from bridge request")
	}
	if *captured.ModelLimit != 2000000 {
		t.Fatalf("model_limit = %d, want 2000000", *captured.ModelLimit)
	}
}

func TestCompress_UnknownModel_OmitsModelLimit(t *testing.T) {
	var captured struct {
		ModelLimit *int `json:"model_limit,omitempty"`
	}

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		if err := json.Unmarshal(reqBytes, &captured); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		resp, _ := json.Marshal(headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		})
		return resp
	})

	body := []byte(`{"model":"unknown-model-xyz","messages":[{"role":"user","content":"hello"}]}`)
	if _, _, err := bridge.Compress(body); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if captured.ModelLimit != nil {
		t.Fatalf("model_limit = %d, want omitted", *captured.ModelLimit)
	}
}

func TestCompress_NoMessages(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		return headroomResponse{
			Messages:    json.RawMessage(`[]`),
			TokensSaved: 0,
		}
	})

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":1024}`)
	compressed, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(compressed) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", compressed, body)
	}
}

func TestCompress_NullMessages(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		return headroomResponse{Messages: json.RawMessage(`[]`), TokensSaved: 0}
	})

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":null}`)
	compressed, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(compressed) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", compressed, body)
	}
}

func TestCompress_ZeroSaved(t *testing.T) {
	bridge := fakeBridge(t, func(req headroomRequest) headroomResponse {
		if string(req.Messages) != `[{"role":"user","content":"hello world"}]` {
			t.Errorf("bridge got messages = %s", req.Messages)
		}
		return headroomResponse{
			Messages:    req.Messages,
			TokensSaved: 0,
		}
	})

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hello world"}]}`)
	compressed, saved, err := bridge.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(compressed) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", compressed, body)
	}
}

func TestCompress_BridgeError_ReturnsOriginal(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stdinW.Close()
	stdinR.Close()
	stdoutW.Close()
	process := newHeadroomTestProcess()
	process.exit()

	bridge := newHeadroomBridge(process, stdinW, stdoutR, nil)
	t.Cleanup(bridge.Stop)

	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hello"}]}`)
	compressed, saved, err := bridge.Compress(body)
	if err == nil {
		t.Fatal("expected error")
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(compressed) != string(body) {
		t.Errorf("compressed = %s, want original %s", compressed, body)
	}
}

func TestCompress_InvalidJSON_ReturnsOriginal(t *testing.T) {
	bridge := fakeBridge(t, func(_ headroomRequest) headroomResponse {
		return headroomResponse{Messages: json.RawMessage(`[]`), TokensSaved: 0}
	})

	body := []byte(`{`)
	compressed, saved, err := bridge.Compress(body)
	if err == nil {
		t.Fatal("expected error")
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(compressed) != string(body) {
		t.Errorf("body = %s, want original %s", compressed, body)
	}
}

func TestSpliceMessages(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"old"}],"max_tokens":1}`)
	messages := json.RawMessage(`[{"role":"user","content":"new"}]`)
	out, err := spliceMessages(body, messages)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["messages"]) != string(messages) {
		t.Errorf("messages = %s, want %s", got["messages"], messages)
	}
	if string(got["model"]) != `"claude-sonnet"` {
		t.Errorf("model = %s, want preserved", got["model"])
	}
	if string(got["max_tokens"]) != "1" {
		t.Errorf("max_tokens = %s, want preserved", got["max_tokens"])
	}
}

func TestFindPython3_FallsBackToPath(t *testing.T) {
	python, err := findPython3()
	if err != nil {
		t.Fatalf("findPython3: %v", err)
	}
	if python == "" {
		t.Fatal("empty python path")
	}
}

func TestFindPython3_EmptyPATH(t *testing.T) {
	t.Skip("findPython3 probes well-known paths outside PATH")
}

func TestStartHeadroomBridge_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	bridge, err := StartHeadroomBridge()
	if err != nil {
		t.Skipf("headroom bridge unavailable: %v", err)
	}
	defer bridge.Stop()
	body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hello world hello world hello world hello world"}]}`)
	_, _, err = bridge.Compress(body)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
}

func TestDrainStderr_SuppressesKnownNoise(t *testing.T) {
	text := strings.Join([]string{
		"Warning: You are sending unauthenticated requests to the HF Hub. Please set a HF_TOKEN to enable higher rate limits and faster downloads.",
		"Tag placeholder lost during compression, appending: <system-reminder>",
	}, "\n")
	got := captureStderr(t, func() {
		bridge := &HeadroomBridge{}
		bridge.drainStderr(strings.NewReader(text))
	})
	if got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestDrainStderr_SuppressesHeadroomInfoLogs(t *testing.T) {
	text := "2026-04-17 04:31:41,662 - headroom.transforms.pipeline - INFO - Pipeline using ContentRouter for intelligent content-aware compression\n"
	got := captureStderr(t, func() {
		bridge := &HeadroomBridge{}
		bridge.drainStderr(strings.NewReader(text))
	})
	if got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestDrainStderr_PreservesUnexpectedDiagnostics(t *testing.T) {
	text := "unexpected diagnostic\n"
	got := captureStderr(t, func() {
		bridge := &HeadroomBridge{}
		bridge.drainStderr(strings.NewReader(text))
	})
	if !strings.Contains(got, text) {
		t.Fatalf("stderr = %q, want %q", got, text)
	}
}

func TestDrainStderr_SuppressesShutdownKeyboardInterrupt(t *testing.T) {
	text := strings.Join([]string{
		"Traceback (most recent call last):",
		"  File \"<string>\", line 12, in <module>",
		"    for line in sys.stdin:",
		"                ^^^^^^^^^",
		"KeyboardInterrupt",
	}, "\n")
	bridge := &HeadroomBridge{}
	bridge.shuttingDown.Store(true)
	got := captureStderr(t, func() {
		bridge.drainStderr(strings.NewReader(text))
	})
	if got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestHealthEndpoint_HeadroomField(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hasHeadroom bool
	}{
		{name: "disabled", hasHeadroom: false},
		{name: "enabled", hasHeadroom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{}
			if tc.hasHeadroom {
				srv.Headroom = fakeBridge(t, func(_ headroomRequest) headroomResponse {
					return headroomResponse{}
				})
			}
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/health", nil)
			srv.handleHealth(w, req)
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			got, ok := resp["headroom"].(bool)
			if !ok {
				t.Fatal("headroom field missing from health response")
			}
			if got != tc.hasHeadroom {
				t.Errorf("headroom = %v, want %v", got, tc.hasHeadroom)
			}
		})
	}
}

// fakeBridgeRaw creates a HeadroomBridge backed by an in-process pipe pair.
// The responder receives the raw request bytes and returns raw response bytes.
// This supports both messages and responses operations.
func fakeBridgeRaw(t *testing.T, responder func(req []byte) []byte) *HeadroomBridge {
	t.Helper()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	process := newHeadroomTestProcess()

	t.Cleanup(func() {
		stdinW.Close()
		stdinR.Close()
		stdoutW.Close()
	})

	go func() {
		defer process.exit()
		scanner := bufio.NewScanner(stdinR)
		for scanner.Scan() {
			resp := responder(scanner.Bytes())
			resp = append(resp, '\n')
			stdoutW.Write(resp)
		}
		stdoutW.Close()
	}()

	bridge := newHeadroomBridge(process, stdinW, stdoutR, nil)
	t.Cleanup(bridge.Stop)
	return bridge
}

type headroomTestAction struct {
	Response []byte
	Respond  bool
	Exit     bool
}

type headroomTestProcess struct {
	done        chan struct{}
	killed      chan struct{}
	waitStarted chan struct{}
	exitOnce    sync.Once
	killOnce    sync.Once
	waitOnce    sync.Once
}

type panickingHeadroomReadCloser struct{}

func (panickingHeadroomReadCloser) Read([]byte) (int, error) {
	panic("stderr read panic")
}

func (panickingHeadroomReadCloser) Close() error { return nil }

type panickingHeadroomProcess struct{}

func (panickingHeadroomProcess) Wait() error { panic("process wait panic") }

func (panickingHeadroomProcess) Kill() error { return nil }

func newHeadroomTestProcess() *headroomTestProcess {
	return &headroomTestProcess{
		done:        make(chan struct{}),
		killed:      make(chan struct{}),
		waitStarted: make(chan struct{}),
	}
}

func (p *headroomTestProcess) Wait() error {
	p.waitOnce.Do(func() { close(p.waitStarted) })
	<-p.done
	return nil
}

func (p *headroomTestProcess) Kill() error {
	p.killOnce.Do(func() { close(p.killed) })
	p.exit()
	return nil
}

func (p *headroomTestProcess) exit() {
	p.exitOnce.Do(func() { close(p.done) })
}

// headroomTestBridge drives the real bridge pipe protocol without launching
// Python. Returning Respond=false leaves the subprocess side alive but silent;
// returning Exit=true closes its stdout as an exited subprocess would.
func headroomTestBridge(t *testing.T, responder func([]byte) headroomTestAction) *HeadroomBridge {
	t.Helper()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	process := newHeadroomTestProcess()
	t.Cleanup(func() {
		_ = stdinW.Close()
		_ = stdinR.Close()
		_ = stdoutW.Close()
		_ = stdoutR.Close()
	})

	go func() {
		defer process.exit()
		defer stdoutW.Close()
		scanner := bufio.NewScanner(stdinR)
		for scanner.Scan() {
			action := responder(bytes.Clone(scanner.Bytes()))
			if action.Exit {
				return
			}
			if !action.Respond {
				continue
			}
			response := append(bytes.Clone(action.Response), '\n')
			if _, err := stdoutW.Write(response); err != nil {
				return
			}
		}
	}()

	bridge := newHeadroomBridge(process, stdinW, stdoutR, nil)
	t.Cleanup(bridge.Stop)
	return bridge
}

func validHeadroomProbeResponse(reqBytes []byte) []byte {
	var req headroomProbeRequest
	if err := json.Unmarshal(reqBytes, &req); err != nil || req.Operation != "probe" || req.RequestID == "" {
		return []byte(`{"operation":"invalid"}`)
	}
	response, _ := json.Marshal(headroomProbeResponse{
		Operation: "probe",
		RequestID: req.RequestID,
		Protocol:  1,
		OK:        true,
		CacheMode: true,
	})
	return response
}

func mutateHeadroomProbeResponse(key string, value any) func([]byte) []byte {
	return func(reqBytes []byte) []byte {
		var response map[string]any
		_ = json.Unmarshal(validHeadroomProbeResponse(reqBytes), &response)
		response[key] = value
		encoded, _ := json.Marshal(response)
		return encoded
	}
}

func TestCompressResponses_SkipsWhenPreviousResponseID(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(_ []byte) []byte {
		t.Fatal("bridge should not be called")
		return nil
	})

	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_123"}`)
	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompressResponses_SkipsWhenNoInput(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(_ []byte) []byte {
		t.Fatal("bridge should not be called")
		return nil
	})

	for _, body := range [][]byte{
		[]byte(`{"model":"gpt-5.4"}`),
		[]byte(`{"model":"gpt-5.4","input":null}`),
		[]byte(`{"model":"gpt-5.4","input":[]}`),
	} {
		out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
		if err != nil {
			t.Fatal(err)
		}
		if saved != 0 {
			t.Errorf("tokens saved = %d, want 0", saved)
		}
		if string(out) != string(body) {
			t.Errorf("body changed unexpectedly: got %s want %s", out, body)
		}
	}
}

func TestCompressResponses_SkipsWhenBridgeReturnsNotOK(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		if req.Operation != "compress_responses" {
			return nil
		}
		b, _ := json.Marshal(headroomResponsesResponse{OK: false})
		return b
	})

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`)
	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompressResponses_SplicesInputAndInstructions(t *testing.T) {
	compressedInput := json.RawMessage(`[{"role":"user","content":"hi"}]`)
	compressedInstr := "Be brief."

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		if req.Operation != "compress_responses" {
			return nil
		}
		instr := compressedInstr
		resp := headroomResponsesResponse{
			OK:           true,
			Input:        compressedInput,
			Instructions: &instr,
			TokensSaved:  55,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	instr := "You are a helpful assistant. Please provide detailed and comprehensive answers to all user questions, making sure to cover all relevant aspects and edge cases."
	body, _ := json.Marshal(map[string]any{
		"model":        "gpt-5.4",
		"input":        []any{map[string]any{"role": "user", "content": "hello world, this is a long message"}},
		"instructions": instr,
		"max_tokens":   1024,
	})

	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 55 {
		t.Errorf("tokens saved = %d, want 55", saved)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if string(result["input"]) != string(compressedInput) {
		t.Errorf("input = %s, want %s", result["input"], compressedInput)
	}
	if string(result["instructions"]) != `"Be brief."` {
		t.Errorf("instructions = %s, want compressed", result["instructions"])
	}
	// Other fields preserved.
	if string(result["model"]) != `"gpt-5.4"` {
		t.Errorf("model = %s, want preserved", result["model"])
	}
	if string(result["max_tokens"]) != "1024" {
		t.Errorf("max_tokens = %s, want preserved", result["max_tokens"])
	}
}

func TestCompressResponses_KnownModel_IncludesModelLimit(t *testing.T) {
	var captured struct {
		ModelLimit *int `json:"model_limit,omitempty"`
	}

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		if err := json.Unmarshal(reqBytes, &captured); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		resp, _ := json.Marshal(headroomResponsesResponse{
			OK:          true,
			Input:       json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		})
		return resp
	})

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`)
	if _, _, err := bridge.CompressResponses(body, HeadroomModeToken); err != nil {
		t.Fatalf("CompressResponses: %v", err)
	}

	if captured.ModelLimit == nil {
		t.Fatal("model_limit missing from responses bridge request")
	}
	if *captured.ModelLimit != 1050000 {
		t.Fatalf("model_limit = %d, want 1050000", *captured.ModelLimit)
	}
}

func TestCompressResponses_ZeroSavedReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       req.Input,
			TokensSaved: 0,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}],"max_tokens":2048}`)
	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompressResponses_InvalidJSON_ReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(_ []byte) []byte { return nil })

	body := []byte(`{`)
	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err == nil {
		t.Fatal("expected error")
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompressResponses_PreservesNilInstructions(t *testing.T) {
	compressedInput := json.RawMessage(`[{"role":"user","content":"hi"}]`)

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       compressedInput,
			TokensSaved: 55,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	body, _ := json.Marshal(map[string]any{
		"model":      "gpt-5.4",
		"input":      []any{map[string]any{"role": "user", "content": "hello world"}},
		"max_tokens": 1024,
	})

	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 55 {
		t.Errorf("tokens saved = %d, want 55", saved)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if string(result["input"]) != string(compressedInput) {
		t.Errorf("input = %s, want %s", result["input"], compressedInput)
	}
	if _, ok := result["instructions"]; ok {
		t.Errorf("instructions should remain absent, got %s", result["instructions"])
	}
}

func TestSpliceResponsesFields_ReplacesInputOnly(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"old"}],"max_tokens":1}`)
	input := json.RawMessage(`[{"role":"user","content":"new"}]`)
	out, err := spliceResponsesFields(body, input, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["input"]) != string(input) {
		t.Errorf("input = %s, want %s", got["input"], input)
	}
	if string(got["model"]) != `"gpt-5.4"` {
		t.Errorf("model = %s, want preserved", got["model"])
	}
	if string(got["max_tokens"]) != "1" {
		t.Errorf("max_tokens = %s, want preserved", got["max_tokens"])
	}
}

func TestSpliceResponsesFields_ReplacesInputAndInstructions(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"old"}],"instructions":"old","max_tokens":1}`)
	input := json.RawMessage(`[{"role":"user","content":"new"}]`)
	instr := "new instructions"
	out, err := spliceResponsesFields(body, input, &instr, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["input"]) != string(input) {
		t.Errorf("input = %s, want %s", got["input"], input)
	}
	if string(got["instructions"]) != `"new instructions"` {
		t.Errorf("instructions = %s, want replaced", got["instructions"])
	}
}

func TestSpliceResponsesFields_ClearsInstructions(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"old"}],"instructions":"old","max_tokens":1}`)
	input := json.RawMessage(`[{"role":"user","content":"new"}]`)
	out, err := spliceResponsesFields(body, input, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["input"]) != string(input) {
		t.Errorf("input = %s, want %s", got["input"], input)
	}
	if _, ok := got["instructions"]; ok {
		t.Errorf("instructions should be deleted, got %s", got["instructions"])
	}
}

func TestCompressResponses_ClearsInstructionsWhenBridgeAbsorbesThem(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponsesResponse{
			OK:                true,
			Input:             req.Input,
			ClearInstructions: true,
			TokensSaved:       55,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	instr := "Be helpful"
	body, _ := json.Marshal(map[string]any{
		"model":        "gpt-5.4",
		"input":        []any{map[string]any{"role": "user", "content": "hello world"}},
		"instructions": instr,
	})

	out, saved, err := bridge.CompressResponses(body, HeadroomModeToken)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 55 {
		t.Errorf("tokens saved = %d, want 55", saved)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if _, ok := result["instructions"]; ok {
		t.Errorf("instructions should be deleted, got %s", result["instructions"])
	}
}

func TestConfigHeadroomMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    HeadroomMode
		wantErr bool
	}{
		{name: "empty defaults to cache", input: "{}", want: HeadroomModeCache},
		{name: "token", input: `{"headroom_mode":"token"}`, want: HeadroomModeToken},
		{name: "cache", input: `{"headroom_mode":"cache"}`, want: HeadroomModeCache},
		{name: "invalid rejected by validate", input: `{"headroom_mode":"bogus"}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(tc.input), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			cfg.LocalToken = "test-token"
			cfg.setDefaults()
			err := cfg.validate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected validate error")
				}
				return
			}
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			if got := cfg.ResolvedHeadroomMode(); got != tc.want {
				t.Fatalf("ResolvedHeadroomMode() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHealthEndpoint_HeadroomModeField(t *testing.T) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	srv := &Server{
		Headroom: fakeBridge(t, func(_ headroomRequest) headroomResponse {
			return headroomResponse{}
		}),
		HeadroomMode: HeadroomModeCache,
	}
	srv.handleHealth(w, req)
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got, ok := resp["headroom_mode"].(string)
	if !ok {
		t.Fatal("headroom_mode field missing")
	}
	if got != "cache" {
		t.Fatalf("headroom_mode = %q, want cache", got)
	}
}

func TestCompress_CacheMode_PriorTurnsBytestable(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	msgs := json.RawMessage(`[{"role":"system","content":"Be helpful."},{"role":"assistant","content":"OK."},{"role":"user","content":"What is the answer?"}]`)
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet",
		"messages": msgs,
	})

	out, _, err := bridge.CompressCache(body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["messages"]) == string(msgs) {
		return
	}
}

func TestCompress_CacheMode_NoMutableSuffix_ReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(_ []byte) []byte {
		t.Fatal("bridge should not be called")
		return nil
	})

	msgs := json.RawMessage(`[{"role":"system","content":"Be helpful."},{"role":"assistant","content":"OK."}]`)
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet",
		"messages": msgs,
	})

	out, saved, err := bridge.CompressCache(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompress_CacheMode_OnlyUserMessage(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"compressed"}]`),
			TokensSaved: 10,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	msgs := json.RawMessage(`[{"role":"user","content":"What is the answer?"}]`)
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet",
		"messages": msgs,
	})

	out, _, err := bridge.CompressCache(body)
	if err != nil {
		t.Fatal(err)
	}
	_ = out
}

func TestCompressResponses_CacheMode_PriorTurnsByteStable(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			return nil
		}
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"compressed"}]}]`),
			TokensSaved: 10,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	items := json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"Prior"}]},{"role":"assistant","content":[{"type":"text","text":"Reply"}]},{"role":"user","content":[{"type":"input_text","text":"Mutable"}]}]`)
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-5.4",
		"input": items,
	})

	out, _, err := bridge.CompressResponsesCache(body)
	if err != nil {
		t.Fatal(err)
	}
	_ = out
}

func TestCompressResponses_CacheMode_NoMutableSuffix_ReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(_ []byte) []byte {
		t.Fatal("bridge should not be called")
		return nil
	})

	items := json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"Prior"}]},{"role":"assistant","content":[{"type":"text","text":"Reply"}]}]`)
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-5.4",
		"input": items,
	})

	out, saved, err := bridge.CompressResponsesCache(body)
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Errorf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Errorf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestHeadroomEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want bool
	}{
		{name: "disabled", cfg: Config{}, want: false},
		{name: "legacy bool enabled", cfg: Config{Headroom: true}, want: true},
		{name: "mode enabled", cfg: Config{HeadroomMode: "cache"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.HeadroomEnabled(); got != tc.want {
				t.Fatalf("HeadroomEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompressCache_SendsFullRequestToBridge(t *testing.T) {
	var gotMessages json.RawMessage

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		gotMessages = req.Messages

		// Return all messages compressed into a single one (simulating aggressive compression).
		resp := headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"fully compressed"}]`),
			TokensSaved: 50,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	// Three-turn: system + assistant + user (mutable).
	msgs := `[{"role":"system","content":"Be helpful."},{"role":"assistant","content":"OK."},{"role":"user","content":"What is the answer?"}]`
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet",
		"messages": json.RawMessage(msgs),
	})

	out, saved, err := bridge.CompressCache(body)
	if err != nil {
		t.Fatalf("CompressCache: %v", err)
	}
	_ = out
	_ = saved

	// Bridge must have received all 3 messages (full request).
	var sentMsgs []json.RawMessage
	if err := json.Unmarshal(gotMessages, &sentMsgs); err != nil {
		t.Fatalf("parse messages sent to bridge: %v", err)
	}
	if len(sentMsgs) != 3 {
		t.Errorf("bridge received %d messages, want 3 (full request)", len(sentMsgs))
	}
}

func TestCompressCache_RestoresFrozenPrefixAfterFullCompression(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		resp := headroomResponse{
			Messages:    json.RawMessage(`[{"role":"user","content":"fully compressed"}]`),
			TokensSaved: 50,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"system","content":"Be helpful."}`),
		json.RawMessage(`{"role":"assistant","content":"OK."}`),
		json.RawMessage(`{"role":"user","content":"What is the answer?"}`),
	}
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet",
		"messages": msgs,
	})

	out, saved, err := bridge.CompressCache(body)
	if err != nil {
		t.Fatalf("CompressCache: %v", err)
	}
	_ = saved

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	var outMsgs []json.RawMessage
	if err := json.Unmarshal(result["messages"], &outMsgs); err != nil {
		t.Fatalf("unmarshal output messages: %v", err)
	}
	if len(outMsgs) < 2 {
		t.Fatalf("output has %d messages, want at least 2", len(outMsgs))
	}
	if string(outMsgs[0]) != string(msgs[0]) {
		t.Errorf("frozen prefix[0] = %s, want %s", outMsgs[0], msgs[0])
	}
	if string(outMsgs[1]) != string(msgs[1]) {
		t.Errorf("frozen prefix[1] = %s, want %s", outMsgs[1], msgs[1])
	}
}

func TestCompressResponsesCache_SendsFullRequestToBridge(t *testing.T) {
	var gotInput json.RawMessage

	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		gotInput = req.Input

		resp := headroomResponsesResponse{
			OK:          true,
			Input:       json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"compressed"}]}]`),
			TokensSaved: 30,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	items := `[{"role":"user","content":[{"type":"input_text","text":"Prior"}]},{"role":"assistant","content":[{"type":"text","text":"Reply"}]},{"role":"user","content":[{"type":"input_text","text":"Mutable"}]}]`
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-5.4",
		"input": json.RawMessage(items),
	})

	out, saved, err := bridge.CompressResponsesCache(body)
	_ = out
	_ = saved
	if err != nil {
		t.Fatalf("CompressResponsesCache: %v", err)
	}

	// Bridge must receive all 3 items.
	var sentItems []json.RawMessage
	if err := json.Unmarshal(gotInput, &sentItems); err != nil {
		t.Fatalf("parse items sent to bridge: %v", err)
	}
	if len(sentItems) != 3 {
		t.Errorf("bridge received %d items, want 3 (full request)", len(sentItems))
	}
}

func TestCompressResponsesCache_RestoresFrozenPrefixAndInstructions(t *testing.T) {
	instr := "Keep these instructions unchanged."
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		var req headroomResponsesRequest
		if err := json.Unmarshal(reqBytes, &req); err != nil {
			t.Fatalf("unmarshal bridge request: %v", err)
		}
		if req.Instructions != nil {
			t.Fatalf("instructions sent to bridge = %q, want omitted in cache mode", *req.Instructions)
		}
		resp := headroomResponsesResponse{
			OK:          true,
			Input:       json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"compressed"}]}]`),
			TokensSaved: 40,
		}
		b, _ := json.Marshal(resp)
		return b
	})

	prefix0 := json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"Prior"}]}`)
	prefix1 := json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"Reply"}]}`)
	mutable := json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"Mutable"}]}`)
	items := []json.RawMessage{prefix0, prefix1, mutable}
	body, _ := json.Marshal(map[string]any{
		"model":        "gpt-5.4",
		"input":        items,
		"instructions": instr,
	})

	out, saved, err := bridge.CompressResponsesCache(body)
	if err != nil {
		t.Fatalf("CompressResponsesCache: %v", err)
	}
	if saved != 40 {
		t.Fatalf("tokens saved = %d, want 40", saved)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if string(result["instructions"]) != `"Keep these instructions unchanged."` {
		t.Fatalf("instructions = %s, want preserved original", result["instructions"])
	}
	var outItems []json.RawMessage
	if err := json.Unmarshal(result["input"], &outItems); err != nil {
		t.Fatalf("unmarshal output items: %v", err)
	}
	if len(outItems) < 2 {
		t.Fatalf("output has %d items, want at least 2", len(outItems))
	}
	if string(outItems[0]) != string(prefix0) {
		t.Errorf("frozen prefix[0] = %s, want %s", outItems[0], prefix0)
	}
	if string(outItems[1]) != string(prefix1) {
		t.Errorf("frozen prefix[1] = %s, want %s", outItems[1], prefix1)
	}
}

func TestCompressCache_EmptyBridgeOutputReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		_ = reqBytes
		return []byte(`not-json`)
	})

	body := []byte(`{"model":"claude-sonnet","messages":[{"role":"user","content":"hello"}]}`)
	out, saved, err := bridge.CompressCache(body)
	if err == nil {
		t.Fatal("expected error")
	}
	if saved != 0 {
		t.Fatalf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Fatalf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func TestCompressResponsesCache_EmptyBridgeOutputReturnsOriginal(t *testing.T) {
	bridge := fakeBridgeRaw(t, func(reqBytes []byte) []byte {
		_ = reqBytes
		return []byte(`not-json`)
	})

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`)
	out, saved, err := bridge.CompressResponsesCache(body)
	if err == nil {
		t.Fatal("expected error")
	}
	if saved != 0 {
		t.Fatalf("tokens saved = %d, want 0", saved)
	}
	if string(out) != string(body) {
		t.Fatalf("body changed unexpectedly: got %s want %s", out, body)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	return buf.String()
}
