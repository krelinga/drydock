package container_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/login"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

// pinnedImage stands in for the Claude image: the login container's image is
// whatever Images returns, and fakeclaude, built static, runs in busybox.
type pinnedImage string

func (p pinnedImage) Ensure(context.Context) (string, error) { return string(p), nil }

// fakeInContainer builds fakeclaude static and scripts it with paths as the
// container sees them: the binary and its script at /opt/fake, the corpus at
// /fixtures, its state at /state. It returns the docker options that mount
// them and a Fake whose event log the test can read from the host.
func fakeInContainer(t *testing.T, accept string) ([]string, *claudetest.Fake) {
	t.Helper()
	dir, state := t.TempDir(), t.TempDir()
	bin := filepath.Join(dir, "claude")
	build := exec.Command("go", "build", "-o", bin, "github.com/krelinga/drydock/internal/claudetest/fakeclaude")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building fakeclaude: %v\n%s", err, out)
	}
	s := claudetest.Script{Corpus: "/fixtures", StateDir: "/state",
		Login: &claudetest.Login{Mode: claudetest.LoginAnswer, AcceptSHA256: claudetest.CodeSHA256(accept)}}
	js, _ := json.Marshal(s)
	if err := os.WriteFile(bin+".json", js, 0o644); err != nil {
		t.Fatal(err)
	}
	corpus, _ := filepath.Abs(filepath.Join("..", "fixtures"))
	extra := []string{
		"--mount", "type=bind,source=" + dir + ",target=/opt/fake,readonly",
		"--mount", "type=bind,source=" + corpus + ",target=/fixtures,readonly",
		"--mount", "type=bind,source=" + state + ",target=/state",
	}
	return extra, &claudetest.Fake{Dir: dir, Path: bin, Script: claudetest.Script{StateDir: state}}
}

func dockerLauncher(p, vol string, extra []string) login.DockerLauncher {
	run := subproc.Exec{}
	return login.DockerLauncher{Run: run, Volumes: container.Manager{Run: run, LabelPrefix: p},
		Image: pinnedImage(config.DefaultCleanupImage), PrepImage: config.DefaultCleanupImage,
		Volume: vol, LabelPrefix: p, UID: os.Getuid(), GID: os.Getgid(),
		Entrypoint: "/opt/fake/claude", Extra: extra}
}

func loginCode() string {
	a, b := make([]byte, 12), make([]byte, 12)
	rand.Read(a)
	rand.Read(b)
	return "cnryC" + hex.EncodeToString(a) + "#cnryS" + hex.EncodeToString(b)
}

func newLoginID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// term reads a Proc's PTY into a buffer the test can wait on.
type term struct {
	mu  sync.Mutex
	out bytes.Buffer
}

func readTerm(p *login.Proc) *term {
	tm := &term{}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := p.Master.Read(b)
			tm.mu.Lock()
			tm.out.Write(b[:n])
			tm.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return tm
}

func (tm *term) bytes() []byte {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return bytes.Clone(tm.out.Bytes())
}

func (tm *term) waitPhase(t *testing.T, want classify.LoginPhase) classify.Login {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if l, err := classify.ClassifyLogin(tm.bytes()); err == nil && l.Phase == want {
			return l
		}
	}
	l, err := classify.ClassifyLogin(tm.bytes())
	t.Fatalf("never reached phase %d: at %d, %v; %d bytes", want, l.Phase, err, len(tm.bytes()))
	return classify.Login{}
}

func loginContainers(t *testing.T, p string) string {
	t.Helper()
	return docker(t, "ps", "-aq", "--filter", "label="+p+"."+login.LabelLogin)
}

// TestLoginPTYThroughDocker measures what Spike 01 relied on, through the
// path Drydock now takes — its own PTY handed to `docker run -it` — with
// fakeclaude in the container replaying the 2.1.289 recordings:
//
//   - the container's terminal is a TTY sized to Drydock's PTY, at the
//     width chosen, from the start (fakeclaude records what it saw);
//   - the authorize URL arrives unbroken and the classifier, per line, reads
//     exactly the URL the recording holds, at 80 columns and at 1000;
//   - the typed code is not echoed by either terminal: neither the code nor
//     either half is anywhere in the stream Drydock reads back, while the
//     verdict for it is (the control);
//   - the code arrives framed as typed, once, CR-terminated — fakeclaude
//     judges it — and is in no `docker inspect` of the running container;
//   - on success the container exits and removes itself.
func TestLoginPTYThroughDocker(t *testing.T) {
	needDocker(t)
	if out, err := exec.Command("docker", "pull", "--quiet", config.DefaultCleanupImage).CombinedOutput(); err != nil {
		t.Fatalf("docker pull: %v: %s", err, out)
	}
	for _, cols := range []int{80, 1000} {
		t.Run(strconv.Itoa(cols), func(t *testing.T) {
			p := prefix(t)
			vol := claudeVolume(p)
			good, bad := loginCode(), loginCode()
			extra, fake := fakeInContainer(t, good)
			d := dockerLauncher(p, vol, extra)
			id := newLoginID()
			ctx := context.Background()
			proc, err := d.Launch(ctx, id, cols, 50)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { syscall.Kill(proc.Pid(), syscall.SIGKILL); d.Remove(ctx, id) })
			tm := readTerm(proc)

			aw := tm.waitPhase(t, classify.LoginAwaitingCode)
			fixture := claudetest.FixtureLoginPrompt
			if cols == 80 {
				fixture = claudetest.FixtureLoginPrompt80
			}
			want, err := classify.ClassifyLogin(claudetest.Transcript(t, fixture))
			if err != nil {
				t.Fatal(err)
			}
			if aw.AuthorizeURL != want.AuthorizeURL {
				t.Errorf("URL through docker %q; want the recording's %q", aw.AuthorizeURL, want.AuthorizeURL)
			}
			starts := claudetest.Kind(fake.Events(t), claudetest.EventStart)
			if len(starts) != 1 || !starts[0].TTY || starts[0].Width != cols || starts[0].Height != 50 {
				t.Errorf("the container's terminal: %+v; want a TTY %dx50", starts, cols)
			}

			before := len(tm.bytes())
			proc.Master.Write([]byte(bad + "\r"))
			tm.waitPhase(t, classify.LoginInvalidCode)
			if inspect := docker(t, "inspect", docker(t, "ps", "-q", "--filter", "label="+p+".login="+id)); strings.Contains(inspect, bad) {
				t.Error("the code is in docker inspect")
			}
			proc.Master.Write([]byte(good + "\r"))
			tm.waitPhase(t, classify.LoginSuccess)
			select {
			case <-proc.Exited():
			case <-time.After(30 * time.Second):
				t.Fatal("docker run did not exit after the login succeeded")
			}
			out := tm.bytes()
			if !bytes.Contains(out[before:], []byte("Invalid code")) {
				t.Fatalf("control: the stream after the first code holds no verdict:\n%q", out[before:])
			}
			for _, c := range []string{good, bad} {
				x, y, _ := strings.Cut(c, "#")
				for _, part := range []string{c, x, y} {
					if bytes.Contains(out, []byte(part)) {
						t.Errorf("a typed code was echoed into the stream Drydock reads")
					}
				}
			}
			subs := claudetest.Kind(fake.Events(t), claudetest.EventSubmission)
			if len(subs) != 2 || subs[0].SHA256 != claudetest.CodeSHA256(bad) || subs[1].Verdict != "accepted" {
				t.Errorf("fakeclaude saw %+v", subs)
			}
			fake.NoViolations(t)
			for i := 0; loginContainers(t, p) != ""; i++ {
				if i > 100 {
					t.Fatalf("the login container outlived its process: %s", loginContainers(t, p))
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}

type loggedIn struct {
	mu  sync.Mutex
	ats []time.Time
}

func (l *loggedIn) LoggedIn(_ context.Context, at time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ats = append(l.ats, at)
	return nil
}

func (l *loggedIn) n() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ats)
}

// TestLoginManagerAgainstRealDocker is the Manager over the real launcher:
// a fresh volume made labelled and given to Drydock's uid; a container killed
// mid-login is failed and nothing is left; a cancel removes the container; a
// docker CLI killed alone leaves its container running — measured, and the
// reason Remove and the boot sweep go by label — and the sweep removes it;
// a login that succeeds tells the watch; and a volume another uid owns is
// refused before anything is written into it.
func TestLoginManagerAgainstRealDocker(t *testing.T) {
	needDocker(t)
	if out, err := exec.Command("docker", "pull", "--quiet", config.DefaultCleanupImage).CombinedOutput(); err != nil {
		t.Fatalf("docker pull: %v: %s", err, out)
	}
	p := prefix(t)
	vol := claudeVolume(p)
	good := loginCode()
	extra, _ := fakeInContainer(t, good)
	d := dockerLauncher(p, vol, extra)

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := events.New(db.DB, sys.RealClock{})
	sub := log.Subscribe()
	defer log.Cancel(sub)
	ids := &loggedIn{}
	m := &login.Manager{Launcher: d, Events: log, Clock: sys.RealClock{}, Identity: ids, Settle: 10 * time.Second}
	t.Cleanup(func() { m.Shutdown(30 * time.Second) })
	next := func(want login.Phase) login.View {
		t.Helper()
		deadline := time.After(90 * time.Second)
		for {
			select {
			case ev := <-sub.C:
				var d struct{ Login login.View }
				if ev.Kind == login.KindLogin && json.Unmarshal(ev.Data, &d) == nil && d.Login.Phase == want {
					return d.Login
				}
			case <-deadline:
				t.Fatalf("no %s; current %+v", want, m.Current())
			}
		}
	}

	// A container killed mid-login.
	if _, err := m.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	next(login.AwaitingCode)
	if got := docker(t, "volume", "inspect", "-f", `{{index .Labels "`+p+`.claude-config"}}`, vol); got != "true" {
		t.Errorf("the volume the login made is not labelled: %q", got)
	}
	owner := docker(t, "run", "--rm", "--network", "none", "--mount", "type=volume,source="+vol+",target=/v,readonly",
		"--entrypoint", "stat", config.DefaultCleanupImage, "-c", "%u:%g %a", "/v")
	if want := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()) + " 700"; owner != want {
		t.Errorf("the fresh volume is %q; want %q, as a workspace's first mount would leave it", owner, want)
	}
	docker(t, append([]string{"kill"}, strings.Fields(loginContainers(t, p))...)...)
	f := next(login.Failed)
	if f.Problem == nil || *f.Problem != login.ProblemExited {
		t.Errorf("killed: %+v", f)
	}
	if left := loginContainers(t, p); left != "" {
		t.Errorf("left behind: %s", left)
	}

	// A cancel.
	v, err := m.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next(login.AwaitingCode)
	m.Cancel(ctx, v.ID)
	next(login.Cancelled)
	if left := loginContainers(t, p); left != "" {
		t.Errorf("a cancel left %s", left)
	}

	// The CLI killed alone: the container stays — so Drydock removes by
	// label, and boot sweeps.
	id := newLoginID()
	proc, err := d.Launch(ctx, id, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	tm := readTerm(proc)
	tm.waitPhase(t, classify.LoginAwaitingCode)
	syscall.Kill(proc.Pid(), syscall.SIGKILL)
	<-proc.Exited()
	time.Sleep(time.Second)
	if loginContainers(t, p) == "" {
		t.Error("measured: with the docker CLI killed, its container went too — the sweep would be unneeded")
	}
	if n, err := m.Sweep(ctx); err != nil || n != 1 {
		t.Errorf("sweep: %d, %v; want 1", n, err)
	}
	if left := loginContainers(t, p); left != "" {
		t.Errorf("the sweep left %s", left)
	}

	// Success.
	v, err = m.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next(login.AwaitingCode)
	if err := m.Submit(ctx, v.ID, []byte(good)); err != nil {
		t.Fatal(err)
	}
	next(login.Succeeded)
	for i := 0; ids.n() == 0; i++ {
		if i > 300 {
			t.Fatal("the watch was never told")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if left := loginContainers(t, p); left != "" {
		t.Errorf("after success: %s", left)
	}

	// A volume another uid owns: refused, and nothing written.
	other := vol + ".other"
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", other).Run() })
	if _, err := (container.Manager{Run: subproc.Exec{}, LabelPrefix: p}).EnsureClaudeVolume(ctx, other); err != nil {
		t.Fatal(err)
	}
	docker(t, "run", "--rm", "--network", "none", "--mount", "type=volume,source="+other+",target=/v",
		"--entrypoint", "sh", config.DefaultCleanupImage, "-c", "touch /v/.claude.json && chown -R 4242:4242 /v")
	d2 := dockerLauncher(p, other, extra)
	_, err = d2.Launch(ctx, newLoginID(), 80, 24)
	var le *login.LaunchError
	if !errors.As(err, &le) || le.Problem != login.ProblemVolumeOwner || !strings.Contains(le.Message, "uid 4242") {
		t.Errorf("a foreign-owned volume: %v", err)
	}
	if got := docker(t, "run", "--rm", "--network", "none", "--mount", "type=volume,source="+other+",target=/v,readonly",
		"--entrypoint", "stat", config.DefaultCleanupImage, "-c", "%u", "/v"); got != "4242" {
		t.Errorf("the refusal changed the owner to %s", got)
	}
}
