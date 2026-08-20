package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestProxyResilienceIsolatedRehearsal29280(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:29280")
	if err != nil {
		t.Fatalf("isolated rehearsal listener: %v", err)
	}
	defer listener.Close()

	root := filepath.Join(t.TempDir(), "resilience")
	options := ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: root, Random: rand.Reader, Now: time.Now}
	if err := InitialiseProxyResilienceState(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	state, err := OpenProxyResilienceState(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	admissions, err := OpenNormalCallerAdmissionStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "admissions"))
	if err != nil {
		t.Fatal(err)
	}
	defer admissions.Close()

	events := []string{}
	first := &runtimeTestWorker{holder: runtimeHolder("rehearsal-worker-1"), events: &events, response: RuntimeHTTPResponseV1{StatusCode: http.StatusAccepted, Body: []byte("normal-1")}}
	second := &runtimeTestWorker{holder: runtimeHolder("rehearsal-worker-2"), events: &events, response: RuntimeHTTPResponseV1{StatusCode: http.StatusAccepted, Body: []byte("normal-2")}}
	supervisor, err := NewRuntimeSupervisor(listener, runtimeHolder("rehearsal-supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{first, second}}, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	manifest := WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "synthetic-artifact"}
	if _, err := supervisor.Boot(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	callerKey := make([]byte, sha256.Size)
	if _, err := rand.Read(callerKey); err != nil {
		t.Fatal(err)
	}
	authority, err := NewNormalCallerAuthority(callerKey, 1, []NormalCallerCredentialV1{{Domain: NormalCallerLocal, Bearer: "synthetic-local-token", SubjectID: "synthetic-local"}}, admissions, time.Now, rand.Reader)
	zeroRuntimeBytes(callerKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAuthority(authority); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerClassifier(NewNormalCallerBranchClassifier(nil)); err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	upstreamCalls := 0
	relay := &RescueRelay{
		Transport: rescueDoerFunc(func(request *http.Request) (*http.Response, error) {
			upstreamCalls++
			if request.URL.String() != "https://chatgpt.com/backend-api/codex/models?client_version=0.147.0" || request.Header.Get("Authorization") != "Bearer synthetic-candidate-token" {
				t.Fatalf("synthetic upstream request = %s auth=%q", request.URL, request.Header.Get("Authorization"))
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"synthetic"}]}`))}, nil
		}),
		Origin: origin, LoopbackHost: "127.0.0.1:29280", ForwardingAcknowledged: true,
		DenyBearer: supervisor.DeniesNormalBearer, Budget: NewRescueBudget(time.Now, state.RescueFairnessKey()),
		Admission: func(*http.Request) RescueIngressKind { return RescueIngressUnverified },
	}
	if err := supervisor.ConfigureRescue(context.Background(), relay, state.RuntimeMode); err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: supervisor, ReadHeaderTimeout: 2 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-serveDone
	}()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}

	request := func(method, path, bearer string, body []byte, headers http.Header) (int, string) {
		t.Helper()
		var reader io.Reader = http.NoBody
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, "http://127.0.0.1:29280"+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		for name, values := range headers {
			for _, value := range values {
				req.Header.Add(name, value)
			}
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		responseBody, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(responseBody)
	}

	if status, body := request(http.MethodPost, "/normal", "synthetic-local-token", []byte(`{"model":"gpt-5"}`), nil); status != http.StatusAccepted || body != "normal-1" {
		t.Fatalf("normal-1 = %d %q", status, body)
	}
	if status, body := request(http.MethodPost, RuntimeRescueEnterPath, "synthetic-local-token", nil, nil); status != http.StatusOK {
		t.Fatalf("enter = %d %q", status, body)
	}
	rescueHeaders := http.Header{"User-Agent": {"codex/0.147.0 (darwin 25.0; arm64) Terminal"}, "Originator": {"codex"}, "Version": {"0.147.0"}}
	if status, body := request(http.MethodGet, "/models?client_version=0.147.0", "synthetic-candidate-token", nil, rescueHeaders); status != http.StatusOK || !strings.Contains(body, "synthetic") {
		t.Fatalf("rescue = %d %q", status, body)
	}
	if status, _ := request(http.MethodPost, RuntimeRescueExitPath, "synthetic-local-token", nil, nil); status != http.StatusOK {
		t.Fatalf("exit = %d", status)
	}
	if status, body := request(http.MethodPost, "/normal", "synthetic-local-token", []byte(`{"model":"gpt-5"}`), nil); status != http.StatusAccepted || body != "normal-2" {
		t.Fatalf("normal-2 = %d %q", status, body)
	}
	if upstreamCalls != 1 || supervisor.TrafficMode() != TrafficModeNormal || !supervisor.AdmissionReady() {
		t.Fatalf("upstream=%d mode=%q ready=%v", upstreamCalls, supervisor.TrafficMode(), supervisor.AdmissionReady())
	}
	record, found, err := state.RuntimeMode.Load(context.Background())
	if err != nil || !found || record.EffectiveMode != TrafficModeNormal || record.Phase != RuntimeModePhaseEffective {
		t.Fatalf("durable mode = %#v found=%v err=%v", record, found, err)
	}
}
