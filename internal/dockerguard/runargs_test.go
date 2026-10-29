package dockerguard

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// runArgsAt is the recorded image run as the guard sees it under p — its own
// additions first — and where the CLI writes a repository's runArgs: right
// after Drydock's id-labels, before what the CLI writes for init,
// privileged, capAdd, securityOpt and the entrypoint (the hostile recording).
func runArgsAt(t *testing.T, p *Policy, root string) ([]string, int) {
	t.Helper()
	base := WithAdded(p, command(t, Recorded(t, "image", root), "run"))
	at := slices.Index(base, "drydock.branch=main") + 1
	if at == 0 {
		t.Fatal("no id-labels in the recorded run")
	}
	return base, at
}

func approve(p *Policy, ra ...string) *Policy {
	raw, _ := json.Marshal(ra)
	p.Approved = []Setting{{"runArgs", raw}}
	return p
}

// The review's probe of #193. A bare "-l" approved as runArgs, written where
// the CLI writes runArgs, followed by what a moved Feature adds — init, then
// privileged: docker reads "-l --init" as a label and runs privileged. Removed
// at the CLI's own id-label "-l" (or, with a policy whose first addition is a
// label, the guard's own), the bare flag left "drydock.workspace=…" (or the
// guard's label) to read as the image, and nothing after it was checked. A
// runArgs ending in an option with no value is refused even approved
// (runArgs_open_ended). The controls: the same argv unapproved is refused for
// privileged, read through; a self-contained approval of the same label,
// "-l x=y", with privileged after it, is refused for privileged; without it,
// it runs.
func TestAnOpenEndedRunArgsIsRefused(t *testing.T) {
	root := t.TempDir()
	for name, policy := range map[string]func() *Policy{
		"the CLI's -l":   func() *Policy { p := labelPolicy(root); p.Name = fixtureName; return p },
		"the guard's -l": func() *Policy { return labelPolicy(root) },
	} {
		base, at := runArgsAt(t, policy(), root)
		probe := insert(base, at, "-l", "--init", "--privileged")
		if d := Check(policy(), probe); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingPrivileged}) {
			t.Errorf("%s, control: unapproved, %+v, want refused naming %s", name, d, SettingPrivileged)
		}
		if d := Check(approve(policy(), "-l"), probe); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingRunArgsOpen}) {
			t.Errorf("%s: a bare -l approved: %+v, want refused naming %s", name, d, SettingRunArgsOpen)
		}
		whole := approve(policy(), "-l", "x=y")
		if d := Check(whole, insert(base, at, "-l", "x=y", "--init", "--privileged")); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingPrivileged}) {
			t.Errorf("%s, control: -l x=y approved, privileged after it: %+v", name, d)
		}
		if d := Check(whole, insert(base, at, "-l", "x=y", "--init")); d.Refused {
			t.Errorf("%s, control: -l x=y approved: refused %+v", name, d)
		}
	}
}

// Self-contained is the last argument: an option that takes a value and
// carries none — by the guard's table, or one it does not know, whose arity
// it cannot vouch for — is open-ended, as is "--"; a boolean of docker run's,
// an inline value, a value after the option, or a short cluster whose value
// is the rest of it is not.
func TestOpenEnded(t *testing.T) {
	for _, ra := range [][]string{
		{"-l"}, {"--label"}, {"-a"}, {"--mount"}, {"--name"}, {"-e"}, {"-u"}, {"-p"},
		{"--network"}, {"--network", "host", "-v"}, {"-il"}, {"-it", "-e"}, {"--"}, {"--", "x"},
	} {
		if openEnded(ra) == "" {
			t.Errorf("%q is self-contained", ra)
		}
	}
	for _, ra := range [][]string{
		{}, {"--network=host"}, {"--network", "host"}, {"--init"}, {"--read-only"}, {"--privileged"},
		{"-it"}, {"-d"}, {"-lfoo=bar"}, {"-ilx=y"}, {"-e", "A=1"}, {"--label=x"}, {"--name=box"},
		{"-v", "/a:/b", "--rm"},
	} {
		if why := openEnded(ra); why != "" {
			t.Errorf("%q: %s", ra, why)
		}
	}
	// -a and --mount, bare, through Check: refused even approved; the
	// control is each with its value.
	root := t.TempDir()
	for _, bare := range [][]string{{"-a"}, {"--mount"}} {
		p := approve(namedPolicy(root, nil), bare...)
		base, at := runArgsAt(t, p, root)
		if d := Check(p, insert(base, at, bare[0], "--init")); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingRunArgsOpen}) {
			t.Errorf("%q approved: %+v", bare, d)
		}
	}
	for _, whole := range [][]string{{"-a", "STDIN"}, {"--mount", "type=tmpfs,target=/t"}} {
		p := approve(namedPolicy(root, nil), whole...)
		base, at := runArgsAt(t, p, root)
		if d := Check(p, insert(base, at, append(whole, "--init")...)); d.Refused {
			t.Errorf("control, %q approved: %+v", whole, d)
		}
	}
}

// An approved runArgs is removed only where it starts at an option boundary
// of the parse, never on the value of an option the CLI wrote. Here a moved
// image sets containerUser to "--init", which the CLI writes as "-u --init"
// — docker's user is "--init" — and then privileged; the repository's
// approved runArgs is "--init". Removed at its first occurrence, the user's
// value, "-u" took "--privileged" as its value and it ran unread. The
// control is the same approval and argv without privileged, which passes:
// the repository's own --init is removed where it starts an option.
func TestApprovedRunArgsAreRemovedOnlyAtAnOptionBoundary(t *testing.T) {
	root := t.TempDir()
	p := approve(namedPolicy(root, nil), "--init")
	base, at := runArgsAt(t, p, root)
	ok := insert(base, at, "-u", "--init", "--init")
	if d := Check(p, ok); d.Refused {
		t.Fatalf("control: refused %+v", d)
	}
	bad := insert(base, at, "-u", "--init", "--privileged", "--init")
	if d := Check(p, bad); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingPrivileged}) {
		t.Errorf("a runArgs matching the value of the CLI's -u: %+v, want refused naming %s", d, SettingPrivileged)
	}
}
