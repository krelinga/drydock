package container_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/ephemeral"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/identity"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

// TestTheCredentialReadRefusesWhatNeverEnds is #37's review finding against
// real Docker. Every workspace mounts the shared volume read-write, so any of
// them can put something at .credentials.json that `cat` would never finish:
// a FIFO (the reviewer's reproduction: the read hung until its deadline and
// left its helper running), a symlink to /dev/zero, or simply a file far
// larger than a credential. Each is now refused by the helper's own script,
// promptly, as a credentials problem — never absent, never a verdict — and
// no helper is left behind. The control is a regular file, which is read.
func TestTheCredentialReadRefusesWhatNeverEnds(t *testing.T) {
	needDocker(t)
	pullImage(t, config.DefaultCleanupImage)
	p := prefix(t)
	ctx := context.Background()
	vol := hangVolume(t, p)
	src := identity.DockerSource{Run: subproc.Exec{}, FileImage: config.DefaultCleanupImage, Volume: vol, LabelPrefix: p}
	put := func(script string) {
		t.Helper()
		docker(t, "run", "--rm", "--network", "none", "--mount", "type=volume,source="+vol+",target=/v",
			"--entrypoint", "sh", config.DefaultCleanupImage, "-c", "rm -rf /v/.credentials.json; "+script)
	}

	put(`printf '{"claudeAiOauth":{}}' > /v/.credentials.json`)
	if b, err := src.Credentials(ctx); err != nil || string(b) != `{"claudeAiOauth":{}}` {
		t.Fatalf("control, a regular file: %q, %v", b, err)
	}

	for _, c := range []struct{ name, script string }{
		{"a FIFO", "mkfifo /v/.credentials.json"},
		{"a symlink to /dev/zero", "ln -s /dev/zero /v/.credentials.json"},
		{"an oversized file", "yes x | head -c 1048576 > /v/.credentials.json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			put(c.script)
			rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			start := time.Now()
			b, err := src.Credentials(rctx)
			var re *identity.ReadError
			if !errors.As(err, &re) || re.Problem != identity.ProblemCredentials || b != nil {
				t.Fatalf("%q, %v; want a credentials ReadError", b, err)
			}
			if took := time.Since(start); took > 30*time.Second {
				t.Errorf("refusing took %v", took)
			}
			if left := docker(t, "ps", "-aq", "--filter", "label="+p+"."+identity.LabelIdentity); left != "" {
				t.Errorf("helpers left behind: %s", left)
			}
		})
	}
}

// TestAHungReadIsCutOffAndItsHelperRemoved: a read that hangs anyway — a
// file swapped for a FIFO between the script's test and its read, or a
// daemon that stopped answering — is cut off by the watch's timeout, and the
// helper container it was running is removed by its label. Measured here,
// with a helper that never ends (sleep, carrying the identity label exactly as
// a read's helper does): the killed `docker run` client leaves its container
// running, which is why the sweep is needed at all. The check fails as a
// timeout, the stored state is kept, and the next check reads normally (the
// control).
func TestAHungReadIsCutOffAndItsHelperRemoved(t *testing.T) {
	needDocker(t)
	pullImage(t, config.DefaultCleanupImage)
	p := prefix(t)
	ctx := context.Background()
	vol := hangVolume(t, p)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := sys.NewFakeClock(time.Date(2026, 10, 4, 6, 14, 37, 0, time.UTC))
	real := identity.DockerSource{Run: subproc.Exec{}, FileImage: config.DefaultCleanupImage, Volume: vol, LabelPrefix: p}
	src := &hangingDocker{DockerSource: real, t: t, p: p, started: make(chan struct{}, 1)}
	w := &identity.Watch{DB: db.DB, Events: events.New(db.DB, clock), Clock: clock, Volume: vol, Window: 72 * time.Hour,
		Timeout: time.Minute, Source: src}
	startWatch(t, w)

	// A first check stores absent: no file on the volume.
	if v, err := w.Check(ctx); err != nil || v.State == nil || *v.State != identity.Absent {
		t.Fatalf("first check: %+v %v", v, err)
	}

	src.hang = true
	got := make(chan error, 1)
	go func() { _, err := w.Check(ctx); got <- err }()
	<-src.started
	clock.Advance(time.Minute)
	var re *identity.ReadError
	select {
	case err := <-got:
		if !errors.As(err, &re) || re.Problem != identity.ProblemTimeout {
			t.Fatalf("hung check: %v; want a timeout", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the check did not end at its timeout")
	}
	if !src.leftRunning {
		t.Error("measured: the killed docker client took its container with it — the removal by label would be unneeded")
	}
	if left := docker(t, "ps", "-aq", "--filter", "label="+p+"."+identity.LabelIdentity); left != "" {
		t.Errorf("the hung helper was left behind: %s", left)
	}
	if v, _ := w.Read(ctx); v.State == nil || *v.State != identity.Absent || v.CheckError == nil || v.CheckError.Problem != identity.ProblemTimeout {
		t.Errorf("after the timeout: %+v", v)
	}

	// Control: the next check is not stuck, and reads.
	src.hang = false
	if v, err := w.Check(ctx); err != nil || v.State == nil || *v.State != identity.Absent || v.CheckError != nil {
		t.Errorf("the check after: %+v %v", v, err)
	}
}

// hangingDocker is the real DockerSource whose credential read, while hang
// is set, runs a helper that never ends instead — the same docker client,
// run as the same ephemeral helper under the same label — and which records
// whether that helper outlived its cut-off client: what the first listing of
// the label after the run returned.
type hangingDocker struct {
	identity.DockerSource
	t           *testing.T
	p           string
	hang        bool
	started     chan struct{}
	cutOff      bool
	leftRunning bool
}

func (h *hangingDocker) Credentials(ctx context.Context) ([]byte, error) {
	if !h.hang {
		return h.DockerSource.Credentials(ctx)
	}
	args, err := h.RunArgs(h.FileImage, "sleep", "600")
	if err != nil {
		return nil, err
	}
	go func() {
		// Signal once the helper is up, so the clock moves after the read
		// has really started.
		for i := 0; i < 300; i++ {
			out, _ := exec.Command("docker", "ps", "-q", "--filter", "label="+h.p+"."+identity.LabelIdentity).Output()
			if strings.TrimSpace(string(out)) != "" {
				h.started <- struct{}{}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	helper := ephemeral.Helper{Docker: observed{h}, Prefix: h.p, Kind: ephemeral.Identity, Value: "1", Clock: h.Clock}
	res, err := helper.Run(ctx, subproc.Cmd{Name: "docker", Args: args})
	if err != nil {
		return nil, err
	}
	return nil, &identity.ReadError{Problem: identity.ProblemDocker, Detail: "hung read ended: " + errString(res.Err)}
}

// observed is the source's runner, noting what the first listing after the
// helper's run found.
type observed struct{ h *hangingDocker }

func (o observed) Run(ctx context.Context, c subproc.Cmd) subproc.Result {
	if c.Args[0] == "ps" && o.h.cutOff {
		var out strings.Builder
		real := c.Stdout
		c.Stdout = &out
		res := o.h.Run.Run(ctx, c)
		if real != nil {
			real.Write([]byte(out.String()))
		}
		o.h.cutOff = false
		o.h.leftRunning = strings.TrimSpace(out.String()) != ""
		return res
	}
	res := o.h.Run.Run(ctx, c)
	if c.Args[0] == "run" {
		o.h.cutOff = true
	}
	return res
}

func (o observed) Start(ctx context.Context, c subproc.Cmd) (subproc.Process, error) {
	return o.h.Run.Start(ctx, c)
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// hangVolume is a fresh shared volume as §6 step 4 makes it.
func hangVolume(t *testing.T, p string) string {
	t.Helper()
	rnd := make([]byte, 4)
	rand.Read(rnd)
	vol := "drydock-test-hang-" + hex.EncodeToString(rnd)
	t.Cleanup(func() {
		if left := strings.Fields(docker(t, "ps", "-aq", "--filter", "label="+p+"."+identity.LabelIdentity)); len(left) > 0 {
			exec.Command("docker", append([]string{"rm", "-f"}, left...)...).Run()
		}
		exec.Command("docker", "volume", "rm", "-f", vol).Run()
	})
	if _, err := manager(p).EnsureClaudeVolume(context.Background(), vol); err != nil {
		t.Fatal(err)
	}
	return vol
}
