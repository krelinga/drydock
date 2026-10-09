package dockerguard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// recordedInspect is test/fixtures/devcontainer/docker-inspect-<name>.json —
// docker inspect of the container a recorded up made, as the daemon wrote
// it — moved to root, as one object, with its id.
func recordedInspect(t *testing.T, name, root string) (map[string]any, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "devcontainer", "docker-inspect-"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var all []map[string]any
	if err := json.Unmarshal([]byte(strings.ReplaceAll(string(b), fixtureRoot, root)), &all); err != nil || len(all) != 1 {
		t.Fatalf("%s: %v", name, err)
	}
	return all[0], all[0]["Id"].(string)
}

// with is the recorded container with HostConfig fields replaced (nil
// deletes one), as inspect JSON.
func with(t *testing.T, c map[string]any, hc map[string]any) []byte {
	t.Helper()
	b, _ := json.Marshal(c)
	var cp map[string]any
	json.Unmarshal(b, &cp)
	h := cp["HostConfig"].(map[string]any)
	for k, v := range hc {
		if v == nil {
			delete(h, k)
			continue
		}
		h[k] = v
	}
	out, _ := json.Marshal([]any{cp})
	return out
}

func mountsPlus(c map[string]any, extra ...map[string]any) []any {
	ms := append([]any(nil), c["HostConfig"].(map[string]any)["Mounts"].([]any)...)
	for _, e := range extra {
		ms = append(ms, e)
	}
	return ms
}

// A start is held to the policy as the container was created, every
// HostConfig field read — against the container a recorded up really made,
// each field changed as docker writes it (measured). Each refusal beside
// its control: the same container with the access approved starts.
func TestStartedContainersAreHeldToThePolicy(t *testing.T) {
	root := t.TempDir()
	c, id := recordedInspect(t, "image", root)
	ids := []string{id}
	if d := CheckStarted(FixturePolicy(root, nil), ids, with(t, c, nil), nil); d.Refused {
		t.Fatalf("control, the recorded container: %+v", d)
	}
	for _, tc := range []struct {
		name     string
		hc       map[string]any
		want     string
		approved []Setting
	}{
		{"privileged", map[string]any{"Privileged": true, "MaskedPaths": []string{}, "ReadonlyPaths": []string{},
			"SecurityOpt": []string{"label=disable"}}, SettingPrivileged, []Setting{{"privileged", json.RawMessage(`true`)}}},
		{"a capability", map[string]any{"CapAdd": []string{"CAP_SYS_ADMIN"}}, SettingCapAdd,
			[]Setting{{"capAdd", json.RawMessage(`["SYS_ADMIN"]`)}}},
		{"a security option", map[string]any{"SecurityOpt": []string{"apparmor=unconfined"}}, SettingSecurityOpt,
			[]Setting{{"securityOpt", json.RawMessage(`["apparmor=unconfined"]`)}}},
		// --security-opt systempaths=unconfined: docker drops it from
		// SecurityOpt and empties both lists (measured).
		{"systempaths=unconfined", map[string]any{"MaskedPaths": []string{}, "ReadonlyPaths": []string{}}, SettingSecurityOpt,
			[]Setting{{"securityOpt", json.RawMessage(`["systempaths=unconfined"]`)}}},
		{"one path unmasked", map[string]any{"MaskedPaths": defaultMaskedPaths[1:]}, SettingSecurityOpt,
			[]Setting{{"securityOpt", json.RawMessage(`["systempaths=unconfined"]`)}}},
		// --device-cgroup-rule 'b *:* rwm': with the default CAP_MKNOD,
		// container root reads the host's disks.
		{"a device cgroup rule", map[string]any{"DeviceCgroupRules": []string{"b *:* rwm"}}, SettingRunArgs,
			[]Setting{{"runArgs", json.RawMessage(`["--device-cgroup-rule","b *:* rwm"]`)}}},
		{"a cgroup parent", map[string]any{"CgroupParent": "foo"}, SettingRunArgs, nil},
		{"a sysctl", map[string]any{"Sysctls": map[string]string{"net.ipv4.ip_forward": "1"}}, SettingRunArgs, nil},
		{"-v", map[string]any{"Binds": []string{"/:/host"}}, SettingRunArgs,
			[]Setting{{"runArgs", json.RawMessage(`["-v","/:/host"]`)}}},
		{"host network", map[string]any{"NetworkMode": "host"}, SettingRunArgs,
			[]Setting{{"runArgs", json.RawMessage(`["--network=host"]`)}}},
		{"another container's pid namespace", map[string]any{"PidMode": "container:abc"}, SettingRunArgs, nil},
		{"the host's cgroup namespace", map[string]any{"CgroupnsMode": "host"}, SettingRunArgs, nil},
		{"a runtime", map[string]any{"Runtime": "sysbox-runc"}, SettingRunArgs, nil},
		{"a restart policy", map[string]any{"RestartPolicy": map[string]any{"Name": "always"}}, SettingRunArgs, nil},
		{"a device", map[string]any{"Devices": []map[string]any{{"PathOnHost": "/dev/kmsg"}}}, SettingRunArgs, nil},
		{"a field docker adds later", map[string]any{"FutureHostAccess": true}, SettingRunArgs, nil},
		// Review of #78, round 3: a log driver runs on the host. gelf to
		// the host's loopback received the container's output (measured);
		// syslog reaches a host unix socket.
		{"gelf", map[string]any{"LogConfig": map[string]any{"Type": "gelf", "Config": map[string]string{"gelf-address": "udp://127.0.0.1:12201"}}}, SettingRunArgs,
			[]Setting{{"runArgs", json.RawMessage(`["--log-driver","gelf","--log-opt","gelf-address=udp://127.0.0.1:12201"]`)}}},
		{"syslog to a unix socket", map[string]any{"LogConfig": map[string]any{"Type": "syslog", "Config": map[string]string{"syslog-address": "unixgram:///dev/log"}}}, SettingRunArgs,
			[]Setting{{"runArgs", json.RawMessage(`["--log-driver","syslog","--log-opt","syslog-address=unixgram:///dev/log"]`)}}},
		{"a driver, no options", map[string]any{"LogConfig": map[string]any{"Type": "fluentd", "Config": map[string]string{}}}, SettingRunArgs, nil},
		{"json-file with options", map[string]any{"LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "1m"}}}, SettingRunArgs, nil},
		{"GPUs", map[string]any{"DeviceRequests": []map[string]any{{"Count": -1}}}, SettingGPU,
			[]Setting{{"hostRequirements.gpu", json.RawMessage(`true`)}}},
		{"a published port", map[string]any{"PortBindings": map[string]any{"80/tcp": []map[string]string{{"HostIp": "", "HostPort": "8080"}}}}, SettingAppPort,
			[]Setting{{"appPort", json.RawMessage(`["8080:80"]`)}}},
		{"a bind of /", map[string]any{"Mounts": mountsPlus(c, map[string]any{"Type": "bind", "Source": "/", "Target": "/host"})}, SettingMounts,
			[]Setting{{"mounts", json.RawMessage(`["type=bind,source=/,target=/host"]`)}}},
		{"an approved bind, shared back to the host", map[string]any{"Mounts": mountsPlus(c, map[string]any{"Type": "bind", "Source": "/etc", "Target": "/e",
			"BindOptions": map[string]any{"Propagation": "rshared"}})}, SettingMounts,
			[]Setting{{"mounts", json.RawMessage(`["type=bind,source=/etc,target=/e,bind-propagation=rshared"]`)}}},
		{"a volume that binds", map[string]any{"Mounts": mountsPlus(c, map[string]any{"Type": "volume", "Source": "x-" + DevcontainerID(FixtureLabels), "Target": "/x",
			"VolumeOptions": map[string]any{"DriverConfig": map[string]any{"Options": map[string]string{"o": "bind", "device": "/"}}}})}, SettingMounts, nil},
		// An approved seccomp profile file is stored as the profile's JSON:
		// not what was approved, so refused even approved (design §6).
		{"a seccomp profile", map[string]any{"SecurityOpt": []string{`seccomp={"defaultAction":"SCMP_ACT_ALLOW"}`}}, SettingSecurityOpt, nil},
	} {
		in := with(t, c, tc.hc)
		d := CheckStarted(FixturePolicy(root, nil), ids, in, nil)
		if !d.Refused || !reflect.DeepEqual(d.Settings, []string{tc.want}) {
			t.Errorf("%s: %+v, want refused naming %s", tc.name, d, tc.want)
		}
		if tc.approved != nil {
			if d := CheckStarted(FixturePolicy(root, tc.approved), ids, in, nil); d.Refused {
				t.Errorf("%s, approved: %+v", tc.name, d)
			}
		}
	}
	// What stays in the container needs nothing: the debugger pair, this
	// workspace's own volume, limits, an unknown field left zero.
	for name, hc := range map[string]map[string]any{
		"SYS_PTRACE and seccomp=unconfined": {"CapAdd": []string{"CAP_SYS_PTRACE"}, "SecurityOpt": []string{"seccomp=unconfined"}},
		"its own volume":                    {"Mounts": mountsPlus(c, map[string]any{"Type": "volume", "Source": "dind-var-lib-docker-" + DevcontainerID(FixtureLabels), "Target": "/var/lib/docker"})},
		"limits":                            {"Memory": 1 << 30, "PidsLimit": 100, "CapDrop": []string{"ALL"}, "ReadonlyRootfs": true},
		"json-file":                         {"LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{}}},
		"local":                             {"LogConfig": map[string]any{"Type": "local", "Config": map[string]string{}}},
		"masking more":                      {"MaskedPaths": append(append([]string(nil), defaultMaskedPaths...), "/proc/new")},
		"an unknown zero":                   {"FutureHostAccess": false, "AnotherFuture": map[string]any{}},
	} {
		if d := CheckStarted(FixturePolicy(root, nil), ids, with(t, c, hc), nil); d.Refused {
			t.Errorf("%s: %+v", name, d)
		}
	}
}

// Fail closed on an answer the guard did not expect: anything but one
// result for each full id given, each with its id and a HostConfig.
func TestStartedAnswersMustBeComplete(t *testing.T) {
	root := t.TempDir()
	c, id := recordedInspect(t, "image", root)
	good := with(t, c, nil)
	other := strings.Repeat("d", 64)
	var one []any
	json.Unmarshal(good, &one)
	two, _ := json.Marshal([]any{one[0], one[0]})
	noHC, _ := json.Marshal([]map[string]any{{"Id": id}})
	nullHC, _ := json.Marshal([]map[string]any{{"Id": id, "HostConfig": nil}})
	for name, tc := range map[string]struct {
		ids []string
		in  []byte
	}{
		"[{}]":                    {[]string{id}, []byte(`[{}]`)},
		`[{"Id":"x"}]`:            {[]string{id}, []byte(`[{"Id":"x"}]`)},
		`[{"HostConfig":null}]`:   {[]string{id}, []byte(`[{"HostConfig":null}]`)},
		"no HostConfig":           {[]string{id}, noHC},
		"a null HostConfig":       {[]string{id}, nullHC},
		"another container":       {[]string{other}, good},
		"one answer for two ids":  {[]string{id, other}, good},
		"two answers for one id":  {[]string{id}, two},
		"the same id twice":       {[]string{id, id}, two},
		"a short id":              {[]string{id[:12]}, good},
		"empty":                   {[]string{id}, []byte(`[]`)},
		"null":                    {[]string{id}, []byte(`null`)},
		"not JSON":                {[]string{id}, []byte(`{`)},
		"Privileged not a bool":   {[]string{id}, with(t, c, map[string]any{"Privileged": "yes"})},
		"CapAdd not a list":       {[]string{id}, with(t, c, map[string]any{"CapAdd": "CAP_SYS_ADMIN"})},
		"Mounts not a list":       {[]string{id}, with(t, c, map[string]any{"Mounts": "x"})},
		"LogConfig not an object": {[]string{id}, with(t, c, map[string]any{"LogConfig": "gelf"})},
		"MaskedPaths missing":     {[]string{id}, with(t, c, map[string]any{"MaskedPaths": nil})},
	} {
		d := CheckStarted(FixturePolicy(root, nil), tc.ids, tc.in, nil)
		if !d.Refused {
			t.Errorf("%s: passed", name)
		}
	}
	if d := CheckStarted(FixturePolicy(root, nil), []string{id}, good, nil); d.Refused {
		t.Errorf("control: %+v", d)
	}
	if d := CheckStarted(nil, []string{id}, good, nil); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingNoPolicy}) {
		t.Errorf("no policy: %+v", d)
	}
}

// Through Main: the guard reads the container with docker inspect (the fake
// docker answers from a file) and refuses the start of a privileged one, so
// docker start never runs; the same start of the recorded one runs.
func TestMainChecksAStart(t *testing.T) {
	self, _ := os.Executable()
	bin := t.TempDir()
	fake := filepath.Join(bin, "docker")
	os.WriteFile(fake, []byte(`#!/bin/sh
if [ "$1" = inspect ]; then cat "$(dirname "$0")/inspect.json"; exit 0; fi
for a; do printf '%s\0' "$a"; done > "$(dirname "$0")/ran"
`), 0o755)
	dir := filepath.Join(t.TempDir(), ".drydock", "guard")
	dp, err := (&Guard{Binary: self, Resolver: fixed{"docker": fake}}).Prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(dir))
	if err := WritePolicy(dir, *FixturePolicy(root, nil)); err != nil {
		t.Fatal(err)
	}
	c, id := recordedInspect(t, "image", root)
	os.WriteFile(filepath.Join(dir, "inspect.json"), with(t, c, map[string]any{"Privileged": true}), 0o600)
	if r := runGuard(t, dp, "", "start", id); r.ranDocker || r.code != ExitRefused || !strings.Contains(r.stderr, Marker+": privileged") {
		t.Errorf("a privileged container's start: ran %v, exit %d, %q", r.ranDocker, r.code, r.stderr)
	}
	os.WriteFile(filepath.Join(dir, "inspect.json"), with(t, c, nil), 0o600)
	if r := runGuard(t, dp, "", "start", id); !r.ranDocker || !reflect.DeepEqual(r.args, []string{"start", id}) {
		t.Errorf("control: the recorded container's start: ran %v with %q, stderr %q", r.ranDocker, r.args, r.stderr)
	}
	os.Remove(filepath.Join(dir, "inspect.json")) // inspect prints nothing
	if r := runGuard(t, dp, "", "start", id); r.ranDocker || !strings.Contains(r.stderr, SettingStartUnread) {
		t.Errorf("an unreadable inspect: ran %v, %q", r.ranDocker, r.stderr)
	}
}

type fixed map[string]string

func (f fixed) Resolve(name string) (string, error) {
	if p, ok := f[name]; ok {
		return p, nil
	}
	return "", os.ErrNotExist
}

// The daemon defaults measured on Docker 29.8.2 (a nested dockerd started
// with each as its --log-driver/--log-opt): what docker inspect shows for a
// container created with no log option, and for the guard's probe alike.
// (journald refuses max-size: the daemon will not start with it.)
var measuredLogDefaults = map[string]LogConfig{
	"json-file, max-size and max-file": {"json-file", map[string]string{"max-file": "3", "max-size": "10m"}},
	"journald, a tag":                  {"journald", map[string]string{"tag": "x"}},
	"local, max-size":                  {"local", map[string]string{"max-size": "5m"}},
}

func daemonSays(l LogConfig, asked *int) func() (*LogConfig, error) {
	return func() (*LogConfig, error) {
		*asked++
		return &l, nil
	}
}

// A container carrying the daemon's own default log configuration — which
// Docker writes into every container created with no log option — starts
// with nothing approved, on every daemon default measured. Each beside its
// controls: the same container is refused when the daemon's default is not
// what it carries, when the default cannot be read, and when nothing can
// ask; a log configuration that differs from the default is refused, and
// starts when runArgs is approved, without the daemon being asked.
func TestStartedLogConfigMayBeTheDaemonsDefault(t *testing.T) {
	root := t.TempDir()
	c, id := recordedInspect(t, "image", root)
	ids := []string{id}
	plain := LogConfig{"json-file", map[string]string{}}
	runArgs := []Setting{{"runArgs", json.RawMessage(`["--log-driver","gelf"]`)}}
	refusedAsRunArgs := func(d Decision) bool { return d.Refused && reflect.DeepEqual(d.Settings, []string{SettingRunArgs}) }
	for name, def := range measuredLogDefaults {
		in := with(t, c, map[string]any{"LogConfig": def})
		asked := 0
		if d := CheckStarted(FixturePolicy(root, nil), ids, in, daemonSays(def, &asked)); d.Refused || asked != 1 {
			t.Errorf("%s, the daemon's default: %+v, asked %d", name, d, asked)
		}
		// Controls: the same container on a daemon whose default is
		// different, one that cannot be asked, and no way to ask at all.
		if d := CheckStarted(FixturePolicy(root, nil), ids, in, daemonSays(plain, new(int))); !refusedAsRunArgs(d) {
			t.Errorf("%s, a daemon defaulting to plain json-file: %+v", name, d)
		}
		failing := func() (*LogConfig, error) { return nil, errors.New("docker create: no such image") }
		if d := CheckStarted(FixturePolicy(root, nil), ids, in, failing); !refusedAsRunArgs(d) ||
			!strings.Contains(strings.Join(d.Why, ";"), "could not be read: docker create: no such image") {
			t.Errorf("%s, the default unreadable: %+v", name, d)
		}
		if d := CheckStarted(FixturePolicy(root, nil), ids, in, nil); !refusedAsRunArgs(d) {
			t.Errorf("%s, nothing to ask: %+v", name, d)
		}
	}
	// What differs from the default only argv could have asked for.
	journald := measuredLogDefaults["journald, a tag"]
	jsonCapped := measuredLogDefaults["json-file, max-size and max-file"]
	for _, tc := range []struct {
		name     string
		def, got LogConfig
	}{
		{"gelf, on a journald daemon", journald, LogConfig{"gelf", map[string]string{"gelf-address": "udp://127.0.0.1:12201"}}},
		{"another journald tag", journald, LogConfig{"journald", map[string]string{"tag": "y"}}},
		{"journald with an option more", journald, LogConfig{"journald", map[string]string{"tag": "x", "labels": "a"}}},
		{"journald with an option fewer", journald, LogConfig{"journald", map[string]string{}}},
		{"syslog, on a json-file daemon", jsonCapped, LogConfig{"syslog", map[string]string{"max-file": "3", "max-size": "10m"}}},
		// --log-opt max-size=1m on a daemon defaulting to 10m (measured).
		{"another max-size", jsonCapped, LogConfig{"json-file", map[string]string{"max-file": "3", "max-size": "1m"}}},
	} {
		in := with(t, c, map[string]any{"LogConfig": tc.got})
		if d := CheckStarted(FixturePolicy(root, nil), ids, in, daemonSays(tc.def, new(int))); !refusedAsRunArgs(d) {
			t.Errorf("%s: %+v, want refused as runArgs", tc.name, d)
		}
		asked := 0
		if d := CheckStarted(FixturePolicy(root, runArgs), ids, in, daemonSays(tc.def, &asked)); d.Refused || asked != 0 {
			t.Errorf("%s, runArgs approved: %+v, the daemon asked %d times", tc.name, d, asked)
		}
	}
	// A file driver with no options needs no daemon to ask, as before.
	asked := 0
	if d := CheckStarted(FixturePolicy(root, nil), ids, with(t, c, map[string]any{"LogConfig": plain}), daemonSays(journald, &asked)); d.Refused || asked != 0 {
		t.Errorf("plain json-file: %+v, asked %d", d, asked)
	}
}

// Through Main, against a fake docker: a start of a container carrying the
// daemon's default asks the daemon with a probe — created from the policy's
// pinned image, with no log option, no network and the probe label, read,
// and removed — and runs; a stray probe is removed first. The controls: a
// container whose log configuration is not the default is refused and
// docker start never runs; an unpinned probe image creates nothing and
// refuses; a file-driver container creates no probe.
func TestMainProbesTheDaemonsLogDefault(t *testing.T) {
	self, _ := os.Executable()
	bin := t.TempDir()
	fake := filepath.Join(bin, "docker")
	probe := strings.Repeat("e", 64)
	os.WriteFile(fake, []byte(`#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/calls"
case "$1" in
inspect) if [ "$5" = `+probe+` ]; then cat "$d/probe.json"; else cat "$d/inspect.json"; fi ;;
ps) if [ -f "$d/stray" ]; then cat "$d/stray"; fi ;;
create) echo `+probe+` ;;
rm) ;;
*) for a; do printf '%s\0' "$a"; done > "$d/ran" ;;
esac
`), 0o755)
	dir := filepath.Join(t.TempDir(), ".drydock", "guard")
	dp, err := (&Guard{Binary: self, Resolver: fixed{"docker": fake}}).Prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(dir))
	image := "busybox:1.37.0@sha256:" + strings.Repeat("b", 64)
	policy := func(img string) {
		p := FixturePolicy(root, nil)
		p.ProbeImage = img
		if err := WritePolicy(dir, *p); err != nil {
			t.Fatal(err)
		}
	}
	c, id := recordedInspect(t, "image", root)
	def := measuredLogDefaults["journald, a tag"]
	b, _ := json.Marshal([]any{map[string]any{"Id": probe, "HostConfig": map[string]any{"LogConfig": def}}})
	os.WriteFile(filepath.Join(dir, "probe.json"), b, 0o600)
	calls := func() []string {
		b, _ := os.ReadFile(filepath.Join(dir, "calls"))
		os.Remove(filepath.Join(dir, "calls"))
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	label := "drydock." + LabelLogProbe + "=" + FixtureLabels["drydock.workspace"]
	wantProbe := []string{
		"ps --all --quiet --no-trunc --filter label=" + label,
		"create --label " + label + " --network none " + image,
		"inspect --type container -- " + probe,
		"rm --force --volumes -- " + probe,
	}

	policy(image)
	os.WriteFile(filepath.Join(dir, "inspect.json"), with(t, c, map[string]any{"LogConfig": def}), 0o600)
	r := runGuard(t, dp, "", "start", id)
	if !r.ranDocker || !reflect.DeepEqual(r.args, []string{"start", id}) {
		t.Errorf("the daemon's default: ran %v with %q, stderr %q", r.ranDocker, r.args, r.stderr)
	}
	if got := calls(); !reflect.DeepEqual(got, append(append([]string{"inspect --type container -- " + id}, wantProbe...), "start "+id)) {
		t.Errorf("docker ran %q", got)
	}

	// A stray probe a killed guard left is removed before the next.
	stray := strings.Repeat("f", 64)
	os.WriteFile(filepath.Join(dir, "stray"), []byte(stray+"\n"), 0o600)
	runGuard(t, dp, "", "start", id)
	if got := calls(); len(got) < 3 || got[2] != "rm --force --volumes -- "+stray || got[3] != wantProbe[1] {
		t.Errorf("with a stray probe, docker ran %q", got)
	}
	os.Remove(filepath.Join(dir, "stray"))

	os.WriteFile(filepath.Join(dir, "inspect.json"), with(t, c, map[string]any{"LogConfig": map[string]any{"Type": "gelf",
		"Config": map[string]string{"gelf-address": "udp://127.0.0.1:12201"}}}), 0o600)
	if r := runGuard(t, dp, "", "start", id); r.ranDocker || r.code != ExitRefused || !strings.Contains(r.stderr, Marker+": runArgs") ||
		!strings.Contains(r.stderr, "not the daemon's default") || strings.Contains(r.stderr, "12201") {
		t.Errorf("gelf: ran %v, exit %d, %q", r.ranDocker, r.code, r.stderr)
	}
	if got := calls(); !reflect.DeepEqual(got[1:], wantProbe) {
		t.Errorf("gelf: docker ran %q", got)
	}

	policy("busybox:1.37.0")
	os.WriteFile(filepath.Join(dir, "inspect.json"), with(t, c, map[string]any{"LogConfig": def}), 0o600)
	if r := runGuard(t, dp, "", "start", id); r.ranDocker || !strings.Contains(r.stderr, "no probe image pinned by digest") {
		t.Errorf("an unpinned probe image: ran %v, %q", r.ranDocker, r.stderr)
	}
	if got := calls(); len(got) != 1 {
		t.Errorf("an unpinned probe image: docker ran %q", got)
	}

	policy(image)
	os.WriteFile(filepath.Join(dir, "inspect.json"), with(t, c, map[string]any{"LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{}}}), 0o600)
	if r := runGuard(t, dp, "", "start", id); !r.ranDocker {
		t.Errorf("plain json-file: %q", r.stderr)
	}
	if got := calls(); len(got) != 2 {
		t.Errorf("plain json-file: docker ran %q", got)
	}
}
