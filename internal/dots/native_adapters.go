package dots

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

//go:embed native_room_cache.js
var nativeRoomCacheJS string

//go:embed native_room_cache_guard.js
var nativeRoomCacheGuardJS string

//go:embed native_display_names.js
var nativeDisplayNamesJS string

//go:embed native_send.js
var nativeSendJS string

//go:embed native_matching_reply.js
var nativeMatchingReplyJS string

func nativeJSON(v any) string { data, _ := json.Marshal(v); return string(data) }
func nativeAsset(source string, s *Settings) string {
	source = strings.ReplaceAll(source, "MODULE_ASSET_PLACEHOLDER", s.ModuleAsset)
	var bindings string
	if b := s.NativeBindings; b != nil {
		bindings = "Ymr:m." + b.API + ",GMr:m." + b.Account + ",XMr:m." + b.User + ",JMr:m." + b.Ready + ",Lpr:m." + b.Cache + ",VAn:m." + b.Executor + ",CQNativeOwnedReads:true"
	} else {
		switch s.ModuleAsset {
		case "app-initial-f3e281fb9a71.js":
			bindings = "Ymr:m.iRr,GMr:m.zRr,XMr:m.BRr,JMr:m.yLr,Lpr:m.B_r,VAn:m.lPn"
		case nativeOwnedModuleAsset:
			bindings = "Ymr:m._lr,GMr:m.Hlr,XMr:m.Ulr,JMr:m.mcr,Lpr:m.g2n,VAn:m.Nen,CQNativeOwnedReads:true"
		}
	}
	if bindings != "" {
		module := "import('app://-/assets/" + s.ModuleAsset + "')"
		// Adapt the import namespace, never identifiers inside user message text.
		source = strings.Replace(source, module, module+".then(m=>({...m,"+bindings+"}))", 1)
	}
	return source
}
func nativeOne(ctx context.Context, e NativeEvaluator, expression string) (any, error) {
	if single, ok := e.(interface {
		Evaluate(context.Context, string) (any, error)
	}); ok {
		return single.Evaluate(ctx, expression)
	}
	values, err := e.EvaluateAll(ctx, expression)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, errors.New("native renderer unavailable")
	}
	return values[0], nil
}
func (e *nativeEvaluator) Evaluate(ctx context.Context, expression string) (any, error) {
	if e.settings == nil || expression == "" {
		return nil, errors.New("native debugger expression invalid")
	}
	if len(expression) > nativeMaxRequest-1024 {
		return nil, errors.New("native debugger expression too large")
	}
	if err := e.check(); err != nil {
		return nil, err
	}
	if e.settings.IntegrationMode != "native" {
		return e.evaluateIsolated(ctx, expression)
	}
	pid, err := nativeProcessPID(ctx, e.settings, 0)
	if err != nil {
		return nil, err
	}
	response, err := nativeRequest(ctx, e.settings, map[string]any{"operation": "evaluate", "app_pid": pid, "expression": expression})
	if err != nil {
		return nil, err
	}
	if response["app_pid"] != float64(pid) {
		return nil, errors.New("native debugger app identity changed")
	}
	return response["value"], nil
}

func InstallNativeRoomCache(ctx context.Context, s *Settings, e NativeEvaluator) (map[string]any, error) {
	if s.IntegrationMode != "native" {
		return map[string]any{"installed": false}, nil
	}
	profiles := make([]string, 0, len(s.Dots))
	for _, dot := range s.Dots {
		profiles = append(profiles, dot.ProfileID)
	}
	sort.Strings(profiles)
	expression := nativeAsset(strings.ReplaceAll(strings.ReplaceAll(nativeRoomCacheJS, "PROFILES", nativeJSON(profiles)), "GUARD", nativeRoomCacheGuardJS), s)
	values, err := e.EvaluateAll(ctx, expression)
	if err != nil {
		return nil, err
	}
	windows, reads := 0, float64(0)
	for _, value := range values {
		row, ok := value.(map[string]any)
		if !ok || row["installed"] != true {
			continue
		}
		windows++
		if count, ok := row["ownedReads"].(float64); ok {
			reads += count
		}
	}
	if windows == 0 {
		return nil, errors.New("native Dot read recovery unavailable")
	}
	return map[string]any{"installed": true, "windows": windows, "ownedReads": reads}, nil
}
func NativeDisplayNames(ctx context.Context, s *Settings, e NativeEvaluator) (map[string]string, error) {
	value, err := nativeOne(ctx, e, nativeAsset(nativeDisplayNamesJS, s))
	if err != nil {
		return nil, err
	}
	rows, ok := value.([]any)
	if !ok || len(rows) != len(s.Dots) {
		return nil, errors.New("native catalogue identity changed")
	}
	expected := map[string]string{}
	for handle, dot := range s.Dots {
		expected[dot.ProfileID] = handle
	}
	result := map[string]string{}
	for _, value := range rows {
		row, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("native catalogue identity changed")
		}
		id, ok := row["id"].(string)
		handle, exists := expected[id]
		if !ok || !exists {
			return nil, errors.New("native catalogue identity changed")
		}
		delete(expected, id)
		name, ok := row["name"].(string)
		if !ok || !validText(name, 80) {
			return nil, errors.New("native catalogue name invalid")
		}
		result[handle] = name
	}
	if len(expected) > 0 {
		return nil, errors.New("native catalogue identity changed")
	}
	return result, nil
}
func nativeSystemDot(s *Settings) (Dot, error) {
	for _, dot := range s.Dots {
		if dot.OwnerAlias == s.SystemAlias {
			return dot, nil
		}
	}
	return Dot{}, errors.New("system Dot unavailable")
}
func nativeUUID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	h := hex.EncodeToString(id[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
func NativeSend(ctx context.Context, s *Settings, e NativeEvaluator, target, text, ledger string) (map[string]any, error) {
	system, err := nativeSystemDot(s)
	if err != nil {
		return nil, err
	}
	dot, ok := s.Dots[target]
	if !ok || text == "" || len([]rune(text)) > 4000 {
		return nil, errors.New("research message outside bound")
	}
	single, singleOK := e.(interface {
		Evaluate(context.Context, string) (any, error)
	})
	if !singleOK {
		return nil, errors.New("native send requires a single renderer evaluator")
	}
	id, err := nativeUUID()
	if err != nil {
		return nil, err
	}
	summary := map[string]any{"at": float64(time.Now().UnixMilli()) / 1000, "target": target, "request_id": id, "outcome": "pending"}
	data, _ := json.Marshal(summary)
	file, err := os.OpenFile(ledger, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, err = file.Write(data)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	expression := fmt.Sprintf(nativeSendJS, nativeJSON(map[string]any{"profile": dot.ProfileID, "text": text, "request": id}), nativeJSON(system.ProfileID))
	value, evaluateErr := single.Evaluate(ctx, nativeAsset(expression, s))
	summary["outcome"] = "unknown"
	if evaluateErr != nil {
		summary["failure_type"] = "native_evaluation_failed"
	} else if result, ok := value.(map[string]any); ok {
		for k, v := range result {
			summary[k] = v
		}
		if result["accepted"] == true {
			summary["outcome"] = "accepted"
		} else if status, ok := result["status"].(float64); ok && status >= 400 && status < 500 && status != 408 {
			summary["outcome"] = "rejected"
		}
	}
	if err = SaveJSON(ledger, summary); err != nil {
		return nil, err
	}
	return summary, nil
}
func NativeMatchingReply(ctx context.Context, s *Settings, e NativeEvaluator, target, marker, excludeID string) (map[string]any, error) {
	if _, err := nativeSystemDot(s); err != nil {
		return nil, err
	}
	dot, ok := s.Dots[target]
	if !ok || marker == "" {
		return nil, errors.New("research target invalid")
	}
	var excluded any
	if excludeID != "" {
		excluded = excludeID
	}
	expression := fmt.Sprintf(nativeMatchingReplyJS, nativeJSON(dot.ProfileID), nativeJSON(excluded), nativeJSON(marker))
	value, err := nativeOne(ctx, e, nativeAsset(expression, s))
	if err != nil {
		return nil, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("native reply result invalid")
	}
	return result, nil
}

// ProjectNativeResponse sets presentation hints without rewriting resource identities.
func ProjectNativeResponse(value any, viewerIDs map[string]bool, direct bool) int {
	object, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	if direct {
		return projectNativeMessage(object, viewerIDs)
	}
	count := 0
	for _, key := range []string{"items", "latest_messages"} {
		messages, _ := object[key].([]any)
		for _, message := range messages {
			if m, ok := message.(map[string]any); ok {
				count += projectNativeMessage(m, viewerIDs)
			}
		}
	}
	return count
}
func projectNativeMessage(m map[string]any, viewerIDs map[string]bool) int {
	count := 0
	role, exists := m["role"]
	if !exists {
		role = "user"
	}
	id, _ := m["account_user_id"].(string)
	_, raw := m["raw_messages"]
	if !raw && role == "user" && id != "" && viewerIDs[id] {
		m["is_self"] = true
		count++
	}
	if reply, ok := m["reply_to"].(map[string]any); ok {
		if message, ok := reply["message"].(map[string]any); ok {
			count += projectNativeMessage(message, viewerIDs)
		}
	}
	return count
}
