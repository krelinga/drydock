package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/events"
)

// extensionContract is extension/contract.json: every route the VS Code
// extension calls and every event kind it reads (vscode-extension §9.1).
type extensionContract struct {
	Routes []struct {
		Name    string `json:"name"`
		Method  string `json:"method"`
		Pattern string `json:"pattern"`
	} `json:"routes"`
	Kinds []string `json:"kinds"`
}

func loadExtensionContract(t *testing.T) extensionContract {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "extension", "contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c extensionContract
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("extension/contract.json: %v", err)
	}
	return c
}

const contractBump = "bump APIVersion in internal/api/token_routes.go (the extension's apiRange then refuses this server), or keep it"

// contractRouteErrors is what the table says against one contracted route,
// empty when it holds.
func contractRouteErrors(table []Route, name, method, pattern string) []string {
	for _, r := range table {
		if r.Name != name {
			continue
		}
		var errs []string
		if r.Mux != MuxAPI {
			errs = append(errs, "is not on the API mux")
		}
		if r.Method != method || r.Pattern != pattern {
			errs = append(errs, "is "+r.Method+" "+r.Pattern+", the extension calls "+method+" "+pattern)
		}
		if r.Auth != AuthRequired {
			errs = append(errs, "is not AuthRequired")
		}
		switch r.Scope {
		case auth.ScopeRead, auth.ScopeLifecycle, auth.ScopeApprove, auth.ScopeSelf:
		default:
			errs = append(errs, "has scope "+string(r.Scope)+", which no token holds")
		}
		return errs
	}
	return []string{"is not in api.Table"}
}

// TestExtensionContract holds the routes and event kinds the extension uses
// to the server, statically, in the ordinary go test (vscode-extension
// §9.1, §12.7): a route renamed, moved or closed to tokens, and a kind
// unregistered or scoped beyond read, fail by name. Mutation-checked by
// renaming workspaces.create's pattern.
func TestExtensionContract(t *testing.T) {
	c := loadExtensionContract(t)
	// The control: the contract is not empty, and names the routes and the
	// kind E1 cannot work without, so a contract that failed to parse into
	// nothing would not pass.
	names := map[string]bool{}
	for _, r := range c.Routes {
		names[r.Name] = true
	}
	for _, must := range []string{"meta.read", "workspaces.create", "events.stream"} {
		if !names[must] {
			t.Fatalf("extension/contract.json lists no %s", must)
		}
	}
	if len(c.Kinds) == 0 {
		t.Fatal("extension/contract.json lists no kinds")
	}

	for _, r := range c.Routes {
		for _, e := range contractRouteErrors(Table, r.Name, r.Method, r.Pattern) {
			t.Errorf("route %s %s: %s", r.Name, e, contractBump)
		}
	}
	for _, k := range c.Kinds {
		s, ok := events.Scopes[events.Kind(k)]
		switch {
		case !ok:
			t.Errorf("kind %s is not in the registry (internal/events/kinds.go): %s", k, contractBump)
		case s != events.ScopeRead:
			t.Errorf("kind %s is scoped %s, so a token's stream never carries it: %s", k, s, contractBump)
		}
	}
}

// The checker itself finds each way a route can drift, and passes the
// table's own entry (the control).
func TestExtensionContractCheckerFindsDrift(t *testing.T) {
	if errs := contractRouteErrors(Table, "workspaces.create", "POST", "/api/workspaces"); len(errs) != 0 {
		t.Fatalf("the table's own route: %v", errs)
	}
	for _, c := range []struct{ name, method, pattern, want string }{
		{"workspaces.create", "POST", "/api/workspace", "the extension calls"},
		{"workspaces.create", "PUT", "/api/workspaces", "the extension calls"},
		{"workspaces.nope", "POST", "/api/workspaces", "not in api.Table"},
		{"secrets.list", "GET", "/api/secrets", "which no token holds"},
	} {
		errs := contractRouteErrors(Table, c.name, c.method, c.pattern)
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "; "), c.want) {
			t.Errorf("%s %s %s: %v, want %q", c.name, c.method, c.pattern, errs, c.want)
		}
	}
}
