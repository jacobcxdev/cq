package main

import (
	"strings"
	"testing"
)

func TestRequireLiveNormalStatusAcceptsMatchingSuccess(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	body := `{"sha":"` + sha + `","statuses":[{"context":"cq/live-normal-routing","state":"success"}]}`
	if err := requireLiveNormalStatus(strings.NewReader(body), sha); err != nil {
		t.Fatalf("require live normal status: %v", err)
	}
}

func TestRequireLiveNormalStatusRejectsUnprovedCommit(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "missing status",
			body: `{"sha":"` + sha + `","statuses":[]}`,
		},
		{
			name: "failed status",
			body: `{"sha":"` + sha + `","statuses":[{"context":"cq/live-normal-routing","state":"failure"}]}`,
		},
		{
			name: "latest status failed",
			body: `{"sha":"` + sha + `","statuses":[{"context":"cq/live-normal-routing","state":"failure"},{"context":"cq/live-normal-routing","state":"success"}]}`,
		},
		{
			name: "stale commit",
			body: `{"sha":"2222222222222222222222222222222222222222","statuses":[{"context":"cq/live-normal-routing","state":"success"}]}`,
		},
		{
			name: "oversized response",
			body: `{"sha":"` + sha + `","statuses":[{"context":"cq/live-normal-routing","state":"success"}]}` + strings.Repeat(" ", (1<<20)+1),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := requireLiveNormalStatus(strings.NewReader(test.body), sha); err == nil {
				t.Fatal("accepted unproved release commit")
			}
		})
	}
}
