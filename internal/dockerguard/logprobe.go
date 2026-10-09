package dockerguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
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
// workspace id: not the workspace label, so reconciliation never sees one,
// and how a probe a killed guard left behind is found and removed before the
// next.
const LabelLogProbe = "log-probe"

// ProbeTimeout bounds the probe, a pull of ProbeImage included.
var ProbeTimeout = 2 * time.Minute

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
func DaemonLogConfig(real string, p *Policy) (*LogConfig, error) {
	if p == nil || !pinnedImage.MatchString(p.ProbeImage) {
		return nil, errors.New("no probe image pinned by digest in the policy")
	}
	ws := p.IDLabels[p.LabelPrefix+".workspace"]
	if p.LabelPrefix == "" || ws == "" || strings.ContainsAny(ws, "=,\n") {
		return nil, errors.New("no workspace id-label in the policy to label the probe with")
	}
	label := p.LabelPrefix + "." + LabelLogProbe + "=" + ws
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	docker := func(args ...string) ([]byte, error) {
		var out, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, real, args...)
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(short(stderr.String())))
		}
		return out.Bytes(), nil
	}
	// A probe an earlier guard was killed beside: removed first, so they
	// never accumulate.
	out, err := docker("ps", "--all", "--quiet", "--no-trunc", "--filter", "label="+label)
	if err != nil {
		return nil, err
	}
	if stray := strings.Fields(string(out)); len(stray) > 0 {
		for _, id := range stray {
			if !fullID.MatchString(id) {
				return nil, fmt.Errorf("docker ps: %q is not a container id", short(id))
			}
		}
		if _, err := docker(append([]string{"rm", "--force", "--volumes", "--"}, stray...)...); err != nil {
			return nil, err
		}
	}
	out, err = docker("create", "--label", label, "--network", "none", p.ProbeImage)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(out))
	if !fullID.MatchString(id) {
		return nil, fmt.Errorf("docker create: %q is not a container id", short(id))
	}
	defer docker("rm", "--force", "--volumes", "--", id)
	out, err = docker("inspect", "--type", "container", "--", id)
	if err != nil {
		return nil, err
	}
	var all []struct {
		ID         string `json:"Id"`
		HostConfig *struct {
			LogConfig *LogConfig
		}
	}
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, errors.New("docker inspect of the probe could not be read")
	}
	if len(all) != 1 || all[0].ID != id || all[0].HostConfig == nil || all[0].HostConfig.LogConfig == nil ||
		all[0].HostConfig.LogConfig.Type == "" {
		return nil, errors.New("docker inspect of the probe did not give its log configuration")
	}
	return all[0].HostConfig.LogConfig, nil
}
