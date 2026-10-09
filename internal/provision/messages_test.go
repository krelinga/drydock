package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/workspace"
)

// upFailing is a fake `up` that writes stderr lines and then the CLI's error
// result for a failed postCreateCommand.
func upFailing(stderr string) string {
	return "cat >&2 <<'EOF'\n" + stderr + "\nEOF\ncat <<'EOF'\n" + fixtureBytes("up-error-postcreate.json") + "\nEOF\nexit 1"
}

// TestUpFailuresNameTheirCause is design §12's three rows that surface as a
// failed `devcontainer up`, each driven by the line the Feature itself prints:
// a Remote Control variable is named (and only names from §2.1's list reach
// the detail, never the line's other text); a broker that did not answer at
// start is "GitHub access unavailable", never a git error; anything else is
// the build failing with the clone kept. The raw stderr — here a canary — is
// never in the detail, and the held build log has it redacted of the
// workspace's secret values. The control is a successful up, which says none
// of these and holds no build log.
func TestUpFailuresNameTheirCause(t *testing.T) {
	const canary = "CANARY-repo-said-this-8f3a"
	cases := []struct {
		name, stderr string
		want         string
		not          []string
	}{
		{"one variable",
			"drydock feature: DO_NOT_TRACK is set in the container's environment, and it disables Claude Code's Remote Control (design §2.1). " + canary,
			"Remote Control cannot work in this container: DO_NOT_TRACK is set", []string{"Remove them"}},
		{"two, one from a settings file, and an unlisted name ignored",
			"drydock feature: ANTHROPIC_BASE_URL is set to something other than https://api.anthropic.com, and any gateway\n" +
				"drydock feature: DISABLE_GROWTHBOOK is set in the env block of /w/.claude/settings.json, and it disables\n" +
				"drydock feature: EVIL_" + canary + " is set in the container's environment",
			"ANTHROPIC_BASE_URL, DISABLE_GROWTHBOOK are set in its environment or a Claude Code settings file", []string{"EVIL"}},
		{"no broker at start",
			"drydock: GitHub access unavailable: no broker socket at /run/drydock/broker.sock\n" +
				"drydock: this container was started without a working broker socket; GitHub access will fail. Restart it from Drydock.",
			NoBrokerSentence, nil},
		{"the build itself",
			"Step 4/9 : RUN make " + canary + "\nerror: exit status 2",
			UpFailedSentence, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.cli.up = upFailing(c.stderr + "\nsecret-value-xyzzy")
			e.wire(t)
			e.p.Redact = func(_ context.Context, id string) ([]string, error) { return []string{"secret-value-xyzzy"}, nil }
			v := e.create(t, alpha, "")
			got := v.Steps[workspace.StepUp]
			if v.State != workspace.Failed || got.Status != "failed" || !strings.Contains(got.Detail, c.want) {
				t.Fatalf("%s: step %+v, want %q", v.State, got, c.want)
			}
			for _, bad := range append(c.not, canary, "secret-value-xyzzy") {
				if strings.Contains(got.Detail, bad) || strings.Contains(deref(v.StateDetail), bad) {
					t.Errorf("the detail carries %q: %s", bad, got.Detail)
				}
			}
			log, ok, err := e.p.BuildLog(context.Background(), v.ID)
			// The held lines are stderr's last ones, in order, each line
			// the fake wrote: its own input's last line, then the masked
			// value, then the CLI's error. Asserted line for line.
			lastIn := c.stderr[strings.LastIndex(c.stderr, "\n")+1:]
			n := len(log.Lines)
			if err != nil || !ok || n < 2 || log.Lines[n-2] != lastIn || log.Lines[n-1] != "[redacted]" {
				t.Errorf("held %v %v, lines %q; want …%q, %q", ok, err, log.Lines, lastIn, "[redacted]")
			}
			if strings.Contains(strings.Join(log.Lines, "\n"), "secret-value-xyzzy") {
				t.Errorf("the build log was not redacted: %q", log.Lines)
			}

			// A start that gets past up clears what the failed one held.
			e.cli.up = "cat <<'EOF'\n" + fixtureBytes("up-ok.json") + "\nEOF\n"
			e.wire(t)
			if err := e.p.Start(context.Background(), v.ID); err != nil {
				t.Fatal(err)
			}
			e.p.idle()
			if _, ok, _ := e.p.BuildLog(context.Background(), v.ID); ok {
				t.Error("a later up kept the earlier failure's build log")
			}
		})
	}

	// Control: the success path says none of it.
	e := newEnv(t)
	e.wire(t)
	v := e.create(t, alpha, "")
	if v.State != workspace.Running {
		t.Fatalf("control: %s", v.State)
	}
	for _, s := range []string{"Remote Control cannot work", NoBrokerSentence, UpFailedSentence} {
		if strings.Contains(v.Steps[workspace.StepUp].Detail, s) || strings.Contains(deref(v.StateDetail), s) {
			t.Errorf("a successful up says %q", s)
		}
	}
	if _, ok, _ := e.p.BuildLog(context.Background(), v.ID); ok {
		t.Error("a successful up holds a build log")
	}
}

func TestUpFailureSentenceTable(t *testing.T) {
	for in, want := range map[string]string{
		"": UpFailedSentence,
		"drydock feature: DO_NOT_TRACK is set in the env block of /x":              RemoteControlSentence([]string{"DO_NOT_TRACK"}),
		"drydock feature: DO_NOT_TRACK is mentioned":                               UpFailedSentence,
		"drydock feature: CLAUDE_CONFIG_DIR is set in the container's environment": UpFailedSentence,
		probeNoBroker + "; GitHub access will fail.":                               NoBrokerSentence,
	} {
		if got := upFailure([]byte(in)); got != want {
			t.Errorf("upFailure(%q) = %q, want %q", in, got, want)
		}
	}
	if s := RemoteControlSentence([]string{"A", "B"}); !strings.Contains(s, "A, B are set") || !strings.Contains(s, "Remove them") {
		t.Errorf("plural: %s", s)
	}
}

// TestTheBuildLogIsMaskedLikeTheSessionLog: the held build log goes through
// the one redactor (internal/redact) when it is served — values before
// patterns, so a secret that contains a token shape is masked whole, not left
// with its prefix showing — and masks a github_pat_ URL, a bearer header, an
// Anthropic key and a token query parameter. A secret granted after the
// failure is masked too, since masking is at serve time. Values that cannot
// be read withhold the log rather than serve it unmasked. The control is a
// plain line, served as written.
func TestTheBuildLogIsMaskedLikeTheSessionLog(t *testing.T) {
	const value = "pfx-SECRETPART-ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	lines := []string{
		"ARG X=" + value,
		"url https://x:github_pat_11AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA@github.com",
		"Authorization: Bearer sk-ant-api03-AAAAAAAAAAAAAAAAAAAA",
		"GET /cb?code=abcdef123456&state=x",
		"later-granted-value-77",
		"Step 2/9 : RUN make",
	}
	e := newEnv(t)
	e.cli.up = upFailing(strings.Join(lines, "\n"))
	e.wire(t)
	values := []string{value}
	var valuesErr error
	e.p.Redact = func(context.Context, string) ([]string, error) { return values, valuesErr }
	v := e.create(t, alpha, "")
	ctx := context.Background()
	log, ok, err := e.p.BuildLog(ctx, v.ID)
	if err != nil || !ok {
		t.Fatalf("held %v %v", ok, err)
	}
	joined := strings.Join(log.Lines, "\n")
	for _, leak := range []string{"SECRETPART", "pfx-", "github_pat_11", "sk-ant-api03", "abcdef123456"} {
		if strings.Contains(joined, leak) {
			t.Errorf("%q survived: %q", leak, log.Lines)
		}
	}
	for _, want := range []string{"ARG X=[redacted]", "Bearer [redacted]", "?code=[redacted]", "Step 2/9 : RUN make", "later-granted-value-77"} {
		if !strings.Contains(joined, want) {
			t.Errorf("want %q in %q", want, log.Lines)
		}
	}
	// Granted after the failure: masked from the next read on.
	values = append(values, "later-granted-value-77")
	log, _, _ = e.p.BuildLog(ctx, v.ID)
	if strings.Contains(strings.Join(log.Lines, "\n"), "later-granted-value-77") {
		t.Error("a value granted after the failure was not masked")
	}
	// Values that cannot be read: withheld, no lines.
	valuesErr = errors.New("a secret does not open")
	log, ok, err = e.p.BuildLog(ctx, v.ID)
	if !errors.Is(err, ErrLogWithheld) || !ok || len(log.Lines) != 0 {
		t.Errorf("unreadable values: %v %v %q", ok, err, log.Lines)
	}
}
