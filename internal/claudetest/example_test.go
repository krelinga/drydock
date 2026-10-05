//go:build linux

package claudetest_test

import (
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/subproc"
)

// How the session supervisor's tests drive it: a registration wait the
// supervisor must sit out, then a serve. The supervisor resolves "claude"
// through its Resolver, so the fake is what runs, with the argv the supervisor
// really built; the fake's log is where that argv is asserted.
func ExampleInstall_supervisor() {
	var t *testing.T // the test's own
	f := claudetest.Install(t, claudetest.Script{RemoteControl: []claudetest.Step{
		{Mode: claudetest.RCRefuseWaitRegistration, For: claudetest.Duration(2 * time.Second)},
		{Mode: claudetest.RCServe, SessionDelay: claudetest.Duration(500 * time.Millisecond)},
	}})
	_ = subproc.FixedResolver{"claude": f.Path} // hand this to the supervisor

	// ... run the supervisor against it, then:
	for _, e := range claudetest.Kind(f.Events(t), claudetest.EventStart) {
		_ = e.Argv  // what the supervisor ran
		_ = e.Width // the PTY it gave the child
	}
	f.NoViolations(t) // nothing typed into the server, no PTY missing
}

// How the login handshake's tests drive it directly on a PTY of a chosen
// width. The script holds the code's hash, never the code, so the canary
// sweep can run over the fake's own files.
func ExampleFake_Start_login() {
	var t *testing.T
	const code = "canaryAAAA#canaryBBBB"
	f := claudetest.Install(t, claudetest.Script{Login: &claudetest.Login{
		Mode: claudetest.LoginAnswer, AcceptSHA256: claudetest.CodeSHA256(code),
	}})
	tm := f.Start(t, 80, "auth", "login", "--claudeai")
	out, _ := tm.WaitFor(10*time.Second, []byte("prompted > "))
	if v, _ := classify.ClassifyLogin(out); v.Phase == classify.LoginAwaitingCode {
		tm.Write([]byte(code + "\r")) // one code, one CR
	}
	if exited, rc, _ := tm.Wait(10 * time.Second); !exited || rc != 0 {
		t.Fatal("login did not complete")
	}
	f.NoViolations(t) // the code arrived once, framed by a single CR
	_ = tm.Signal(syscall.SIGTERM)
}
