package provision

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/workspace"
)

// pendingBaseline reads the baseline a workspace's stored request carries.
func (e *env) pendingBaseline(t *testing.T, id string) string {
	t.Helper()
	_, p, err := e.p.Workspaces.Pending(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p.Baseline
}

// interlope writes an approval for repository repo directly, as a second
// workspace's approval of the same repository would, which one workspace per
// repository keeps from happening today.
func (e *env) interlope(t *testing.T, repo int64, ws, hash string) {
	t.Helper()
	if _, err := e.p.Workspaces.DB.Exec(`INSERT INTO config_approval
		(repository_id, hash, settings, workspace_id, approved_by, approved_at) VALUES (?, ?, '[]', ?, 'other', '2026-10-13T00:00:00Z')`,
		repo, hash, ws); err != nil {
		t.Fatal(err)
	}
}

// dindEnv is approvalEnv with docker-in-docker's recording, cloning plain.
func dindEnv(t *testing.T) (*env, func(string)) {
	t.Helper()
	e, set := approvalEnv(t)
	set(fixture(t, "read-configuration-merged-dind.json"))
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/krelinga/alpha.git", "/krelinga/plain.git")
	e.wire(t)
	return e, set
}

// TestAnApprovalIsOnlyForTheBaselineItWasShownAgainst is the approval's
// baseline check (vscode-extension §10.4, overall §6): a request records the
// repository's approval its difference was computed against, and Approve
// refuses it — approval_stale, nothing recorded, no config.approved, no up,
// the request kept — when the repository's current approval has moved on
// since: what the operator was shown is then not what approving adds. The
// controls: the same request approves once the baseline is back, and a later
// request records the approval it was diffed against and approves.
func TestAnApprovalIsOnlyForTheBaselineItWasShownAgainst(t *testing.T) {
	ctx := context.Background()
	e, set := dindEnv(t)

	// A first request: no approval to differ from.
	d := e.create(t, plain, "")
	_, first := e.approval(t, d.ID)
	if first.Hash == "" || e.pendingBaseline(t, d.ID) != "" {
		t.Fatalf("a first request: hash %q, baseline %q", first.Hash, e.pendingBaseline(t, d.ID))
	}

	// The repository's approval moves on underneath it.
	interloper := "sha256:" + strings.Repeat("e", 64)
	e.interlope(t, plain, d.ID, interloper)
	ups := len(e.cli.callsTo(t, "up"))
	if err := e.p.ApproveConfig(ctx, d.ID, first.Hash, "session-x"); !errors.Is(err, workspace.ErrApprovalStale) {
		t.Fatalf("approving against a moved baseline: %v", err)
	}
	if cur, sup := e.approvals(t); cur != 1 || sup != 0 {
		t.Errorf("a refused approval recorded: current %d superseded %d", cur, sup)
	}
	if n := e.kinds(t, d.ID, string(events.KindConfigApproved)); n != 0 {
		t.Errorf("%d config.approved events for a refused approval", n)
	}
	if _, a := e.approval(t, d.ID); a.Hash != first.Hash {
		t.Errorf("a refused approval cleared the request")
	}
	if n := len(e.cli.callsTo(t, "up")); n != ups {
		t.Errorf("up ran after a refused approval")
	}

	// Control: the baseline restored, the same request approves.
	if _, err := e.p.Workspaces.DB.Exec(`DELETE FROM config_approval WHERE hash = ?`, interloper); err != nil {
		t.Fatal(err)
	}
	if err := e.p.ApproveConfig(ctx, d.ID, first.Hash, "session-x"); err != nil {
		t.Fatalf("the same request with its baseline intact: %v", err)
	}
	e.p.idle()
	if r := e.view(t, d.ID); r.State != workspace.Running {
		t.Fatalf("after the approval: %s", r.State)
	}

	// A later request records the approval it was diffed against.
	set(withMerged(t, fixture(t, "read-configuration-merged-dind.json"), map[string]any{"initializeCommand": "make host-setup"}))
	if err := e.p.Rebuild(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.p.idle()
	_, second := e.approval(t, d.ID)
	if second.Hash == "" || second.Hash == first.Hash {
		t.Fatalf("a second request: %+v", second)
	}
	if b := e.pendingBaseline(t, d.ID); b != first.Hash {
		t.Errorf("the second request's baseline is %q, want the approval it was diffed against, %q", b, first.Hash)
	}
	// Moved on again: refused; restored: approved.
	e.interlope(t, plain, d.ID, interloper)
	if err := e.p.ApproveConfig(ctx, d.ID, second.Hash, "session-x"); !errors.Is(err, workspace.ErrApprovalStale) {
		t.Errorf("the second request against a moved baseline: %v", err)
	}
	e.p.Workspaces.DB.Exec(`DELETE FROM config_approval WHERE hash = ?`, interloper)
	if err := e.p.ApproveConfig(ctx, d.ID, second.Hash, "session-x"); err != nil {
		t.Errorf("the second request: %v", err)
	}
	e.p.idle()
	if cur, sup := e.approvals(t); cur != 1 || sup != 1 {
		t.Errorf("approvals current %d superseded %d; want the second current, the first superseded", cur, sup)
	}
}

// TestARequestFromBeforeTheBaselineIsApprovedOnlyWithoutAnApproval: a
// request stored before requests carried a baseline decodes with "". It is
// approved for a repository that still has no approval (it can only have been
// diffed against none), and refused as stale for one that has one.
func TestARequestFromBeforeTheBaselineIsApprovedOnlyWithoutAnApproval(t *testing.T) {
	ctx := context.Background()
	e, _ := dindEnv(t)
	d := e.create(t, plain, "")
	_, req := e.approval(t, d.ID)
	var raw string
	e.p.Workspaces.DB.QueryRow(`SELECT pending_approval FROM workspace WHERE id = ?`, d.ID).Scan(&raw)
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["baseline"]; !has {
		t.Fatalf("a new request carries no baseline: %s", raw)
	}
	delete(m, "baseline")
	b, _ := json.Marshal(m)
	if _, err := e.p.Workspaces.DB.Exec(`UPDATE workspace SET pending_approval = ? WHERE id = ?`, string(b), d.ID); err != nil {
		t.Fatal(err)
	}
	e.interlope(t, plain, d.ID, "sha256:"+strings.Repeat("f", 64))
	if err := e.p.ApproveConfig(ctx, d.ID, req.Hash, "session-x"); !errors.Is(err, workspace.ErrApprovalStale) {
		t.Errorf("a legacy request with an approval in place: %v", err)
	}
	// Control: with no approval, the same legacy request approves.
	if _, err := e.p.Workspaces.DB.Exec(`DELETE FROM config_approval`); err != nil {
		t.Fatal(err)
	}
	if err := e.p.ApproveConfig(ctx, d.ID, req.Hash, "session-x"); err != nil {
		t.Errorf("a legacy request with no approval: %v", err)
	}
	e.p.idle()
}
