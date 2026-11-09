package dots

import (
	"slices"
)

// Callers hold h.op. Each native connection owns a separate subscription.
func (h *cloudHarness) suspendSubscriptions(id string) (string, func(string) error, error) {
	h.mu.Lock()
	var sources []string
	if thread := h.threads[id]; thread != nil && thread.owned {
		for source := range thread.subscriptions {
			sources = append(sources, source)
		}
		if thread.subscriptions == nil {
			sources = append(sources, "stdio")
		}
	}
	h.mu.Unlock()
	if len(sources) == 0 {
		return "", nil, &cloudHarnessRPCError{reason: "subscription_missing"}
	}
	restoreSource := make(map[string]bool, len(sources))
	for _, source := range sources {
		restoreSource[source] = true
	}
	// A successful large resume response can be chunked. Probe the sources
	// whose original request targeted this chat without parsing its history.
	if h.gateway != nil {
		sources = append(sources, h.gateway.ResumeSources(id)...)
	}
	slices.Sort(sources)
	sources = slices.Compact(sources)
	resume := make(map[string]cloudMessage, len(sources))
	for _, source := range sources {
		params := cloudMessage{"threadId": cloudHarnessRaw(id), "excludeTurns": cloudHarnessRaw(true)}
		if source != "stdio" && h.gateway != nil {
			if original := h.gateway.ResumeParams(source, id); original != nil {
				params = cloudHarnessClone(original)
				params["threadId"] = cloudHarnessRaw(id)
				params["excludeTurns"] = cloudHarnessRaw(true)
				delete(params, "history")
				delete(params, "path")
			}
		}
		resume[source] = params
	}
	restored := false
	restore := func(except string) error {
		if restored {
			return nil
		}
		var failure error
		for _, source := range sources {
			if source == except || !restoreSource[source] {
				continue
			}
			result, err := h.callOn(source, "thread/resume", resume[source])
			if err != nil || cloudThreadID(cloudHarnessRaw(result)) != id {
				if err != nil {
					failure = err
				} else {
					failure = &cloudHarnessRPCError{reason: "invalid_response"}
				}
			}
		}
		restored = failure == nil
		return failure
	}
	for _, source := range sources {
		result, err := h.callOn(source, "thread/unsubscribe", map[string]string{"threadId": id})
		status := cloudHarnessString(result, "status")
		if err != nil || (status != "unsubscribed" && status != "notSubscribed" && status != "notLoaded") {
			if err == nil {
				err = &cloudHarnessRPCError{reason: "invalid_response"}
			}
			return "", nil, &cloudHarnessSubscriptionError{cause: err, rollback: restore("")}
		}
		restoreSource[source] = status == "unsubscribed"
	}
	primary := ""
	for _, source := range sources {
		if restoreSource[source] && (primary == "" || source == "stdio") {
			primary = source
		}
	}
	if primary == "" {
		return "", nil, &cloudHarnessRPCError{reason: "subscription_missing"}
	}
	return primary, restore, nil
}
