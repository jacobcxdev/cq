package modelregistry

// Publication is the receipt for a specific refresh and its actual cache writes.
// It is shared with the local proxy; it contains no provider-native payloads.
type Publication struct {
	Status      string              `json:"status"`
	Via         string              `json:"via"`
	ActiveCount int                 `json:"active_count"`
	Sources     []PublicationSource `json:"sources"`
	Targets     []PublicationTarget `json:"targets"`
}
type PublicationSource struct {
	Provider       string  `json:"provider"`
	Status         string  `json:"status"`
	NativeCount    int     `json:"native_count"`
	MalformedCount int     `json:"malformed_count"`
	ErrorCode      *string `json:"error_code"`
	Message        *string `json:"message"`
}
type PublicationTarget struct {
	Target    string  `json:"target"`
	Path      string  `json:"path"`
	Status    string  `json:"status"`
	Reason    string  `json:"reason"`
	ErrorCode *string `json:"error_code"`
	Message   *string `json:"message"`
}
type ModelIdentity struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

func PublicProvider(p Provider) string {
	if p == ProviderAnthropic {
		return "claude"
	}
	return string(p)
}
func NewPublication(diag RefreshDiagnostics, active int, targets []PublicationTarget, via string) Publication {
	p := Publication{Via: via, ActiveCount: active, Sources: []PublicationSource{}, Targets: targets}
	if p.Targets == nil {
		p.Targets = []PublicationTarget{}
	}
	for _, provider := range []Provider{ProviderAnthropic, ProviderCodex} {
		s := PublicationSource{Provider: PublicProvider(provider), Status: "refreshed", NativeCount: diag.Counts[string(provider)], MalformedCount: diag.MalformedCounts[provider]}
		_, present := diag.Counts[string(provider)]
		if diag.SourceErrors[provider] != nil || !present {
			s.Status = "failed"
			code, message := "models_source_failed", "Model source refresh failed."
			s.ErrorCode = &code
			s.Message = &message
		}
		p.Sources = append(p.Sources, s)
	}
	p.RecomputeStatus()
	return p
}
func (p *Publication) RecomputeStatus() {
	success, failed := false, len(p.Targets) == 0
	for _, s := range p.Sources {
		if s.Status == "refreshed" {
			success = true
		} else {
			failed = true
		}
	}
	for _, t := range p.Targets {
		if t.Status == "written" {
			success = true
		}
		if t.Status == "failed" {
			failed = true
		}
	}
	p.Status = "complete"
	if failed {
		p.Status = "failed"
		if success {
			p.Status = "partial"
		}
	}
}
