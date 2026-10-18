package dots

import (
	"context"
	"net/url"
	"regexp"
)

var probeComputerOperation = regexp.MustCompile(`^/backend-api/tbo/([^/]+)/computers/([^/]+)/(connect|connect-and-replace|disconnect)$`)

func probeComputerTarget(ctx context.Context, s *Settings, c *CatalogueClient, path string, body []byte, f *CatalogueFixture) (string, error) {
	parsed, err := url.Parse(path)
	if err != nil || len(body) > 0 || f == nil {
		return "", nil
	}
	match := probeComputerOperation.FindStringSubmatch(parsed.EscapedPath())
	if match == nil {
		return "", nil
	}
	profileID, err := url.PathUnescape(match[1])
	if err != nil {
		return "", nil
	}
	owner, ok := f.Owners[profileID]
	if !ok || owner.Alias == s.SystemAlias {
		return "", nil
	}
	var profile map[string]any
	count := 0
	for _, candidate := range f.Profiles {
		if candidate["id"] == profileID {
			profile = candidate
			count++
		}
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || count != 1 || len(query) != 1 || len(query["expected_thread_id"]) != 1 || query.Get("expected_thread_id") != profile["active_root_thread_id"] {
		return "", nil
	}
	rootOwner, ok := f.Owners[probeString(profile["active_root_thread_id"])]
	if !ok || !probeEqual(owner, rootOwner) {
		return "", nil
	}
	row, err := SecondaryExecutorMapping(s, owner.Alias)
	if err != nil {
		return "", err
	}
	environment := ""
	if row != nil && row["profile_id"] == profileID && (match[2] == row["environment_id"] || match[2] == row["primary_environment_id"]) {
		environment = probeString(row["environment_id"])
	} else if match[3] == "disconnect" && validExecutorEnvironment(match[2]) {
		value, err := c.Get(ctx, owner, "/backend-api/tbo/"+match[1]+"/computers", nil)
		if err != nil {
			return "", nil
		}
		computers, ok := value["computers"].([]any)
		if !ok {
			return "", nil
		}
		matches := 0
		for _, item := range computers {
			computer, ok := item.(map[string]any)
			if !ok {
				return "", nil
			}
			if computer["kind"] == "user" && computer["environment_id"] == match[2] && computer["can_disconnect"] == true {
				matches++
			}
		}
		if matches != 1 {
			return "", nil
		}
		environment = match[2]
	}
	if environment == "" {
		return "", nil
	}
	return "/backend-api/tbo/" + match[1] + "/computers/" + environment + "/" + match[3] + "?" + parsed.RawQuery, nil
}
