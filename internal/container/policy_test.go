package container

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fields renders a subset as "field/source=value" strings, for comparison.
func fields(h HostAccess) []string {
	var out []string
	for _, s := range h.Settings {
		src := "repo"
		if s.Source == SourceFeature {
			src = "feature"
		}
		out = append(out, s.Field+"/"+src+"="+string(s.Value))
	}
	return out
}

// TestHostAccessAgainstTheCorpus: the recorded merged configurations, each
// with the subset its .meta says it must yield. The hostile one is the
// reviewer's attack on PR #25 plus every other way to the host in one file;
// the ok one is a Go repository as the official Feature makes it, whose
// debugger pair must not need an approval, or every Go repository asks.
func TestHostAccessAgainstTheCorpus(t *testing.T) {
	read := func(name string) Configuration {
		t.Helper()
		c, err := ParseConfiguration([]byte(fixture(t, name)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return c
	}
	for _, name := range []string{"read-configuration-merged-ok.json", "read-configuration-merged-override.json"} {
		h, err := HostAccessOf(read(name), "/srv/drydock/ws/FIXTURE/repo", false)
		if err != nil || !h.Empty() || h.Hash != "" {
			t.Errorf("%s: %v %v %q", name, fields(h), err, h.Hash)
		}
	}
	for name, want := range map[string][]string{
		"read-configuration-merged-hostile.json": {
			`appPort/repo=[8080]`,
			`capAdd/repo=["SYS_ADMIN"]`,
			`hostRequirements.gpu/repo=true`,
			`initializeCommand/repo="id -un \u003e /srv/drydock/ws/FIXTURE/canary-initialize"`,
			`mounts/repo=["source=/var/run/docker.sock,target=/var/run/docker.sock,type=bind"]`,
			`privileged/repo=true`,
			`runArgs/repo=["--privileged"]`,
			`securityOpt/repo=["apparmor=unconfined"]`,
			`workspaceMount/repo="source=/,target=/host,type=bind"`,
		},
		// The repository never says privileged; docker-in-docker does. Its
		// volume carries ${devcontainerId}, so it is not in the subset.
		"read-configuration-merged-dind.json":    {`privileged/feature=true`},
		"read-configuration-merged-compose.json": {`dockerComposeFile/repo="compose.yml"`, `service/repo="app"`},
	} {
		h, err := HostAccessOf(read(name), "/srv/drydock/ws/FIXTURE/x", false)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(fields(h), want) || !strings.HasPrefix(h.Hash, "sha256:") {
			t.Errorf("%s:\n got %v\nwant %v (hash %q)", name, fields(h), want, h.Hash)
		}
	}
	// A result read without --include-merged-configuration is not empty.
	if _, err := HostAccessOf(read("read-configuration-ok.json"), "", false); err == nil {
		t.Error("no merged configuration: no error")
	}
}

func cfg(t *testing.T, own string) Configuration {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(own), &m); err != nil {
		t.Fatalf("%s: %v", own, err)
	}
	return Configuration{Own: m, Merged: m}
}

const base = `"image":"mcr.microsoft.com/devcontainers/base:debian"`

// TestEachHostAccessFieldIsInTheSubset: one field at a time onto a config
// with an empty subset, with the safe form of the same field beside it as
// the control — so a subset that held everything, or nothing, fails here by
// name.
func TestEachHostAccessFieldIsInTheSubset(t *testing.T) {
	// The positive control: everything a typical repository uses at once.
	ok := `{` + base + `,"name":"x","features":{"ghcr.io/devcontainers/features/go:1":{}},
		"postCreateCommand":"make deps","postStartCommand":["sh","-c","true"],"onCreateCommand":{"a":"true"},
		"containerEnv":{"A":"1"},"remoteEnv":{"B":"2"},"remoteUser":"vscode","forwardPorts":[3000],
		"customizations":{"vscode":{"extensions":["golang.go"]}},"init":true,"privileged":false,
		"capAdd":["SYS_PTRACE"],"securityOpt":["seccomp=unconfined"],"runArgs":[],"initializeCommand":"",
		"mounts":["source=cache-${devcontainerId},target=/cache,type=volume",{"type":"volume","source":"x-${devcontainerId}","target":"/x"},
			"type=volume,target=/anon","type=tmpfs,target=/tmp/t,tmpfs-size=1m"],
		"hostRequirements":{"cpus":2,"memory":"4gb","gpu":false},"workspaceFolder":"/workspaces/repo",
		"shutdownAction":"none","waitFor":"postCreateCommand","$schema":"x"}`
	if h, err := HostAccessOf(cfg(t, ok), "", false); err != nil || !h.Empty() {
		t.Fatalf("the typical config has host access: %v %v", fields(h), err)
	}

	for _, c := range []struct{ field, value, want string }{
		{"initializeCommand", `"id > /tmp/x"`, `initializeCommand/repo="id \u003e /tmp/x"`},
		{"initializeCommand", `["sh","-c","id"]`, `initializeCommand/repo=["sh","-c","id"]`},
		{"initializeCommand", `{"b":"id","a":"x"}`, `initializeCommand/repo={"a":"x","b":"id"}`},
		{"runArgs", `["--network=host"]`, `runArgs/repo=["--network=host"]`},
		{"runArgs", `["-v","/:/host"]`, `runArgs/repo=["-v","/:/host"]`},
		{"privileged", `true`, `privileged/repo=true`},
		{"capAdd", `["SYS_ADMIN"]`, `capAdd/repo=["SYS_ADMIN"]`},
		// Only what is outside the debugger pair.
		{"capAdd", `["SYS_PTRACE","NET_ADMIN"]`, `capAdd/repo=["NET_ADMIN"]`},
		{"capAdd", `"SYS_PTRACE"`, `capAdd/repo="SYS_PTRACE"`}, // not a list: in the subset, not guessed
		{"securityOpt", `["apparmor=unconfined"]`, `securityOpt/repo=["apparmor=unconfined"]`},
		{"securityOpt", `["seccomp=/etc/x.json","seccomp=unconfined"]`, `securityOpt/repo=["seccomp=/etc/x.json"]`},
		{"mounts", `["source=/var/run/docker.sock,target=/var/run/docker.sock,type=bind"]`, `mounts/repo=["source=/var/run/docker.sock,target=/var/run/docker.sock,type=bind"]`},
		{"mounts", `[{"type":"bind","source":"/","target":"/host"}]`, `mounts/repo=[{"source":"/","target":"/host","type":"bind"}]`},
		// Only the mount that is not the container's alone.
		{"mounts", `["source=c-${devcontainerId},target=/c,type=volume","source=drydock-claude,target=/d,type=volume"]`,
			`mounts/repo=["source=drydock-claude,target=/d,type=volume"]`},
		{"mounts", `["source=x-${devcontainerId},target=/c,type=volume,volume-opt=device=/,volume-opt=o=bind"]`, `mounts/repo=["source=x-${devcontainerId},target=/c,type=volume,volume-opt=device=/,volume-opt=o=bind"]`},
		{"mounts", `["source=x-${devcontainerId},target=/c,type=volume,volume-driver=local"]`, `mounts/repo=["source=x-${devcontainerId},target=/c,type=volume,volume-driver=local"]`},
		{"mounts", `["source=x-${devcontainerId},target=/c"]`, `mounts/repo=["source=x-${devcontainerId},target=/c"]`},
		{"mounts", `["source=/srv/x-${devcontainerId},target=/c,type=volume"]`, `mounts/repo=["source=/srv/x-${devcontainerId},target=/c,type=volume"]`},
		{"mounts", `["type=volume,\"source=a,b\",target=/c"]`, `mounts/repo=["type=volume,\"source=a,b\",target=/c"]`},
		{"mounts", `["type=tmpfs,source=x,target=/c"]`, `mounts/repo=["type=tmpfs,source=x,target=/c"]`},
		{"mounts", `["type=npipe,target=/c"]`, `mounts/repo=["type=npipe,target=/c"]`},
		{"appPort", `[8080]`, `appPort/repo=[8080]`},
		{"workspaceMount", `"source=/,target=/host,type=bind"`, `workspaceMount/repo="source=/,target=/host,type=bind"`},
		{"dockerComposeFile", `"compose.yml"`, `dockerComposeFile/repo="compose.yml"`},
		{"service", `"app"`, `service/repo="app"`},
		{"runServices", `["db"]`, `runServices/repo=["db"]`},
		{"hostRequirements", `{"gpu":true}`, `hostRequirements.gpu/repo=true`},
		{"hostRequirements", `{"gpu":"optional","cpus":2}`, `hostRequirements.gpu/repo="optional"`},
		{"build", `{"dockerfile":"Dockerfile","options":["--network=host"]}`, `build.options/repo=["--network=host"]`},
		{"build", `{"dockerfile":"Dockerfile","secrets":{"a":"b"}}`, `build.secrets/repo={"a":"b"}`},
		{"someFutureField", `{"a":1}`, `someFutureField/repo={"a":1}`},
		{"<script>", `1`, `<script>/repo=1`},
	} {
		in := `{` + base + `,"` + c.field + `":` + c.value + `}`
		h, err := HostAccessOf(cfg(t, in), "", false)
		if got := fields(h); err != nil || len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: %v %v, want %s", in, got, err, c.want)
		}
	}
}

// TestHostAccessFromFeatureMetadata: the merged configuration is what `up`
// applies, so what a Feature or the image adds is in the subset too, as the
// part the configuration's own value lacks, and says where it came from.
func TestHostAccessFromFeatureMetadata(t *testing.T) {
	c := cfg(t, `{`+base+`,"features":{"ghcr.io/example/f:1":{}},"capAdd":["SYS_PTRACE"]}`)
	merged := cfg(t, `{`+base+`,"features":{"ghcr.io/example/f:1":{}},"privileged":false,"capAdd":["SYS_PTRACE"],"init":true,
		"mounts":[{"type":"volume","source":"f-${devcontainerId}","target":"/f"}],"entrypoints":["/usr/local/bin/f"]}`)
	c.Merged = merged.Merged
	if h, err := HostAccessOf(c, "", false); err != nil || !h.Empty() {
		t.Fatalf("a Feature that stays in the container: %v %v", fields(h), err)
	}
	for field, c2 := range map[string]struct{ value, want string }{
		"privileged":  {`true`, `privileged/feature=true`},
		"capAdd":      {`["SYS_PTRACE","SYS_ADMIN"]`, `capAdd/feature=["SYS_ADMIN"]`},
		"securityOpt": {`["apparmor=unconfined"]`, `securityOpt/feature=["apparmor=unconfined"]`},
		"mounts":      {`[{"type":"bind","source":"/etc","target":"/etc2"}]`, `mounts/feature=[{"source":"/etc","target":"/etc2","type":"bind"}]`},
		"newMerged":   {`"x"`, `newMerged/feature="x"`},
	} {
		m := map[string]json.RawMessage{}
		for k, v := range merged.Merged {
			m[k] = v
		}
		m[field] = json.RawMessage(c2.value)
		c.Merged = m
		h, err := HostAccessOf(c, "", false)
		if got := fields(h); err != nil || !reflect.DeepEqual(got, []string{c2.want}) {
			t.Errorf("merged %s=%s: %v %v, want %s", field, c2.value, got, err, c2.want)
		}
	}
	// The repository's own bind mount and a Feature's: one each, by source.
	own := cfg(t, `{`+base+`,"mounts":["source=/a,target=/a,type=bind"]}`)
	own.Merged = cfg(t, `{`+base+`,"mounts":[{"type":"bind","source":"/f","target":"/f"},"source=/a,target=/a,type=bind"]}`).Merged
	h, _ := HostAccessOf(own, "", false)
	if want := []string{`mounts/feature=[{"source":"/f","target":"/f","type":"bind"}]`, `mounts/repo=["source=/a,target=/a,type=bind"]`}; !reflect.DeepEqual(fields(h), want) {
		t.Errorf("both sources: %v", fields(h))
	}
}

// TestHostAccessHash: the hash is of the canonical subset — the same
// configuration in two workspaces of one repository hashes the same, as does
// one with its keys in another order — and moves with any change to a field,
// a value or a source.
func TestHostAccessHash(t *testing.T) {
	hash := func(own, clone string) string {
		t.Helper()
		h, err := HostAccessOf(cfg(t, own), clone, false)
		if err != nil {
			t.Fatal(err)
		}
		return h.Hash
	}
	a := hash(`{`+base+`,"runArgs":["--network=host"],"mounts":["source=/srv/drydock/ws/A/repo/cache,target=/c,type=bind"]}`, "/srv/drydock/ws/A/repo")
	b := hash(`{"mounts":["source=/srv/drydock/ws/B/repo/cache,target=/c,type=bind"],"runArgs":["--network=host"],`+base+`}`, "/srv/drydock/ws/B/repo")
	if a == "" || a != b {
		t.Errorf("one configuration in two workspaces: %q and %q", a, b)
	}
	for _, other := range []string{
		`{` + base + `,"runArgs":["--network=host"]}`,
		`{` + base + `,"runArgs":["--network=bridge"],"mounts":["source=${localWorkspaceFolder}/cache,target=/c,type=bind"]}`,
		`{` + base + `,"runArgs":["--network=host"],"mounts":["source=${localWorkspaceFolder}/cache,target=/d,type=bind"]}`,
		`{` + base + `,"runArgs":["--network=host"],"mounts":["source=${localWorkspaceFolder}/cache,target=/c,type=bind"],"privileged":true}`,
	} {
		if h := hash(other, "/srv/drydock/ws/A/repo"); h == a {
			t.Errorf("%s hashes like the original", other)
		}
	}
	// A setting's source is part of it.
	s := []HostSetting{{Field: "privileged", Source: SourceRepository, Value: json.RawMessage(`true`)}}
	f := []HostSetting{{Field: "privileged", Source: SourceFeature, Value: json.RawMessage(`true`)}}
	if HashSettings(s) == HashSettings(f) || HashSettings(nil) != "" {
		t.Error("the source is not in the hash, or an empty subset hashes")
	}
}

// TestDiffSettings: what the operator is shown is the difference from the
// approved subset, by field and source.
func TestDiffSettings(t *testing.T) {
	v := func(s string) json.RawMessage { return json.RawMessage(s) }
	approved := []HostSetting{
		{Field: "privileged", Source: SourceFeature, Value: v(`true`)},
		{Field: "runArgs", Source: SourceRepository, Value: v(`["--network=host"]`)},
		{Field: "appPort", Source: SourceRepository, Value: v(`[80]`)},
	}
	current := []HostSetting{
		{Field: "privileged", Source: SourceFeature, Value: v(`true`)},
		{Field: "runArgs", Source: SourceRepository, Value: v(`["--network=host","--pid=host"]`)},
		{Field: "initializeCommand", Source: SourceRepository, Value: v(`"id"`)},
	}
	added, changed, removed := DiffSettings(approved, current)
	if len(added) != 1 || added[0].Field != "initializeCommand" ||
		len(changed) != 1 || changed[0].Field != "runArgs" || string(changed[0].From) != `["--network=host"]` ||
		len(removed) != 1 || removed[0].Field != "appPort" {
		t.Errorf("added %v changed %v removed %v", added, changed, removed)
	}
	added, changed, removed = DiffSettings(nil, nil)
	if added == nil || changed == nil || removed == nil {
		t.Error("an empty diff encodes as null")
	}
}

// TestALinkOutOfTheCloneIsNotApprovable: a Dockerfile, build context or bind
// source the configuration names inside the clone that leads, through a link,
// outside it is ErrPathEscapes — the approval would be of the path, which the
// container can point elsewhere, so the docker guard refuses it even
// approved, and step 3 says so first. The controls: the same names without
// the link are inside, and a path outside the clone, written as such, is an
// approvable setting (TestHostAccessPaths).
func TestALinkOutOfTheCloneIsNotApprovable(t *testing.T) {
	top := t.TempDir()
	clone := filepath.Join(top, "repo")
	devc := filepath.Join(clone, ".devcontainer")
	outside := filepath.Join(top, "outside")
	os.MkdirAll(devc, 0o755)
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM x\n"), 0o644)
	file := filepath.Join(devc, "devcontainer.json")
	os.WriteFile(file, []byte("{}"), 0o644)
	cases := []string{
		`{"build":{"dockerfile":"Dockerfile","context":"../escape"}}`,
		`{"build":{"dockerfile":"../escape/Dockerfile"}}`,
		`{` + base + `,"mounts":["source=` + clone + `/escape,target=/x,type=bind"]}`,
		`{` + base + `,"mounts":[{"source":"` + clone + `/escape","target":"/x","type":"bind"}]}`,
		`{` + base + `,"workspaceMount":"source=` + clone + `/escape,target=/w,type=bind"}`,
	}
	os.MkdirAll(filepath.Join(clone, "escape"), 0o755) // control: a directory
	os.WriteFile(filepath.Join(clone, "escape", "Dockerfile"), []byte("FROM x\n"), 0o644)
	for _, own := range cases {
		c := cfg(t, own)
		c.ConfigFile = file
		if _, err := HostAccessOf(c, clone, true); err != nil {
			t.Errorf("control %s: %v", own, err)
		}
	}
	os.RemoveAll(filepath.Join(clone, "escape"))
	os.Symlink(outside, filepath.Join(clone, "escape"))
	for _, own := range cases {
		c := cfg(t, own)
		c.ConfigFile = file
		if _, err := HostAccessOf(c, clone, true); !errors.Is(err, ErrPathEscapes) {
			t.Errorf("%s: %v, want ErrPathEscapes", own, err)
		}
	}
}

// TestHostAccessPaths: the configuration file must be a regular file inside
// the clone, and a build's Dockerfile or context that resolves outside it,
// symbolic links followed, is in the subset — the host's daemon reads it.
func TestHostAccessPaths(t *testing.T) {
	top := t.TempDir()
	clone := filepath.Join(top, "repo")
	devc := filepath.Join(clone, ".devcontainer")
	outside := filepath.Join(top, "outside")
	for _, d := range []string{devc, outside, filepath.Join(clone, "sub")} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(devc, "Dockerfile"), []byte("FROM x\n"), 0o644)
	os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM x\n"), 0o644)
	os.WriteFile(filepath.Join(outside, "devcontainer.json"), []byte("{}"), 0o644)
	os.Symlink(outside, filepath.Join(clone, "escape"))
	file := filepath.Join(devc, "devcontainer.json")
	os.WriteFile(file, []byte("{}"), 0o644)

	check := func(own string, configFile string) (HostAccess, error) {
		c := cfg(t, own)
		c.ConfigFile = configFile
		return HostAccessOf(c, clone, true)
	}
	for _, own := range []string{
		`{` + base + `}`,
		`{"build":{"dockerfile":"Dockerfile"}}`,
		`{"build":{"dockerfile":"Dockerfile","context":".."}}`,
		`{"build":{"dockerfile":"Dockerfile","context":"../sub","args":{"A":"1"},"target":"dev","cacheFrom":"x"}}`,
		`{"dockerFile":"Dockerfile","context":".."}`,
	} {
		if h, err := check(own, file); err != nil || !h.Empty() {
			t.Errorf("%s: %v %v", own, fields(h), err)
		}
	}
	for own, want := range map[string]string{
		`{"build":{"dockerfile":"Dockerfile","context":"../.."}}`: `build.context/repo="../.."`,
		`{"build":{"dockerfile":"Dockerfile","context":"/etc"}}`:  `build.context/repo="/etc"`,
		`{"build":{"dockerfile":"../../outside/Dockerfile"}}`:     `build.dockerfile/repo="../../outside/Dockerfile"`,
		`{"dockerFile":"Dockerfile","context":"../.."}`:           `build.context/repo="../.."`,
		// A cache buildx would import from a host directory, read as buildx
		// reads it; the registry one beside it is the control.
		`{"build":{"dockerfile":"Dockerfile","cacheFrom":["ghcr.io/x/y:c","TYPE=local,src=/x"]}}`:   `build.cacheFrom/repo=["TYPE=local,src=/x"]`,
		`{"build":{"dockerfile":"Dockerfile","cacheFrom":"type=registry,ref=x,type=local,src=/x"}}`: `build.cacheFrom/repo=["type=registry,ref=x,type=local,src=/x"]`,
		`{"build":{"dockerfile":"Dockerfile","context":"../does-not-exist"}}`:                       `build.context/repo="../does-not-exist"`,
	} {
		h, err := check(own, file)
		if err != nil || !reflect.DeepEqual(fields(h), []string{want}) {
			t.Errorf("%s: %v %v, want %s", own, fields(h), err, want)
		}
	}
	// The configuration file itself is not a setting: a symlink, even to a
	// file inside the clone, or a file outside it, is an error.
	link := filepath.Join(clone, ".devcontainer.json")
	os.Symlink(filepath.Join(outside, "devcontainer.json"), link)
	for _, f := range []string{link, filepath.Join(outside, "devcontainer.json"), filepath.Join(devc, "missing.json")} {
		if _, err := check(`{`+base+`}`, f); !errors.Is(err, ErrConfigFileOutside) {
			t.Errorf("config file %s: %v", f, err)
		}
	}
}

func TestFieldNames(t *testing.T) {
	got := FieldNames([]HostSetting{{Field: "privileged"}, {Field: "privileged", Source: SourceFeature},
		{Field: "build.options"}, {Field: "<script>"}, {Field: "a b"}})
	if want := []string{"privileged", "build.options", "a field with an unusual name"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}
