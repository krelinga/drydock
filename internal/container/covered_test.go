package container

import (
	"encoding/json"
	"testing"
)

// TestCovered: less than was approved runs without asking; anything new or
// changed asks (design §6). Each refusal sits beside controls that pass.
func TestCovered(t *testing.T) {
	v := func(s string) json.RawMessage { return json.RawMessage(s) }
	repo := func(f, val string) HostSetting { return HostSetting{Field: f, Source: SourceRepository, Value: v(val)} }
	feat := func(f, val string) HostSetting { return HostSetting{Field: f, Source: SourceFeature, Value: v(val)} }
	approved := []HostSetting{
		feat("privileged", `true`),
		repo("runArgs", `["--network=host","-v","/a:/a"]`),
		repo("capAdd", `["NET_ADMIN","SYS_ADMIN"]`),
		repo("mounts", `["source=/x,target=/x,type=bind",{"source":"/y","target":"/y","type":"bind"}]`),
		repo("initializeCommand", `"true"`),
	}
	for name, cur := range map[string][]HostSetting{
		"nothing":            nil,
		"the same":           approved,
		"one setting fewer":  approved[:2],
		"a capability fewer": {repo("capAdd", `["SYS_ADMIN"]`)},
		"a mount fewer":      {repo("mounts", `[{"source":"/y","target":"/y","type":"bind"}]`)},
		"reordered items":    {repo("capAdd", `["SYS_ADMIN","NET_ADMIN"]`)},
	} {
		if !Covered(approved, cur) {
			t.Errorf("%s: not covered", name)
		}
	}
	for name, cur := range map[string][]HostSetting{
		"a new setting":     {repo("appPort", `[80]`)},
		"a changed value":   {repo("initializeCommand", `"id"`)},
		"another source":    {repo("privileged", `true`)},
		"a capability more": {repo("capAdd", `["NET_ADMIN","SYS_PTRACE2"]`)},
		"a mount more":      {repo("mounts", `["source=/z,target=/z,type=bind"]`)},
		// runArgs is argv: meaning is in the sequence, so only the
		// approved value itself is within it.
		"runArgs shorter":    {repo("runArgs", `["-v","/a:/a"]`)},
		"runArgs recombined": {repo("runArgs", `["--network=host","-v"]`)},
		"runArgs reordered":  {repo("runArgs", `["-v","/a:/a","--network=host"]`)},
	} {
		if Covered(approved, cur) {
			t.Errorf("%s: covered", name)
		}
	}
	if Covered(nil, []HostSetting{repo("initializeCommand", `"true"`)}) || !Covered(nil, nil) {
		t.Error("with nothing approved: only nothing is covered")
	}
}
