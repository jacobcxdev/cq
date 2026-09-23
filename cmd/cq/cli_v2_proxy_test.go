package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func init() {
	registerV2Fixture("state-conflict", func(t *testing.T) *v2Fixture {
		f := &v2Fixture{}
		deps := v2ProxyDependencies{
			LoadConfig: func() (*proxy.Config, error) {
				f.Call("config-read")
				return &proxy.Config{ProxyResilienceStateDir: "/already-owned"}, nil
			},
		}
		f.Lookup = func(path string) (cli.Handler, bool) {
			_, ok := lookupV2Proxy(path)
			return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
				return handleV2ProxyWithDependencies(ctx, inv, s, deps)
			}, ok
		}
		return f
	})
}
func TestCLIV2ProxyContract(t *testing.T) {
	runV2Case(t, v2Case{Name: "state initialise cannot silently retarget", Scenario: "state-conflict", Args: []string{"proxy", "state", "initialise", "--state-dir", "/tmp/cq-v2-other-state", "--json"}, Exit: 6, Command: "proxy state initialise", Code: "proxy_state_conflict", Forbid: []string{"filesystem-write", "service"}})
}

func runV2ProxyTest(t *testing.T, ctx context.Context, args []string, deps v2ProxyDependencies) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, args, &cli.Session{Out: &stdout, Err: &stderr}, func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Proxy(path)
		return func(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
			return handleV2ProxyWithDependencies(ctx, inv, s, deps)
		}, ok
	})
	return code, stdout.String(), stderr.String()
}
func decodeV2ProxyData[T any](t *testing.T, output string) T {
	t.Helper()
	var e struct {
		Data T `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &e); err != nil {
		t.Fatal(err)
	}
	return e.Data
}
func absentV2ProxySnapshot() proxy.ProxySnapshot {
	return InspectProxy(context.Background(), ProxyInspectionTarget{
		Inspector: func(context.Context) proxy.Fact[proxy.InspectorIdentity] {
			return proxy.KnownFact(proxy.InspectorIdentity{Executable: "/fixture/cq", Version: "fixture"})
		},
		Desired: func(context.Context) proxy.Fact[proxy.DesiredProxyState] {
			return proxy.AbsentFact[proxy.DesiredProxyState]()
		},
		Service:  func(context.Context) proxy.Fact[proxy.ServiceState] { return proxy.AbsentFact[proxy.ServiceState]() },
		Listener: func(context.Context) proxy.Fact[proxy.ListenerState] { return proxy.AbsentFact[proxy.ListenerState]() },
		Process:  func(context.Context) proxy.Fact[proxy.ProcessState] { return proxy.AbsentFact[proxy.ProcessState]() },
		Runtime: func(context.Context) proxy.Fact[proxy.RuntimeIdentity] {
			return proxy.AbsentFact[proxy.RuntimeIdentity]()
		},
		DataPlane: func(context.Context) proxy.Fact[proxy.DataPlaneProof] {
			return proxy.AbsentFact[proxy.DataPlaneProof]()
		},
	})
}
func TestCLIV2ProxyHealth(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		status, exit int
		healthy      bool
	}{
		{"healthy", `{"status":"ok","token":"never-emit-response-secret"}`, 200, 0, true},
		{"non2xx", `never-emit-response-secret`, 401, 1, false},
		{"malformed", `never-emit-response-secret`, 200, 1, false},
		{"null", `{"status":null}`, 200, 1, false},
		{"two documents", `{"status":"ok"}{}`, 200, 1, false},
		{"wrong status", `{"status":1}`, 200, 1, false},
		{"oversized", `{"status":"ok","padding":"` + strings.Repeat("x", 1<<20) + `"}`, 200, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health" || r.Header.Get("Authorization") != "" {
					t.Error("unexpected request")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			port := server.Listener.Addr().(*net.TCPAddr).Port
			deps := v2ProxyDependencies{LoadConfig: func() (*proxy.Config, error) { panic("explicit health port read config") }}
			exit, out, stderr := runV2ProxyTest(t, context.Background(), []string{"proxy", "health", "--port", strconv.Itoa(port), "--json"}, deps)
			if exit != tc.exit || stderr != "" || strings.Contains(out, "never-emit-response-secret") {
				t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
			}
			data := decodeV2ProxyData[v2ProxyHealth](t, out)
			if !data.Reachable || data.Healthy != tc.healthy || data.HTTPStatus == nil || *data.HTTPStatus != tc.status || data.DurationMS < 0 {
				t.Fatalf("data=%+v", data)
			}
		})
	}
	t.Run("no redirect", func(t *testing.T) {
		redirected := false
		destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
		defer destination.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
		defer server.Close()
		exit, _, _ := runV2ProxyTest(t, context.Background(), []string{"proxy", "health", "--port", strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port), "--json"}, v2ProxyDependencies{})
		if exit != 1 || redirected {
			t.Fatalf("exit=%d redirected=%v", exit, redirected)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		exit, out, _ := runV2ProxyTest(t, context.Background(), []string{"proxy", "health", "--port", strconv.Itoa(port), "--json"}, v2ProxyDependencies{})
		data := decodeV2ProxyData[v2ProxyHealth](t, out)
		if exit != 4 || data.Reachable || data.HTTPStatus != nil {
			t.Fatalf("exit=%d data=%+v", exit, data)
		}
	})
	t.Run("timeout retains response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer server.Close()
		exit, out, _ := runV2ProxyTest(t, context.Background(), []string{"proxy", "health", "--port", strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port), "--timeout", "20ms", "--json"}, v2ProxyDependencies{})
		if exit != 7 || !decodeV2ProxyData[v2ProxyHealth](t, out).Reachable || !strings.Contains(out, "proxy_health_timeout") {
			t.Fatalf("exit=%d %s", exit, out)
		}
	})
}
func TestCLIV2ProxyHealthReadOnly(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(strconv.FormatBool(absent), func(t *testing.T) {
			reads := 0
			wantPort := 32123
			deps := v2ProxyDependencies{LoadConfig: func() (*proxy.Config, error) {
				reads++
				if absent {
					return nil, os.ErrNotExist
				}
				return &proxy.Config{Port: wantPort}, nil
			}, Client: &http.Client{Transport: v2ProxyRoundTrip(func(r *http.Request) (*http.Response, error) {
				if absent {
					wantPort = proxy.DefaultPort
				}
				if r.URL.Host != net.JoinHostPort("127.0.0.1", strconv.Itoa(wantPort)) {
					t.Errorf("host=%s", r.URL.Host)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"ok"}`)), Header: make(http.Header)}, nil
			})}}
			exit, out, _ := runV2ProxyTest(t, context.Background(), []string{"proxy", "health"}, deps)
			if exit != 0 || reads != 1 || !strings.Contains(out, "Proxy HTTP health: ok") {
				t.Fatalf("exit=%d reads=%d %s", exit, reads, out)
			}
		})
	}
	// Production explicit-port preparation must survive an invalid config root.
	t.Setenv("XDG_CONFIG_HOME", "relative")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"status":"ok"}`) }))
	defer server.Close()
	var out bytes.Buffer
	exit := cli.Run(context.Background(), []string{"proxy", "health", "--port", strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port), "--json"}, &cli.Session{Out: &out, Err: io.Discard}, lookupV2Proxy)
	if exit != 0 {
		t.Fatalf("exit=%d %s", exit, out.String())
	}
}

type v2ProxyRoundTrip func(*http.Request) (*http.Response, error)

func (f v2ProxyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCLIV2ProxyStatusFacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*proxy.ProxySnapshot)
		state  string
		strict int
	}{
		{"absent", func(*proxy.ProxySnapshot) {}, "absent", 3},
		{"stopped", func(s *proxy.ProxySnapshot) {
			s.Desired = proxy.KnownFact(proxy.DesiredProxyState{Configured: true})
			s.Service = proxy.KnownFact(proxy.ServiceState{Manager: "manual", State: "stopped"})
		}, "stopped", 3},
		{"indeterminate", func(s *proxy.ProxySnapshot) {
			s.Service = proxy.UnavailableFact[proxy.ServiceState]("service_unavailable")
		}, "indeterminate", 4},
		{"unhealthy", func(s *proxy.ProxySnapshot) {
			s.Service = proxy.InvalidFact[proxy.ServiceState]("service_runtime_mismatch")
		}, "degraded", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := absentV2ProxySnapshot()
			tc.change(&snapshot)
			snapshot = proxy.ReconcileProxySnapshot(snapshot)
			calls := 0
			deps := v2ProxyDependencies{Inspect: func(context.Context, string) proxy.ProxySnapshot { calls++; return snapshot }, LoadConfig: func() (*proxy.Config, error) { return nil, os.ErrNotExist }}
			exit, out, _ := runV2ProxyTest(t, context.Background(), []string{"proxy", "status", "--json"}, deps)
			if exit != 0 {
				t.Fatal(exit)
			}
			data := decodeV2ProxyData[v2ProxyStatus](t, out)
			if data.State != tc.state || data.Scope != "live" || data.StateDir != nil || len(data.Facts) != 7 {
				t.Fatalf("data=%+v", data)
			}
			exit, human, _ := runV2ProxyTest(t, context.Background(), []string{"proxy", "status"}, deps)
			if exit != 0 {
				t.Fatal(exit)
			}
			var want strings.Builder
			fmtV2ProxyHuman(&want, data)
			if human != want.String() {
				t.Fatalf("human=%q want=%q", human, want.String())
			}
			exit, strictOut, _ := runV2ProxyTest(t, context.Background(), []string{"proxy", "status", "--strict", "--json"}, deps)
			strictData := decodeV2ProxyData[v2ProxyStatus](t, strictOut)
			if exit != tc.strict || !reflect.DeepEqual(data, strictData) || calls != 3 {
				t.Fatalf("strict=%d data=%+v calls=%d", exit, strictData, calls)
			}
			for i, name := range []string{"inspector", "desired", "service", "listener", "process", "runtime", "data_plane"} {
				if data.Facts[i].Name != name {
					t.Fatal(data.Facts)
				}
			}
			if !strings.Contains(out, `"commit":null`) || !strings.Contains(out, `"digest":null`) {
				t.Fatal(out)
			}
		})
	}
}
func fmtV2ProxyHuman(w io.Writer, data v2ProxyStatus) {
	io.WriteString(w, "Proxy: "+data.State+"\nScope: "+data.Scope+"\n")
	for _, fact := range data.Facts {
		io.WriteString(w, fact.Name+": "+string(fact.State)+" — "+cli.HumanValue(fact.Detail)+"\n")
	}
}
func TestCLIV2ProxyStatusCandidate(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	deps := v2ProxyDependencies{Inspect: func(ctx context.Context, r string) proxy.ProxySnapshot { got = r; return absentV2ProxySnapshot() }}
	exit, out, _ := runV2ProxyTest(t, context.Background(), []string{"proxy", "status", "--state-dir", root, "--json"}, deps)
	data := decodeV2ProxyData[v2ProxyStatus](t, out)
	if exit != 0 || got != root || data.StateDir == nil || *data.StateDir != root || data.Scope != "candidate" {
		t.Fatalf("exit=%d data=%+v root=%s", exit, data, got)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	got = ""
	exit, _, _ = runV2ProxyTest(t, context.Background(), []string{"proxy", "status", "--state-dir", link, "--json"}, deps)
	if exit != 2 || got != "" {
		t.Fatalf("exit=%d got=%s", exit, got)
	}
}
func TestCLIV2ProxyBudgetPreparation(t *testing.T) {
	for _, path := range []string{"proxy health", "proxy status", "proxy state initialise"} {
		t.Run(path, func(t *testing.T) {
			args := strings.Fields(path)
			args = append(args, "--timeout", "1ms")
			if strings.HasSuffix(path, "initialise") {
				args = append(args, "--state-dir", "/fixture")
			}
			inv, err := cli.Parse(args)
			if err != nil {
				t.Fatal(err)
			}
			out := handleV2ProxyWithPreparation(context.Background(), inv, &cli.Session{}, func(ctx context.Context) (v2ProxyDependencies, error) {
				<-ctx.Done()
				return v2ProxyDependencies{}, errors.New("preparation failed after budget")
			})
			if out.ExitCode != 7 || len(out.Errors) != 1 || out.Errors[0].Code != strings.ReplaceAll(path, " ", "_")+"_timeout" {
				t.Fatalf("out=%+v", out)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			out = handleV2ProxyWithPreparation(ctx, inv, &cli.Session{}, func(context.Context) (v2ProxyDependencies, error) {
				t.Fatal("prepared after cancel")
				return v2ProxyDependencies{}, nil
			})
			if out.ExitCode != 130 {
				t.Fatalf("out=%+v", out)
			}
		})
	}
}
func newV2ProxyStateFixture(t *testing.T) (string, proxy.DefaultPaths, v2ProxyDependencies) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := proxy.PathsForRoots(userdirs.Roots{Config: filepath.Join(dir, "config"), State: filepath.Join(dir, "local-state")})
	root := filepath.Join(dir, "authority")
	inspect := func(context.Context, string) proxy.ProxySnapshot { return absentV2ProxySnapshot() }
	deps := v2ProxyDependencies{LoadConfig: func() (*proxy.Config, error) { return proxy.LoadExistingConfigAt(paths) }, Inspect: inspect}
	deps.Initialise = func(ctx context.Context, root string, cfg *proxy.Config) (v2ProxyState, error) {
		return initialiseProxyState(ctx, root, cfg, paths, inspect)
	}
	return root, paths, deps
}
func TestCLIV2ProxyStateInitialise(t *testing.T) {
	root, paths, deps := newV2ProxyStateFixture(t)
	args := []string{"proxy", "state", "initialise", "--state-dir", root, "--json"}
	exit, out, _ := runV2ProxyTest(t, context.Background(), args, deps)
	if exit != 0 {
		t.Fatalf("exit=%d %s", exit, out)
	}
	data := decodeV2ProxyData[v2ProxyState](t, out)
	if !data.Created || data.RestartRequired || data.StateDir != root {
		t.Fatalf("data=%+v", data)
	}
	key, err := os.ReadFile(filepath.Join(root, "authority.key"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadExistingConfigAt(paths)
	if err != nil || cfg.ProxyResilienceStateDir != root {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if strings.Contains(out, cfg.LocalToken) {
		t.Fatal("token printed")
	}
	exit, out, _ = runV2ProxyTest(t, context.Background(), args, deps)
	data = decodeV2ProxyData[v2ProxyState](t, out)
	if exit != 0 || data.Created {
		t.Fatalf("exit=%d %s", exit, out)
	}
	after, err := os.ReadFile(filepath.Join(root, "authority.key"))
	if err != nil || !bytes.Equal(key, after) {
		t.Fatal("authority rotated")
	}
	other := filepath.Join(filepath.Dir(root), "other")
	exit, _, _ = runV2ProxyTest(t, context.Background(), []string{"proxy", "state", "initialise", "--state-dir", other, "--json"}, deps)
	if exit != 6 {
		t.Fatal(exit)
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conflict created target")
	}
}
func TestCLIV2ProxyStateRejectsBeforeWrites(t *testing.T) {
	for _, mode := range []string{"cancelled", "foreign", "symlink", "unknown-service", "running-service", "corrupt", "locked"} {
		t.Run(mode, func(t *testing.T) {
			root, paths, deps := newV2ProxyStateFixture(t)
			ctx := context.Background()
			want := 6
			switch mode {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = 130
			case "foreign":
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "foreign"), []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Dir(root), root); err != nil {
					t.Fatal(err)
				}
			case "unknown-service", "running-service":
				want = 1
				deps.Initialise = func(ctx context.Context, root string, cfg *proxy.Config) (v2ProxyState, error) {
					return initialiseProxyState(ctx, root, cfg, paths, func(context.Context, string) proxy.ProxySnapshot {
						s := absentV2ProxySnapshot()
						s.Service = proxy.UnavailableFact[proxy.ServiceState]("service_unavailable")
						if mode == "running-service" {
							s.Service = proxy.KnownFact(proxy.ServiceState{Manager: "manual", State: "running", PID: 123, Executable: "/fixture/cq"})
						}
						return s
					})
				}
			case "corrupt", "locked":
				options := proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: root, Random: strings.NewReader(strings.Repeat("a", 4096)), Now: time.Now}
				if err := proxy.InitialiseProxyResilienceState(context.Background(), options); err != nil {
					t.Fatal(err)
				}
				if mode == "corrupt" {
					if err := os.WriteFile(filepath.Join(root, "authority.key"), []byte("bad"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					state, err := proxy.OpenProxyResilienceState(context.Background(), options)
					if err != nil {
						t.Fatal(err)
					}
					defer state.Close()
				}
			}
			exit, out, _ := runV2ProxyTest(t, ctx, []string{"proxy", "state", "initialise", "--state-dir", root, "--json"}, deps)
			if exit != want {
				t.Fatalf("exit=%d want=%d %s", exit, want, out)
			}
			if _, err := os.Stat(paths.ConfigFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed initialisation wrote config")
			}
		})
	}
}
func TestCLIV2ProxyServeEvents(t *testing.T) {
	for _, mode := range []string{"shutdown", "interrupt", "terminate", "runtime-failure", "bind-failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			drained := false
			steps := []string{}
			deps := v2ProxyDependencies{Serve: func(ctx context.Context, opts proxyCommandOptions, ready func(string, []string) error) error {
				if opts.Port != 29280 || !opts.MigrateLegacyManaged {
					t.Fatalf("opts=%+v", opts)
				}
				if mode == "bind-failure" {
					return &net.OpError{Op: "listen", Addr: &net.TCPAddr{Port: 29280}, Err: syscall.EADDRINUSE}
				}
				steps = append(steps, "bound", "initialised")
				if err := ready("127.0.0.1:29280", []string{"claude", "codex"}); err != nil {
					return err
				}
				steps = append(steps, "ready")
				if mode == "interrupt" {
					cancel(context.Canceled)
				}
				if mode == "terminate" {
					cancel(errV2ProxyTerminated)
				}
				steps = append(steps, "drained", "closed")
				drained = true
				if mode == "runtime-failure" {
					return errors.New("never-emit-runtime-secret")
				}
				return nil
			}}
			exit, out, stderr := runV2ProxyTest(t, ctx, []string{"proxy", "serve", "--port", "29280", "--migrate-legacy-managed", "--json"}, deps)
			want := 0
			if mode == "interrupt" {
				want = 130
			}
			if mode == "runtime-failure" {
				want = 1
			}
			if mode == "bind-failure" {
				want = 6
			}
			if exit != want || stderr != "" || strings.Contains(out, "never-emit-runtime-secret") {
				t.Fatalf("exit=%d out=%s err=%s", exit, out, stderr)
			}
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if mode == "bind-failure" {
				if len(lines) != 1 || strings.Contains(out, `"event":"ready"`) {
					t.Fatal(out)
				}
				return
			}
			if !drained || len(lines) != 2 || !reflect.DeepEqual(steps, []string{"bound", "initialised", "ready", "drained", "closed"}) {
				t.Fatalf("steps=%v %s", steps, out)
			}
			ready := decodeV2ProxyData[v2ProxyEvent](t, lines[0])
			if ready.Event != "ready" || ready.Reason != nil || ready.PID <= 0 || len(ready.Providers) != 2 {
				t.Fatalf("ready=%+v", ready)
			}
			if mode == "runtime-failure" {
				if strings.Contains(lines[1], `"event":"stopped"`) {
					t.Fatal(out)
				}
				return
			}
			stopped := decodeV2ProxyData[v2ProxyEvent](t, lines[1])
			if stopped.Event != "stopped" || stopped.Reason == nil {
				t.Fatal(out)
			}
			if mode == "interrupt" && !strings.Contains(lines[1], `"code":"interrupted"`) {
				t.Fatal(out)
			}
		})
	}
}
func TestCLIV2ProxyServeOutputFailure(t *testing.T) {
	inv, err := cli.Parse([]string{"proxy", "serve", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	cleaned := false
	deps := v2ProxyDependencies{Serve: func(ctx context.Context, opts proxyCommandOptions, ready func(string, []string) error) error {
		defer func() { cleaned = true }()
		return ready("127.0.0.1:29280", []string{"claude", "codex"})
	}}
	out := handleV2ProxyWithDependencies(context.Background(), inv, &cli.Session{Out: v2ProxyFailWriter{}, Err: io.Discard}, deps)
	if out.ExitCode != 1 || !out.Streamed || !cleaned {
		t.Fatalf("out=%+v cleaned=%v", out, cleaned)
	}
}

type v2ProxyFailWriter struct{}

func (v2ProxyFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestCLIV2ProxyHelpAndValidation(t *testing.T) {
	for _, args := range [][]string{{"proxy", "health"}, {"proxy", "status"}, {"proxy", "serve"}, {"proxy", "state", "initialise"}} {
		var stdout bytes.Buffer
		code := cli.Run(context.Background(), append(args, "--help", "--json"), &cli.Session{Out: &stdout, Err: io.Discard}, func(string) (cli.Handler, bool) { t.Fatal("help accessed state"); return nil, false })
		path := strings.Join(args, " ")
		want, err := os.ReadFile("../../specs/cli-v2/help/" + strings.ReplaceAll(path, " ", "-") + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		if code != 0 || stdout.String() != string(want) {
			t.Fatalf("help differs: exit=%d", code)
		}
	}
	for _, port := range []string{"0", "65536"} {
		runV2Case(t, v2Case{Name: "serve invalid port " + port, Scenario: "no-access", Args: []string{"proxy", "serve", "--port", port, "--json"}, Command: "proxy serve", Exit: 2, Code: "invalid_argument", Forbid: []string{"lookup"}})
	}
	if _, ok := lookupV2Proxy("proxy stop"); ok {
		t.Fatal("registered nonowned leaf")
	}
}

func TestCLIV2ProxyRealSupervisorDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan struct {
		code   int
		output string
	}, 1)
	deps := v2ProxyDependencies{Serve: func(ctx context.Context, opts proxyCommandOptions, ready func(string, []string) error) error {
		defer listener.Close()
		if err := ready(listener.Addr().String(), []string{"claude", "codex"}); err != nil {
			return err
		}
		return serveRuntimeSupervisor(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; w.WriteHeader(204) }))
	}}
	go func() {
		code, out, _ := runV2ProxyTest(t, ctx, []string{"proxy", "serve", "--json"}, deps)
		result <- struct {
			code   int
			output string
		}{code, out}
	}()
	requested := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if response != nil {
			response.Body.Close()
		}
		requested <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request not started")
	}
	cancel()
	select {
	case got := <-result:
		t.Fatalf("stopped before drain: %+v", got)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-requested; err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.code != 130 || !strings.Contains(got.output, `"event":"stopped"`) {
			t.Fatalf("result=%+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not finish")
	}
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 50*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("listener remained open")
	}
}
func TestCLIV2ProxyForegroundRuntimeSeam(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
	oldLoad, oldRun, oldAdopt := loadProxyStartConfigFn, runProxyOwnedRuntimeFn, adoptProxyListenerFn
	oldConsume := consumeInstalledHTTPValidationStartupRequestFn
	t.Cleanup(func() {
		loadProxyStartConfigFn, runProxyOwnedRuntimeFn, adoptProxyListenerFn = oldLoad, oldRun, oldAdopt
		consumeInstalledHTTPValidationStartupRequestFn = oldConsume
	})
	cfg := &proxy.Config{Port: 19280, LocalToken: "never-print-local-token"}
	loadProxyStartConfigFn = func() (*proxy.Config, error) { return cfg, nil }
	adoptProxyListenerFn = func() (net.Listener, error) { t.Fatal("foreground adopted machine fd"); return nil, nil }
	consumeInstalledHTTPValidationStartupRequestFn = func(string) (*installedHTTPValidationConsumedRequest, error) {
		t.Fatal("foreground consumed installed validation intent")
		return nil, nil
	}
	readyCalls := 0
	ctx := context.WithValue(context.Background(), v2ProxyContextTestKey{}, "present")
	runProxyOwnedRuntimeFn = func(got context.Context, port int, serve func(context.Context, net.Listener, http.Handler) error) (bool, error) {
		if got.Value(v2ProxyContextTestKey{}) != "present" || port != 29280 {
			t.Fatalf("lost context or port %d", port)
		}
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return true, err
		}
		defer listener.Close()
		// A supervisor that failed to boot must not emit ready or hang in Serve.
		return true, serve(got, listener, &proxy.RuntimeSupervisor{})
	}
	err = runProxyStartWithContext(ctx, proxyCommandOptions{Port: 29280}, func(string, []string) error { readyCalls++; return nil })
	if !errors.Is(err, proxy.ErrRuntimeSupervisorUnavailable) || readyCalls != 0 || cfg.Port != 19280 {
		t.Fatalf("err=%v ready=%d port=%d", err, readyCalls, cfg.Port)
	}
	runProxyOwnedRuntimeFn = func(got context.Context, port int, serve func(context.Context, net.Listener, http.Handler) error) (bool, error) {
		if _, ok := got.Value(proxyForegroundMigrationKey{}).(func(context.Context) error); !ok {
			t.Fatal("explicit migration request lost")
		}
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return true, err
		}
		defer listener.Close()
		supervisor := &proxy.RuntimeSupervisor{}
		if err := supervisor.ConfigureRescue(got, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), v2ProxyRescueEvidence{}); err != nil {
			return true, err
		}
		return true, serve(got, listener, supervisor)
	}
	stop := errors.New("stop at ready")
	err = runProxyStartWithContext(ctx, proxyCommandOptions{Port: 29280, MigrateLegacyManaged: true}, func(_ string, providers []string) error {
		if !reflect.DeepEqual(providers, []string{"codex"}) {
			t.Fatalf("rescue providers=%v", providers)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("rescue ready=%v", err)
	}

}

type v2ProxyContextTestKey struct{}

func TestCLIV2ProxyPartialObservations(t *testing.T) {
	snapshot := absentV2ProxySnapshot()
	deps := v2ProxyDependencies{Inspect: func(ctx context.Context, _ string) proxy.ProxySnapshot { <-ctx.Done(); return snapshot }}
	exit, out, _ := runV2ProxyTest(t, context.Background(), []string{"proxy", "status", "--timeout", "1ms", "--json"}, deps)
	if exit != 7 || len(decodeV2ProxyData[v2ProxyStatus](t, out).Facts) != 7 || !strings.Contains(out, "proxy_status_timeout") {
		t.Fatalf("exit=%d %s", exit, out)
	}
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		deps := v2ProxyDependencies{LoadConfig: func() (*proxy.Config, error) { return nil, os.ErrNotExist }, Initialise: func(context.Context, string, *proxy.Config) (v2ProxyState, error) {
			if cancelled {
				cancel()
			}
			return v2ProxyState{StateDir: "/fixture", Created: true}, errors.New("failed after durable creation")
		}}
		exit, out, _ := runV2ProxyTest(t, ctx, []string{"proxy", "state", "initialise", "--state-dir", "/fixture", "--json"}, deps)
		cancel()
		want := 8
		if cancelled {
			want = 130
		}
		if exit != want || !decodeV2ProxyData[v2ProxyState](t, out).Created {
			t.Fatalf("exit=%d %s", exit, out)
		}
	}
}
func TestCLIV2ProxyServeHumanAndDrainFailure(t *testing.T) {
	deps := v2ProxyDependencies{Serve: func(ctx context.Context, opts proxyCommandOptions, ready func(string, []string) error) error {
		return ready("127.0.0.1:29280", []string{"claude", "codex"})
	}}
	exit, out, stderr := runV2ProxyTest(t, context.Background(), []string{"proxy", "serve"}, deps)
	if exit != 0 || stderr != "" || strings.Count(out, "Proxy listening on") != 1 || strings.Contains(out, "stopped") {
		t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
	}
	deps.Serve = func(ctx context.Context, opts proxyCommandOptions, ready func(string, []string) error) error {
		if err := ready("127.0.0.1:29280", []string{"claude", "codex"}); err != nil {
			return err
		}
		return errors.Join(context.Canceled, errors.New("drain failed"))
	}
	exit, out, _ = runV2ProxyTest(t, context.Background(), []string{"proxy", "serve", "--json"}, deps)
	if exit != 1 || strings.Contains(out, `"event":"stopped"`) {
		t.Fatalf("exit=%d out=%s", exit, out)
	}
}

func readyV2ProxySnapshot() proxy.ProxySnapshot {
	s := absentV2ProxySnapshot()
	s.Desired = proxy.KnownFact(proxy.DesiredProxyState{Manager: "launchagent", Configured: true, Listener: "127.0.0.1:19280"})
	s.Service = proxy.KnownFact(proxy.ServiceState{Manager: "launchagent", State: "running", PID: 123, Executable: "/fixture/cq"})
	s.Listener = proxy.KnownFact(proxy.ListenerState{State: "listening", Listener: "127.0.0.1:19280", PID: 123, Executable: "/fixture/cq"})
	s.Process = proxy.KnownFact(proxy.ProcessState{PID: 123, Executable: "/fixture/cq"})
	s.Runtime = proxy.KnownFact(proxy.RuntimeIdentity{Reachable: true, PID: 123, Executable: "/fixture/cq", Health: "healthy"})
	s.DataPlane = proxy.KnownFact(proxy.DataPlaneProof{Proven: true})
	return proxy.ReconcileProxySnapshot(s)
}
func TestCLIV2ProxyConflictAttribution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*proxy.ProxySnapshot)
		invalid []string
		code    string
	}{
		{"service pid", func(s *proxy.ProxySnapshot) { s.Service.Value.PID = 456 }, []string{"service", "listener"}, "pid_conflict"},
		{"process pid", func(s *proxy.ProxySnapshot) { s.Process.Value.PID = 456 }, []string{"listener", "process"}, "pid_conflict"},
		{"runtime pid", func(s *proxy.ProxySnapshot) { s.Runtime.Value.PID = 456 }, []string{"listener", "runtime"}, "pid_conflict"},
		{"service executable", func(s *proxy.ProxySnapshot) { s.Service.Value.Executable = "/another" }, []string{"service", "listener"}, "executable_conflict"},
		{"process executable", func(s *proxy.ProxySnapshot) { s.Process.Value.Executable = "/another" }, []string{"listener", "process"}, "executable_conflict"},
		{"runtime executable", func(s *proxy.ProxySnapshot) { s.Runtime.Value.Executable = "/another" }, []string{"listener", "runtime"}, "executable_conflict"},
		{"manager", func(s *proxy.ProxySnapshot) { s.Desired.Value.Manager = "manual" }, []string{"desired", "service"}, "manager_conflict"},
		{"address", func(s *proxy.ProxySnapshot) { s.Desired.Value.Listener = "127.0.0.1:29280" }, []string{"desired", "listener"}, "listener_conflict"},
		{"foreign", func(s *proxy.ProxySnapshot) { s.Listener.Value.State = "foreign" }, []string{"listener"}, "foreign_listener"},
		{"orphan", func(s *proxy.ProxySnapshot) { s.Service = proxy.AbsentFact[proxy.ServiceState]() }, []string{"listener"}, "orphan_listener"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := readyV2ProxySnapshot()
			tc.change(&s)
			data := projectV2ProxySnapshot(s, "")
			if data.State != "degraded" {
				t.Fatalf("state=%s", data.State)
			}
			for _, fact := range data.Facts {
				involved := false
				for _, name := range tc.invalid {
					if fact.Name == name {
						involved = true
					}
				}
				if involved {
					if fact.State != proxy.FactInvalid || fact.ErrorCode == nil || *fact.ErrorCode != tc.code || fact.Value != nil {
						t.Fatalf("fact=%+v", fact)
					}
				} else if fact.State == proxy.FactInvalid {
					t.Fatalf("unrelated fact invalid=%+v", fact)
				}
			}
		})
	}
	data := projectV2ProxySnapshot(readyV2ProxySnapshot(), "")
	if data.State != "ready" {
		t.Fatalf("data=%+v", data)
	}
}

func TestCLIV2ProxySignalHelper(t *testing.T) {
	if os.Getenv("CQ_V2_PROXY_SIGNAL_HELPER") != "1" {
		return
	}
	loadProxyStartConfigFn = func() (*proxy.Config, error) {
		return &proxy.Config{Port: 19280, LocalToken: "fixture-not-for-output"}, nil
	}
	runProxyOwnedRuntimeFn = func(ctx context.Context, _ int, serve func(context.Context, net.Listener, http.Handler) error) (bool, error) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return true, err
		}
		defer listener.Close()
		return true, serve(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	}
	code := cli.Run(context.Background(), []string{"proxy", "serve", "--json"}, &cli.Session{Out: os.Stdout, Err: os.Stderr}, lookupV2Proxy)
	os.Exit(code)
}
func TestCLIV2ProxyNativeSignals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Process.Signal is unavailable on Windows; context tests cover portable semantics")
	}
	for _, signal := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIV2ProxySignalHelper$")
			command.Env = append(os.Environ(), "CQ_V2_PROXY_SIGNAL_HELPER=1", "XDG_CONFIG_HOME="+root)
			var stderr bytes.Buffer
			command.Stderr = &stderr
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(stdout)
			ready, err := reader.ReadString('\n')
			if err != nil {
				_ = command.Wait()
				t.Fatalf("no ready: %v stderr=%s", err, stderr.String())
			}
			if !strings.Contains(ready, `"event":"ready"`) {
				t.Fatalf("ready=%s", ready)
			}
			if err := command.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			tail, readErr := io.ReadAll(reader)
			if readErr != nil {
				t.Fatal(readErr)
			}
			err = command.Wait()
			exit := 0
			if err != nil {
				var process *exec.ExitError
				if !errors.As(err, &process) {
					t.Fatal(err)
				}
				exit = process.ExitCode()
			}
			want := 0
			if signal == os.Interrupt {
				want = 130
			}
			if exit != want || strings.Count(string(tail), `"event":"stopped"`) != 1 || !strings.Contains(string(tail), `"reason":"interrupted"`) {
				t.Fatalf("exit=%d want=%d output=%s stderr=%s", exit, want, tail, stderr.String())
			}
			if signal == syscall.SIGTERM && !strings.Contains(string(tail), `"ok":true`) {
				t.Fatal(string(tail))
			}
		})
	}
}

func TestCLIV2ProxyInitialiseBindingPartial(t *testing.T) {
	root, paths, deps := newV2ProxyStateFixture(t)
	options := proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: root, Random: strings.NewReader(strings.Repeat("k", 4096)), Now: time.Now}
	if err := proxy.InitialiseProxyResilienceState(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(paths.RescueBootstrap), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	exit, out, _ := runV2ProxyTest(t, context.Background(), []string{"proxy", "state", "initialise", "--state-dir", root, "--json"}, deps)
	data := decodeV2ProxyData[v2ProxyState](t, out)
	if exit != 8 || data.Created || data.RestartRequired || data.StateDir != root || !strings.Contains(out, "proxy_state_initialise_partial") {
		t.Fatalf("exit=%d out=%s", exit, out)
	}
	saved, err := proxy.LoadExistingConfigAt(paths)
	if err != nil || saved.ProxyResilienceStateDir != root {
		t.Fatalf("binding receipt false: %v", err)
	}
	if strings.Contains(out, `"bound"`) {
		t.Fatal("internal receipt leaked into DTO")
	}
}
func TestCLIV2ProxyServePreparationInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inv, err := cli.Parse([]string{"proxy", "serve"})
	if err != nil {
		t.Fatal(err)
	}
	out := handleV2ProxyWithPreparation(ctx, inv, &cli.Session{}, func(context.Context) (v2ProxyDependencies, error) {
		cancel()
		return v2ProxyDependencies{}, errors.New("late preparation failure")
	})
	if out.ExitCode != 130 {
		t.Fatalf("out=%+v", out)
	}
}

type v2ProxyRescueEvidence struct{}

func (v2ProxyRescueEvidence) Load(context.Context) (proxy.RuntimeModeEvidenceV1, bool, error) {
	return proxy.RuntimeModeEvidenceV1{SchemaVersion: 1, Generation: 1, DesiredMode: proxy.TrafficModeRescue, EffectiveMode: proxy.TrafficModeRescue, Phase: proxy.RuntimeModePhaseEffective}, true, nil
}
func (v2ProxyRescueEvidence) Commit(context.Context, proxy.RuntimeModeEvidenceV1) error { return nil }

func TestCLIV2ProxyServeWarningsOnce(t *testing.T) {
	deps := v2ProxyDependencies{Serve: func(ctx context.Context, opts proxyCommandOptions, ready func(string, []string) error) error {
		return ready("127.0.0.1:19280", []string{"codex"})
	}}
	exit, out, stderr := runV2ProxyTest(t, context.Background(), []string{"proxy", "start", "--json"}, deps)
	if exit != 0 || strings.Count(stderr, "cq: warning:") != 1 || strings.Count(out, `"warnings":[{`) != 2 {
		t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
	}
}
func TestCLIV2ProxyServeTerminationBeforeReady(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	deps := v2ProxyDependencies{Serve: func(context.Context, proxyCommandOptions, func(string, []string) error) error {
		cancel(errV2ProxyTerminated)
		return context.Canceled
	}}
	exit, out, stderr := runV2ProxyTest(t, ctx, []string{"proxy", "serve", "--json"}, deps)
	if exit != 0 || strings.Contains(out, `"event"`) || stderr != "" {
		t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
	}
}
