package provision

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/workspace"
)

// approvalEnv is lifecycleEnv with read-configuration answering from a file
// the test writes: a recorded result, its folder rewritten to the clone.
func approvalEnv(t *testing.T) (*env, func(json string)) {
	t.Helper()
	e := lifecycleEnv(t)
	rc := filepath.Join(e.cli.dir, "rc.json")
	e.cli.readConfig = `sed "s#/srv/drydock/ws/FIXTURE/[a-z0-9]*#$3#g" '` + rc + `'`
	e.wire(t)
	set := func(json string) {
		if err := os.WriteFile(rc, []byte(json), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set(fixture(t, "read-configuration-merged-ok.json"))
	return e, set
}

// withMerged is a recorded result with fields set in its merged
// configuration only — what a Feature's metadata adds.
func withMerged(t *testing.T, recorded string, fields map[string]any) string {
	t.Helper()
	var r map[string]map[string]any
	if err := json.Unmarshal([]byte(recorded), &r); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		r["mergedConfiguration"][k] = v
	}
	b, _ := json.Marshal(r)
	return string(b)
}

type approvalView struct {
	Hash    string
	Added   []struct{ Field, Source string }
	Changed []struct{ Field, Source string }
	Removed []struct{ Field, Source string }
}

func (e *env) approval(t *testing.T, id string) (workspace.View, approvalView) {
	t.Helper()
	v := e.view(t, id)
	var a approvalView
	if v.Approval != nil {
		b, _ := json.Marshal(v.Approval)
		json.Unmarshal(b, &a)
	}
	return v, a
}

func names(list []struct{ Field, Source string }) string {
	var out []string
	for _, s := range list {
		out = append(out, s.Field+"/"+s.Source)
	}
	return strings.Join(out, " ")
}

func (e *env) approvals(t *testing.T) (current, superseded int) {
	t.Helper()
	e.p.Workspaces.DB.QueryRow(`SELECT count(*) FROM config_approval WHERE superseded_at IS NULL`).Scan(&current)
	e.p.Workspaces.DB.QueryRow(`SELECT count(*) FROM config_approval WHERE superseded_at IS NOT NULL`).Scan(&superseded)
	return
}

// TestHostAccessNeedsAnApprovalThatMatches is the approval gate end to end
// with the CLI faked (design §6, "What a configuration may ask of the host"):
//
//   - an empty subset runs, with no approval and no request (the control);
//   - docker-in-docker's privileged stops a create at resolve_config —
//     needs_approval, not failed — with the workspace stopped, the request
//     on its view naming privileged as the Feature's, and no up;
//   - an approval with a stale hash is refused and records nothing; the
//     right hash records it and the run reaches running;
//   - a rebuild with the same subset runs without asking;
//   - a Feature that adds privileged to a repository that had none asks.
func TestHostAccessNeedsAnApprovalThatMatches(t *testing.T) {
	ctx := context.Background()
	e, set := approvalEnv(t)

	// The control: the typical Go repository runs with nothing to approve.
	v := e.running(t, alpha)
	if v.Approval != nil {
		t.Fatalf("an empty subset carries a request: %+v", v.Approval)
	}
	if cur, _ := e.approvals(t); cur != 0 {
		t.Errorf("%d approvals recorded for an empty subset", cur)
	}
	// A Feature that adds privileged: the rebuild asks, naming the Feature.
	set(withMerged(t, fixture(t, "read-configuration-merged-ok.json"), map[string]any{"privileged": true}))
	ups := len(e.cli.callsTo(t, "up"))
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	r, a := e.approval(t, v.ID)
	if r.State != workspace.Stopped || names(a.Added) != "privileged/feature_or_image" || !strings.HasPrefix(a.Hash, "sha256:") {
		t.Fatalf("a Feature adding privileged: %s %+v", r.State, a)
	}
	if st := r.Steps[workspace.StepResolveConfig]; st.Status != "needs_approval" || !strings.Contains(st.Detail, "privileged") {
		t.Errorf("resolve_config: %+v", st)
	}
	if n := len(e.cli.callsTo(t, "up")); n != ups {
		t.Errorf("up ran while an approval was pending")
	}
	for _, c := range e.containers(t, v.ID) {
		if c != "exited" {
			t.Errorf("a container is %s while the approval is pending", c)
		}
	}
	if cl := e.broker.closes(); len(cl) == 0 || cl[len(cl)-1] != v.ID {
		t.Errorf("the socket of a workspace waiting for approval was not closed: %v", cl)
	}
	// Approved, the rebuild continues as a rebuild.
	if err := e.p.ApproveConfig(ctx, v.ID, a.Hash, "session-x"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	all := e.cli.callsTo(t, "up")
	if r := e.view(t, v.ID); r.State != workspace.Running || len(all) != ups+1 ||
		!strings.Contains(strings.Join(all[len(all)-1], " "), "--remove-existing-container") {
		t.Errorf("the approved rebuild: %s, up %v", r.State, all[len(all)-1])
	}

	// docker-in-docker on the create of another repository.
	set(fixture(t, "read-configuration-merged-dind.json"))
	e.cli.exec = strings.ReplaceAll(e.cli.exec, "/krelinga/alpha.git", "/krelinga/plain.git")
	e.wire(t)
	d := e.create(t, plain, "")
	d2, da := e.approval(t, d.ID)
	if d2.State != workspace.Stopped || names(da.Added) != "privileged/feature_or_image" {
		t.Fatalf("docker-in-docker's create: %s %+v", d2.State, da)
	}
	if st := d2.Steps[workspace.StepResolveConfig]; st.Status != "needs_approval" {
		t.Errorf("resolve_config: %+v", st)
	}
	for _, st := range []workspace.Step{workspace.StepUp, workspace.StepVerify} {
		if _, ran := d2.Steps[st]; ran {
			t.Errorf("step %s ran before the approval", st)
		}
	}
	// A stale hash: refused, nothing recorded, still waiting.
	if err := e.p.ApproveConfig(ctx, d.ID, "sha256:"+strings.Repeat("0", 64), "session-x"); !errors.Is(err, workspace.ErrApprovalStale) {
		t.Errorf("a stale hash: %v", err)
	}
	if cur, _ := e.approvals(t); cur != 1 {
		t.Errorf("a stale approval was recorded")
	}
	if _, a := e.approval(t, d.ID); a.Hash != da.Hash {
		t.Errorf("a stale approval cleared the request")
	}
	// The right hash.
	ups = len(e.cli.callsTo(t, "up"))
	if err := e.p.ApproveConfig(ctx, d.ID, da.Hash, "session-x"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if r, a := e.approval(t, d.ID); r.State != workspace.Running || r.Approval != nil || a.Hash != "" {
		t.Fatalf("after the approval: %s (%s) %+v", r.State, deref(r.StateDetail), r.Approval)
	}
	if n := len(e.cli.callsTo(t, "up")); n != ups+1 {
		t.Errorf("up ran %d times after the approval", n-ups)
	}
	var by, hash string
	e.p.Workspaces.DB.QueryRow(`SELECT approved_by, hash FROM config_approval WHERE repository_id = ? AND superseded_at IS NULL`, plain).Scan(&by, &hash)
	if by != "session-x" || hash != da.Hash {
		t.Errorf("recorded %q %q", by, hash)
	}
	if n := e.kinds(t, d.ID, workspace.KindApproved); n != 1 {
		t.Errorf("%d config.approved events", n)
	}
	// Approving again: nothing is pending.
	if err := e.p.ApproveConfig(ctx, d.ID, da.Hash, "session-x"); !errors.Is(err, workspace.ErrNoApproval) {
		t.Errorf("a second approval: %v", err)
	}

	// The same subset on a rebuild: no prompt.
	ups = len(e.cli.callsTo(t, "up"))
	if err := e.p.Rebuild(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if r := e.view(t, d.ID); r.State != workspace.Running || r.Approval != nil ||
		!strings.Contains(r.Steps[workspace.StepResolveConfig].Detail, "host access the operator approved") {
		t.Errorf("a rebuild with the approved subset: %s %+v %+v", r.State, r.Approval, r.Steps[workspace.StepResolveConfig])
	}
	if n := len(e.cli.callsTo(t, "up")); n != ups+1 {
		t.Errorf("up ran %d times on the rebuild", n-ups)
	}

	// The approved setting removed: it runs, with less, and nothing is
	// recorded; put back, it runs again without asking, the hash matching.
	set(fixture(t, "read-configuration-merged-ok.json"))
	if err := e.p.Rebuild(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if r := e.view(t, d.ID); r.State != workspace.Running {
		t.Errorf("with the approved setting removed: %s", r.State)
	}
	set(fixture(t, "read-configuration-merged-dind.json"))
	if err := e.p.Rebuild(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if r := e.view(t, d.ID); r.State != workspace.Running || r.Approval != nil {
		t.Errorf("the approved setting put back: %s %+v", r.State, r.Approval)
	}
	if cur, sup := e.approvals(t); cur != 2 || sup != 0 {
		t.Errorf("approvals current %d superseded %d; want one per repository", cur, sup)
	}
}

// TestARebuildAsksForAConfigTheContainerRewrote is the attack on PR #25 with
// the CLI faked: a running workspace's container rewrites the clone's
// devcontainer.json with an initializeCommand, and the operator rebuilds.
// The rebuild stops the old container before it reads the configuration —
// so what is checked is what up would read — and stops for an approval
// naming the field, never running up. Approved, it runs: the owner's choice
// (design §6). A decline instead leaves it stopped, and start asks again.
func TestARebuildAsksForAConfigTheContainerRewrote(t *testing.T) {
	ctx := context.Background()
	e, set := approvalEnv(t)
	v := e.running(t, alpha)

	// From inside the container: the clone is its to write.
	set(fixture(t, "read-configuration-merged-hostile.json"))
	ups := len(e.cli.callsTo(t, "up"))
	mark := len(e.cli.calls(t))
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	r, a := e.approval(t, v.ID)
	if r.State != workspace.Stopped || !strings.Contains(names(a.Added), "initializeCommand/repository") {
		t.Fatalf("a rebuild of a rewritten config: %s %+v", r.State, a)
	}
	if st := r.Steps[workspace.StepResolveConfig]; st.Status != "needs_approval" || !strings.Contains(st.Detail, "initializeCommand") {
		t.Errorf("resolve_config: %+v", st)
	}
	if n := len(e.cli.callsTo(t, "up")); n != ups {
		t.Errorf("up ran %d more times before the approval", n-ups)
	}
	var order []string
	for _, c := range e.cli.calls(t)[mark:] {
		switch {
		case len(c) > 1 && c[0] == "docker" && c[1] == "stop":
			order = append(order, "stop")
		case len(c) > 0 && c[0] == "read-configuration":
			order = append(order, "read-configuration")
		}
	}
	if strings.Join(order, " ") != "stop read-configuration" {
		t.Errorf("the rebuild ran %v; want the container stopped before the configuration is read", order)
	}

	// Declined: stopped, no request, nothing recorded; start asks again.
	if err := e.p.DeclineConfig(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.view(t, v.ID); r.State != workspace.Stopped || r.Approval != nil || deref(r.StateDetail) != workspace.DeclineDetail {
		t.Errorf("after a decline: %s %+v %q", r.State, r.Approval, deref(r.StateDetail))
	}
	if err := e.p.DeclineConfig(ctx, v.ID); !errors.Is(err, workspace.ErrNoApproval) {
		t.Errorf("a second decline: %v", err)
	}
	if err := e.p.Start(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	r, a = e.approval(t, v.ID)
	if r.State != workspace.Stopped || a.Hash == "" {
		t.Fatalf("start after a decline: %s %+v", r.State, a)
	}
	if n := len(e.cli.callsTo(t, "up")); n != ups {
		t.Errorf("up ran before the approval")
	}

	// Approved: it runs.
	if err := e.p.ApproveConfig(ctx, v.ID, a.Hash, "session-y"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if r := e.view(t, v.ID); r.State != workspace.Running {
		t.Fatalf("after the approval: %s (%s)", r.State, deref(r.StateDetail))
	}
	// The approved run is the start's, which reattaches: the request keeps
	// the stopped run's --remove-existing-container, and a start from
	// stopped has none.
	ups2 := e.cli.callsTo(t, "up")
	if n := len(ups2); n != ups+1 || strings.Contains(strings.Join(ups2[n-1], " "), "--remove-existing-container") {
		t.Errorf("the continued start's up: %v", ups2[len(ups2)-1])
	}

	// A different subset later is shown as the difference from this one.
	set(withMerged(t, fixture(t, "read-configuration-merged-hostile.json"), map[string]any{"securityOpt": []string{"apparmor=unconfined", "label=disable"}}))
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	_, a = e.approval(t, v.ID)
	if names(a.Added) != "securityOpt/feature_or_image" || len(a.Changed) != 0 || len(a.Removed) != 0 {
		t.Errorf("the difference from the approved subset: %+v", a)
	}
	if err := e.p.ApproveConfig(ctx, v.ID, a.Hash, "session-y"); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if cur, sup := e.approvals(t); cur != 1 || sup != 1 {
		t.Errorf("a second approval: current %d superseded %d; want the first kept, superseded", cur, sup)
	}
}
