package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newV2OperationState(t *testing.T, phase string) (*fsutil.MemFS, string) {
	t.Helper()
	fs := fsutil.NewMemFS()
	root := "/synthetic-resilience"
	dirPath := root + "/operator-control"
	if err := fs.MkdirAll(dirPath, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{42}, 32)
	if err := fs.WriteFile(dirPath+"/key", key, 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := fs.OpenSecureDirectory(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	lock, err := proxy.AcquireSelectorCASLock(fs, dir, "mutation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	store, err := proxy.OpenOperationCoordinatorStore(context.Background(), fs, dir, proxy.NewAuthorityObjectPublisher(fs, rand.Reader, lock), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, digest := strings.Repeat("a", 32), strings.Repeat("b", 64)
	failure := sha256.Sum256([]byte(`{"outcome":"failed","reason":"synthetic refusal"}`))
	failureDigest := hex.EncodeToString(failure[:])
	if phase != "idle" {
		if err := store.PublishIntent(id, digest); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "anchor" || phase == "receipt" || phase == "terminal" {
		if err := store.PublishAnchor(id, digest); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "receipt" || phase == "terminal" {
		if err := store.PublishReceipt(id, failureDigest); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "terminal" {
		if err := store.PublishTerminal(id, failureDigest); err != nil {
			t.Fatal(err)
		}
	}
	return fs, root
}
func init() {
	for scenario, phase := range map[string]string{"operation-pending": "intent", "operation-failed-receipt": "terminal"} {
		registerV2Fixture(scenario, func(t *testing.T) *v2Fixture {
			fs, root := newV2OperationState(t, phase)
			before := v2RescueInventory(t, fs, root)
			t.Cleanup(func() {
				if after := v2RescueInventory(t, fs, root); !reflect.DeepEqual(before, after) {
					t.Error("operation fixture modified state")
				}
			})
			f := &v2Fixture{In: noAccessV2Input{}}
			f.Lookup = v2RescueLookup(v2RescueDependencies{Inspect: func(ctx context.Context, id string) (proxy.OperationCoordinatorInspectionV1, error) {
				f.Call("filesystem-read")
				return proxy.InspectOperationCoordinatorState(ctx, fs, root, id)
			}})
			return f
		})
	}
	registerV2Fixture("rescue-timeout", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{In: noAccessV2Input{}, Secrets: []string{"fixture-token"}}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer fixture-token" {
				http.Error(w, "denied", 401)
				return
			}
			f.Call("accepted-transition")
			<-r.Context().Done()
		}))
		t.Cleanup(server.Close)
		port := server.Listener.Addr().(*net.TCPAddr).Port
		f.Lookup = v2RescueLookup(v2RescueDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "fixture-token"}, nil }, Doer: server.Client()})
		return f
	})
}
func TestCLIV2RescueContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "pending operation is successful inspection", Scenario: "operation-pending", Args: []string{"proxy", "operation", "status", "--json"}, Exit: 0, Command: "proxy operation status", WantJSON: `{"state":"pending","result_available":false,"recovery_supported":false}`, Forbid: []string{"filesystem-write", "service", "consume"}})
	runV2Case(t, v2Case{Name: "failed action is an inspectable result", Scenario: "operation-failed-receipt", Args: []string{"proxy", "operation", "status", "--json"}, Command: "proxy operation status", WantJSON: `{"state":"result_available","result_available":true,"recovery_supported":false}`, Forbid: []string{"filesystem-write", "service", "consume"}})
	runV2Case(t, v2Case{Name: "accepted transition is never replayed", Scenario: "rescue-timeout", Args: []string{"proxy", "rescue", "enter", "--timeout", "1s", "--json"}, Exit: 7, Code: "rescue_timeout", Command: "proxy rescue enter", Calls: map[string]int{"accepted-transition": 1}, Forbid: []string{"service", "consume"}})

}

func v2RescueLookup(deps v2RescueDependencies) cli.Lookup {
	return func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Rescue(path)
		return func(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
			return handleV2RescueWithPreparation(ctx, inv, session, func(context.Context, cli.Invocation) (v2RescueDependencies, error) { return deps, nil })
		}, ok
	}
}
func runV2Rescue(t *testing.T, ctx context.Context, args []string, lookup cli.Lookup) (int, string, string) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	exit := cli.Run(ctx, args, &cli.Session{In: noAccessV2Input{}, Out: &out, Err: &diagnostic}, lookup)
	return exit, out.String(), diagnostic.String()
}
func v2RescueReply(body string) v2RescueDependencies {
	return v2RescueDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{LocalToken: "fixture-token", Port: 29280}, nil }, Doer: testDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
}
func v2RescueBody(mode string) string {
	return `{"mode":"` + mode + `","generation":18446744073709551615,"active_rescue_requests":0}`
}
func TestCLIV2RescueModesResourcesAndHuman(t *testing.T) {
	for _, action := range []string{"enter", "exit", "status"} {
		for _, mode := range []string{"normal", "drain", "rescue_draining", "rescue", "rescue_exit_draining"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				deps := v2RescueReply(v2RescueBody(mode))
				exit, out, diagnostic := runV2Rescue(t, context.Background(), []string{"proxy", "rescue", action, "--json"}, v2RescueLookup(deps))
				want := `{"mode":"` + mode + `","generation":18446744073709551615,"active_rescue_requests":0,"draining_sessions":[]}`
				if exit != 0 || diagnostic != "" {
					t.Fatalf("exit=%d stderr=%s", exit, diagnostic)
				}
				if err := validateV2Envelope([]byte(out), v2Case{Command: "proxy rescue " + action, WantJSON: want}); err != nil {
					t.Fatal(err)
				}
				var envelope struct{ Data map[string]json.RawMessage }
				if json.Unmarshal([]byte(out), &envelope) != nil || len(envelope.Data) != 4 {
					t.Fatalf("resource shape=%s", out)
				}
				exit, out, diagnostic = runV2Rescue(t, context.Background(), []string{"proxy", "rescue", action}, v2RescueLookup(deps))
				expected := fmt.Sprintf("Proxy mode: %s\nGeneration: 18446744073709551615\nActive rescue requests: 0\nDraining sessions: 0\n", mode)
				if exit != 0 || out != expected || diagnostic != "" {
					t.Fatalf("human exit=%d out=%q stderr=%q", exit, out, diagnostic)
				}
			})
		}
	}
	t.Run("sort session hints", func(t *testing.T) {
		body := `{"mode":"rescue_exit_draining","generation":4,"active_rescue_requests":3,"draining_sessions":["codex-window:bbbbbbbbbbbb","claude-session:aaaaaaaaaaaa"]}`
		exit, out, _ := runV2Rescue(t, context.Background(), []string{"proxy", "rescue", "status", "-j"}, v2RescueLookup(v2RescueReply(body)))
		if exit != 0 || !strings.Contains(out, `"draining_sessions":["claude-session:aaaaaaaaaaaa","codex-window:bbbbbbbbbbbb"]`) {
			t.Fatalf("exit=%d out=%s", exit, out)
		}
	})
}
func TestCLIV2RescueAuthenticatedPortsAndMethods(t *testing.T) {
	for _, action := range []string{"enter", "exit", "status"} {
		for _, cfgPort := range []int{0, 12345} {
			for _, override := range []int{0, 23456} {
				t.Run(fmt.Sprint(action, cfgPort, override), func(t *testing.T) {
					calls := 0
					deps := v2RescueReply(v2RescueBody("normal"))
					deps.LoadConfig = func() (*proxy.Config, error) { return &proxy.Config{Port: cfgPort, LocalToken: "fixture-token"}, nil }
					deps.Doer = testDoer(func(r *http.Request) (*http.Response, error) {
						calls++
						port := cfgPort
						if port == 0 {
							port = proxy.DefaultPort
						}
						if override != 0 {
							port = override
						}
						method := http.MethodPost
						if action == "status" {
							method = http.MethodGet
						}
						if r.Method != method || r.URL.String() != fmt.Sprintf("http://127.0.0.1:%d/_cq/control/rescue/%s", port, action) || r.Header.Get("Authorization") != "Bearer fixture-token" || r.Body != http.NoBody {
							t.Fatalf("unexpected control request %s %s", r.Method, r.URL)
						}
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(v2RescueBody("normal")))}, nil
					})
					args := []string{"proxy", "rescue", action, "-j"}
					if override != 0 {
						args = append(args, "--port", strconv.Itoa(override))
					}
					exit, out, _ := runV2Rescue(t, context.Background(), args, v2RescueLookup(deps))
					if exit != 0 || calls != 1 {
						t.Fatalf("exit=%d calls=%d out=%s", exit, calls, out)
					}
				})
			}
		}
	}
}
func TestCLIV2RescueInvalidResponses(t *testing.T) {
	good := v2RescueBody("normal")
	cases := map[string]string{"empty": "", "truncated": "{", "array": "[]", "null": "null", "trailing": good + "{}", "missing": "{}", "unknown": strings.Replace(good, `"mode"`, `"extra":1,"mode"`, 1), "duplicate": strings.Replace(good, `"mode"`, `"mode":"rescue","mode"`, 1), "invalid mode": v2RescueBody("unexpected"), "utf8": strings.Replace(good, "normal", string([]byte{255}), 1), "negative": strings.Replace(good, `"active_rescue_requests":0`, `"active_rescue_requests":-1`, 1), "float": strings.Replace(good, `"active_rescue_requests":0`, `"active_rescue_requests":1.2`, 1), "overflow": strings.Replace(good, "18446744073709551615", "18446744073709551616", 1), "null mode": strings.Replace(good, `"normal"`, "null", 1), "null count": strings.Replace(good, `"active_rescue_requests":0`, `"active_rescue_requests":null`, 1), "null generation": strings.Replace(good, "18446744073709551615", "null", 1), "null sessions": strings.TrimSuffix(good, "}") + `,"draining_sessions":null}`, "null hint": strings.TrimSuffix(good, "}") + `,"draining_sessions":[null]}`, "wrong sessions": strings.TrimSuffix(good, "}") + `,"draining_sessions":4}`, "oversize": good + strings.Repeat(" ", proxyRescueResponseMaxBytes-len(good)+1)}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			exit, out, _ := runV2Rescue(t, context.Background(), []string{"proxy", "rescue", "enter", "-j"}, v2RescueLookup(v2RescueReply(body)))
			if exit != 1 {
				t.Fatalf("exit=%d out=%s", exit, out)
			}
			if err := validateV2Envelope([]byte(out), v2Case{Command: "proxy rescue enter", Exit: 1, Code: "rescue_response_invalid", WantJSON: `null`}); err != nil {
				t.Fatal(err)
			}
		})
	}
	exact := good + strings.Repeat(" ", proxyRescueResponseMaxBytes-len(good))
	if exit, out, _ := runV2Rescue(t, context.Background(), []string{"proxy", "rescue", "status", "-j"}, v2RescueLookup(v2RescueReply(exact))); exit != 0 {
		t.Fatalf("exact64KiB exit=%d out=%s", exit, out)
	}
}
func TestCLIV2RescueOperationalErrors(t *testing.T) {
	for _, status := range []int{301, 400, 401, 403, 404, 409, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			body := &v2HookUnreadBody{}
			deps := v2RescueReply("")
			deps.Doer = testDoer(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: body}, nil
			})
			exit, out, _ := runV2Rescue(t, context.Background(), []string{"proxy", "rescue", "exit", "--json"}, v2RescueLookup(deps))
			want, code := 4, "rescue_unavailable"
			if status == 401 || status == 403 {
				want, code = 5, "rescue_auth_failed"
			}
			if status == 409 {
				want, code = 6, "rescue_transition_conflict"
			}
			if exit != want || !body.closed {
				t.Fatalf("exit=%d want=%d closed=%t out=%s", exit, want, body.closed, out)
			}
			if err := validateV2Envelope([]byte(out), v2Case{Command: "proxy rescue exit", Exit: want, Code: code}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, name := range []string{"missing config", "missing token", "typed token", "transport", "nil response", "nil body", "read error"} {
		t.Run(name, func(t *testing.T) {
			deps := v2RescueReply("")
			want, code := 4, "rescue_unavailable"
			switch name {
			case "missing config":
				deps.LoadConfig = func() (*proxy.Config, error) { return nil, os.ErrNotExist }
			case "missing token":
				deps.LoadConfig = func() (*proxy.Config, error) { return &proxy.Config{}, nil }
				want, code = 5, "rescue_auth_failed"
			case "typed token":
				deps.LoadConfig = func() (*proxy.Config, error) { return nil, fmt.Errorf("wrapped: %w", proxy.ErrLocalTokenRequired) }
				want, code = 5, "rescue_auth_failed"
			case "transport":
				deps.Doer = testDoer(func(*http.Request) (*http.Response, error) { return nil, errors.New("private transport text") })
			case "nil response":
				deps.Doer = testDoer(func(*http.Request) (*http.Response, error) { return nil, nil })
			case "nil body":
				deps.Doer = testDoer(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 200}, nil })
				want, code = 1, "rescue_response_invalid"
			case "read error":
				deps.Doer = testDoer(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(v2RescueBadReader{})}, nil
				})
				want, code = 1, "rescue_response_invalid"
			}
			exit, out, _ := runV2Rescue(t, context.Background(), []string{"proxy", "rescue", "status", "-j"}, v2RescueLookup(deps))
			if exit != want {
				t.Fatalf("exit=%d out=%s", exit, out)
			}
			if err := validateV2Envelope([]byte(out), v2Case{Command: "proxy rescue status", Exit: want, Code: code}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "private") {
				t.Fatal("unsafe error leaked")
			}
		})
	}
}

type v2RescueBadReader struct{}

func (v2RescueBadReader) Read([]byte) (int, error) { return 0, errors.New("private read failure") }

func TestCLIV2RescueProductionPreparationBudget(t *testing.T) {
	original := prepareV2RescueDependencies
	t.Cleanup(func() { prepareV2RescueDependencies = original })
	for _, timeout := range []string{"", "1s", "5m"} {
		t.Run("deadline"+timeout, func(t *testing.T) {
			var preparedDeadline time.Time
			deps := v2RescueReply(v2RescueBody("normal"))
			prepareV2RescueDependencies = func(ctx context.Context, _ cli.Invocation) (v2RescueDependencies, error) {
				var ok bool
				preparedDeadline, ok = ctx.Deadline()
				if !ok {
					t.Fatal("preparation has no deadline")
				}
				want := 30 * time.Second
				if timeout != "" {
					want, _ = time.ParseDuration(timeout)
				}
				if remaining := time.Until(preparedDeadline); remaining > want || remaining < want-time.Second {
					t.Fatalf("remaining=%s want=%s", remaining, want)
				}
				deps.Doer = testDoer(func(req *http.Request) (*http.Response, error) {
					got, _ := req.Context().Deadline()
					if !got.Equal(preparedDeadline) {
						t.Fatal("deadline reset after preparation")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(v2RescueBody("normal")))}, nil
				})
				return deps, nil
			}
			args := []string{"proxy", "rescue", "status", "-j"}
			if timeout != "" {
				args = append(args, "--timeout", timeout)
			}
			if exit, out, _ := runV2Rescue(t, context.Background(), args, lookupV2Rescue); exit != 0 {
				t.Fatalf("exit=%d out=%s", exit, out)
			}
		})
	}
	for _, phase := range []string{"preparation", "load", "request", "cleanup"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			deps := v2RescueReply(v2RescueBody("normal"))
			prepareV2RescueDependencies = func(work context.Context, _ cli.Invocation) (v2RescueDependencies, error) {
				if phase == "preparation" {
					<-work.Done()
					return v2RescueDependencies{}, errors.New("late preparation error")
				}
				if phase == "load" {
					deps.LoadConfig = func() (*proxy.Config, error) { <-work.Done(); return nil, errors.New("late load error") }
				}
				if phase == "request" {
					deps.Doer = testDoer(func(req *http.Request) (*http.Response, error) {
						<-req.Context().Done()
						return nil, errors.New("late transport error")
					})
				}
				if phase == "cleanup" {
					deps.Doer = testDoer(func(*http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: 200, Body: &v2RescueCloseBody{Reader: strings.NewReader(v2RescueBody("normal")), close: func() { <-work.Done() }}}, nil
					})
				}
				return deps, nil
			}
			exit, out, _ := runV2Rescue(t, ctx, []string{"proxy", "rescue", "enter", "-j"}, lookupV2Rescue)
			if exit != 7 || !strings.Contains(out, `"code":"rescue_timeout"`) {
				t.Fatalf("exit=%d out=%s", exit, out)
			}
		})
	}
	t.Run("cancelled before preparation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		prepareV2RescueDependencies = func(context.Context, cli.Invocation) (v2RescueDependencies, error) {
			t.Fatal("cancelled preparation called")
			return v2RescueDependencies{}, nil
		}
		if exit, _, _ := runV2Rescue(t, ctx, []string{"proxy", "rescue", "enter", "-j"}, lookupV2Rescue); exit != 130 {
			t.Fatalf("exit=%d", exit)
		}
	})
}

type v2RescueCloseBody struct {
	io.Reader
	close func()
}

func (b *v2RescueCloseBody) Close() error { b.close(); return nil }

func TestCLIV2RescueLostResponseNeverReplays(t *testing.T) {
	for _, action := range []string{"enter", "exit"} {
		t.Run(action, func(t *testing.T) {
			requests := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.Method + " " + r.URL.Path
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					http.Error(w, "denied", 401)
					return
				}
				// Accept the synthetic transition then lose the response until cancellation.
				<-r.Context().Done()
			}))
			defer server.Close()
			port := server.Listener.Addr().(*net.TCPAddr).Port
			deps := v2RescueDependencies{LoadConfig: func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "fixture-token"}, nil }, Doer: server.Client()}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancel()
			exit, out, _ := runV2Rescue(t, ctx, []string{"proxy", "rescue", action, "-j"}, v2RescueLookup(deps))
			if exit != 7 || !strings.Contains(out, "rescue_timeout") || len(requests) != 1 {
				t.Fatalf("exit=%d requests=%d out=%s", exit, len(requests), out)
			}
			if got := <-requests; got != "POST /_cq/control/rescue/"+action {
				t.Fatalf("unexpected request=%s", got)
			}
		})
	}
}

func v2RescueInventory(t *testing.T, fs fsutil.FileSystem, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	var walk func(string)
	walk = func(path string) {
		info, err := fs.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		value := fmt.Sprint(info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.IsDir() {
			entries, err := fs.ReadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				walk(filepath.Join(path, entry.Name()))
			}
		} else {
			body, err := fs.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			value += " " + hex.EncodeToString(sum[:])
		}
		result[path] = value
	}
	walk(root)
	return result
}
func TestCLIV2RescueOperationStatesAndReadOnly(t *testing.T) {
	for _, phase := range []string{"idle", "intent", "anchor", "receipt", "terminal"} {
		t.Run(phase, func(t *testing.T) {
			fs, root := newV2OperationState(t, phase)
			before := v2RescueInventory(t, fs, root)
			deps := v2RescueDependencies{Inspect: func(ctx context.Context, id string) (proxy.OperationCoordinatorInspectionV1, error) {
				return proxy.InspectOperationCoordinatorState(ctx, fs, root, id)
			}}
			for _, explicit := range []bool{false, true} {
				args := []string{"proxy", "operation", "status"}
				if explicit {
					args = append(args, strings.Repeat("a", 32))
				}
				args = append(args, "-j")
				exit, out, _ := runV2Rescue(t, context.Background(), args, v2RescueLookup(deps))
				if phase == "idle" && explicit {
					if exit != 3 || !strings.Contains(out, "operation_not_found") {
						t.Fatalf("missing=%d %s", exit, out)
					}
					continue
				}
				var envelope struct{ Data v2OperationStatus }
				if exit != 0 || json.Unmarshal([]byte(out), &envelope) != nil {
					t.Fatalf("exit=%d out=%s", exit, out)
				}
				data := envelope.Data
				wantPhase := phase
				if phase == "receipt" {
					wantPhase = "anchor"
				}
				wantState := "pending"
				if phase == "idle" {
					wantState = "idle"
				}
				if phase == "receipt" || phase == "terminal" {
					wantState = "result_available"
				}
				if data.State != wantState || data.ResultAvailable != (phase == "receipt" || phase == "terminal") || data.RecoverySupported {
					t.Fatalf("data=%+v", data)
				}
				if phase == "idle" && (data.OperationID != nil || data.Phase != nil || data.ResultDigest != nil) {
					t.Fatalf("idle nullability=%s", out)
				}
				if phase != "idle" && (data.OperationID == nil || *data.OperationID != strings.Repeat("a", 32) || data.Phase == nil || *data.Phase != wantPhase || data.ResultDigest == nil) {
					t.Fatalf("selected fields=%s", out)
				}
				var exact struct{ Data map[string]json.RawMessage }
				_ = json.Unmarshal([]byte(out), &exact)
				if len(exact.Data) != 6 {
					t.Fatalf("extra or missing fields=%s", out)
				}
			}
			exit, out, _ := runV2Rescue(t, context.Background(), []string{"proxy", "operation", "status"}, v2RescueLookup(deps))
			id, humanPhase, state, available := "idle", "—", "idle", false
			if phase != "idle" {
				id, humanPhase, state = strings.Repeat("a", 32), phase, "pending"
			}
			if phase == "receipt" {
				humanPhase = "anchor"
			}
			if phase == "receipt" || phase == "terminal" {
				state, available = "result_available", true
			}
			want := fmt.Sprintf("Operation: %s\nState: %s\nPhase: %s\nResult available: %t\nRecovery supported: false\n", id, state, humanPhase, available)
			if exit != 0 || out != want {
				t.Fatalf("human=%d %q want=%q", exit, out, want)
			}
			exit, out, diagnostic := runV2Rescue(t, context.Background(), []string{"operation", "status", "-j"}, v2RescueLookup(deps))
			if exit != 0 || !strings.Contains(diagnostic, "Deprecated") || !strings.Contains(out, `"command":"proxy operation status"`) {
				t.Fatalf("alias=%d %s %s", exit, out, diagnostic)
			}
			exit, out, _ = runV2Rescue(t, context.Background(), []string{"proxy", "operation", "status", strings.Repeat("f", 32), "-j"}, v2RescueLookup(deps))
			if exit != 3 || !strings.Contains(out, "operation_not_found") {
				t.Fatalf("missing=%d %s", exit, out)
			}
			if after := v2RescueInventory(t, fs, root); !reflect.DeepEqual(before, after) {
				t.Fatal("inspection modified retained inventory/bytes/metadata")
			}
		})
	}
}
func TestCLIV2RescueFailedReceiptAndRetiredRecovery(t *testing.T) {
	fs, root := newV2OperationState(t, "terminal")
	before := v2RescueInventory(t, fs, root)
	deps := v2RescueDependencies{Inspect: func(ctx context.Context, id string) (proxy.OperationCoordinatorInspectionV1, error) {
		return proxy.InspectOperationCoordinatorState(ctx, fs, root, id)
	}}
	exit, out, _ := runV2Rescue(t, context.Background(), []string{"proxy", "operation", "status", "-j"}, v2RescueLookup(deps))
	if exit != 0 || !strings.Contains(out, `"state":"result_available"`) || strings.Contains(out, "succeeded") {
		t.Fatalf("failed-action receipt mislabelled: %d %s", exit, out)
	}
	id := strings.Repeat("a", 32)
	exit, out, _ = runV2Rescue(t, context.Background(), []string{"operation", "recover", "--operation-id", id, "-j"}, func(string) (cli.Handler, bool) { t.Fatal("retired recover accessed dependencies"); return nil, false })
	want := "Active operation recovery is unavailable; use cq proxy operation status OPERATION_ID to inspect retained state."
	if exit != 4 || !strings.Contains(out, `"code":"operation_recovery_unavailable"`) || !strings.Contains(out, want) {
		t.Fatalf("recover=%d %s", exit, out)
	}
	if after := v2RescueInventory(t, fs, root); !reflect.DeepEqual(before, after) {
		t.Fatal("retired recovery mutated state")
	}
}
func TestCLIV2RescueOperationUnavailable(t *testing.T) {
	for _, kind := range []string{"corrupt key", "corrupt anchor", "unreadable", "invalid result"} {
		t.Run(kind, func(t *testing.T) {
			fs, root := newV2OperationState(t, "intent")
			switch kind {
			case "corrupt key":
				_ = fs.WriteFile(root+"/operator-control/key", []byte("bad"), 0600)
			case "corrupt anchor":
				_ = fs.WriteFile(root+"/operator-control/anchor", []byte("{}"), 0600)
			}
			before := v2RescueInventory(t, fs, root)
			deps := v2RescueDependencies{Inspect: func(ctx context.Context, id string) (proxy.OperationCoordinatorInspectionV1, error) {
				if kind == "unreadable" {
					return proxy.OperationCoordinatorInspectionV1{}, os.ErrPermission
				}
				if kind == "invalid result" {
					return proxy.OperationCoordinatorInspectionV1{Found: true, OperationID: "private\nunsafe", Phase: "executing", ValueDigest: "bad"}, nil
				}
				return proxy.InspectOperationCoordinatorState(ctx, fs, root, id)
			}}
			exit, out, _ := runV2Rescue(t, context.Background(), []string{"proxy", "operation", "status", "-j"}, v2RescueLookup(deps))
			if exit != 4 || !strings.Contains(out, "operation_state_unavailable") || strings.Contains(out, "private") {
				t.Fatalf("exit=%d out=%s", exit, out)
			}
			if after := v2RescueInventory(t, fs, root); !reflect.DeepEqual(before, after) {
				t.Fatal("failure repaired or cleaned state")
			}
		})
	}
}
func TestCLIV2RescueParserAndHelpNoAccess(t *testing.T) {
	lookup := func(string) (cli.Handler, bool) { t.Fatal("pre-state case accessed dependencies"); return nil, false }
	for _, path := range []string{"proxy operation", "proxy operation status", "proxy rescue", "proxy rescue enter", "proxy rescue exit", "proxy rescue status"} {
		t.Run(path, func(t *testing.T) {
			args := strings.Fields(path)
			if path != "proxy operation" && path != "proxy rescue" {
				args = append(args, "--help")
			}
			args = append(args, "--json")
			exit, out, diagnostic := runV2Rescue(t, context.Background(), args, lookup)
			expected, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "help", strings.ReplaceAll(path, " ", "-")+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			if exit != 0 || out != string(expected) || diagnostic != "" {
				t.Fatalf("exact help mismatch: exit=%d out=%q stderr=%q", exit, out, diagnostic)
			}
		})
	}
	for _, path := range []string{"proxy operation status", "proxy rescue enter", "proxy rescue exit", "proxy rescue status"} {
		extras := [][]string{{"--unknown"}, {"--json", "--json"}, {"extra", "extra"}, {"--timeout", "0"}, {"--port", "0"}}
		if strings.Contains(path, "rescue") {
			extras = append(extras, []string{"--port", "65536"}, []string{"--port", "1", "--port", "2"}, []string{"--timeout", "999ms"}, []string{"--timeout", "301s"}, []string{"--timeout", "30s", "--timeout", "30s"}, []string{"unexpected"})
		} else {
			extras = append(extras, []string{strings.Repeat("A", 32)}, []string{strings.Repeat("a", 31)})
		}
		for _, extra := range extras {
			t.Run(path+fmt.Sprint(extra), func(t *testing.T) {
				args := append(strings.Fields(path), extra...)
				exit, _, _ := runV2Rescue(t, context.Background(), args, lookup)
				if exit != 2 {
					t.Fatalf("args=%v exit=%d", args, exit)
				}
			})
		}
	}
	for _, args := range [][]string{{"operation", "recover"}, {"operation", "recover", "--operation-id", "invalid"}} {
		if exit, _, _ := runV2Rescue(t, context.Background(), args, lookup); exit != 2 {
			t.Fatalf("retired malformed=%v exit=%d", args, exit)
		}
	}
	for _, path := range []string{"proxy operation recover", "proxy status", "codex proxy rescue enter", "operation status"} {
		if _, ok := lookupV2Rescue(path); ok {
			t.Fatalf("family claimed %s", path)
		}
	}
}

func TestCLIV2RescueOperationCapturedConfig(t *testing.T) {
	for _, scenario := range []string{"no config", "no state binding", "absent state", "pending", "failed receipt", "bad config", "corrupt state"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			paths := proxy.DefaultPaths{ConfigFile: filepath.Join(root, "proxy.json")}
			stateRoot := filepath.Join(root, "authority")
			if scenario != "no config" {
				cfg := proxy.Config{LocalToken: "synthetic-local-token", ProxyResilienceStateDir: stateRoot}
				if scenario == "no state binding" {
					cfg.ProxyResilienceStateDir = ""
				}
				body, _ := json.Marshal(cfg)
				if scenario == "bad config" {
					body = []byte("{")
				}
				if err := os.WriteFile(paths.ConfigFile, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "pending" || scenario == "failed receipt" || scenario == "corrupt state" {
				phase := "intent"
				if scenario == "failed receipt" {
					phase = "terminal"
				}
				fs, memRoot := newV2OperationState(t, phase)
				dir := filepath.Join(stateRoot, "operator-control")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				entries, _ := fs.ReadDir(memRoot + "/operator-control")
				for _, entry := range entries {
					body, err := fs.ReadFile(memRoot + "/operator-control/" + entry.Name())
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(filepath.Join(dir, entry.Name()), body, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "corrupt state" {
					if err := os.WriteFile(filepath.Join(dir, "key"), []byte("bad"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := v2RescueInventory(t, fsutil.OSFileSystem{}, root)
			deps := v2RescueDependencies{Inspect: func(ctx context.Context, id string) (proxy.OperationCoordinatorInspectionV1, error) {
				return inspectOperatorOperationAt(ctx, id, paths)
			}}
			exit, out, _ := runV2Rescue(t, context.Background(), []string{"proxy", "operation", "status", "--json"}, v2RescueLookup(deps))
			want := 0
			state := "idle"
			if scenario == "pending" {
				state = "pending"
			}
			if scenario == "failed receipt" {
				state = "result_available"
			}
			if scenario == "bad config" || scenario == "corrupt state" {
				want = 4
			}
			if exit != want || (want == 0 && !strings.Contains(out, `"state":"`+state+`"`)) {
				t.Fatalf("exit=%d want=%d out=%s", exit, want, out)
			}
			if after := v2RescueInventory(t, fsutil.OSFileSystem{}, root); !reflect.DeepEqual(before, after) {
				t.Fatal("production inspection changed files")
			}
		})
	}
}

func TestCLIV2RescueProductionTransportAndRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			followed := make(chan struct{}, 1)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed <- struct{}{}; w.WriteHeader(200) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Method != "GET" || r.URL.Path != proxy.RuntimeRescueStatusPath {
					t.Error("production request changed authentication or endpoint")
				}
				if redirect {
					http.Redirect(w, r, target.URL, 302)
					return
				}
				_, _ = io.WriteString(w, v2RescueBody("normal"))
			}))
			defer server.Close()
			deps, err := prepareV2RescueDependencies(context.Background(), cli.Invocation{Path: "proxy rescue status"})
			if err != nil {
				t.Fatal(err)
			}
			client, ok := deps.Doer.(*http.Client)
			if !ok {
				t.Fatal("missing production client")
			}
			transport, ok := client.Transport.(*http.Transport)
			if !ok || transport.Proxy != nil || !transport.DisableKeepAlives {
				t.Fatal("production loopback transport uses proxy or pooled retries")
			}
			port := server.Listener.Addr().(*net.TCPAddr).Port
			deps.LoadConfig = func() (*proxy.Config, error) { return &proxy.Config{Port: port, LocalToken: "fixture-token"}, nil }
			exit, out, _ := runV2Rescue(t, context.Background(), []string{"proxy", "rescue", "status", "--json"}, v2RescueLookup(deps))
			want := 0
			if redirect {
				want = 4
			}
			if exit != want || len(followed) != 0 {
				t.Fatalf("exit=%d want=%d redirected=%d out=%s", exit, want, len(followed), out)
			}
		})
	}
}
