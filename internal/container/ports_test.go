package container

import (
	"encoding/json"
	"reflect"
	"testing"
)

func portCfg(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	if s == "" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestDeclaredPorts: forwardPorts (own, then merged), then appPort, each port
// once, labelled from portsAttributes — the repository's own label over a
// Feature's. Another Compose service's port, a UDP appPort, a range label and
// everything malformed are skipped, not refused.
func TestDeclaredPorts(t *testing.T) {
	c := Configuration{
		Own: portCfg(t, `{
			"forwardPorts": [5173, "3000", "localhost:4000", "db:5432", "x", 0, 70000, 1.5, null, 5173],
			"appPort": ["8080:9000", "127.0.0.1:8081:9001/tcp", "53:53/udp", 9002],
			"portsAttributes": {"5173": {"label": "vite"}, "3000-3010": {"label": "range"}, "9000": {"label": "app"}, "04000": {"label": "padded"}}
		}`),
		Merged: portCfg(t, `{
			"forwardPorts": [5173, 6006],
			"portsAttributes": {"5173": {"label": "feature's"}, "6006": {"label": "storybook"}}
		}`),
	}
	want := []DeclaredPort{
		{5173, "vite"}, {3000, ""}, {4000, ""}, {6006, "storybook"},
		{9000, "app"}, {9001, ""}, {9002, ""},
	}
	if got := DeclaredPorts(c); !reflect.DeepEqual(got, want) {
		t.Errorf("DeclaredPorts =\n%v\nwant\n%v", got, want)
	}
	// A single appPort that is not an array, and a configuration with none.
	if got := DeclaredPorts(Configuration{Own: portCfg(t, `{"appPort": 3000}`)}); !reflect.DeepEqual(got, []DeclaredPort{{3000, ""}}) {
		t.Errorf("a bare appPort = %v", got)
	}
	if got := DeclaredPorts(Configuration{Own: portCfg(t, `{"image": "x"}`)}); len(got) != 0 {
		t.Errorf("no declaration = %v", got)
	}
}
