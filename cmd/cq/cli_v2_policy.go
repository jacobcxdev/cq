package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func lookupV2Policy(path string) (cli.Handler, bool) {
	switch path {
	case "codex proxy policy apply", "codex proxy policy show", "codex proxy pool rename", "codex proxy pool set", "codex proxy pool value", "codex proxy session bind", "codex proxy session digest", "codex proxy session list", "codex proxy session show", "codex proxy session unbind":
		return handleV2Policy, true
	}
	return nil, false
}

func handleV2Policy(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2PolicyWithPreparation(ctx, inv, session, func(ctx context.Context) (proxyPolicyDependencies, error) {
		roots, err := userdirs.Default(userdirs.ConfigRoot, userdirs.StateRoot)
		if err != nil {
			return proxyPolicyDependencies{}, err
		}
		paths := proxy.PathsForRoots(roots)
		accounts := &codex.Accounts{FS: fsutil.OSFileSystem{}, StateDir: roots.State}
		return proxyPolicyDependencies{
			LoadConfig:     func() (*proxy.Config, error) { return proxy.LoadExistingConfigAt(paths) },
			Doer:           &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("proxy policy redirect refused") }},
			ListInventory:  accounts.Inspect,
			LoadAliasIndex: func() (codex.AccountAliasIndex, error) { return accounts.InspectAliases(ctx) },
		}, nil
	})
}
func handleV2PolicyWithDependencies(ctx context.Context, inv cli.Invocation, session *cli.Session, deps proxyPolicyDependencies) cli.Outcome {
	return handleV2PolicyWithPreparation(ctx, inv, session, func(context.Context) (proxyPolicyDependencies, error) { return deps, nil })
}
func handleV2PolicyWithPreparation(parent context.Context, inv cli.Invocation, session *cli.Session, prepare func(context.Context) (proxyPolicyDependencies, error)) cli.Outcome {
	timeout, err := time.ParseDuration(inv.Options["timeout"][0])
	if err != nil {
		return v2SelectionFailure(2, "routing_invalid_argument", "Invalid argument: timeout.")
	}
	budget := cli.BeginBudget(parent, timeout, 0)
	defer budget.Close()
	ctx := budget.Work()
	if out, stopped := v2SelectionStopped(parent, ctx); stopped {
		return out
	}
	// Existing digests are pure values; do not even resolve filesystem roots.
	if inv.Path == "codex proxy session digest" && len(inv.Options["digest"]) > 0 {
		return v2PolicyResult(struct {
			Digest string `json:"session_digest"`
		}{inv.Options["digest"][0]}, false, "")
	}
	deps, err := prepare(ctx)
	if out, stopped := v2SelectionStopped(parent, ctx); stopped {
		return out
	}
	if err != nil {
		if out, ok := v2EnvironmentFailure(err); ok {
			return out
		}
		return v2PolicyIO("resolve proxy paths")
	}
	out := executeV2Policy(ctx, inv, session, deps)
	if stopped, ok := v2SelectionStopped(parent, ctx); ok {
		return stopped
	}
	return out
}
func executeV2Policy(ctx context.Context, inv cli.Invocation, session *cli.Session, deps proxyPolicyDependencies) cli.Outcome {
	parts := strings.Fields(inv.Path)
	kind, action := parts[2], parts[3]
	port := 0
	if v := inv.Options["port"]; len(v) > 0 {
		port, _ = strconv.Atoi(v[0])
	}
	if kind == "policy" {
		var document proxy.RoutingPolicyDocument
		if action == "apply" {
			var err error
			document, err = readV2PolicyFile(inv.Options["file"][0])
			if err != nil {
				return v2PolicyFailure(err, inv)
			}
		}
		if v := inv.Options["state-dir"]; len(v) > 0 {
			state, err := proxy.OpenProxyResilienceState(ctx, proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: v[0], Random: rand.Reader, Now: proxyPolicyNow})
			if err != nil {
				var validation *proxy.RoutingPolicyValidationError
				if errors.As(err, &validation) {
					return v2PolicyIO("read existing authority")
				}
				return v2PolicyFailure(err, inv)
			}
			if err = ctx.Err(); err == nil && action == "apply" {
				err = state.Routing.PublishDocument(document)
			}
			if err == nil {
				document, err = state.Routing.Document()
			}
			err = errors.Join(err, state.Close())
			if err != nil {
				return v2PolicyFailure(err, inv)
			}
		} else {
			method := http.MethodGet
			var request any
			if action == "apply" {
				method = http.MethodPut
				request = document
			}
			var err error
			document, err = proxyPolicyControl(ctx, deps, method, proxy.RuntimePolicyPath, port, request)
			if err != nil {
				return v2PolicyFailure(err, inv)
			}
		}
		if document.SchemaVersion != 1 {
			return v2SelectionFailure(6, "routing_conflict", "Routing state conflict: no policy has been published.")
		}
		return v2PolicyOutcome(document, kind)
	}
	if kind == "pool" {
		if action != "set" {
			mutation := proxy.PoolMutationRequest{Operation: action}
			if action == "rename" {
				mutation.Name = inv.Arguments["old-name"][0]
				mutation.NewName = inv.Arguments["new-name"][0]
			} else {
				mutation.Name = inv.Arguments["name"][0]
				value, _ := strconv.ParseUint(inv.Arguments["value"][0], 10, 32)
				mutation.Value = proxy.PoolValue(value)
			}
			updated, err := proxyPolicyControl(ctx, deps, http.MethodPost, proxy.RuntimePolicyPoolPath, port, mutation)
			if err != nil {
				return v2PolicyFailure(err, inv)
			}
			return v2PolicyOutcome(updated, kind)
		}
		if deps.ListInventory == nil || deps.LoadAliasIndex == nil {
			return v2SelectionInventoryFailure()
		}
		inventory, err := deps.ListInventory(ctx)
		if err != nil || proxyCodexDefaultInventoryIncomplete(inventory) {
			return v2SelectionInventoryFailure()
		}
		if ctx.Err() != nil {
			return v2PolicyIO("account inventory")
		}
		aliases, err := deps.LoadAliasIndex()
		if err != nil {
			return v2SelectionInventoryFailure()
		}
		members := make([]codex.AccountKey, 0, len(inv.Options["account"]))
		seen := map[codex.AccountKey]bool{}
		for _, ref := range inv.Options["account"] {
			key, err := codex.ResolveAccountReference(inventory, aliases, ref)
			if err != nil {
				return v2SelectionAccountFailure(ref, err)
			}
			if seen[key] {
				return v2SelectionFailure(2, "routing_invalid_argument", "Invalid argument: duplicate resolved account.")
			}
			seen[key] = true
			members = append(members, key)
		}
		sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
		current, err := proxyPolicyControl(ctx, deps, http.MethodGet, proxy.RuntimePolicyPath, port, nil)
		if err != nil {
			return v2PolicyFailure(err, inv)
		}
		next := nextProxyRoutingPolicy(current)
		name := inv.Arguments["name"][0]
		index := -1
		for i, pool := range next.Pools {
			if strings.EqualFold(pool.Name, name) {
				index = i
				break
			}
		}
		if index < 0 {
			index = len(next.Pools)
			next.Pools = append(next.Pools, proxy.AccountPoolDocument{Name: name})
		}
		next.Pools[index].Members = members
		if values := inv.Options["value"]; len(values) > 0 {
			value, _ := strconv.ParseUint(values[0], 10, 32)
			next.Pools[index].Value = proxy.PoolValue(value)
		}
		sort.Slice(next.Pools, func(i, j int) bool { return next.Pools[i].Name < next.Pools[j].Name })
		updated, err := proxyPolicyControl(ctx, deps, http.MethodPut, proxy.RuntimePolicyPath, port, next)
		if err != nil {
			return v2PolicyFailure(err, inv)
		}
		return v2PolicyOutcome(updated, kind)
	}
	digest := ""
	if action != "list" {
		if values := inv.Options["digest"]; len(values) > 0 {
			digest = values[0]
		} else {
			var raw []byte
			if values := inv.Options["session-id"]; len(values) > 0 {
				raw = []byte(values[0])
			} else {
				if session == nil || session.In == nil {
					return v2PolicyIO("read session identifier")
				}
				var err error
				raw, err = readV2PolicySession(ctx, session.In)
				if err != nil {
					zeroProxyPolicyBytes(raw)
					return v2PolicyIO("read session identifier")
				}
			}
			defer zeroProxyPolicyBytes(raw)
			if len(raw) < 1 || len(raw) > 4096 || !utf8.Valid(raw) {
				return v2SelectionFailure(2, "routing_invalid_argument", "Invalid argument: session identifier must contain 1–4096 UTF-8 bytes.")
			}
			var err error
			digest, err = proxyPolicySessionDigest(ctx, deps, port, raw)
			if err != nil {
				return v2PolicyFailure(err, inv)
			}
		}
	}
	if action == "digest" {
		return v2PolicyResult(struct {
			Digest string `json:"session_digest"`
		}{digest}, false, "")
	}
	current, err := proxyPolicyControl(ctx, deps, http.MethodGet, proxy.RuntimePolicyPath, port, nil)
	if err != nil {
		return v2PolicyFailure(err, inv)
	}
	sort.Slice(current.SessionBindings, func(i, j int) bool {
		return current.SessionBindings[i].SessionDigest < current.SessionBindings[j].SessionDigest
	})
	if action == "list" {
		return v2PolicyResult(struct {
			Bindings []proxy.SessionBindingDocument `json:"bindings"`
		}{append([]proxy.SessionBindingDocument{}, current.SessionBindings...)}, false, "")
	}
	index := -1
	for i, binding := range current.SessionBindings {
		if binding.SessionDigest == digest {
			index = i
			break
		}
	}
	if action == "show" || action == "unbind" {
		if index < 0 {
			return v2SelectionFailure(3, "session_binding_not_found", "Session binding not found.")
		}
	}
	if action == "show" {
		return v2PolicyResult(struct {
			Binding proxy.SessionBindingDocument `json:"binding"`
		}{current.SessionBindings[index]}, false, "")
	}
	next := nextProxyRoutingPolicy(current)
	if action == "bind" {
		name := inv.Options["pool"][0]
		poolName := ""
		for _, pool := range current.Pools {
			if strings.EqualFold(pool.Name, name) {
				poolName = pool.Name
				break
			}
		}
		if poolName == "" {
			return v2SelectionFailure(3, "pool_not_found", fmt.Sprintf("Pool not found: %s.", name))
		}
		binding := proxy.SessionBindingDocument{SessionDigest: digest, Pool: poolName}
		if index < 0 {
			next.SessionBindings = append(next.SessionBindings, binding)
		} else {
			next.SessionBindings[index] = binding
		}
		sort.Slice(next.SessionBindings, func(i, j int) bool {
			return next.SessionBindings[i].SessionDigest < next.SessionBindings[j].SessionDigest
		})
	} else {
		next.SessionBindings = append(next.SessionBindings[:index], next.SessionBindings[index+1:]...)
	}
	updated, err := proxyPolicyControl(ctx, deps, http.MethodPut, proxy.RuntimePolicyPath, port, next)
	if err != nil {
		return v2PolicyFailure(err, inv)
	}
	return v2PolicyOutcome(updated, kind)
}

// Reading caller-owned stdin is the only asynchronous work here. All state
// access and mutations stay on the operation goroutine. The input goroutine
// relinquishes and clears its buffer when cancellation wins; it never closes
// a caller-owned reader or performs a late control request.
func readV2PolicySession(ctx context.Context, input io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result)
	go func() {
		var value result
		defer func() {
			if recover() != nil {
				zeroProxyPolicyBytes(value.data)
				value = result{err: errors.New("stdin unavailable")}
			}
			select {
			case done <- value:
			case <-ctx.Done():
				zeroProxyPolicyBytes(value.data)
			}
		}()
		value.data, value.err = io.ReadAll(io.LimitReader(input, 4097))
	}()
	select {
	case value := <-done:
		return value.data, value.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func readV2PolicyFile(path string) (proxy.RoutingPolicyDocument, error) {
	info, err := os.Stat(path)
	if err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	if !info.Mode().IsRegular() {
		return proxy.RoutingPolicyDocument{}, &proxy.RoutingPolicyValidationError{Err: errors.New("expected a regular policy file")}
	}
	file, err := os.Open(path)
	if err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	invalid := func() (proxy.RoutingPolicyDocument, error) {
		return proxy.RoutingPolicyDocument{}, &proxy.RoutingPolicyValidationError{Err: errors.New("expected one strict schema 1 policy document")}
	}
	if !info.Mode().IsRegular() {
		return invalid()
	}
	body, err := io.ReadAll(io.LimitReader(file, proxyPolicyMaxBytes+1))
	if err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	if len(body) > proxyPolicyMaxBytes || !utf8.Valid(body) {
		return invalid()
	}
	var document proxy.RoutingPolicyDocument
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) || document.SchemaVersion != 1 {
		return invalid()
	}
	// Go accepts null for non-pointer scalars and slices. Reject those inputs
	// before returning the document for publication. Check types first so this
	// walk only sees the shallow public schema, not arbitrary nested objects.
	shape := json.NewDecoder(bytes.NewReader(body))
	shape.UseNumber()
	if !validV2PolicyNullability(shape, "") {
		return invalid()
	}
	return document, nil
}

// Only these two public field paths are nullable. Walking tokens also checks
// every occurrence of a duplicate field before decoding can overwrite it.
func validV2PolicyNullability(decoder *json.Decoder, path string) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	if token == nil {
		return path == "capability_pool" || path == "capability_routing_evidence[].expires_at"
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return true
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return false
			}
			seen[name] = true
			if path != "" {
				name = path + "." + name
			}
			if !validV2PolicyNullability(decoder, name) {
				return false
			}
		}
	case '[':
		for decoder.More() {
			if !validV2PolicyNullability(decoder, path+"[]") {
				return false
			}
		}
	default:
		return false
	}
	_, err = decoder.Token()
	return err == nil
}

func v2PolicyIO(detail string) cli.Outcome {
	return v2SelectionFailure(1, "routing_io_failed", "Routing operation failed: "+detail+".")
}
func v2PolicyFailure(err error, inv cli.Invocation) cli.Outcome {
	code := "routing_io_failed"
	exit := 1
	message := "Routing operation failed: policy control or state; inspect current state before retrying."
	var control *proxyPolicyControlError
	var validation *proxy.RoutingPolicyValidationError
	switch {
	case errors.Is(err, proxy.ErrLocalTokenRequired):
		code = "routing_auth_failed"
	case errors.As(err, &validation):
		if validation.Generation {
			code = "policy_generation_conflict"
		} else {
			code = "policy_document_invalid"
		}
	case errors.Is(err, proxy.ErrAuthorityPriorMismatch), errors.Is(err, fsutil.ErrExclusiveLockHeld), errors.Is(err, proxy.ErrLifecycleLockHeld):
		code = "routing_conflict"
	case errors.As(err, &control):
		switch {
		case control.kind == "auth" || control.status == 401 || control.status == 403:
			code = "routing_auth_failed"
		case control.kind == "io":
		case control.kind == "control":
			code = "routing_control_unavailable"
		case control.status == 409 || control.status == 400:
			code = "routing_conflict"
			if control.status == 400 {
				code = "routing_invalid_argument"
			}
			switch control.code {
			case "policy_document_invalid", "policy_generation_conflict", "routing_io_failed", "routing_conflict", "routing_invalid_argument", "pool_not_found", "pool_name_conflict":
				code = control.code
			case "invalid_pool_name":
				code = "routing_invalid_argument"
			}
		default:
			code = "routing_control_unavailable"
		}
	}
	if (code == "pool_name_conflict" && !strings.HasPrefix(inv.Path, "codex proxy pool ")) || (code == "pool_not_found" && strings.HasPrefix(inv.Path, "codex proxy policy ")) {
		code = "routing_conflict"
	}
	if !strings.HasPrefix(inv.Path, "codex proxy policy ") && (code == "policy_generation_conflict" || code == "policy_document_invalid") {
		code = "routing_conflict"
	}
	switch code {
	case "policy_generation_conflict":
		exit = 6
		message = "Policy generations conflict; show current policy and prepare a fresh document."
	case "policy_document_invalid":
		exit = 2
		message = "Invalid routing-policy document: schema or references are invalid."
	case "routing_auth_failed":
		exit = 5
		message = "Local proxy authentication failed."
	case "routing_control_unavailable":
		exit = 4
		message = "Running CQ proxy control is unavailable."
	case "routing_conflict":
		exit = 6
		message = "Routing state conflict: policy update rejected or authority is owned."
	case "routing_invalid_argument":
		exit = 2
		message = "Invalid argument: policy control."
	case "pool_not_found", "pool_name_conflict":
		name := ""
		for _, key := range []string{"name", "old-name"} {
			if v := inv.Arguments[key]; len(v) > 0 {
				name = v[0]
			}
		}
		exit = 3
		message = fmt.Sprintf("Pool not found: %s.", name)
		if code == "pool_name_conflict" {
			exit = 6
			if values := inv.Arguments["new-name"]; len(values) > 0 {
				name = values[0]
			}
			message = fmt.Sprintf("Pool name already exists: %s.", name)
		}
	}
	return v2SelectionFailure(exit, code, message)
}

// Public schema 1 deliberately differs from the authenticated storage document.
type v2PolicyDocument struct {
	SchemaVersion             int                               `json:"schema_version"`
	AuthorityGeneration       uint64                            `json:"authority_generation"`
	RoutingGeneration         uint64                            `json:"routing_generation"`
	EffectiveGeneration       uint64                            `json:"effective_generation"`
	Pools                     []v2PolicyPool                    `json:"pools"`
	SessionBindings           []proxy.SessionBindingDocument    `json:"session_bindings"`
	CapabilityEvidence        []proxy.CapabilityEvidenceV1      `json:"capability_evidence"`
	CapabilityPool            *string                           `json:"capability_pool"`
	CapabilityPredicates      []proxy.CapabilityPredicateCoreV1 `json:"capability_predicates"`
	CapabilityRoutingEvidence []v2PolicyEvidence                `json:"capability_routing_evidence"`
	Delegations               []proxy.CallerDelegationV1        `json:"delegations"`
}
type v2PolicyPool struct {
	Name    string             `json:"name"`
	Value   proxy.PoolValue    `json:"value"`
	Members []codex.AccountKey `json:"members"`
}
type v2PolicyEvidence struct {
	SchemaVersion     int                                  `json:"schema_version"`
	AccountKey        codex.AccountKey                     `json:"account_key"`
	AccountKeyHMAC    string                               `json:"account_key_hmac"`
	Workspace         string                               `json:"workspace"`
	Capability        string                               `json:"capability"`
	ProductSurface    string                               `json:"product_surface"`
	AccessPath        string                               `json:"access_path"`
	AuthMode          string                               `json:"auth_mode"`
	RequestedModel    string                               `json:"requested_model"`
	EffectiveModel    string                               `json:"effective_model"`
	Source            string                               `json:"source"`
	State             proxy.CapabilityRoutingEvidenceState `json:"state"`
	ObservedAt        time.Time                            `json:"observed_at"`
	ExpiresAt         *time.Time                           `json:"expires_at"`
	RoutingGeneration uint64                               `json:"routing_generation"`
	Authenticated     bool                                 `json:"authenticated"`
}

func v2PublicPolicy(p proxy.RoutingPolicyDocument) v2PolicyDocument {
	result := v2PolicyDocument{SchemaVersion: p.SchemaVersion, AuthorityGeneration: p.AuthorityGeneration, RoutingGeneration: p.RoutingGeneration, EffectiveGeneration: p.EffectiveGeneration, Pools: []v2PolicyPool{}, SessionBindings: append([]proxy.SessionBindingDocument{}, p.SessionBindings...), CapabilityEvidence: append([]proxy.CapabilityEvidenceV1{}, p.CapabilityEvidence...), CapabilityPredicates: append([]proxy.CapabilityPredicateCoreV1{}, p.CapabilityPredicates...), CapabilityRoutingEvidence: []v2PolicyEvidence{}, Delegations: append([]proxy.CallerDelegationV1{}, p.Delegations...)}
	if p.CapabilityPool != "" {
		result.CapabilityPool = &p.CapabilityPool
	}
	for _, pool := range p.Pools {
		result.Pools = append(result.Pools, v2PolicyPool{Name: pool.Name, Value: pool.Value, Members: append([]codex.AccountKey{}, pool.Members...)})
	}
	sort.Slice(result.SessionBindings, func(i, j int) bool {
		return result.SessionBindings[i].SessionDigest < result.SessionBindings[j].SessionDigest
	})
	for _, e := range p.CapabilityRoutingEvidence {
		var expires *time.Time
		if e.ExpiresAt != nil {
			utc := e.ExpiresAt.UTC()
			expires = &utc
		}
		result.CapabilityRoutingEvidence = append(result.CapabilityRoutingEvidence, v2PolicyEvidence{e.SchemaVersion, e.AccountKey, e.AccountKeyHMAC, e.Workspace, e.Capability, e.ProductSurface, e.AccessPath, e.AuthMode, e.RequestedModel, e.EffectiveModel, e.Source, e.State, e.ObservedAt.UTC(), expires, e.RoutingGeneration, e.Authenticated})
	}
	for i := range result.Delegations {
		result.Delegations[i].ExpiresAt = result.Delegations[i].ExpiresAt.UTC()
	}
	return result
}
func v2PolicyOutcome(p proxy.RoutingPolicyDocument, kind string) cli.Outcome {
	data := struct {
		Policy v2PolicyDocument `json:"policy"`
	}{v2PublicPolicy(p)}
	prefix := ""
	if kind == "pool" {
		prefix = "Pool policy updated.\n"
	}
	return v2PolicyResult(data, kind != "session", prefix)
}
func v2PolicyResult(data any, policyOnly bool, prefix string) cli.Outcome {
	raw, _ := json.Marshal(data)
	humanData := data
	if policyOnly {
		humanData = data.(struct {
			Policy v2PolicyDocument `json:"policy"`
		}).Policy
	}
	pretty, _ := json.MarshalIndent(humanData, "", "  ")
	// JSON escaping already quotes ASCII control characters; HumanValue also
	// neutralises Unicode control characters without changing JSON output.
	lines := strings.Split(string(pretty), "\n")
	for i, line := range lines {
		lines[i] = cli.HumanValue(line)
	}
	return cli.Outcome{Data: raw, Human: prefix + strings.Join(lines, "\n") + "\n"}
}
