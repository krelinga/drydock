package provision

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/krelinga/drydock/internal/workspace"
)

// withOwn is a recorded result with fields set in the configuration itself,
// and so in its merged configuration too.
func withOwn(t *testing.T, recorded string, fields map[string]any) string {
	t.Helper()
	var r map[string]map[string]any
	if err := json.Unmarshal([]byte(recorded), &r); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		r["configuration"][k] = v
		r["mergedConfiguration"][k] = v
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// TestLessThanApprovedRunsWithoutAsking is the containment rule (design §6):
// a subset within the approved set — a setting dropped, a list that lost an
// element — runs with no prompt and leaves the approval as it was, so the
// dropped setting put back runs too; a new setting, a changed value, or a
// list that gained an element asks.
func TestLessThanApprovedRunsWithoutAsking(t *testing.T) {
	ctx := context.Background()
	e, set := approvalEnv(t)
	ok := fixture(t, "read-configuration-merged-ok.json")
	with := func(own, merged map[string]any) string { return withMerged(t, withOwn(t, ok, own), merged) }
	full := with(map[string]any{"initializeCommand": "true"},
		map[string]any{"privileged": true, "capAdd": []string{"SYS_PTRACE", "NET_ADMIN", "SYS_ADMIN"}})

	set(full)
	v := e.create(t, alpha, "")
	_, a := e.approval(t, v.ID)
	if names(a.Added) != "capAdd/feature_or_image initializeCommand/repository privileged/feature_or_image" {
		t.Fatalf("the first request: %+v", a)
	}
	if err := e.p.ApproveConfig(ctx, v.ID, a.Hash, "s"); err != nil {
		t.Fatal(err)
	}
	e.p.idle()
	if r := e.view(t, v.ID); r.State != workspace.Running {
		t.Fatalf("approved: %s (%s)", r.State, deref(r.StateDetail))
	}

	rebuild := func(cfg string) workspace.View {
		t.Helper()
		set(cfg)
		if err := e.p.Rebuild(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		e.p.idle()
		return e.view(t, v.ID)
	}
	// In order: each runs without asking, and none narrows the approval.
	for _, c := range []struct{ name, cfg string }{
		{"privileged dropped", with(map[string]any{"initializeCommand": "true"},
			map[string]any{"capAdd": []string{"SYS_PTRACE", "NET_ADMIN", "SYS_ADMIN"}})},
		{"privileged put back", full},
		{"a capability fewer", with(map[string]any{"initializeCommand": "true"},
			map[string]any{"privileged": true, "capAdd": []string{"NET_ADMIN"}})},
		{"everything dropped", ok},
		{"all of it again", full},
	} {
		if r := rebuild(c.cfg); r.State != workspace.Running || r.Approval != nil {
			t.Errorf("%s: %s %+v; want it run without asking", c.name, r.State, r.Approval)
		}
	}
	for _, c := range []struct{ name, cfg, asked string }{
		{"a new setting", with(map[string]any{"initializeCommand": "true", "runArgs": []string{"--pid=host"}},
			map[string]any{"privileged": true}), "added runArgs/repository"},
		{"a changed value", with(map[string]any{"initializeCommand": "id"},
			map[string]any{"privileged": true}), "changed initializeCommand/repository"},
		// The approved privileged is the Feature's; the repository's own is
		// another setting.
		{"the same setting from the repository", with(map[string]any{"initializeCommand": "true", "privileged": true},
			map[string]any{}), "added privileged/repository"},
		{"a capability more", with(map[string]any{"initializeCommand": "true"},
			map[string]any{"capAdd": []string{"NET_ADMIN", "SYS_TIME"}}), "changed capAdd/feature_or_image"},
	} {
		r := rebuild(c.cfg)
		_, a := e.approval(t, v.ID)
		got := ""
		if len(a.Added) > 0 {
			got = "added " + names(a.Added)
		} else if len(a.Changed) > 0 {
			got = "changed " + names(a.Changed)
		}
		if r.State != workspace.Stopped || got != c.asked {
			t.Errorf("%s: %s, %q; want it to ask, %s", c.name, r.State, got, c.asked)
		}
		if r.State == workspace.Stopped {
			if err := e.p.DeclineConfig(ctx, v.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Running less never narrowed the approval: one, never superseded.
	if cur, sup := e.approvals(t); cur != 1 || sup != 0 {
		t.Errorf("approvals current %d superseded %d", cur, sup)
	}
}
