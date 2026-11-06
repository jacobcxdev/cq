package dots

// Stdio is ordered locally. Idle transitions follow one subscribed source;
// secondary activity keeps a conservative guard until a fresh native read.
func cloudHarnessActivitySource(thread *cloudHarnessThread, source string) bool {
	if thread.subscriptions == nil || thread.subscriptions["stdio"] {
		return source == "stdio"
	}
	first := ""
	for candidate := range thread.subscriptions {
		if first == "" || candidate < first {
			first = candidate
		}
	}
	return first != "" && source == first
}

func cloudHarnessTurnStarted(thread *cloudHarnessThread, id string) {
	if id != "" && id == thread.completedTurnID {
		return
	}
	if !thread.active || thread.activeTurnID != id {
		thread.activityRevision++
	}
	thread.active = true
	thread.activeTurnID = id
}

func cloudHarnessTurnCompleted(thread *cloudHarnessThread, id string) {
	if thread.activeTurnID != "" && id != thread.activeTurnID {
		thread.refresh = true
		return
	}
	if thread.active || thread.activeTurnID != "" || thread.completedTurnID != id {
		thread.activityRevision++
	}
	thread.active = false
	thread.activeTurnID = ""
	thread.awaitingRollout = false
	thread.completedTurnID = id
}

func cloudHarnessStatus(thread *cloudHarnessThread, kind string) {
	if kind == "active" {
		thread.activityRevision++
		thread.active = true
	} else if kind == "idle" {
		thread.activityRevision++
		if thread.activeTurnID != "" {
			thread.refresh = true
		} else if thread.active {
			thread.active = false
		}
	}
}
