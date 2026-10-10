package container

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/krelinga/drydock/internal/dockerguard"
	"github.com/krelinga/drydock/internal/subproc"
)

// The docker guard (design §6, "The docker guard"; internal/dockerguard).
//
// Step 3 approves a configuration from what read-configuration says it asks
// for, and `up` computes that again from the same registries: a Feature or
// image behind a moved tag can ask `up` for what it never showed the check.
// So every devcontainer invocation here is given --docker-path pointing at
// the guard, in the workspace's own directory beside the clone, and `up`
// writes the guard a policy first — Drydock's own part of the argv and the
// repository's approval — which the guard holds each `docker run`, `create`
// and `build` to, in the form docker reads it. A refusal never runs docker.

// GuardDir is a workspace's guard directory, from its clone: the clone is
// <root>/<id>/repo and the guard <root>/<id>/.drydock/guard, beside it and
// never inside it, so no container sees it.
func GuardDir(folder string) string {
	return filepath.Join(filepath.Dir(folder), ".drydock", "guard")
}

// TempDirFor is the TMPDIR an up of the clone at folder runs with, the
// workspace's own: <root>/<id>/.drydock/tmp. Up runs with no other, since the
// policy lets a build read from it.
func TempDirFor(folder string) string {
	return filepath.Join(filepath.Dir(folder), ".drydock", "tmp")
}

// SweepPolicy removes a guard policy and refusal an up left behind — a
// Drydock killed during one — so the guard directory holds a policy only
// while an up runs. Boot calls it for each workspace with no job in flight.
func SweepPolicy(folder string) error {
	dir := GuardDir(folder)
	return errors.Join(dockerguard.RemovePolicy(dir), dockerguard.ClearRefusal(dir))
}

// GuardRefusal is an `up` the guard stopped: a docker command asked for host
// access outside the policy, and docker was never run for it. Settings are
// the guard's closed set of names (dockerguard.Setting*), safe to put in a
// sentence; nothing the configuration chose is.
type GuardRefusal struct {
	Settings []string
}

func (e *GuardRefusal) Error() string {
	return "the docker guard refused " + strings.Join(e.Settings, ", ")
}

// ErrNoGuard is an up asked of a Manager with no guard: refused rather than
// run unchecked.
var ErrNoGuard = errors.New("container: devcontainer up needs the docker guard")

// dockerPath prepares the workspace's guard and returns the arguments that
// hand it to the CLI, or nothing when the Manager has no guard.
func (m Manager) dockerPath(folder string) ([]string, error) {
	if m.Guard == nil {
		return nil, nil
	}
	// The docker the guard passes commands to is the docker this Manager
	// runs itself, unless the guard names its own resolver.
	g := *m.Guard
	if e, ok := subproc.Underlying(m.Run).(subproc.Exec); ok && g.Resolver == nil {
		g.Resolver = e.Resolver
	}
	p, err := g.Prepare(GuardDir(folder))
	if err != nil {
		return nil, fmt.Errorf("container: docker guard: %w", err)
	}
	// Compose too: CLI 0.89.0 runs `docker compose` through --docker-path
	// (measured), but falls back to a docker-compose binary where the
	// plugin is missing. Named as the guard, that fallback's argv reads as
	// docker options the guard does not know, and is refused.
	return []string{"--docker-path", p, "--docker-compose-path", p}, nil
}

// withDockerPath puts the guard's arguments at the end of the CLI's
// options: before exec's "--", or last.
func withDockerPath(args, dp []string) []string {
	if len(dp) == 0 {
		return args
	}
	for i, a := range args {
		if a == "--" {
			out := append(append([]string(nil), args[:i]...), dp...)
			return append(out, args[i:]...)
		}
	}
	return append(append([]string(nil), args...), dp...)
}

// guardPolicy is what one up may ask docker for.
func (m Manager) guardPolicy(s UpSpec) dockerguard.Policy {
	p := dockerguard.Policy{
		Clone: s.Folder, TempDir: s.TempDir, ConfigDir: s.ConfigDir,
		LabelPrefix: m.LabelPrefix, IDLabels: map[string]string{},
		ProbeImage: m.CleanupImage,
	}
	for _, kv := range m.idLabels(s) {
		k, v, _ := strings.Cut(kv, "=")
		p.IDLabels[k] = v
	}
	p.Labels = specLabels(s)
	p.OwnMounts = m.ownMounts(s)
	for _, a := range s.Approved {
		p.Approved = append(p.Approved, dockerguard.Setting{Field: a.Field, Value: a.Value})
	}
	return p
}
