package dots

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

type cloudHarnessReceiptStore interface {
	Acquire(string) (fsutil.ExclusiveLock, error)
	Load(string) (harness.Receipt, error)
	Save(harness.Receipt) error
}

type cloudHarnessGatewayTransport interface {
	Write(string, []byte) error
	ResumeParams(string, string) cloudMessage
	ResumeSources(string) []string
}

// The launcher owns the native connection. Config is changed only at an idle
// boundary, by releasing and immediately resuming that same subscription.
type cloudHarness struct {
	b        *cloudBridge
	op       sync.Mutex
	mu       sync.Mutex
	threads  map[string]*cloudHarnessThread
	requests map[string]cloudHarnessRequest
	calls    map[string]chan cloudMessage
	prefix   string
	sequence uint64
	wake     chan struct{}
	gateway  cloudHarnessGatewayTransport
}

type cloudHarnessThread struct {
	id, model, cwd       string
	baseline             cloudMessage
	settings             cloudMessage
	fullConfig           cloudMessage
	fullConfigCwd        string
	minimal              bool
	active, owned        bool
	subscriptions        map[string]bool
	sourceEpoch          uint64
	lastAttemptEpoch     uint64
	pendingTurns         int
	pendingSettings      int
	completedTurnID      string
	activeTurnID         string
	activityRevision     uint64
	awaitingRollout      bool
	ephemeral            bool
	selection            harness.Selection
	refresh              bool
	failureKey           string
	reportedPending      string
	lease                fsutil.ExclusiveLock
	receiptState         string
	plugins              []string
	pluginsKnown         bool
	restorePlugins       bool
	diverged             bool
	handoffReceipt       *harness.Receipt
	profilePlan          *cloudHarnessProfilePlan
	profileManaged       bool
	profileFingerprint   string
	profileRefresh       bool
	profileNeedsSettings bool
	pendingProfile       *cloudHarnessProfilePlan
	appliedPlugins       []string
	runtimeIssues        []string
	runtimeConfig        cloudMessage
}

type cloudHarnessRequest struct {
	method        string
	params        cloudMessage
	selection     harness.Selection
	minimal       bool
	fullConfig    cloudMessage
	fullConfigCwd string
	profilePlan   *cloudHarnessProfilePlan
}

func newCloudHarness(b *cloudBridge) *cloudHarness {
	if b.deps.readHarnessPolicy == nil && b.deps.readHarnessContextPolicy == nil {
		return nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil
	}
	return &cloudHarness{b: b, threads: make(map[string]*cloudHarnessThread), requests: make(map[string]cloudHarnessRequest), calls: make(map[string]chan cloudMessage), prefix: "cq-harness/" + hex.EncodeToString(nonce[:]) + "/", wake: make(chan struct{}, 1)}
}

func (h *cloudHarness) run() {
	defer h.b.workers.Done()
	h.b.guard("harness", func() {
		interval := h.b.deps.statusInterval
		if interval <= 0 || interval > 2*time.Second {
			interval = 2 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-h.b.ctx.Done():
				return
			case <-ticker.C:
			case <-h.wake:
			}
			func() {
				h.op.Lock()
				defer h.op.Unlock()
				h.mu.Lock()
				ids := make([]string, 0, len(h.threads))
				for id, t := range h.threads {
					if t.owned || t.lease != nil {
						ids = append(ids, id)
					}
				}
				h.mu.Unlock()
				for _, id := range ids {
					if h.b.ctx.Err() != nil {
						break
					}
					if h.refresh(id) != nil {
						continue
					}
					_ = h.reconcile(id, "", "")
				}
			}()
		}
	})
}

func (h *cloudHarness) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *cloudHarness) close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	var leases []fsutil.ExclusiveLock
	for _, thread := range h.threads {
		if thread.lease != nil {
			leases = append(leases, thread.lease)
			thread.lease = nil
		}
	}
	h.mu.Unlock()
	for _, lease := range leases {
		_ = lease.Close()
	}
}

func (h *cloudHarness) release(id string) {
	h.mu.Lock()
	thread := h.threads[id]
	var lease fsutil.ExclusiveLock
	if thread != nil {
		lease = thread.lease
		thread.lease = nil
	}
	h.mu.Unlock()
	if lease != nil {
		_ = lease.Close()
	}
}

func cloudHarnessPluginEqual(first, second []string) bool {
	first = slices.Clone(first)
	second = slices.Clone(second)
	slices.Sort(first)
	slices.Sort(second)
	return slices.Equal(first, second)
}

// The same per-chat lease is shared with the stock-Desktop plugin controller.
// A handoff may restore its baseline only while the observed values still match
// its last applied values; intervening user choices are never overwritten.
func (h *cloudHarness) acquire(id string) error {
	store := h.b.deps.harnessReceipts
	if store == nil {
		return nil
	}
	h.mu.Lock()
	thread := h.threads[id]
	if thread == nil || !thread.owned || thread.lease != nil {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	lease, err := store.Acquire(id)
	if err != nil {
		return errors.New("this chat's harness is owned by another controller")
	}
	receipt, err := store.Load(id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = lease.Close()
		return errors.New("native harness receipt unavailable")
	}
	h.mu.Lock()
	thread = h.threads[id]
	if thread == nil || !thread.owned {
		h.mu.Unlock()
		_ = lease.Close()
		return nil
	}
	thread.lease = lease
	if err == nil && receipt.Mode == "plugins" && (receipt.State == "applied" || receipt.State == "partial" || receipt.State == "prepared" || receipt.State == "pending") {
		matches := cloudHarnessPluginEqual(thread.plugins, receipt.AppliedPlugins) || cloudHarnessPluginEqual(thread.plugins, receipt.BaselinePlugins)
		if receipt.State == "prepared" || receipt.State == "pending" {
			matches = matches || cloudHarnessPluginEqual(thread.plugins, receipt.PendingPlugins)
		}
		if !thread.pluginsKnown || !matches {
			thread.diverged = true
			thread.handoffReceipt = &receipt
		} else {
			thread.baseline["disabledPluginIds"] = cloudHarnessRaw(receipt.BaselinePlugins)
			thread.restorePlugins = true
		}
	}
	h.mu.Unlock()
	return nil
}

func (h *cloudHarness) record(id string, selection harness.Selection, state string) error {
	h.mu.Lock()
	thread := h.threads[id]
	if thread == nil {
		h.mu.Unlock()
		return nil
	}
	baseline := []string{}
	_ = json.Unmarshal(thread.baseline["disabledPluginIds"], &baseline)
	plugins := slices.Clone(thread.plugins)
	model, cwd := thread.model, thread.cwd
	var unsupported []string
	issues := slices.Clone(thread.runtimeIssues)
	var pendingPlugins []string
	recordedPlan := thread.profilePlan
	if thread.pendingProfile != nil {
		recordedPlan = thread.pendingProfile
	}
	if recordedPlan != nil {
		unsupported = slices.Clone(recordedPlan.unsupported)
		issues = append(issues, recordedPlan.issues...)
		if state == "prepared" {
			pendingPlugins = slices.Clone(recordedPlan.plugins)
		}
	}
	if state == "applied" && len(thread.runtimeIssues) > 0 {
		state = "partial"
	}
	h.mu.Unlock()
	if store := h.b.deps.harnessReceipts; store != nil {
		receipt := harness.Receipt{Version: 1, ChatID: id, Mode: "full", Profile: selection.Profile, Source: selection.Source, State: state, BaselinePlugins: baseline, AppliedPlugins: plugins, PendingPlugins: pendingPlugins, UpdatedAt: time.Now().UTC(), Fingerprint: selection.Fingerprint, Model: model, Cwd: cwd, Unsupported: unsupported, Issues: issues}
		if err := store.Save(receipt); err != nil {
			h.event(id, selection, "failed")
			return errors.New("native harness receipt could not be saved")
		}
	}
	h.mu.Lock()
	if thread = h.threads[id]; thread != nil {
		thread.receiptState = state
		thread.selection = selection
	}
	h.mu.Unlock()
	if state != "prepared" {
		h.event(id, selection, state)
	}
	return nil
}

func cloudHarnessString(message cloudMessage, key string) string {
	var value string
	_ = json.Unmarshal(message[key], &value)
	return value
}
func cloudHarnessObject(raw json.RawMessage) cloudMessage {
	var value cloudMessage
	_ = json.Unmarshal(raw, &value)
	if value == nil {
		return make(cloudMessage)
	}
	return value
}
func cloudHarnessClone(value cloudMessage) cloudMessage {
	result := make(cloudMessage, len(value))
	for key, raw := range value {
		result[key] = bytes.Clone(raw)
	}
	return result
}
func cloudHarnessRaw(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

func cloudHarnessID(raw json.RawMessage) string {
	var id any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&id) != nil {
		return string(raw)
	}
	return string(cloudHarnessRaw(id))
}

func cloudHarnessModel(params cloudMessage) string {
	if model := cloudHarnessString(params, "model"); model != "" {
		return model
	}
	if model := cloudHarnessString(cloudHarnessObject(params["config"]), "model"); model != "" {
		return model
	}
	mode := cloudHarnessObject(params["collaborationMode"])
	return cloudHarnessString(cloudHarnessObject(mode["settings"]), "model")
}

func cloudHarnessRequestedModel(params cloudMessage) string {
	mode := cloudHarnessObject(params["collaborationMode"])
	if model := cloudHarnessString(cloudHarnessObject(mode["settings"]), "model"); model != "" {
		return model
	}
	return cloudHarnessModel(params)
}

// prepare runs on the input goroutine, never on the native output reader. Native
// responses continue flowing while a private config/reload RPC is in flight.
func (h *cloudHarness) prepare(request cloudMessage, raw []byte) ([]byte, error) {
	return h.prepareFrom("stdio", request, raw)
}

// A remote-control client has its own native subscription and request-ID space.
func (h *cloudHarness) prepareFrom(source string, request cloudMessage, raw []byte) ([]byte, error) {
	if h == nil {
		return raw, nil
	}
	method := request.method()
	if method != "thread/start" && method != "thread/resume" && method != "turn/start" && method != "thread/settings/update" && method != "thread/unsubscribe" && method != "thread/archive" {
		return raw, nil
	}
	if len(request["id"]) == 0 {
		return raw, nil
	}
	h.op.Lock()
	defer h.op.Unlock()
	params := cloudHarnessObject(request["params"])
	pending := cloudHarnessRequest{method: method, params: cloudHarnessClone(params)}
	if method == "thread/start" {
		model, cwd := cloudHarnessModel(params), cloudHarnessString(params, "cwd")
		if cwd == "" {
			if current, err := os.Getwd(); err == nil {
				cwd = current
			}
		}
		policy, err := h.readPolicy(cwd)
		if model != "" && !strings.HasPrefix(strings.ToLower(model), "glm-") && (err != nil || policy.Version != 2) {
			pending.selection = harness.Selection{Profile: harness.Full, Source: "model"}
			h.mu.Lock()
			h.requests[cloudHarnessRequestKey(source, request["id"])] = pending
			h.mu.Unlock()
			return raw, nil
		}
		if err != nil {
			return nil, errors.New("harness policy unavailable")
		}
		var native cloudMessage
		if policy.Enabled && model == "" {
			native, err = h.config(cwd)
			if err != nil {
				return nil, err
			}
			model = cloudHarnessString(cloudHarnessObject(native["config"]), "model")
		}
		pending.selection = policy.Resolve(model, cwd, "")
		if policy.Version == 2 && pending.selection.Managed {
			if native == nil {
				native, err = h.config(cwd)
				if err != nil {
					return nil, err
				}
			}
			snapshot := cloudHarnessThread{model: model, cwd: cwd, baseline: cloudHarnessClone(params)}
			plan, baseline, err := h.profilePlan(policy.ResolveEffective(model, cwd, ""), native, snapshot)
			if err != nil {
				return nil, err
			}
			pending.fullConfig = baseline
			pending.fullConfigCwd = cwd
			pending.profilePlan = &plan
			updated := cloudHarnessClone(params)
			updated["config"] = cloudHarnessRaw(plan.config)
			message := cloudHarnessClone(request)
			message["params"] = cloudHarnessRaw(updated)
			raw = append(cloudHarnessRaw(message), '\n')
		} else if pending.selection.Managed && pending.selection.Profile == harness.Minimal {
			if native == nil {
				native, err = h.config(cwd)
				if err != nil {
					return nil, err
				}
			}
			pending.fullConfig, err = h.baselineConfig(native, params["config"], "")
			if err != nil {
				return nil, err
			}
			pending.fullConfigCwd = cwd
			updated := cloudHarnessClone(params)
			updated["config"] = cloudHarnessRaw(cloudHarnessMinimalConfig(native, params["config"]))
			message := cloudHarnessClone(request)
			message["params"] = cloudHarnessRaw(updated)
			raw = append(cloudHarnessRaw(message), '\n')
			pending.minimal = true
		}
	} else if method == "turn/start" {
		id := cloudThreadID(request["params"])
		h.mu.Lock()
		if thread := h.threads[id]; thread != nil {
			thread.profileRefresh = true
		}
		h.mu.Unlock()
		if err := h.refresh(id); err != nil {
			return nil, errors.New("native harness current model unavailable")
		}
		if err := h.reconcile(id, cloudHarnessRequestedModel(params), cloudHarnessString(params, "cwd")); err != nil {
			return nil, err
		}
	}
	h.mu.Lock()
	if method == "turn/start" {
		if thread := h.threads[cloudThreadID(request["params"])]; thread != nil {
			thread.pendingTurns++
		}
	}
	h.requests[cloudHarnessRequestKey(source, request["id"])] = pending
	if method == "thread/settings/update" {
		if thread := h.threads[cloudThreadID(request["params"])]; thread != nil {
			thread.pendingSettings++
		}
	}
	h.mu.Unlock()
	return raw, nil
}

func (h *cloudHarness) config(cwd string) (cloudMessage, error) {
	params := map[string]any{"includeLayers": true}
	if cwd != "" {
		params["cwd"] = cwd
	}
	result, err := h.call("config/read", params)
	if err != nil {
		return nil, err
	}
	var config cloudMessage
	var layers []cloudMessage
	if json.Unmarshal(result["config"], &config) != nil || config == nil || json.Unmarshal(result["layers"], &layers) != nil || layers == nil {
		return nil, errors.New("native harness configuration layers unavailable")
	}
	return result, nil
}

func cloudHarnessConfigValue(object cloudMessage, path []string) json.RawMessage {
	if len(path) >= 2 && path[0] == "mcp_servers" {
		field := strings.Join(path[2:], ".")
		quoted, literal := "mcp_servers."+string(cloudHarnessRaw(path[1])), "mcp_servers.'"+path[1]+"'"
		if field != "" {
			quoted += "." + field
			literal += "." + field
		}
		for _, key := range []string{cloudHarnessMCPKey(path[1], field), quoted, literal} {
			if value, ok := object[key]; ok {
				return value
			}
		}
		for key, value := range object {
			if name, property, ok := cloudHarnessMCPPath(key); ok && name == path[1] && property == field {
				return value
			}
		}
	} else if value, ok := object[strings.Join(path, ".")]; ok {
		return value
	}
	current := object
	for index, key := range path {
		value, ok := current[key]
		if !ok {
			return nil
		}
		if index == len(path)-1 {
			return value
		}
		current = cloudHarnessObject(value)
	}
	return nil
}

// Only the server segment of a dotted override needs decoding. Native
// config/read still supplies the configuration; this is not a TOML parser.
func cloudHarnessMCPPath(key string) (name, property string, ok bool) {
	if !strings.HasPrefix(key, "mcp_servers.") {
		return "", "", false
	}
	tail := strings.TrimPrefix(key, "mcp_servers.")
	if tail == "" {
		return "", "", false
	}
	var rest string
	switch tail[0] {
	case '"':
		quoted, err := strconv.QuotedPrefix(tail)
		if err != nil {
			return "", "", false
		}
		name, err = strconv.Unquote(quoted)
		if err != nil {
			return "", "", false
		}
		rest = tail[len(quoted):]
	case '\'':
		end := strings.IndexByte(tail[1:], '\'')
		if end < 0 {
			return "", "", false
		}
		name = tail[1 : end+1]
		rest = tail[end+2:]
	default:
		if dot := strings.IndexByte(tail, '.'); dot >= 0 {
			name, rest = tail[:dot], tail[dot:]
		} else {
			name = tail
		}
	}
	if name == "" || (rest != "" && !strings.HasPrefix(rest, ".")) {
		return "", "", false
	}
	return name, strings.TrimPrefix(rest, "."), true
}

func cloudHarnessMCPKey(name, property string) string {
	bare := name != ""
	for _, character := range name {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			bare = false
			break
		}
	}
	if !bare {
		name = string(cloudHarnessRaw(name))
	}
	key := "mcp_servers." + name
	if property != "" {
		key += "." + property
	}
	return key
}

// The merged native values include effective MCP enabled defaults. Raw layers
// cover additional server definitions; origins select the winning layer when
// typed config omits a setting. Conflicting un-attributed values fail closed.
func cloudHarnessBaselineBool(native cloudMessage, original cloudMessage, path []string) (bool, bool, error) {
	decode := func(raw json.RawMessage) (bool, bool, error) {
		if len(raw) == 0 {
			return false, false, nil
		}
		var value bool
		if json.Unmarshal(raw, &value) != nil {
			return false, false, errors.New("native harness baseline setting is not boolean")
		}
		return value, true, nil
	}
	if raw := cloudHarnessConfigValue(original, path); len(raw) > 0 {
		return decode(raw)
	}
	if raw := cloudHarnessConfigValue(cloudHarnessObject(native["config"]), path); len(raw) > 0 {
		return decode(raw)
	}
	var layers []cloudMessage
	_ = json.Unmarshal(native["layers"], &layers)
	origins := cloudHarnessObject(native["origins"])
	for length := len(path); length > 0; length-- {
		origin := cloudHarnessObject(cloudHarnessConfigValue(origins, path[:length]))
		version := cloudHarnessString(origin, "version")
		if version != "" {
			for _, layer := range layers {
				if cloudHarnessString(layer, "disabledReason") == "" && cloudHarnessString(layer, "version") == version {
					if raw := cloudHarnessConfigValue(cloudHarnessObject(layer["config"]), path); len(raw) > 0 {
						return decode(raw)
					}
				}
			}
		}
	}
	var value bool
	found := false
	for _, layer := range layers {
		if cloudHarnessString(layer, "disabledReason") != "" {
			continue
		}
		next, ok, err := decode(cloudHarnessConfigValue(cloudHarnessObject(layer["config"]), path))
		if err != nil {
			return false, false, err
		}
		if ok {
			if found && next != value {
				return false, false, errors.New("native harness baseline layer precedence unavailable")
			}
			value, found = next, true
		}
	}
	return value, found, nil
}

func (h *cloudHarness) baselineConfig(native cloudMessage, originalRaw json.RawMessage, threadID string) (cloudMessage, error) {
	original := cloudHarnessObject(originalRaw)
	restore := cloudHarnessClone(original)
	minimal := cloudHarnessMinimalConfig(native, originalRaw)
	for key := range minimal {
		if key != "skills.include_instructions" && key != "features.plugins" && !(strings.HasPrefix(key, "mcp_servers.") && strings.HasSuffix(key, ".enabled")) {
			continue
		}
		var path []string
		switch key {
		case "skills.include_instructions":
			path = []string{"skills", "include_instructions"}
		case "features.plugins":
			path = []string{"features", "plugins"}
		default:
			name := strings.TrimSuffix(strings.TrimPrefix(key, "mcp_servers."), ".enabled")
			if strings.HasPrefix(name, "\"") {
				_ = json.Unmarshal([]byte(name), &name)
			}
			path = []string{"mcp_servers", name, "enabled"}
		}
		value, found, err := cloudHarnessBaselineBool(native, original, path)
		if err != nil {
			return nil, err
		}
		if !found && key == "features.plugins" {
			params := map[string]any{"limit": 1000}
			if threadID != "" {
				params["threadId"] = threadID
			}
			features, err := h.call("experimentalFeature/list", params)
			if err != nil {
				return nil, err
			}
			var rows []struct {
				Name    string
				Enabled bool
			}
			_ = json.Unmarshal(features["data"], &rows)
			for _, feature := range rows {
				if feature.Name == "plugins" {
					value, found = feature.Enabled, true
					break
				}
			}
			if !found {
				return nil, errors.New("native harness plugin baseline unavailable")
			}
		}
		if !found {
			value = true
		} // Native skills/MCP enabled defaults.
		restore[key] = cloudHarnessRaw(value)
	}
	return restore, nil
}

func (h *cloudHarness) refresh(id string) error {
	h.mu.Lock()
	thread := h.threads[id]
	needed := thread != nil && thread.owned && thread.refresh
	var revision uint64
	if thread != nil {
		revision = thread.activityRevision
	}
	h.mu.Unlock()
	if !needed {
		return nil
	}
	result, err := h.call("thread/read", map[string]any{"threadId": id, "includeTurns": false})
	if err != nil {
		return err
	}
	if cloudThreadID(cloudHarnessRaw(result)) != id {
		return errors.New("native harness read a different chat")
	}
	if cloudHarnessModel(cloudHarnessObject(result["thread"])) == "" {
		return errors.New("native harness current model unavailable")
	}
	h.mu.Lock()
	if thread = h.threads[id]; thread != nil {
		current := thread.activityRevision == revision
		if current && cloudHarnessString(cloudHarnessObject(cloudHarnessObject(result["thread"])["status"]), "type") == "idle" {
			if thread.activeTurnID != "" {
				thread.completedTurnID = thread.activeTurnID
				thread.activeTurnID = ""
				thread.activityRevision++
			}
		}
		cloudHarnessActualWithActivity(thread, result, current)
		thread.refresh = !current
	}
	h.mu.Unlock()
	return nil
}

// Native config/read is the authority for server names, including raw project
// layers which the merged serialised config can omit. No TOML is parsed here.
func cloudHarnessMinimalConfig(native cloudMessage, original json.RawMessage) cloudMessage {
	config := cloudHarnessClone(cloudHarnessObject(original))
	names := make(map[string]bool)
	collect := func(raw json.RawMessage) {
		object := cloudHarnessObject(raw)
		for name := range cloudHarnessObject(object["mcp_servers"]) {
			names[name] = true
		}
		for key := range object {
			if name, property, ok := cloudHarnessMCPPath(key); ok {
				names[name] = true
				if property == "enabled" {
					delete(config, key)
				}
			}
		}
	}
	collect(native["config"])
	collect(original)
	var layers []cloudMessage
	_ = json.Unmarshal(native["layers"], &layers)
	for _, layer := range layers {
		if cloudHarnessString(layer, "disabledReason") == "" {
			collect(layer["config"])
		}
	}
	config["skills.include_instructions"] = json.RawMessage("false")
	config["features.plugins"] = json.RawMessage("false")
	for name := range names {
		// Config overrides use TOML dotted paths. Quote each server name so a
		// name containing a dot cannot select a different configuration path.
		key := cloudHarnessMCPKey(name, "enabled")
		config[key] = json.RawMessage("false")
	}
	return config
}

func (h *cloudHarness) call(method string, params any) (cloudMessage, error) {
	return h.callOn("stdio", method, params)
}

func (h *cloudHarness) callOn(source, method string, params any) (cloudMessage, error) {
	if err := h.b.ctx.Err(); err != nil {
		return nil, err
	}
	timeout := h.b.deps.harnessTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	h.mu.Lock()
	h.sequence++
	id := h.prefix + fmt.Sprint(h.sequence)
	key := string(cloudHarnessRaw(id))
	reply := make(chan cloudMessage, 1)
	h.calls[key] = reply
	h.mu.Unlock()
	// The nonce plus issued sequence identifies late replies without retaining
	// an ever-growing table of completed calls.
	defer func() { h.mu.Lock(); delete(h.calls, key); h.mu.Unlock() }()
	raw := append(cloudHarnessRaw(map[string]any{"id": id, "method": method, "params": params}), '\n')
	err := func() error {
		if source != "stdio" {
			if h.gateway == nil {
				return errors.New("native gateway unavailable")
			}
			return h.gateway.Write(source, raw)
		}
		h.b.localWrite.Lock()
		defer h.b.localWrite.Unlock()
		if writer, ok := h.b.local.input.(interface{ SetWriteDeadline(time.Time) error }); ok {
			if writer.SetWriteDeadline(deadline) == nil {
				defer writer.SetWriteDeadline(time.Time{})
			}
		}
		_, err := h.b.local.input.Write(raw)
		return err
	}()
	if err != nil {
		return nil, errors.New("native harness request failed")
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-h.b.ctx.Done():
		return nil, h.b.ctx.Err()
	case <-timer.C:
		return nil, errors.New("native harness request timed out")
	case response := <-reply:
		if failure := bytes.TrimSpace(response["error"]); len(failure) > 0 && !bytes.Equal(failure, []byte("null")) {
			return nil, errors.New("native harness request rejected")
		}
		return cloudHarnessObject(response["result"]), nil
	}
}

func (h *cloudHarness) event(threadID string, selection harness.Selection, state string) {
	event := map[string]any{"harness_profile": string(selection.Profile), "source": selection.Source, "thread_id": threadID, "scope": "full", "state": state}
	if state == "pending" {
		h.mu.Lock()
		if thread := h.threads[threadID]; thread != nil {
			event["active"] = thread.active
			event["pending_turns"] = thread.pendingTurns
			event["pending_settings"] = thread.pendingSettings
			event["awaiting_rollout"] = thread.awaitingRollout
			event["ephemeral"] = thread.ephemeral
			event["refresh"] = thread.refresh
		}
		h.mu.Unlock()
	}
	h.b.event(event)
}

func (h *cloudHarness) reconcile(id, model, cwd string) error {
	requestedModel, requestedCwd := model, cwd
	newBaseline := false
	h.mu.Lock()
	thread := h.threads[id]
	if thread == nil || !thread.owned {
		h.mu.Unlock()
		h.release(id)
		return nil
	}
	snapshot := *thread
	snapshot.baseline = cloudHarnessClone(thread.baseline)
	snapshot.settings = cloudHarnessClone(thread.settings)
	h.mu.Unlock()
	retryRuntime := len(snapshot.runtimeIssues) > 0 && snapshot.sourceEpoch != snapshot.lastAttemptEpoch
	if model == "" {
		model = snapshot.model
	}
	if cwd == "" {
		cwd = snapshot.cwd
	}
	policy, err := h.readPolicy(cwd)
	if err != nil {
		if !snapshot.minimal && !snapshot.profileManaged && !snapshot.restorePlugins && !strings.HasPrefix(strings.ToLower(model), "glm-") {
			return nil
		}
		h.event(id, snapshot.selection, "failed")
		return errors.New("harness policy unavailable")
	}
	if policy.Version != 2 && !snapshot.minimal && !snapshot.profileManaged && !snapshot.restorePlugins && !strings.HasPrefix(strings.ToLower(model), "glm-") && !retryRuntime {
		return nil
	}
	selection := policy.Resolve(model, cwd, id)
	if !selection.Managed && !snapshot.profileManaged && !snapshot.minimal && !snapshot.restorePlugins && !retryRuntime {
		return nil
	}
	contextChanged := snapshot.fullConfig != nil && snapshot.fullConfigCwd != cwd
	if !policy.Enabled && !snapshot.minimal && !snapshot.profileManaged && !snapshot.restorePlugins && !retryRuntime {
		return nil
	}
	if err := h.acquire(id); err != nil {
		return err
	}
	h.mu.Lock()
	snapshot = *h.threads[id]
	snapshot.baseline = cloudHarnessClone(h.threads[id].baseline)
	snapshot.settings = cloudHarnessClone(h.threads[id].settings)
	h.mu.Unlock()
	retryRuntime = len(snapshot.runtimeIssues) > 0 && snapshot.sourceEpoch != snapshot.lastAttemptEpoch
	if snapshot.diverged {
		if snapshot.receiptState != "diverged" && snapshot.handoffReceipt != nil {
			receipt := *snapshot.handoffReceipt
			receipt.State = "diverged"
			if h.b.deps.harnessReceipts.Save(receipt) == nil {
				h.mu.Lock()
				h.threads[id].receiptState = "diverged"
				h.mu.Unlock()
			}
			h.event(id, selection, "failed")
		}
		return errors.New("chat plugin choices changed; harness baseline was left untouched")
	}
	minimal := selection.Managed && selection.Profile == harness.Minimal
	key := string(cloudHarnessRaw(policy)) + "\n" + model + "\n" + cwd + "\n" + string(cloudHarnessRaw(selection)) + "\n" + strconv.FormatUint(snapshot.sourceEpoch, 10)
	if snapshot.failureKey == key {
		return errors.New("native harness reload previously failed; profile was not applied")
	}
	if policy.Version != 2 && !snapshot.profileManaged && minimal == snapshot.minimal && !snapshot.restorePlugins && !contextChanged && !retryRuntime {
		state := cloudHarnessProfileState(nil, snapshot.runtimeIssues...)
		if selection.Managed && (snapshot.receiptState != state || snapshot.selection != selection) {
			return h.record(id, selection, state)
		}
		return nil
	}
	if policy.Version == 2 && snapshot.profilePlan != nil && snapshot.profileFingerprint == selection.Fingerprint && !snapshot.profileRefresh && !snapshot.profileNeedsSettings && !snapshot.restorePlugins && !contextChanged && !retryRuntime {
		state := cloudHarnessProfileState(snapshot.profilePlan, snapshot.runtimeIssues...)
		if snapshot.receiptState != state || snapshot.selection != selection {
			return h.record(id, selection, state)
		}
		return nil
	}
	if snapshot.active || snapshot.pendingTurns > 0 || snapshot.pendingSettings > 0 {
		if snapshot.reportedPending != key {
			h.mu.Lock()
			if current := h.threads[id]; current != nil {
				current.reportedPending = key
			}
			h.mu.Unlock()
			_ = h.record(id, selection, "pending")
		}
		return nil
	}
	var native cloudMessage
	var desiredPlan *cloudHarnessProfilePlan
	compiledPlugins := bytes.Clone(snapshot.baseline["disabledPluginIds"])
	if policy.Version == 2 && snapshot.profilePlan != nil && snapshot.profileFingerprint == selection.Fingerprint && !snapshot.profileRefresh && !contextChanged {
		desiredPlan = snapshot.profilePlan
	} else if policy.Version == 2 {
		native, err = h.config(cwd)
		if err != nil {
			return err
		}
		profileSnapshot := snapshot
		profileSnapshot.model, profileSnapshot.cwd = model, cwd
		plan, baseline, err := h.profilePlan(policy.ResolveEffective(model, cwd, id), native, profileSnapshot)
		if err != nil {
			return err
		}
		desiredPlan = &plan
		newBaseline = snapshot.fullConfig == nil || contextChanged
		h.mu.Lock()
		h.threads[id].fullConfig = baseline
		h.threads[id].fullConfigCwd = cwd
		h.mu.Unlock()
	} else if minimal {
		native, err = h.config(cwd)
		if err != nil {
			h.event(id, selection, "failed")
			return err
		}
		if snapshot.fullConfig == nil || contextChanged {
			snapshot.fullConfig, err = h.baselineConfig(native, snapshot.baseline["config"], id)
			if err != nil {
				return err
			}
			h.mu.Lock()
			h.threads[id].fullConfig = snapshot.fullConfig
			h.threads[id].fullConfigCwd = cwd
			newBaseline = true
			h.mu.Unlock()
		}
	}
	if desiredPlan != nil {
		h.mu.Lock()
		h.threads[id].pendingProfile = desiredPlan
		latest := *h.threads[id]
		latest.baseline = cloudHarnessClone(latest.baseline)
		latest.settings = cloudHarnessClone(latest.settings)
		h.mu.Unlock()
		if latest.active || latest.pendingTurns > 0 || latest.pendingSettings > 0 {
			_ = h.record(id, selection, "pending")
			return nil
		}
		if !bytes.Equal(compiledPlugins, latest.baseline["disabledPluginIds"]) || (requestedModel == "" && model != latest.model) || (requestedCwd == "" && cwd != latest.cwd) {
			if newBaseline {
				h.mu.Lock()
				h.threads[id].fullConfig = nil
				h.mu.Unlock()
			}
			return h.reconcile(id, requestedModel, requestedCwd)
		}
		snapshot = latest
		retryRuntime = len(snapshot.runtimeIssues) > 0 && snapshot.sourceEpoch != snapshot.lastAttemptEpoch
		if snapshot.profilePlan != nil && snapshot.profilePlan.key == desiredPlan.key && !snapshot.restorePlugins && !retryRuntime {
			if snapshot.profileNeedsSettings {
				if err := h.record(id, selection, "prepared"); err != nil {
					return err
				}
				if _, err := h.call("thread/settings/update", map[string]any{"threadId": id, "disabledPluginIds": desiredPlan.plugins}); err != nil {
					_ = h.record(id, selection, "failed")
					return err
				}
			}
			h.mu.Lock()
			if current := h.threads[id]; current != nil {
				current.profileNeedsSettings = false
				current.appliedPlugins = nil
				current.profileRefresh = false
				current.profileManaged = selection.Managed
				current.profileFingerprint = selection.Fingerprint
				current.profilePlan = desiredPlan
				current.pendingProfile = nil
				current.plugins = slices.Clone(desiredPlan.plugins)
				current.pluginsKnown = true
			}
			h.mu.Unlock()
			state := cloudHarnessProfileState(desiredPlan, snapshot.runtimeIssues...)
			if snapshot.receiptState != state || snapshot.selection != selection || snapshot.profileRefresh || snapshot.profileNeedsSettings {
				return h.record(id, selection, state)
			}
			return nil
		}
	}
	// Native thread/start has no durable rollout until its first turn finishes.
	// Releasing that subscription would make both resume and rollback impossible.
	// Ephemeral chats never acquire a durable rollout.
	if snapshot.awaitingRollout || snapshot.ephemeral {
		pluginsUpdated := false
		// Plugin settings do not require a reload. Apply them before admitting
		// the turn, even if policy changed before initial exclusions were sent.
		if desiredPlan != nil && (!snapshot.pluginsKnown || !cloudHarnessPluginEqual(snapshot.plugins, desiredPlan.plugins)) {
			if err := h.record(id, selection, "prepared"); err != nil {
				return err
			}
			if _, err := h.call("thread/settings/update", map[string]any{"threadId": id, "disabledPluginIds": desiredPlan.plugins}); err != nil {
				_ = h.record(id, selection, "failed")
				return err
			}
			h.mu.Lock()
			if current := h.threads[id]; current != nil {
				current.plugins = slices.Clone(desiredPlan.plugins)
				current.pluginsKnown = true
				current.profileNeedsSettings = true
				current.appliedPlugins = append([]string{}, desiredPlan.plugins...)
			}
			h.mu.Unlock()
			pluginsUpdated = true
		}
		if pluginsUpdated || snapshot.reportedPending != key || snapshot.receiptState != "pending" {
			h.mu.Lock()
			if current := h.threads[id]; current != nil {
				current.reportedPending = key
			}
			h.mu.Unlock()
			return h.record(id, selection, "pending")
		}
		return nil
	}
	if err := h.record(id, selection, "prepared"); err != nil {
		return err
	}
	// Resume can reuse an assembly retained by another native subscriber. Read
	// its actual MCP catalogue before reload; RPC success alone proves nothing.
	beforeRuntime, _ := h.readMCPRuntime(id)
	beforePlugins, beforePluginsErr := h.readPluginRuntime(id)
	// A native notification or an already forwarded turn can arrive while
	// config/read is in flight. Recheck before releasing the subscription.
	h.mu.Lock()
	current := h.threads[id]
	becameActive := current != nil && (current.active || current.pendingTurns > 0 || current.pendingSettings > 0)
	if current != nil {
		snapshot = *current
		snapshot.baseline = cloudHarnessClone(current.baseline)
		snapshot.settings = cloudHarnessClone(current.settings)
	}
	h.mu.Unlock()
	if becameActive {
		_ = h.record(id, selection, "pending")
		return nil
	}
	if (requestedModel == "" && model != snapshot.model) || (requestedCwd == "" && cwd != snapshot.cwd) {
		if newBaseline {
			h.mu.Lock()
			h.threads[id].fullConfig = nil
			h.mu.Unlock()
		}
		return h.reconcile(id, requestedModel, requestedCwd)
	}
	params := cloudHarnessResumeParams(snapshot, model, cwd)
	if desiredPlan != nil {
		params["config"] = cloudHarnessRaw(desiredPlan.config)
	} else if minimal {
		params["config"] = cloudHarnessRaw(cloudHarnessMinimalConfig(native, snapshot.baseline["config"]))
	}
	attemptEpoch := snapshot.sourceEpoch
	primary, restoreSubscriptions, err := h.suspendSubscriptions(id)
	if err != nil {
		h.mu.Lock()
		if current := h.threads[id]; current != nil {
			current.failureKey = key
		}
		h.mu.Unlock()
		_ = h.record(id, selection, "failed")
		return errors.New("native harness could not release this chat subscription")
	}
	defer func() { _ = restoreSubscriptions(primary) }()
	result, err := h.callOn(primary, "thread/resume", params)
	if err == nil && cloudThreadID(cloudHarnessRaw(result)) != id {
		err = errors.New("native harness resumed a different chat")
	}
	var resumedPlugins bool
	var resumedPluginsErr error
	if err == nil {
		// Observe the resumed assembly before replaying hot plugin settings.
		resumedPlugins, resumedPluginsErr = h.readPluginRuntime(id)
	}
	if err == nil {
		replay := cloudHarnessReplaySettings(snapshot, model, cwd)
		if desiredPlan != nil {
			replay["disabledPluginIds"] = cloudHarnessRaw(desiredPlan.plugins)
		}
		_, err = h.call("thread/settings/update", replay)
	}
	if err == nil {
		err = restoreSubscriptions(primary)
	}
	if err != nil {
		rollback := cloudHarnessResumeParams(snapshot, snapshot.model, snapshot.cwd)
		// Rollback must also release background owners of the attempted config.
		rollbackPrimary, rollbackRestore, rollbackErr := h.suspendSubscriptions(id)
		var rollbackResult cloudMessage
		if rollbackErr == nil {
			primary, restoreSubscriptions = rollbackPrimary, rollbackRestore
			rollbackResult, rollbackErr = h.callOn(primary, "thread/resume", rollback)
		}
		if rollbackErr == nil && cloudThreadID(cloudHarnessRaw(rollbackResult)) != id {
			rollbackErr = errors.New("native harness rollback changed chat identity")
		}
		if rollbackErr == nil {
			_, rollbackErr = h.call("thread/settings/update", cloudHarnessReplaySettings(snapshot, snapshot.model, snapshot.cwd))
		}
		if rollbackErr == nil {
			rollbackErr = restoreSubscriptions(primary)
		}
		h.mu.Lock()
		if current := h.threads[id]; current != nil {
			current.minimal = false
			current.profileManaged = false
			current.profileFingerprint = ""
			current.profilePlan = nil
			current.profileNeedsSettings = false
			current.appliedPlugins = nil
			current.selection = harness.Selection{Profile: harness.Full, Source: "rollback"}
			current.failureKey = key
			if rollbackErr != nil {
				current.owned = false
			}
		}
		h.mu.Unlock()
		_ = h.record(id, selection, "failed")
		return errors.New("native harness reload failed; profile was not applied")
	}
	previousConfig := snapshot.runtimeConfig
	if previousConfig == nil {
		previousConfig = snapshot.fullConfig
	}
	attemptedConfig := previousConfig
	if snapshot.profilePlan != nil {
		attemptedConfig = snapshot.profilePlan.config
	} else if snapshot.minimal {
		attemptedConfig = cloudHarnessMinimalConfig(nil, cloudHarnessRaw(snapshot.fullConfig))
	}
	desiredConfig := cloudHarnessObject(params["config"])
	runtimeIssues, reassembled := h.verifyMCPRuntime(id, desiredConfig, attemptedConfig, beforeRuntime)
	pluginsEnabled, pluginsErr := h.readPluginRuntime(id)
	if pluginsErr != nil {
		runtimeIssues = append(runtimeIssues, "native_plugin_runtime_unverified")
	} else if pluginsEnabled != cloudHarnessBool(desiredConfig, "features.plugins", true) {
		runtimeIssues = append(runtimeIssues, "native_plugin_runtime_retained")
	} else if beforePluginsErr == nil && resumedPluginsErr == nil && beforePlugins != resumedPlugins && resumedPlugins == pluginsEnabled {
		reassembled = true
	}
	if (cloudHarnessSkillsChanged(previousConfig, desiredConfig) || cloudHarnessSkillsChanged(attemptedConfig, desiredConfig) || slices.Contains(snapshot.runtimeIssues, "native_skill_runtime_unverified")) && !reassembled {
		runtimeIssues = append(runtimeIssues, "native_skill_runtime_unverified")
	}
	h.mu.Lock()
	if current := h.threads[id]; current != nil {
		current.minimal = minimal
		current.selection = selection
		current.model = model
		current.cwd = cwd
		current.failureKey = ""
		current.reportedPending = ""
		current.restorePlugins = false
		current.profileManaged = policy.Version == 2 && selection.Managed
		current.profileFingerprint = selection.Fingerprint
		current.profilePlan = desiredPlan
		current.pendingProfile = nil
		current.profileRefresh = false
		current.profileNeedsSettings = false
		current.appliedPlugins = nil
		current.runtimeIssues = runtimeIssues
		current.lastAttemptEpoch = attemptEpoch
		if len(runtimeIssues) == 0 {
			current.runtimeConfig = cloudHarnessClone(desiredConfig)
		}
		cloudHarnessActual(current, result)
		if desiredPlan != nil {
			current.plugins = slices.Clone(desiredPlan.plugins)
			current.pluginsKnown = true
		} else if !minimal || snapshot.restorePlugins {
			var plugins []string
			if json.Unmarshal(snapshot.baseline["disabledPluginIds"], &plugins) == nil {
				current.plugins = slices.Clone(plugins)
				current.pluginsKnown = true
			}
		}
	}
	h.mu.Unlock()
	return h.record(id, selection, cloudHarnessProfileState(desiredPlan, runtimeIssues...))
}

// Skills have no thread-scoped readback API. A changed MCP runtime can witness
// the same configuration reassembly; otherwise the skill edit is unverified.
func cloudHarnessSkillsChanged(previous, desired cloudMessage) bool {
	if cloudHarnessBool(previous, "skills.include_instructions", true) != cloudHarnessBool(desired, "skills.include_instructions", true) {
		return true
	}
	before, after := cloudHarnessSkillSettings(previous["skills.config"]), cloudHarnessSkillSettings(desired["skills.config"])
	for path, setting := range after {
		if cloudHarnessBool(setting, "enabled", true) != cloudHarnessBool(before[path], "enabled", true) {
			return true
		}
	}
	for path, setting := range before {
		if _, exists := after[path]; !exists && !cloudHarnessBool(setting, "enabled", true) {
			return true
		}
	}
	return false
}

func cloudHarnessActual(thread *cloudHarnessThread, result cloudMessage) {
	cloudHarnessActualWithActivity(thread, result, true)
}

func cloudHarnessObserved(thread *cloudHarnessThread, result cloudMessage, source string) {
	primary := cloudHarnessActivitySource(thread, source)
	if !primary && thread.subscriptions[source] {
		status := cloudHarnessString(cloudHarnessObject(cloudHarnessObject(result["thread"])["status"]), "type")
		if status == "active" {
			cloudHarnessStatus(thread, status)
			thread.refresh = true
		}
	}
	cloudHarnessActualWithActivity(thread, result, primary)
}

func cloudHarnessActualWithActivity(thread *cloudHarnessThread, result cloudMessage, activity bool) {
	if plugins, ok := result["disabledPluginIds"]; ok {
		var decoded []string
		if json.Unmarshal(plugins, &decoded) == nil {
			thread.plugins = slices.Clone(decoded)
			thread.pluginsKnown = true
		}
	}
	if model := cloudHarnessModel(result); model != "" {
		thread.model = model
	}
	if cwd := cloudHarnessString(result, "cwd"); cwd != "" {
		thread.cwd = cwd
	}
	returned := cloudHarnessObject(result["thread"])
	if raw, ok := returned["ephemeral"]; ok {
		_ = json.Unmarshal(raw, &thread.ephemeral)
	}
	if model := cloudHarnessModel(returned); model != "" {
		thread.model = model
	}
	if cwd := cloudHarnessString(returned, "cwd"); cwd != "" {
		thread.cwd = cwd
	}
	status := cloudHarnessString(cloudHarnessObject(returned["status"]), "type")
	if !activity {
		return
	}
	cloudHarnessStatus(thread, status)
}

// Current settings are distinct from the original harness config. Only accepted
// requests and authoritative native snapshots may update this replay state.
func cloudHarnessCaptureSettings(thread *cloudHarnessThread, source cloudMessage, actual bool) {
	if thread.settings == nil {
		thread.settings = make(cloudMessage)
	}
	for _, key := range []string{"approvalPolicy", "approvalsReviewer", "collaborationMode", "effort", "multiAgentMode", "permissions", "personality", "sandboxPolicy", "serviceTier", "summary", "runtimeWorkspaceRoots", "modelProvider"} {
		if value, ok := source[key]; ok {
			if !actual && key != "serviceTier" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			thread.settings[key] = bytes.Clone(value)
		}
	}
	if value, ok := source["permissions"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		delete(thread.settings, "sandboxPolicy")
	}
	if value, ok := source["sandboxPolicy"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		delete(thread.settings, "permissions")
	}
	if actual {
		if effort, ok := source["reasoningEffort"]; ok {
			thread.settings["effort"] = bytes.Clone(effort)
		}
		profile := cloudHarnessObject(source["activePermissionProfile"])
		if id := cloudHarnessString(profile, "id"); id != "" {
			thread.settings["permissions"] = cloudHarnessRaw(id)
			delete(thread.settings, "sandboxPolicy")
		} else if policy := cloudHarnessObject(source["sandbox"]); len(policy) > 0 {
			thread.settings["sandboxPolicy"] = cloudHarnessRaw(policy)
			delete(thread.settings, "permissions")
		}
	}
}

func cloudHarnessResumeParams(snapshot cloudHarnessThread, model, cwd string) cloudMessage {
	params := cloudHarnessClone(snapshot.baseline)
	if snapshot.fullConfig != nil {
		params["config"] = cloudHarnessRaw(snapshot.fullConfig)
	}
	for _, key := range []string{"approvalPolicy", "approvalsReviewer", "permissions", "personality", "serviceTier", "runtimeWorkspaceRoots", "modelProvider", "sandbox"} {
		delete(params, key)
	}
	for _, key := range []string{"approvalPolicy", "approvalsReviewer", "permissions", "personality", "serviceTier", "runtimeWorkspaceRoots", "modelProvider"} {
		if value, ok := snapshot.settings[key]; ok {
			params[key] = bytes.Clone(value)
		}
	}
	if _, named := params["permissions"]; !named {
		policy := cloudHarnessObject(snapshot.settings["sandboxPolicy"])
		if mode := map[string]string{"dangerFullAccess": "danger-full-access", "readOnly": "read-only", "workspaceWrite": "workspace-write"}[cloudHarnessString(policy, "type")]; mode != "" {
			params["sandbox"] = cloudHarnessRaw(mode)
		}
	}
	params["threadId"] = cloudHarnessRaw(snapshot.id)
	if model != "" {
		params["model"] = cloudHarnessRaw(model)
	}
	if cwd != "" {
		params["cwd"] = cloudHarnessRaw(cwd)
	}
	return params
}

func cloudHarnessReplaySettings(snapshot cloudHarnessThread, model, cwd string) cloudMessage {
	params := make(cloudMessage)
	for _, key := range []string{"approvalPolicy", "approvalsReviewer", "collaborationMode", "effort", "multiAgentMode", "permissions", "personality", "sandboxPolicy", "serviceTier", "summary"} {
		if value, ok := snapshot.settings[key]; ok {
			params[key] = bytes.Clone(value)
		}
	}
	if mode := cloudHarnessObject(params["collaborationMode"]); len(mode) > 0 {
		settings := cloudHarnessObject(mode["settings"])
		if model != "" {
			settings["model"] = cloudHarnessRaw(model)
		}
		if effort, ok := snapshot.settings["effort"]; ok {
			settings["reasoning_effort"] = bytes.Clone(effort)
		}
		mode["settings"] = cloudHarnessRaw(settings)
		params["collaborationMode"] = cloudHarnessRaw(mode)
	}
	params["threadId"] = cloudHarnessRaw(snapshot.id)
	if model != "" {
		params["model"] = cloudHarnessRaw(model)
	}
	if cwd != "" {
		params["cwd"] = cloudHarnessRaw(cwd)
	}
	if plugins, ok := snapshot.baseline["disabledPluginIds"]; ok {
		params["disabledPluginIds"] = bytes.Clone(plugins)
	}
	return params
}

func (h *cloudHarness) observe(raw []byte) bool {
	return h.observeFrom("stdio", raw)
}

func cloudHarnessRequestKey(source string, id json.RawMessage) string {
	if source == "stdio" {
		return cloudHarnessID(id)
	}
	return source + "\x00" + cloudHarnessID(id)
}

// The caller holds h.mu. An existing unlabelled owner is the stdio connection.
func cloudHarnessSubscription(thread *cloudHarnessThread, source string, subscribed bool) {
	if thread.subscriptions == nil {
		thread.subscriptions = make(map[string]bool)
		if thread.owned {
			thread.subscriptions["stdio"] = true
		}
	}
	if thread.subscriptions[source] != subscribed {
		thread.sourceEpoch++
		thread.activityRevision++
		if thread.active || len(thread.runtimeIssues) != 0 || thread.failureKey != "" {
			thread.refresh = true
		}
	}
	if subscribed {
		thread.subscriptions[source] = true
	} else {
		delete(thread.subscriptions, source)
	}
	thread.owned = len(thread.subscriptions) != 0
}

// A retained native subscription survives relay disconnects. Retry failed
// assembly only after its existing source has a concrete writer again.
func (h *cloudHarness) gatewayRecovered(source string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	changed := false
	for _, thread := range h.threads {
		candidate := h.gateway != nil && slices.Contains(h.gateway.ResumeSources(thread.id), source)
		if thread.subscriptions[source] || candidate {
			thread.sourceEpoch++
			thread.activityRevision++
			thread.refresh = true
			changed = true
		}
	}
	h.mu.Unlock()
	if changed {
		h.signal()
	}
}

// A large resume response can be chunked and remain opaque. Its original
// request still identifies a possible owner; unsubscribe verifies membership.
func (h *cloudHarness) gatewayCandidateChanged(id string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	thread := h.threads[id]
	changed := thread != nil && thread.owned
	if changed {
		thread.sourceEpoch++
		if len(thread.runtimeIssues) != 0 || thread.failureKey != "" {
			thread.refresh = true
		}
	}
	h.mu.Unlock()
	if changed {
		h.signal()
	}
}

func (h *cloudHarness) observeFrom(source string, raw []byte) bool {
	if h == nil {
		return false
	}
	var message cloudMessage
	if json.Unmarshal(raw, &message) != nil {
		return false
	}
	id := cloudHarnessID(message["id"])
	h.mu.Lock()
	var internalID string
	_ = json.Unmarshal(message["id"], &internalID)
	issued := false
	if strings.HasPrefix(internalID, h.prefix) {
		suffix := strings.TrimPrefix(internalID, h.prefix)
		sequence, err := strconv.ParseUint(suffix, 10, 64)
		issued = err == nil && sequence > 0 && sequence <= h.sequence && strconv.FormatUint(sequence, 10) == suffix
	}
	_, hasResult := message["result"]
	_, hasError := message["error"]
	if message.method() == "" && (hasResult || hasError) && issued {
		reply := h.calls[id]
		if reply != nil {
			select {
			case reply <- message:
			default:
			}
		}
		h.mu.Unlock()
		return true
	}
	requestKey := cloudHarnessRequestKey(source, message["id"])
	var pending cloudHarnessRequest
	found := false
	if message.method() == "" && (hasResult || hasError) {
		pending, found = h.requests[requestKey]
	}
	if found {
		delete(h.requests, requestKey)
	}
	failed := len(message["error"]) > 0 && !bytes.Equal(bytes.TrimSpace(message["error"]), []byte("null"))
	if found && pending.method == "turn/start" {
		if thread := h.threads[cloudThreadID(cloudHarnessRaw(pending.params))]; thread != nil && thread.pendingTurns > 0 {
			thread.pendingTurns--
		}
	}
	if found && pending.method == "thread/settings/update" {
		if thread := h.threads[cloudThreadID(cloudHarnessRaw(pending.params))]; thread != nil && thread.pendingSettings > 0 {
			thread.pendingSettings--
		}
	}
	if found && !failed {
		result := cloudHarnessObject(message["result"])
		threadID := cloudThreadID(cloudHarnessRaw(result))
		if threadID == "" {
			threadID = cloudThreadID(cloudHarnessRaw(pending.params))
		}
		thread := h.threads[threadID]
		switch pending.method {
		case "thread/start", "thread/resume":
			if threadID != "" {
				if thread == nil {
					thread = &cloudHarnessThread{id: threadID, baseline: cloudHarnessClone(pending.params)}
					h.threads[threadID] = thread
				} else if !thread.owned {
					for key, value := range pending.params {
						thread.baseline[key] = bytes.Clone(value)
					}
				}
				cloudHarnessSubscription(thread, source, true)
				if pending.method == "thread/start" {
					thread.awaitingRollout = true
					_ = json.Unmarshal(pending.params["ephemeral"], &thread.ephemeral)
					thread.minimal = pending.minimal
					thread.selection = pending.selection
					thread.fullConfig = pending.fullConfig
					thread.fullConfigCwd = pending.fullConfigCwd
					thread.profilePlan = pending.profilePlan
					thread.profileManaged = pending.profilePlan != nil
					thread.profileFingerprint = pending.selection.Fingerprint
					if pending.profilePlan != nil {
						thread.runtimeConfig = cloudHarnessClone(pending.profilePlan.config)
					} else if pending.minimal {
						thread.runtimeConfig = cloudHarnessMinimalConfig(nil, cloudHarnessRaw(pending.fullConfig))
					}
				}
				cloudHarnessObserved(thread, result, source)
				cloudHarnessCaptureSettings(thread, pending.params, false)
				cloudHarnessCaptureSettings(thread, result, true)
				if plugin, ok := result["disabledPluginIds"]; ok {
					if pending.method == "thread/resume" && thread.profileManaged {
						cloudHarnessManualPlugins(thread, plugin)
					} else {
						thread.baseline["disabledPluginIds"] = bytes.Clone(plugin)
					}
				}
				if pending.profilePlan != nil {
					thread.profileNeedsSettings = !cloudHarnessPluginEqual(thread.plugins, pending.profilePlan.plugins)
				}
			}
		case "turn/start":
			if thread != nil {
				turn := cloudHarnessObject(result["turn"])
				turnID := cloudHarnessString(turn, "id")
				status := cloudHarnessString(turn, "status")
				if cloudHarnessActivitySource(thread, source) || (status != "completed" && status != "interrupted" && status != "failed") {
					if thread.activeTurnID != "" && thread.activeTurnID != turnID {
						thread.activityRevision++
						thread.refresh = true
					} else if status == "completed" || status == "interrupted" || status == "failed" {
						cloudHarnessTurnCompleted(thread, turnID)
					} else {
						cloudHarnessTurnStarted(thread, turnID)
						if !cloudHarnessActivitySource(thread, source) {
							thread.refresh = true
						}
					}
				}
				cloudHarnessObserved(thread, result, source)
				cloudHarnessCaptureSettings(thread, pending.params, false)
				cloudHarnessCaptureSettings(thread, result, true)
				if cloudHarnessModel(result) == "" && (thread.profileManaged || thread.minimal || strings.HasPrefix(strings.ToLower(thread.model), "glm-") || strings.HasPrefix(strings.ToLower(cloudHarnessRequestedModel(pending.params)), "glm-")) {
					thread.refresh = true
				}
				if plugin, ok := pending.params["disabledPluginIds"]; ok {
					cloudHarnessManualPlugins(thread, plugin)
				}
			}
		case "thread/settings/update":
			if thread != nil {
				cloudHarnessObserved(thread, result, source)
				cloudHarnessCaptureSettings(thread, pending.params, false)
				cloudHarnessCaptureSettings(thread, result, true)
				thread.refresh = true
				if plugin, ok := pending.params["disabledPluginIds"]; ok {
					cloudHarnessManualPlugins(thread, plugin)
				}
			}
		case "thread/unsubscribe":
			if thread != nil {
				cloudHarnessSubscription(thread, source, false)
			}
		case "thread/archive":
			if thread != nil {
				thread.subscriptions = nil
				thread.owned = false
			}
		}
	}
	params := cloudHarnessObject(message["params"])
	threadID := cloudThreadID(message["params"])
	if thread := h.threads[threadID]; thread != nil {
		switch message.method() {
		case "turn/started":
			if cloudHarnessActivitySource(thread, source) {
				cloudHarnessTurnStarted(thread, cloudHarnessString(cloudHarnessObject(params["turn"]), "id"))
			} else if thread.subscriptions[source] {
				turnID := cloudHarnessString(cloudHarnessObject(params["turn"]), "id")
				thread.activityRevision++
				if thread.activeTurnID == "" || thread.activeTurnID == turnID {
					cloudHarnessTurnStarted(thread, turnID)
				}
				thread.refresh = true
			}
		case "turn/completed":
			if cloudHarnessActivitySource(thread, source) {
				cloudHarnessTurnCompleted(thread, cloudHarnessString(cloudHarnessObject(params["turn"]), "id"))
			} else if thread.subscriptions[source] {
				// Secondary completion can follow the last active read. Keep
				// the guard until native readback confirms this chat is idle.
				thread.activityRevision++
				thread.refresh = true
			}
		case "thread/status/changed":
			status := cloudHarnessObject(params["status"])
			kind := cloudHarnessString(status, "type")
			if cloudHarnessActivitySource(thread, source) {
				cloudHarnessStatus(thread, kind)
			} else if thread.subscriptions[source] && kind == "active" {
				cloudHarnessStatus(thread, kind)
				thread.refresh = true
			} else if thread.subscriptions[source] && kind == "idle" {
				thread.activityRevision++
				thread.refresh = true
			}
		}
		cloudHarnessObserved(thread, params, source)
		if message.method() == "thread/settings/updated" {
			cloudHarnessActual(thread, cloudHarnessObject(params["threadSettings"]))
			cloudHarnessCaptureSettings(thread, cloudHarnessObject(params["threadSettings"]), true)
			if plugin, ok := cloudHarnessObject(params["threadSettings"])["disabledPluginIds"]; ok && thread.profileManaged {
				cloudHarnessManualPlugins(thread, plugin)
			}
		}
	}
	h.mu.Unlock()
	if found || message.method() == "turn/completed" || message.method() == "thread/status/changed" || message.method() == "thread/settings/updated" {
		h.signal()
	}
	return false
}
