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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const proxyPolicyMaxBytes = 1 << 20

var proxyPolicyNow = time.Now

type proxyPolicyOptions struct {
	StateRoot string
	File      string
}

type proxyPolicyDependencies struct {
	LoadConfig     func() (*proxy.Config, error)
	Doer           httputil.Doer
	Stdin          io.Reader
	ListInventory  func(context.Context) (codexprov.Inventory, error)
	LoadAliasIndex func() (codexprov.AccountAliasIndex, error)
}

func runProxyPolicy(args []string, output io.Writer) error {
	fsys := fsutil.OSFileSystem{}
	var home string
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("proxy policy redirect refused")
		},
	}
	return runProxyPolicyWithDependencies(context.Background(), args, output, proxyPolicyDependencies{
		LoadConfig: proxy.LoadConfig,
		Doer:       client,
		Stdin:      os.Stdin,
		ListInventory: func(ctx context.Context) (codexprov.Inventory, error) {
			inventory, resolvedHome, err := listProxyCodexDefaultInventory(ctx, fsys)
			home = resolvedHome
			return inventory, err
		},
		LoadAliasIndex: func() (codexprov.AccountAliasIndex, error) {
			return (codexprov.Registry{FS: fsys, Home: home}).AccountAliasIndex()
		},
	})
}

func runProxyPolicyWithDependencies(ctx context.Context, args []string, output io.Writer, deps proxyPolicyDependencies) error {
	if len(args) == 0 {
		return errors.New("usage: cq proxy policy <initialise|apply|status|pool|session>")
	}
	command := args[0]
	if command == "pool" {
		return runProxyPolicyPool(ctx, args[1:], output, deps)
	}
	if command == "session" {
		return runProxyPolicySession(ctx, args[1:], output, deps)
	}
	options, err := parseProxyPolicyOptions(args[1:])
	if err != nil {
		return err
	}
	if output == nil {
		return errors.New("proxy policy output unavailable")
	}
	switch command {
	case "initialise":
		if options.StateRoot == "" || options.File != "" {
			return errors.New("usage: cq proxy policy initialise --state-root DIR")
		}
		if err := proxy.InitialiseProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: options.StateRoot, Random: rand.Reader, Now: proxyPolicyNow}); err != nil {
			return err
		}
		cfg, err := proxy.LoadConfig()
		if err != nil {
			return err
		}
		cfg.ProxyResilienceStateDir = options.StateRoot
		if err := proxy.SaveConfig(cfg); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "proxy resilience state initialised: %s\n", options.StateRoot)
		return err
	case "apply":
		if options.File == "" {
			return errors.New("usage: cq proxy policy apply --file FILE [--state-root DIR]")
		}
		policy, err := readProxyRoutingPolicy(options.File)
		if err != nil {
			return err
		}
		if options.StateRoot == "" {
			current, err := proxyPolicyControl(ctx, deps, http.MethodPut, proxy.RuntimePolicyPath, 0, policy)
			if err != nil {
				return err
			}
			return writeProxyPolicyJSON(output, current)
		}
		root, err := resolveProxyPolicyRoot(options.StateRoot)
		if err != nil {
			return err
		}
		state, err := proxy.OpenProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: root, Random: rand.Reader, Now: proxyPolicyNow})
		if err != nil {
			return err
		}
		defer state.Close()
		if err := state.Routing.PublishDocument(policy); err != nil {
			return err
		}
		current, err := state.Routing.Document()
		if err != nil {
			return err
		}
		return writeProxyPolicyJSON(output, current)
	case "status":
		if options.File != "" {
			return errors.New("usage: cq proxy policy status [--state-root DIR]")
		}
		if options.StateRoot == "" {
			current, err := proxyPolicyControl(ctx, deps, http.MethodGet, proxy.RuntimePolicyPath, 0, nil)
			if err != nil {
				return err
			}
			return writeProxyPolicyJSON(output, current)
		}
		root, err := resolveProxyPolicyRoot(options.StateRoot)
		if err != nil {
			return err
		}
		state, err := proxy.OpenProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{FS: fsutil.OSFileSystem{}, Root: root, Random: rand.Reader, Now: proxyPolicyNow})
		if err != nil {
			return err
		}
		defer state.Close()
		current, err := state.Routing.Document()
		if err != nil {
			return err
		}
		return writeProxyPolicyJSON(output, current)
	default:
		return fmt.Errorf("unknown proxy policy command: %s", command)
	}
}

func proxyPolicyControl(ctx context.Context, deps proxyPolicyDependencies, method, path string, port int, requestValue any) (proxy.RoutingPolicyDocument, error) {
	if ctx == nil || deps.LoadConfig == nil || deps.Doer == nil {
		return proxy.RoutingPolicyDocument{}, errors.New("proxy policy control unavailable")
	}
	cfg, err := deps.LoadConfig()
	if err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	if port == 0 {
		port = cfg.Port
		if port == 0 {
			port = proxy.DefaultPort
		}
	}
	var body io.Reader = http.NoBody
	if requestValue != nil {
		encoded, err := json.Marshal(requestValue)
		if err != nil {
			return proxy.RoutingPolicyDocument{}, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), body)
	if err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := deps.Doer.Do(request)
	if err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	if response == nil || response.Body == nil {
		return proxy.RoutingPolicyDocument{}, errors.New("proxy policy response unavailable")
	}
	defer response.Body.Close()
	responseBody, err := httputil.ReadBody(response.Body)
	if err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if path == proxy.RuntimePolicyPoolPath {
			var failure struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(responseBody, &failure) == nil {
				switch failure.Error {
				case "invalid_pool_name":
					return proxy.RoutingPolicyDocument{}, errors.New("invalid pool name")
				case "pool_not_found":
					return proxy.RoutingPolicyDocument{}, errors.New("pool not found")
				case "pool_name_conflict":
					return proxy.RoutingPolicyDocument{}, errors.New("pool name already exists")
				case "pool_mutation_rejected":
					return proxy.RoutingPolicyDocument{}, errors.New("pool mutation rejected")
				}
			}
		}
		return proxy.RoutingPolicyDocument{}, fmt.Errorf("proxy policy control failed: HTTP %d", response.StatusCode)
	}
	var policy proxy.RoutingPolicyDocument
	if err := json.Unmarshal(responseBody, &policy); err != nil {
		return proxy.RoutingPolicyDocument{}, errors.New("proxy policy response invalid")
	}
	return policy, nil
}

func proxyPolicySessionDigest(ctx context.Context, deps proxyPolicyDependencies, port int, session []byte) (string, error) {
	if ctx == nil || deps.LoadConfig == nil || deps.Doer == nil {
		return "", errors.New("proxy policy control unavailable")
	}
	cfg, err := deps.LoadConfig()
	if err != nil {
		return "", err
	}
	if port == 0 {
		port = cfg.Port
		if port == 0 {
			port = proxy.DefaultPort
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", port, proxy.RuntimePolicySessionDigestPath), bytes.NewReader(session))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := deps.Doer.Do(request)
	if err != nil {
		return "", err
	}
	if response == nil || response.Body == nil {
		return "", errors.New("proxy policy response unavailable")
	}
	defer response.Body.Close()
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("proxy policy control failed: HTTP %d", response.StatusCode)
	}
	var result struct {
		SessionDigest string `json:"session_digest"`
	}
	if json.Unmarshal(body, &result) != nil || !validProxyPolicyDigest(result.SessionDigest) {
		return "", errors.New("proxy policy digest response invalid")
	}
	return result.SessionDigest, nil
}

func nextProxyRoutingPolicy(current proxy.RoutingPolicyDocument) proxy.RoutingPolicyDocument {
	if current.SchemaVersion != 1 {
		return proxy.RoutingPolicyDocument{SchemaVersion: 1, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1}
	}
	current.AuthorityGeneration++
	current.RoutingGeneration++
	current.EffectiveGeneration = current.RoutingGeneration
	return current
}

func runProxyPolicyPool(ctx context.Context, args []string, output io.Writer, deps proxyPolicyDependencies) error {
	if len(args) == 0 {
		return errors.New("usage: cq proxy policy pool <set|rename|value>")
	}
	if args[0] == "rename" || args[0] == "value" {
		if len(args) < 3 {
			if args[0] == "rename" {
				return errors.New("usage: cq proxy policy pool rename OLD_NAME NEW_NAME [--port PORT]")
			}
			return errors.New("usage: cq proxy policy pool value NAME VALUE [--port PORT]")
		}
		port := 0
		for index := 3; index < len(args); index++ {
			if args[index] != "--port" || port != 0 {
				return fmt.Errorf("proxy policy pool: unknown option %s", args[index])
			}
			value, next, err := parseProxyPolicyPort(args, index)
			if err != nil {
				return errors.New("proxy policy pool: invalid port")
			}
			port, index = value, next
		}
		mutation := proxy.PoolMutationRequest{Operation: args[0], Name: args[1]}
		if args[0] == "rename" {
			mutation.NewName = args[2]
		} else {
			value, err := strconv.ParseUint(args[2], 10, 32)
			if err != nil {
				return errors.New("proxy policy pool: invalid value")
			}
			mutation.Value = proxy.PoolValue(value)
		}
		updated, err := proxyPolicyControl(ctx, deps, http.MethodPost, proxy.RuntimePolicyPoolPath, port, mutation)
		if err != nil {
			if args[0] == "rename" {
				return fmt.Errorf("proxy policy pool rename %q to %q: %w", args[1], args[2], err)
			}
			return fmt.Errorf("proxy policy pool value %q: %w", args[1], err)
		}
		return writeProxyPolicyJSON(output, updated)
	}
	if len(args) < 2 || args[0] != "set" {
		return errors.New("usage: cq proxy policy pool set NAME --account ACCOUNT [--account ACCOUNT ...] [--value VALUE] [--port PORT]")
	}
	name := args[1]
	var references []string
	port := 0
	var value proxy.PoolValue
	valueSet := false
	for index := 2; index < len(args); index++ {
		switch args[index] {
		case "--account":
			if index+1 >= len(args) {
				return errors.New("proxy policy pool: --account requires a value")
			}
			references = append(references, args[index+1])
			index++
		case "--port":
			value, next, err := parseProxyPolicyPort(args, index)
			if err != nil || port != 0 {
				return errors.New("proxy policy pool: invalid port")
			}
			port, index = value, next
		case "--value":
			if index+1 >= len(args) || valueSet {
				return errors.New("proxy policy pool: invalid value")
			}
			parsed, err := strconv.ParseUint(args[index+1], 10, 32)
			if err != nil {
				return errors.New("proxy policy pool: invalid value")
			}
			value = proxy.PoolValue(parsed)
			valueSet = true
			index++
		default:
			return fmt.Errorf("proxy policy pool: unknown option %s", args[index])
		}
	}
	if name == "" || len(references) == 0 || deps.ListInventory == nil || deps.LoadAliasIndex == nil {
		return errors.New("usage: cq proxy policy pool set NAME --account ACCOUNT [--account ACCOUNT ...] [--value VALUE] [--port PORT]")
	}
	inventory, err := deps.ListInventory(ctx)
	if err != nil || proxyCodexDefaultInventoryIncomplete(inventory) {
		return errors.New("list Codex account inventory: unavailable")
	}
	aliases, err := deps.LoadAliasIndex()
	if err != nil {
		return errors.New("load Codex account aliases: unavailable")
	}
	members := make([]codexprov.AccountKey, 0, len(references))
	seen := make(map[codexprov.AccountKey]struct{}, len(references))
	for _, reference := range references {
		member, err := codexprov.ResolveAccountReference(inventory, aliases, reference)
		if err != nil {
			return err
		}
		if _, duplicate := seen[member]; duplicate {
			return errors.New("proxy policy pool: duplicate account")
		}
		seen[member] = struct{}{}
		members = append(members, member)
	}
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
	current, err := proxyPolicyControl(ctx, deps, http.MethodGet, proxy.RuntimePolicyPath, port, nil)
	if err != nil {
		return err
	}
	next := nextProxyRoutingPolicy(current)
	replaced := false
	for index := range next.Pools {
		if strings.EqualFold(next.Pools[index].Name, name) {
			next.Pools[index].Members = members
			if valueSet {
				next.Pools[index].Value = value
			}
			replaced = true
		}
	}
	if !replaced {
		next.Pools = append(next.Pools, proxy.AccountPoolDocument{Name: name, Value: value, Members: members})
	}
	sort.Slice(next.Pools, func(i, j int) bool { return next.Pools[i].Name < next.Pools[j].Name })
	updated, err := proxyPolicyControl(ctx, deps, http.MethodPut, proxy.RuntimePolicyPath, port, next)
	if err != nil {
		return err
	}
	return writeProxyPolicyJSON(output, updated)
}

type proxyPolicySessionOptions struct {
	Pool      string
	Digest    string
	SessionID []byte
	Port      int
}

func runProxyPolicySession(ctx context.Context, args []string, output io.Writer, deps proxyPolicyDependencies) error {
	if len(args) == 0 {
		return errors.New("usage: cq proxy policy session <bind|show|list|unbind|digest>")
	}
	command := args[0]
	options, err := parseProxyPolicySessionOptions(args[1:], deps.Stdin)
	if err != nil {
		return err
	}
	defer zeroProxyPolicyBytes(options.SessionID)
	if command == "list" {
		if options.Pool != "" || options.Digest != "" || len(options.SessionID) != 0 {
			return errors.New("usage: cq proxy policy session list [--port PORT]")
		}
		current, err := proxyPolicyControl(ctx, deps, http.MethodGet, proxy.RuntimePolicyPath, options.Port, nil)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(current.SessionBindings)
	}
	if options.Digest == "" {
		if len(options.SessionID) == 0 {
			return errors.New("proxy policy session: session selector required")
		}
		options.Digest, err = proxyPolicySessionDigest(ctx, deps, options.Port, options.SessionID)
		if err != nil {
			return err
		}
	}
	if command == "digest" {
		if options.Pool != "" {
			return errors.New("usage: cq proxy policy session digest (--session-id ID | --session-id-stdin) [--port PORT]")
		}
		return json.NewEncoder(output).Encode(map[string]string{"session_digest": options.Digest})
	}
	current, err := proxyPolicyControl(ctx, deps, http.MethodGet, proxy.RuntimePolicyPath, options.Port, nil)
	if err != nil {
		return err
	}
	match := -1
	for index, binding := range current.SessionBindings {
		if binding.SessionDigest == options.Digest {
			match = index
			break
		}
	}
	switch command {
	case "show":
		if options.Pool != "" || match < 0 {
			return errors.New("proxy policy session binding not found")
		}
		return json.NewEncoder(output).Encode(current.SessionBindings[match])
	case "bind":
		if options.Pool == "" {
			return errors.New("proxy policy session bind: --pool is required")
		}
		poolName := ""
		for _, pool := range current.Pools {
			if strings.EqualFold(pool.Name, options.Pool) {
				poolName = pool.Name
			}
		}
		if poolName == "" {
			return errors.New("proxy policy session bind: pool not found")
		}
		next := nextProxyRoutingPolicy(current)
		binding := proxy.SessionBindingDocument{SessionDigest: options.Digest, Pool: poolName}
		if match < 0 {
			next.SessionBindings = append(next.SessionBindings, binding)
		} else {
			next.SessionBindings[match] = binding
		}
		sort.Slice(next.SessionBindings, func(i, j int) bool {
			return next.SessionBindings[i].SessionDigest < next.SessionBindings[j].SessionDigest
		})
		updated, err := proxyPolicyControl(ctx, deps, http.MethodPut, proxy.RuntimePolicyPath, options.Port, next)
		if err != nil {
			return err
		}
		return writeProxyPolicyJSON(output, updated)
	case "unbind":
		if options.Pool != "" || match < 0 {
			return errors.New("proxy policy session binding not found")
		}
		next := nextProxyRoutingPolicy(current)
		next.SessionBindings = append(next.SessionBindings[:match], next.SessionBindings[match+1:]...)
		updated, err := proxyPolicyControl(ctx, deps, http.MethodPut, proxy.RuntimePolicyPath, options.Port, next)
		if err != nil {
			return err
		}
		return writeProxyPolicyJSON(output, updated)
	default:
		return fmt.Errorf("unknown proxy policy session command: %s", command)
	}
}

func parseProxyPolicySessionOptions(args []string, stdin io.Reader) (proxyPolicySessionOptions, error) {
	var options proxyPolicySessionOptions
	selectors := 0
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--pool", "--session-id", "--digest":
			if index+1 >= len(args) {
				return options, fmt.Errorf("proxy policy session: %s requires a value", args[index])
			}
			value := args[index+1]
			switch args[index] {
			case "--pool":
				if options.Pool != "" {
					return options, errors.New("proxy policy session: duplicate --pool")
				}
				options.Pool = value
			case "--session-id":
				selectors++
				options.SessionID = []byte(value)
			case "--digest":
				selectors++
				if !validProxyPolicyDigest(value) {
					return options, errors.New("proxy policy session: invalid digest")
				}
				options.Digest = value
			}
			index++
		case "--session-id-stdin":
			selectors++
			if stdin == nil {
				return options, errors.New("proxy policy session: stdin unavailable")
			}
			value, err := io.ReadAll(io.LimitReader(stdin, 4097))
			if err != nil || len(value) > 4096 {
				return options, errors.New("proxy policy session: invalid stdin session ID")
			}
			options.SessionID = value
		case "--port":
			value, next, err := parseProxyPolicyPort(args, index)
			if err != nil || options.Port != 0 {
				return options, errors.New("proxy policy session: invalid port")
			}
			options.Port, index = value, next
		default:
			return options, fmt.Errorf("proxy policy session: unknown option %s", args[index])
		}
	}
	if selectors > 1 {
		zeroProxyPolicyBytes(options.SessionID)
		return proxyPolicySessionOptions{}, errors.New("proxy policy session: choose exactly one session selector")
	}
	return options, nil
}

func parseProxyPolicyPort(args []string, index int) (int, int, error) {
	if index+1 >= len(args) {
		return 0, index, errors.New("port requires a value")
	}
	port, err := strconv.Atoi(args[index+1])
	if err != nil || port < 1 || port > 65535 {
		return 0, index, errors.New("invalid port")
	}
	return port, index + 1, nil
}

func validProxyPolicyDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func zeroProxyPolicyBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func parseProxyPolicyOptions(args []string) (proxyPolicyOptions, error) {
	var options proxyPolicyOptions
	for index := 0; index < len(args); index++ {
		if index+1 >= len(args) {
			return options, fmt.Errorf("proxy policy: %s requires a value", args[index])
		}
		value := args[index+1]
		switch args[index] {
		case "--state-root":
			if options.StateRoot != "" {
				return options, errors.New("proxy policy: duplicate --state-root")
			}
			clean := filepath.Clean(value)
			if !filepath.IsAbs(value) || clean != value || clean == string(filepath.Separator) {
				return options, errors.New("proxy policy: state root must be a clean absolute non-root path")
			}
			options.StateRoot = value
		case "--file":
			if options.File != "" {
				return options, errors.New("proxy policy: duplicate --file")
			}
			options.File = value
		default:
			return options, fmt.Errorf("proxy policy: unknown option %s", args[index])
		}
		index++
	}
	return options, nil
}

func resolveProxyPolicyRoot(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	cfg, err := proxy.LoadExistingConfig()
	if err != nil {
		return "", err
	}
	if cfg.ProxyResilienceStateDir == "" {
		return "", errors.New("proxy resilience state is not configured")
	}
	return cfg.ProxyResilienceStateDir, nil
}

func readProxyRoutingPolicy(path string) (proxy.RoutingPolicyDocument, error) {
	file, err := os.Open(path)
	if err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, proxyPolicyMaxBytes+1))
	if err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	if len(body) > proxyPolicyMaxBytes {
		return proxy.RoutingPolicyDocument{}, errors.New("proxy policy exceeds 1 MiB")
	}
	var policy proxy.RoutingPolicyDocument
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return proxy.RoutingPolicyDocument{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return proxy.RoutingPolicyDocument{}, errors.New("proxy policy contains trailing data")
	}
	return policy, nil
}

func writeProxyPolicyJSON(output io.Writer, policy proxy.RoutingPolicyDocument) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(policy)
}
