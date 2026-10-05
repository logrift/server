package entry

import (
	"net/url"
	"regexp"
	"strings"
)

// RequestFields extracts normalized values for configurable search filters.
// Signatures are applied at query time so edits also affect historical logs.
func (e Entry) RequestFields() (agents, paths []string) {
	addPath := func(target string) {
		u, err := url.Parse(strings.TrimSpace(target))
		if err == nil && u.Path != "" {
			paths = append(paths, strings.ToLower(u.Path))
		}
	}
	var walk func(map[string]any)
	walk = func(fields map[string]any) {
		for key, value := range fields {
			if nested, ok := value.(map[string]any); ok {
				walk(nested)
				continue
			}
			key = strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
			switch key {
			case "useragent", "httpuseragent", "requestuseragent", "requseragent":
				agents = append(agents, strings.ToLower(toString(value)))
			case "path", "url", "uri", "requesturi", "requesturl", "requestpath", "originalurl", "target", "httptarget", "urlpath", "httpurl", "httprequesturi", "httprequestpath":
				addPath(toString(value))
			}
		}
	}
	walk(e.Attrs)
	if match := requestTarget.FindStringSubmatch(e.Message); len(match) > 1 {
		addPath(match[1])
		// Access messages often carry the agent after the request and status.
		agents = append(agents, strings.ToLower(e.Message))
	}
	return agents, paths
}

var requestTarget = regexp.MustCompile(`(?i)\b(?:GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS|CONNECT|TRACE)\s+([^\s"']+)`)
