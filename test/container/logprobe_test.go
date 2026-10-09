package container_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/dockerguard"
)

// The guard's probe of the daemon's default log configuration, against the
// real daemon: what it reads is what a plain `docker create` with no log
// option gets — on whatever default this daemon has — and it leaves no
// container behind. Then the start check, on real inspect output: that plain
// container starts with nothing approved, and one created with a log option
// the default does not have (only argv can give it one) is refused, its
// control the same container with runArgs approved. The daemon's default
// cannot be changed here without restarting the daemon under every other
// test, so the other defaults (journald, max-size) were measured by hand on a
// nested Docker 29 and are pinned in internal/dockerguard's unit tests.
func TestTheLogProbeReadsWhatAPlainCreateGets(t *testing.T) {
	needDocker(t)
	p := prefix(t)
	real, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	policy := &dockerguard.Policy{Version: dockerguard.PolicyVersion, LabelPrefix: p,
		IDLabels:   map[string]string{p + ".workspace": "01JLOGPR0BE000000000000000"},
		ProbeImage: config.DefaultCleanupImage, Clone: t.TempDir(), TempDir: t.TempDir()}
	got, err := dockerguard.DaemonLogConfig(real, policy, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if left := docker(t, "ps", "-aq", "--filter", "label="+p+"."+dockerguard.LabelLogProbe); left != "" {
		t.Errorf("the probe left %s", left)
	}
	create := func(args ...string) (string, []byte) {
		id := docker(t, append(append([]string{"create", "--label", "drydock-logprobe-test=" + p}, args...), config.DefaultCleanupImage)...)
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
		return id, []byte(docker(t, "inspect", "--type", "container", "--", id))
	}
	plainID, plain := create()
	var all []struct {
		HostConfig struct{ LogConfig dockerguard.LogConfig }
	}
	if err := json.Unmarshal(plain, &all); err != nil || len(all) != 1 {
		t.Fatalf("inspect: %v", err)
	}
	want := all[0].HostConfig.LogConfig
	if got.Type != want.Type || len(got.Config) != len(want.Config) || (len(want.Config) > 0 && !reflect.DeepEqual(got.Config, want.Config)) {
		t.Errorf("the probe read %+v, a plain create got %+v", *got, want)
	}
	probe := func() (*dockerguard.LogConfig, error) { return dockerguard.DaemonLogConfig(real, policy, os.Stderr) }
	if d := dockerguard.CheckStarted(policy, []string{plainID}, plain, probe); d.Refused {
		t.Errorf("a plain container: %+v", d)
	}
	if want.Type != "json-file" && want.Type != "local" {
		t.Skipf("the daemon's default driver %s takes no max-size", want.Type)
	}
	// A max-size this daemon's default does not have.
	size := "7m"
	if want.Config["max-size"] == size {
		size = "9m"
	}
	optID, opt := create("--log-opt", "max-size="+size)
	if d := dockerguard.CheckStarted(policy, []string{optID}, opt, probe); !d.Refused ||
		!reflect.DeepEqual(d.Settings, []string{dockerguard.SettingRunArgs}) || !strings.Contains(strings.Join(d.Why, ";"), "not the daemon's default") {
		t.Errorf("a container with --log-opt max-size=%s: %+v", size, d)
	}
	approved := *policy
	approved.Approved = []dockerguard.Setting{{Field: "runArgs", Value: json.RawMessage(`["--log-opt","max-size=` + size + `"]`)}}
	if d := dockerguard.CheckStarted(&approved, []string{optID}, opt, probe); d.Refused {
		t.Errorf("the same, runArgs approved: %+v", d)
	}
}
