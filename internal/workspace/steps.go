package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/krelinga/drydock/internal/events"
)

// Step is one of design §6's eight steps between the click and a usable
// session. Every one writes an event when it starts and when it ends, so a
// failure at step six says step six rather than "failed".
type Step string

const (
	StepAllocate         Step = "allocate"
	StepClone            Step = "clone"
	StepResolveConfig    Step = "resolve_config"
	StepCredentialVolume Step = "credential_volume"
	StepBrokerSocket     Step = "broker_socket"
	StepUp               Step = "up"
	StepVerify           Step = "verify"
	StepSessionServer    Step = "session_server"
)

// Steps is the order they run in.
var Steps = []Step{
	StepAllocate, StepClone, StepResolveConfig, StepCredentialVolume,
	StepBrokerSocket, StepUp, StepVerify, StepSessionServer,
}

// stateFor is the workspace state a step runs in. Clone has its own so a
// failed clone is retried from the clone; everything from resolving the
// config to the probe is the build.
func stateFor(s Step) State {
	switch s {
	case StepAllocate:
		return Pending
	case StepClone:
		return Cloning
	case StepSessionServer:
		return Running
	}
	return Building
}

// StepFunc does one step's work.
type StepFunc func(ctx context.Context, w Workspace) error

// PublicError is an error whose Public text is safe to show the operator and
// to write into the event log. A step's other errors are not: their text can
// carry anything a subprocess printed — git's stderr, a repository's own
// devcontainer.json commands — so only the step's name reaches the event.
// The full error is returned to the caller, which logs it where redaction
// applies.
type PublicError interface {
	error
	Public() string
}

// Public wraps err with an operator-facing sentence.
func Public(sentence string, err error) error { return publicErr{sentence, err} }

type publicErr struct {
	sentence string
	err      error
}

func (e publicErr) Error() string  { return e.sentence + ": " + e.err.Error() }
func (e publicErr) Public() string { return e.sentence }
func (e publicErr) Unwrap() error  { return e.err }

// Note is what a step returns when it succeeded with something the operator
// should read on the step itself: a step that deliberately does nothing yet,
// or one that took a path worth naming (a repository with no devcontainer.json
// getting Drydock's minimal configuration, §6 step 3). Provision records it
// as "done" with the sentence as its detail. It is an error value only so a
// StepFunc keeps one return; Provision never treats it as a failure.
func Note(sentence string) error { return note(sentence) }

type note string

func (n note) Error() string { return string(n) }

// StepError is what Provision returns when a step fails.
type StepError struct {
	Step Step
	Err  error
}

func (e *StepError) Error() string { return fmt.Sprintf("step %s: %v", e.Step, e.Err) }
func (e *StepError) Unwrap() error { return e.Err }

// Provision runs the steps from `first` to the end, in order, against
// workspace id, moving it through the states they run in. On a failure it
// moves the workspace to Failed with a detail naming the step and returns a
// *StepError. Every step from `first` on must have a function: a missing one
// is a wiring bug, refused before anything runs.
//
// A fresh workspace starts at StepAllocate; a start or a rebuild starts at
// StepResolveConfig, since the clone survives both (§6, §14).
//
// The session server is handed off after the workspace is Running, and its
// failure is reported as a failed step without failing the workspace: the
// container is fine, and the supervisor has its own states for the rest
// (§8 — a refused start may be a wait, not a crash).
func (s *Store) Provision(ctx context.Context, id string, first Step, run map[Step]StepFunc) error {
	start := -1
	for i, st := range Steps {
		if st == first {
			start = i
		}
	}
	if start < 0 {
		return fmt.Errorf("workspace: unknown first step %q", first)
	}
	for _, st := range Steps[start:] {
		if run[st] == nil {
			return fmt.Errorf("workspace: no function for step %s", st)
		}
	}

	for _, st := range Steps[start:] {
		w, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		if want := stateFor(st); w.State != want {
			if w, err = s.Move(ctx, id, want, ""); err != nil {
				return err
			}
		}
		if err := s.stepEvent(ctx, id, st, "started", events.Info, ""); err != nil {
			return err
		}
		runErr := run[st](ctx, w)
		var n note
		if runErr == nil || errors.As(runErr, &n) {
			if err := s.stepEvent(ctx, id, st, "done", events.Info, string(n)); err != nil {
				return err
			}
			continue
		}

		detail := fmt.Sprintf("The %s step failed.", label(st))
		var pub PublicError
		if errors.As(runErr, &pub) {
			detail = fmt.Sprintf("The %s step failed: %s", label(st), pub.Public())
		}
		if err := s.stepEvent(ctx, id, st, "failed", events.Error, detail); err != nil {
			return err
		}
		if st != StepSessionServer {
			if _, err := s.Move(ctx, id, Failed, detail); err != nil {
				return err
			}
		}
		return &StepError{Step: st, Err: runErr}
	}
	// The probe passed and the session server is up. Running was entered
	// before the hand-off, so there is nothing left to move.
	return nil
}

func (s *Store) stepEvent(ctx context.Context, id string, st Step, status string, level events.Level, detail string) error {
	data := map[string]any{"step": st, "status": status}
	msg := fmt.Sprintf("%s: %s.", capitalize(label(st)), status)
	if detail != "" {
		data["detail"] = detail
		msg = detail
		if status == "done" {
			msg = fmt.Sprintf("%s: done. %s", capitalize(label(st)), detail)
		}
	}
	_, err := s.Events.Emit(ctx, id, level, KindStep, msg, data)
	return err
}

func label(s Step) string {
	return map[Step]string{
		StepAllocate: "allocate", StepClone: "clone", StepResolveConfig: "resolve config",
		StepCredentialVolume: "credential volume", StepBrokerSocket: "broker socket",
		StepUp: "container start", StepVerify: "verify", StepSessionServer: "session server",
	}[s]
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// IsNote reports whether err is a Note, and its sentence: a step that
// succeeded with something to say.
func IsNote(err error) (string, bool) {
	var n note
	if errors.As(err, &n) {
		return string(n), true
	}
	return "", false
}
