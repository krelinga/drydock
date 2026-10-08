package provision

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/krelinga/drydock/internal/redact"
)

// Design §12 pairs each failure with a sentence that names its cause and the
// one action that fixes it. Three of them surface at §6 step 6, as a failed
// `devcontainer up`, and its stderr is the only place the cause is written —
// stderr that can quote the repository's own configuration and commands, so it
// never reaches an event (workspace.Public). What reaches the step's detail is
// one of these fixed sentences, chosen by matching lines the Feature itself
// prints. Nothing from the line is copied into the sentence but a name from a
// fixed list.

// remoteControlVars are design §2.1's variables, which disable Remote Control
// while everything else builds and starts. The Feature's preflight
// (drydock-preflight, postCreateCommand) refuses a container that sets any of
// them, naming it.
var remoteControlVars = []string{
	"ANTHROPIC_BASE_URL", "DISABLE_TELEMETRY", "DO_NOT_TRACK",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_GROWTHBOOK",
}

// preflightVar matches the preflight's three wordings for one of them: set in
// the environment, set in a settings file's env block, and ANTHROPIC_BASE_URL
// set to something else.
var preflightVar = regexp.MustCompile(`drydock feature: ([A-Z_]+) is set (?:in the|to something other than)`)

// probeNoBroker is drydock-probe's line (postStartCommand) when the broker
// socket did not answer at the container's start.
const probeNoBroker = "drydock: this container was started without a working broker socket"

const (
	// UpFailedSentence is an image build or container start that failed for
	// a reason of the repository's own: the configuration, a Feature, a
	// lifecycle command. The clone is kept, which is what makes a fix and a
	// rebuild one click (§12, *Image build fails*).
	UpFailedSentence = "The image build or the container's start failed. The clone is kept: fix the dev container " +
		"configuration and Rebuild. Until Drydock restarts, the workspace page shows the build's last lines."
	// NoBrokerSentence is §12's *Broker socket missing or stale*, at a start:
	// "GitHub access unavailable", never a git error.
	NoBrokerSentence = "GitHub access unavailable for this workspace: its broker socket did not answer when the " +
		"container started. Start it again, or Rebuild if that does not help; the clone is kept."
)

// RemoteControlSentence is §12's *Remote Control unavailable*: the build
// failed on purpose, naming the variables, rather than produce a container
// whose sessions never connect.
func RemoteControlSentence(names []string) string {
	verb := "is"
	if len(names) > 1 {
		verb = "are"
	}
	return "Remote Control cannot work in this container: " + strings.Join(names, ", ") + " " + verb +
		" set in its environment or a Claude Code settings file, which disables it. Remove " +
		map[bool]string{true: "them", false: "it"}[len(names) > 1] +
		" from the dev container configuration, then Rebuild; the clone is kept."
}

// upFailure is the sentence for a failed `up`, read from its stderr.
func upFailure(stderr []byte) string {
	found := map[string]bool{}
	for _, m := range preflightVar.FindAllSubmatch(stderr, -1) {
		found[string(m[1])] = true
	}
	var names []string
	for _, v := range remoteControlVars { // only names from the fixed list, in its order
		if found[v] {
			names = append(names, v)
		}
	}
	switch {
	case len(names) > 0:
		return RemoteControlSentence(names)
	case strings.Contains(string(stderr), probeNoBroker):
		return NoBrokerSentence
	}
	return UpFailedSentence
}

// BuildLog is the last lines of a workspace's latest failed `devcontainer up`
// (§12, *Image build fails*: "the last 50 build lines"), held in memory only —
// like the session server's log, never persisted — and cleared when the next
// `up` starts or the workspace is deleted. A Drydock restart loses it.
//
// Masked twice, with the one redactor the session server's log uses
// (internal/redact: granted secret values first, then credential shapes):
//
//   - when it is kept, with the values granted then — and only that masked
//     copy is held, so the raw log never stays in memory, and a value later
//     rotated, ungranted or deleted stays masked (the session log's
//     never-forget property, by another route);
//   - when it is served, with the values granted now, so a secret granted
//     after the failure is masked too.
//
// Each line is masked whole and only then cut to maxBuildLine, at serve time,
// so no value or token straddling the cut can show its prefix. Values that
// cannot be read fail closed at either point: a log that could not be masked
// when kept holds no lines at all and is withheld for good, and one that
// cannot be masked now is withheld until it can.
type BuildLog struct {
	Lines []string
	At    time.Time
	// unmasked: the values could not be read when the log was kept, so no
	// line was kept (Lines is empty) and it is never served.
	unmasked bool
}

// buildLogLines is how many lines a BuildLog keeps, and maxBuildLine how
// many bytes of each are served. The lines kept are bounded already: they
// come from the last MiB of `up`'s stderr (container.UpStderrTail).
const (
	buildLogLines = 50
	maxBuildLine  = 2000
)

// ErrLogWithheld is a held build log that cannot be served because the
// values it must be masked of could not be read — the secrets snapshot is
// undeliverable. It fails closed: no lines rather than unmasked ones.
var ErrLogWithheld = errors.New("provision: the build log is withheld: the secret values to mask it of cannot be read")

// values is the workspace's granted secret values, for masking.
func (p *Provisioner) values(ctx context.Context, id string) ([]string, error) {
	if p.Redact == nil {
		return nil, nil
	}
	return p.Redact(ctx, id)
}

func (p *Provisioner) keepBuildLog(ctx context.Context, id string, stderr []byte) {
	lines := splitLines(stderr)
	if len(lines) > buildLogLines {
		lines = lines[len(lines)-buildLogLines:]
	}
	b := BuildLog{At: p.Workspaces.Env.Clock.Now()}
	if values, err := p.values(context.WithoutCancel(ctx), id); err != nil {
		b.unmasked = true // nothing raw is held
	} else {
		b.Lines = make([]string, len(lines))
		for i, l := range lines {
			b.Lines[i] = redact.String(l, values)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buildLogs == nil {
		p.buildLogs = map[string]BuildLog{}
	}
	p.buildLogs[id] = b
}

func (p *Provisioner) dropBuildLog(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.buildLogs, id)
}

// BuildLog is the workspace's held build log, masked now: held is false when
// there is none, and ErrLogWithheld when there is one that cannot be masked.
func (p *Provisioner) BuildLog(ctx context.Context, id string) (BuildLog, bool, error) {
	p.mu.Lock()
	b, ok := p.buildLogs[id]
	p.mu.Unlock()
	if !ok {
		return BuildLog{}, false, nil
	}
	if b.unmasked {
		return BuildLog{At: b.At}, true, ErrLogWithheld
	}
	values, err := p.values(ctx, id)
	if err != nil {
		return BuildLog{At: b.At}, true, ErrLogWithheld
	}
	out := make([]string, len(b.Lines))
	for i, l := range b.Lines {
		l = redact.String(l, values) // masked whole, then cut
		if len(l) > maxBuildLine {
			l = l[:maxBuildLine]
		}
		out[i] = l
	}
	return BuildLog{Lines: out, At: b.At}, true, nil
}
