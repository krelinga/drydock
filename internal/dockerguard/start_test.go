package dockerguard

import (
	"encoding/json"
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
	if d := CheckStarted(FixturePolicy(root, nil), ids, with(t, c, nil)); d.Refused {
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
		d := CheckStarted(FixturePolicy(root, nil), ids, in)
		if !d.Refused || !reflect.DeepEqual(d.Settings, []string{tc.want}) {
			t.Errorf("%s: %+v, want refused naming %s", tc.name, d, tc.want)
		}
		if tc.approved != nil {
			if d := CheckStarted(FixturePolicy(root, tc.approved), ids, in); d.Refused {
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
		if d := CheckStarted(FixturePolicy(root, nil), ids, with(t, c, hc)); d.Refused {
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
		d := CheckStarted(FixturePolicy(root, nil), tc.ids, tc.in)
		if !d.Refused {
			t.Errorf("%s: passed", name)
		}
	}
	if d := CheckStarted(FixturePolicy(root, nil), []string{id}, good); d.Refused {
		t.Errorf("control: %+v", d)
	}
	if d := CheckStarted(nil, []string{id}, good); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingNoPolicy}) {
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
