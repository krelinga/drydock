//go:build linux

// Package logintest is a login.Launcher for the component tier: fakeclaude
// (internal/claudetest) on a real PTY, started directly rather than in a
// container. What it stands in for is DockerLauncher's half — the volume, the
// image, `docker run -it` — which has its own argv tests and the container
// tier; everything the Manager does with the PTY is the real thing.
package logintest

import (
	"context"
	"errors"
	"sync"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/login"
	"github.com/krelinga/drydock/internal/subproc"
)

// Launcher runs fakeclaude's `auth login` on a PTY.
type Launcher struct {
	Fake *claudetest.Fake
	// ConfigDir is the fake's CLAUDE_CONFIG_DIR.
	ConfigDir string
	// Hold, when set, keeps every launch waiting until it is closed or the
	// launch is abandoned — a slow image build, for a cancel or a timeout
	// during `starting`.
	Hold chan struct{}
	// Fail, when set, is what Launch returns.
	Fail error

	mu       sync.Mutex
	launched []string
	removed  []string
	killed   []bool
	swept    []string
	order    []string
	procs    map[string]*login.Proc
}

// Launch implements login.Launcher.
func (l *Launcher) Launch(ctx context.Context, id string, cols, rows int) (*login.Proc, error) {
	l.mu.Lock()
	l.launched = append(l.launched, id)
	l.order = append(l.order, "launch "+id)
	l.mu.Unlock()
	if l.Hold != nil {
		select {
		case <-l.Hold:
		case <-ctx.Done():
			return nil, &login.LaunchError{Problem: login.ProblemStart, Detail: "abandoned"}
		}
	}
	if l.Fail != nil {
		return nil, l.Fail
	}
	// Through subproc's PTY start, as DockerLauncher's docker is, with the
	// environment replaced: nothing of the test process's reaches it.
	r := subproc.Exec{Resolver: subproc.FixedResolver{"claude": l.Fake.Path}}
	p, err := login.StartProc(r, subproc.Cmd{Name: "claude", Args: []string{"auth", "login", "--claudeai"},
		Env: []string{"CLAUDE_CONFIG_DIR=" + l.ConfigDir, "HOME=" + l.ConfigDir, "DISABLE_AUTOUPDATER=1"}}, cols, rows)
	if err != nil {
		return nil, &login.LaunchError{Problem: login.ProblemStart, Detail: err.Error()}
	}
	l.mu.Lock()
	if l.procs == nil {
		l.procs = map[string]*login.Proc{}
	}
	l.procs[id] = p
	l.mu.Unlock()
	return p, nil
}

// Remove implements login.Launcher: recorded, since there is no container.
func (l *Launcher) Remove(_ context.Context, id string, killed bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.removed = append(l.removed, id)
	l.killed = append(l.killed, killed)
	return nil
}

// Killed is each Remove's killed, in the order of Removed.
func (l *Launcher) Killed() []bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bool(nil), l.killed...)
}

// Sweep implements login.Launcher.
func (l *Launcher) Sweep(_ context.Context, keep string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.swept = append(l.swept, keep)
	l.order = append(l.order, "sweep "+keep)
	return 0, nil
}

// Proc is the process launched for id.
func (l *Launcher) Proc(id string) (*login.Proc, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p, ok := l.procs[id]; ok {
		return p, nil
	}
	return nil, errors.New("logintest: no process for " + id)
}

// Launched, Removed and Swept are what was asked of the launcher, in order.
func (l *Launcher) Launched() []string { return l.copy(&l.launched) }
func (l *Launcher) Removed() []string  { return l.copy(&l.removed) }
func (l *Launcher) Swept() []string    { return l.copy(&l.swept) }

// Order is every Sweep and Launch in turn, as "sweep <keep>" and
// "launch <id>".
func (l *Launcher) Order() []string { return l.copy(&l.order) }

func (l *Launcher) copy(s *[]string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), *s...)
}
