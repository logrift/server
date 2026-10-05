package entry

import (
	"reflect"
	"testing"
)

func TestRequestFields(t *testing.T) {
	for _, tc := range []struct {
		name, raw     string
		agents, paths []string
	}{
		{"nested", `{"req":{"headers":{"User-Agent":"Custom Probe/1"},"url":"https://example.com/%2Eenv.production?q=1"}}`, []string{"custom probe/1"}, []string{"/.env.production"}},
		{"dotted", `{"http.target":"/PHPINFO.php?test=1","http.user_agent":"CustomBot"}`, []string{"custombot"}, []string{"/phpinfo.php"}},
		{"access", `{"msg":"192.0.2.1 GET /.env HTTP/1.1 404 CustomBot"}`, []string{"192.0.2.1 get /.env http/1.1 404 custombot"}, []string{"/.env"}},
		{"prose", `{"msg":"Could not load .env; CustomBot configuration missing"}`, nil, nil},
		{"query", `{"path":"/search?q=/.env"}`, nil, []string{"/search"}},
		{"missing", `{"msg":"request completed"}`, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			agents, paths := e.RequestFields()
			if !reflect.DeepEqual(agents, tc.agents) || !reflect.DeepEqual(paths, tc.paths) {
				t.Fatalf("fields = %v, %v; want %v, %v", agents, paths, tc.agents, tc.paths)
			}
		})
	}
}
