package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/cli"
)

type v2Case struct {
	Name     string
	Scenario string
	Args     []string
	Exit     int
	Code     string
	Command  string
	WantJSON string
	Forbid   []string
	Calls    map[string]int
}

// Family test files register constructors during init. Constructors must inject
// their real domain engines; a precomputed Outcome is not a contract fixture.
type v2Fixture struct {
	Lookup      cli.Lookup
	In          io.Reader
	Interactive bool
	Secrets     []string
	mu          sync.Mutex
	calls       map[string]int
}

func (f *v2Fixture) Call(boundary string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[boundary]++
}

type v2FixtureConstructor func(*testing.T) *v2Fixture

var v2Fixtures = map[string]v2FixtureConstructor{"no-access": noAccessV2Fixture}

func registerV2Fixture(name string, constructor v2FixtureConstructor) {
	if name == "" || constructor == nil {
		panic("invalid CLI v2 fixture registration")
	}
	if _, exists := v2Fixtures[name]; exists {
		panic("duplicate CLI v2 fixture: " + name)
	}
	v2Fixtures[name] = constructor
}

func findV2Fixture(name string) (v2FixtureConstructor, error) {
	constructor, ok := v2Fixtures[name]
	if !ok {
		return nil, fmt.Errorf("CLI v2 fixture %q is not registered; implement its real engine fixture", name)
	}
	return constructor, nil
}

type noAccessV2Input struct{}

func (noAccessV2Input) Read([]byte) (int, error) { panic("CLI v2 no-access fixture read stdin") }

func noAccessV2Fixture(t *testing.T) *v2Fixture {
	t.Helper()
	f := &v2Fixture{In: noAccessV2Input{}, Secrets: []string{"cq-fixture-secret-never-output"}}
	// Reject lookup itself, before a credential/network/write/service dependency
	// can even be constructed. This also rejects unimplemented command execution.
	f.Lookup = func(path string) (cli.Handler, bool) {
		switch path {
		case "proxy candidate start", "proxy candidate client-safety refresh", "proxy candidate release activate", "proxy candidate release validate":
			return func(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
				return handleV2CandidateWithPreparation(ctx, inv, session, func(context.Context) (v2CandidateDependencies, error) {
					f.Call("environment")
					panic("unavailable candidate constructed dependencies")
				})
			}, true
		}
		f.Call("lookup")
		panic("CLI v2 no-access fixture reached handler lookup")
	}
	return f
}

func runV2Case(t *testing.T, c v2Case) {
	t.Helper()
	t.Run(c.Name, func(t *testing.T) {
		constructor, err := findV2Fixture(c.Scenario)
		if err != nil {
			t.Fatal(err)
		}
		fixture := constructor(t)
		if fixture == nil || fixture.Lookup == nil {
			t.Fatal("CLI v2 fixture did not install its real handler lookup")
		}
		var stdout, stderr bytes.Buffer
		session := &cli.Session{In: fixture.In, Out: &stdout, Err: &stderr, Interactive: fixture.Interactive}
		exit := cli.Run(context.Background(), c.Args, session, fixture.Lookup)
		for _, secret := range fixture.Secrets {
			if v2OutputsContainSecret(stdout.Bytes(), stderr.Bytes(), secret) {
				t.Fatal("fixture secret leaked to output")
			}
		}
		// Check sentinels before any assertion can quote captured output.
		if exit != c.Exit {
			t.Fatalf("exit=%d want=%d; stdout=%q stderr=%q", exit, c.Exit, stdout.String(), stderr.String())
		}
		if err := validateV2Envelope(stdout.Bytes(), c); err != nil {
			t.Fatal(err)
		}
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		for _, boundary := range c.Forbid {
			if fixture.calls[boundary] != 0 {
				t.Errorf("forbidden %s calls=%d", boundary, fixture.calls[boundary])
			}
		}
		for boundary, want := range c.Calls {
			if fixture.calls[boundary] != want {
				t.Errorf("%s calls=%d want=%d", boundary, fixture.calls[boundary], want)
			}
		}
	})
}

var v2JSONEscapes = regexp.MustCompile(`(?:\\(?:["\\/bfnrt]|u[0-9a-fA-F]{4}))+`)

func v2OutputsContainSecret(stdout, stderr []byte, secret string) bool {
	if secret == "" {
		return false
	}
	for _, stream := range [][]byte{stdout, stderr} {
		if bytes.Contains(stream, []byte(secret)) {
			return true
		}
		// Decode escapes within either stream, including plain stderr diagnostic
		// lines that are not complete JSON values. Group adjacent escapes so
		// UTF-16 surrogate pairs are decoded together by encoding/json.
		decoded := v2JSONEscapes.ReplaceAllStringFunc(string(stream), func(escaped string) string {
			var value string
			if err := json.Unmarshal([]byte(`"`+escaped+`"`), &value); err != nil {
				return escaped
			}
			return value
		})
		if strings.Contains(decoded, secret) {
			return true
		}
	}
	return false
}

func validateV2Envelope(stdout []byte, c v2Case) error {
	if !utf8.Valid(stdout) || len(stdout) == 0 || stdout[len(stdout)-1] != '\n' || bytes.Count(stdout, []byte{'\n'}) != 1 || bytes.ContainsRune(stdout, '\x1b') {
		return fmt.Errorf("expected one UTF-8 JSON document plus LF, got %q", stdout)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(stdout, &envelope); err != nil {
		return fmt.Errorf("invalid envelope: %w", err)
	}
	keys := []string{"schema_version", "command", "ok", "data", "errors", "warnings"}
	if len(envelope) != len(keys) {
		return fmt.Errorf("envelope has %d keys, expected six", len(envelope))
	}
	for _, key := range keys {
		if _, ok := envelope[key]; !ok {
			return fmt.Errorf("missing envelope key %s", key)
		}
	}
	var schema int
	var command string
	var ok bool
	if err := json.Unmarshal(envelope["schema_version"], &schema); err != nil || schema != 2 {
		return fmt.Errorf("invalid schema_version: %s", envelope["schema_version"])
	}
	if err := json.Unmarshal(envelope["command"], &command); err != nil || command != c.Command {
		return fmt.Errorf("command=%s want=%q", envelope["command"], c.Command)
	}
	if string(envelope["ok"]) != "true" && string(envelope["ok"]) != "false" {
		return fmt.Errorf("ok is not a boolean")
	}
	if err := json.Unmarshal(envelope["ok"], &ok); err != nil || ok != (c.Exit == 0) {
		return fmt.Errorf("ok=%t disagrees with exit %d", ok, c.Exit)
	}
	found := false
	for _, field := range []string{"errors", "warnings"} {
		var records []map[string]json.RawMessage
		if err := json.Unmarshal(envelope[field], &records); err != nil || records == nil {
			return fmt.Errorf("%s must be an explicit array", field)
		}
		if field == "errors" && c.Code == "" && len(records) != 0 {
			return fmt.Errorf("unexpected errors: %s", envelope[field])
		}
		for _, record := range records {
			var code, message string
			if len(record) != 2 || json.Unmarshal(record["code"], &code) != nil || json.Unmarshal(record["message"], &message) != nil || code == "" || string(record["message"]) == "null" {
				return fmt.Errorf("invalid %s diagnostic", field)
			}
			if field == "errors" && code == c.Code {
				found = true
			}
		}
	}
	if c.Code != "" && !found {
		return fmt.Errorf("missing error code %q", c.Code)
	}
	if c.WantJSON != "" {
		var got, want any
		for _, value := range []struct {
			raw    []byte
			target *any
		}{{envelope["data"], &got}, {[]byte(c.WantJSON), &want}} {
			decoder := json.NewDecoder(bytes.NewReader(value.raw))
			decoder.UseNumber()
			if err := decoder.Decode(value.target); err != nil {
				return fmt.Errorf("invalid data/subset: %w", err)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				return fmt.Errorf("extra data/subset value")
			}
		}
		if err := v2JSONSubset(got, want, "data"); err != nil {
			return err
		}
	}
	return nil
}

func v2JSONSubset(got, want any, path string) error {
	switch expected := want.(type) {
	case map[string]any:
		actual, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not an object", path)
		}
		for key, value := range expected {
			child, exists := actual[key]
			if !exists {
				return fmt.Errorf("%s missing %s", path, key)
			}
			if err := v2JSONSubset(child, value, path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		actual, ok := got.([]any)
		if !ok || len(actual) != len(expected) {
			return fmt.Errorf("%s array length/type differs", path)
		}
		for i, value := range expected {
			if err := v2JSONSubset(actual[i], value, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case json.Number:
		actual, ok := got.(json.Number)
		if !ok {
			return fmt.Errorf("%s is not a number", path)
		}
		left, validLeft := new(big.Rat).SetString(string(actual))
		right, validRight := new(big.Rat).SetString(string(expected))
		if !validLeft || !validRight || left.Cmp(right) != 0 {
			return fmt.Errorf("%s=%v want=%v", path, got, want)
		}
	default:
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("%s=%v want=%v", path, got, want)
		}
	}
	return nil
}

func TestCLIV2RejectsMutationTail(t *testing.T) {
	runV2Case(t, v2Case{Name: "unknown tail before access", Scenario: "no-access", Args: []string{"codex", "account", "remove", "alice@example.com", "--dry-run", "--json"}, Exit: 2, Code: "unknown_option", Command: "codex account remove", WantJSON: `null`, Forbid: []string{"lookup", "credentials", "network", "filesystem-write", "service", "consume"}})
}

func TestCLIV2OutputHarnessRejectsInvalidEvidence(t *testing.T) {
	if _, err := findV2Fixture("not-implemented"); err == nil {
		t.Fatal("missing fixture silently accepted")
	}
	valid := `{"schema_version":2,"command":"check","ok":true,"data":{"rows":[{"name":"a","extra":1}],"nullable":null},"errors":[],"warnings":[]}` + "\n"
	c := v2Case{Command: "check", WantJSON: `{"rows":[{"name":"a"}],"nullable":null}`}
	if err := validateV2Envelope([]byte(valid), c); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{valid + valid, strings.Replace(valid, `"errors":[]`, `"errors":null`, 1), strings.Replace(valid, `"warnings":[]`, `"extra":[],"warnings":[]`, 1), strings.Replace(valid, `"ok":true`, `"ok":null`, 1), strings.Replace(valid, `"name":"a"`, `"name":"b"`, 1), strings.Replace(valid, `"nullable":null`, `"nullable":false`, 1), strings.TrimSuffix(valid, "\n")} {
		if err := validateV2Envelope([]byte(bad), c); err == nil {
			t.Errorf("invalid evidence accepted: %q", bad)
		}
	}
	// Numeric subsets compare values without losing precision to float64.
	if err := v2JSONSubset(json.Number("1"), json.Number("1.0"), "data.count"); err != nil {
		t.Fatal(err)
	}
	if err := v2JSONSubset(json.Number("9007199254740992"), json.Number("9007199254740993"), "data.count"); err == nil {
		t.Fatal("large unequal numbers collapsed")
	}
	if err := v2JSONSubset([]any{"a", "b"}, []any{"b", "a"}, "data.rows"); err == nil {
		t.Fatal("array order ignored")
	}
	if err := v2JSONSubset([]any{"a", "b"}, []any{"a"}, "data.rows"); err == nil {
		t.Fatal("extra array row ignored")
	}
	if !v2OutputsContainSecret([]byte(`{"nested":[{"secret":"fixture\u002dsecret"}]}`), nil, "fixture-secret") {
		t.Fatal("escaped fixture secret missed")
	}
}

func TestCLIV2OutputHarnessSecretEscapes(t *testing.T) {
	clean := []byte(`{"schema_version":2,"command":"check","ok":true,"data":null,"errors":[],"warnings":[]}` + "\n")
	for _, stderr := range []string{`cq: warning: fixture\u002dsecret` + "\n", `{"message":"fixture\u002dsecret"}`, `fixture-secret`} {
		if !v2OutputsContainSecret(clean, []byte(stderr), "fixture-secret") {
			t.Errorf("stderr secret was not rejected before diagnostic quoting: %q", stderr)
		}
	}
	if v2OutputsContainSecret(clean, []byte("cq: warning: unrelated message\n"), "fixture-secret") {
		t.Fatal("clean output rejected")
	}
	if !v2OutputsContainSecret(clean, []byte(`cq: fixture\u002d\ud83d\ude00`), "fixture-😀") {
		t.Fatal("adjacent escapes and surrogate pair missed")
	}
	if v2OutputsContainSecret(clean, []byte(`cq: malformed escape \uXYZW`), "fixture-secret") {
		t.Fatal("malformed unrelated escape rejected")
	}
}
