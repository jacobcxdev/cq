package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestHelpListsExactDirectChildren(t *testing.T) {
	groups := 0
	for path, page := range terminalHelpByPath {
		var want []string
		for _, command := range catalogue {
			parent := ""
			if i := strings.LastIndex(command.Path, " "); i >= 0 {
				parent = command.Path[:i]
			}
			if parent == path {
				want = append(want, command.Path[strings.LastIndex(command.Path, " ")+1:])
			}
		}
		if len(want) == 0 {
			continue
		}
		if path != "" {
			groups++
		}
		var got []string
		for _, command := range page.Commands {
			got = append(got, command.Label)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q terminal children = %v, want %v", path, got, want)
		}
		reference, ok := Help(path)
		if !ok {
			t.Fatalf("missing reference %q", path)
		}
		section := strings.SplitN(reference, "Commands:\n", 2)
		if len(section) != 2 {
			t.Fatalf("missing Commands section %q", path)
		}
		sectionText := strings.SplitN(section[1], "\n\n", 2)[0]
		got = nil
		for _, line := range strings.Split(sectionText, "\n") {
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
				got = append(got, strings.TrimSpace(line))
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q reference children = %v, want %v", path, got, want)
		}
	}
	if groups != 37 {
		t.Fatalf("checked %d groups, want 37", groups)
	}
}

func TestHelpRelatedCommandsResolve(t *testing.T) {
	for path, page := range terminalHelpByPath {
		seen := map[string]bool{}
		reference, _ := Help(path)
		for _, related := range page.Related {
			target := strings.TrimPrefix(related.Label, "cq ")
			if _, ok := terminalHelpByPath[target]; !ok || target == path || seen[target] || related.Description == "" {
				t.Errorf("invalid related entry %q: %+v", path, related)
			}
			seen[target] = true
			if !strings.Contains(reference, related.Label) || !strings.Contains(reference, related.Description) {
				t.Errorf("reference missing related entry %q: %q", path, target)
			}
		}
	}
	for _, path := range []string{"proxy"} {
		reference, _ := Help(path)
		terminal, _ := terminalHelp(path, 100, false)
		for _, want := range []string{"cq codex proxy reserve", "cq codex proxy policy", "cq codex proxy", "cq claude proxy pin", "cq service"} {
			if !strings.Contains(reference, want) || !strings.Contains(terminal, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
	}
}
