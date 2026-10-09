package dockerguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/krelinga/drydock/internal/ephemeral"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

// The daemon's default log configuration, which a started container may
// carry without runArgs approved.
//
// Docker writes into every container it creates the log configuration it
// gives it: --log-driver and --log-opt where argv names them, and otherwise
// the daemon's own default — /etc/docker/daemon.json's "log-driver" and
// "log-opts", or dockerd's flags. So a container created through the guard,
// with no log options in its argv, carries `{"Type":"journald",…}` or
// `{"Type":"json-file","Config":{"max-size":"10m",…}}` on a host whose
// operator chose them, and a start held to the file drivers with no options
// refused every second start of a stopped workspace there.
//
// `docker info` names the default driver but not its options, and
// daemon.json is neither the only place they are set nor readable by
// everyone. So the guard asks the daemon itself: it creates a container with
// no log options from ProbeImage — never started, no network — reads its
// LogConfig, and removes it. That is, by construction, what this daemon now
// gives a container whose argv names no log option (measured on Docker
// 29.8.2 under json-file with max-size and max-file, journald with a tag,
// local with max-size, and no default at all: the probe's LogConfig equalled
// a plain create's every time). It is asked only when a start needs it.

// LabelLogProbe is the label a probe container carries, valued with the
// workspace id: ephemeral.LogProbe's. Not the workspace label, so
// reconciliation never sees one, and how a probe a killed guard left behind
// is found and removed before the next, and by boot's sweep.
const LabelLogProbe = string(ephemeral.LogProbe)

// ProbeTimeout bounds the probe, a pull of ProbeImage included.
// RemoveTimeout bounds removing it, apart: the probe's own time may be what
// ran out. ProbeSettle is how long a create that was cut off is given to
// finish on the daemon before its label is listed again.
var (
	ProbeTimeout  = 2 * time.Minute
	RemoveTimeout = 30 * time.Second
	ProbeSettle   = 3 * time.Second
)

// pinnedImage is an image reference pinned by digest, as
// config.CleanupImage must be.
var pinnedImage = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[0-9a-f]{64}$`)

// LogConfig is HostConfig.LogConfig as docker inspect writes it.
type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

// equal: the same driver and the same options, an empty Config and a
// missing one alike.
func (l LogConfig) equal(o LogConfig) bool {
	if l.Type != o.Type || len(l.Config) != len(o.Config) {
		return false
	}
	for k, v := range l.Config {
		if w, ok := o.Config[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// String is the log configuration for the service log: the driver and the
// option names, never their values, which can carry an address or a token.
func (l LogConfig) String() string {
	keys := make([]string, 0, len(l.Config))
	for k := range l.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("driver %q, options %q", l.Type, keys)
}

// DaemonLogConfig learns the daemon's default log configuration by creating
// a container with no log options, through real (the real docker), reading
// it and removing it. The container is created from p.ProbeImage, which must
// be pinned by digest, with --network none and the probe label, and is never
// started.
//
// It is an internal/ephemeral helper, on the real clock (the guard is a
// process of its own, with no Drydock clock to join): a probe an earlier
// guard was killed beside is removed by the label before the create; the
// probe is removed by its id and its label however this ends, under a bound
// of its own (RemoveTimeout), since the probe's may be what ran out; and a
// create cut off, which the daemon may still finish, is waited for (up to
// ProbeSettle) and removed. A probe it could not remove is said on warn (the
// guard's stderr, which reaches the service log with up's), and boot's helper
// sweep removes it.
func DaemonLogConfig(real string, p *Policy, warn io.Writer) (*LogConfig, error) {
	if p == nil || !pinnedImage.MatchString(p.ProbeImage) {
		return nil, errors.New("no probe image pinned by digest in the policy")
	}
	ws := p.IDLabels[p.LabelPrefix+"."+ephemeral.WorkspaceLabel]
	if p.LabelPrefix == "" || ws == "" || strings.ContainsAny(ws, "=,\n") {
		return nil, errors.New("no workspace id-label in the policy to label the probe with")
	}
	// A child docker left holding the pipes must not hold this up.
	run := subproc.Exec{Resolver: subproc.FixedResolver{"docker": real}, WaitDelay: time.Second}
	h := ephemeral.Helper{Docker: run, Prefix: p.LabelPrefix, Kind: ephemeral.LogProbe, Value: ws,
		Settle: ProbeSettle, RemoveTimeout: RemoveTimeout}
	label, err := h.Label()
	if err != nil {
		return nil, err
	}
	// The guard's own process: nothing above it to inherit a context from.
	ctx, cancel := sys.WithTimeout(context.Background(), sys.RealClock{}, ProbeTimeout)
	defer cancel()
	// After a create that returned its id: nothing is still landing, so no
	// settle — the id itself is removed, and whatever the label lists.
	end := func(what string) {
		if _, err := h.End(ctx, false); err != nil && warn != nil {
			fmt.Fprintf(warn, "drydock-docker-guard: %s could not be removed: %v; boot's helper sweep removes it\n", what, err)
		}
	}
	// create pulls ProbeImage when the daemon lacks it, deliberately: it is
	// pinned by digest (the same busybox the cleanup and volume-owner
	// helpers run), the pull is bounded by ProbeTimeout, and a host that
	// cannot pull fails closed: the start is refused, as before this probe.
	var stderr bytes.Buffer
	id, err := h.Create(ctx, []string{"create", "--label", label, "--network", "none", p.ProbeImage}, &capped{buf: &stderr, max: 4 << 10})
	if err != nil {
		if errors.Is(err, ephemeral.ErrNotRun) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(short(stderr.String())))
	}
	defer end("the log probe " + id)
	var out bytes.Buffer
	stderr.Reset()
	res := run.Run(ctx, subproc.Cmd{Name: "docker", Args: []string{"inspect", "--type", "container", "--", id},
		Stdout: &capped{buf: &out, max: 1 << 20}, Stderr: &capped{buf: &stderr, max: 4 << 10}})
	if res.Err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("docker inspect: exit %d %v: %s", res.ExitCode, res.Err, strings.TrimSpace(short(stderr.String())))
	}
	var all []struct {
		ID         string `json:"Id"`
		HostConfig *struct {
			LogConfig *LogConfig
		}
	}
	if err := json.Unmarshal(out.Bytes(), &all); err != nil {
		return nil, errors.New("docker inspect of the probe could not be read")
	}
	if len(all) != 1 || all[0].ID != id || all[0].HostConfig == nil || all[0].HostConfig.LogConfig == nil ||
		all[0].HostConfig.LogConfig.Type == "" {
		return nil, errors.New("docker inspect of the probe did not give its log configuration")
	}
	return all[0].HostConfig.LogConfig, nil
}

// capped keeps the first max bytes and reports every write whole.
type capped struct {
	buf *bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}
