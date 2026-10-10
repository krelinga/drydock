package config

import "testing"

// TestVSCodeSSHHost: the value is spliced into a URL the operator's VS Code
// opens and handed to ssh, so only [user@]host[:port] validates — never a
// scheme, a path, a space, a second '@', an option, or a port out of range.
// The controls are the forms an operator writes, and empty, which is off.
func TestVSCodeSSHHost(t *testing.T) {
	base := Default()
	base.UIOrigin, base.UIHost = "https://drydock.example.com", "drydock.example.com"
	for in, want := range map[string]SSHHost{
		"":                      {},
		"devbox":                {Host: "devbox"},
		"devbox.lan":            {Host: "devbox.lan"},
		"owner@devbox.lan":      {User: "owner", Host: "devbox.lan"},
		"owner@192.168.1.20:22": {User: "owner", Host: "192.168.1.20", Port: 22},
		"My_Alias":              {Host: "My_Alias"},
		"a.b-c@host:65535":      {User: "a.b-c", Host: "host", Port: 65535},
	} {
		c := base
		c.VSCodeSSHHost = in
		if err := c.Validate(); err != nil {
			t.Errorf("control %q: %v", in, err)
			continue
		}
		if in == "" {
			continue
		}
		got, err := ParseSSHHost(in)
		if err != nil || got != want || got.String() != in {
			t.Errorf("%q: %+v (%q), %v", in, got, got.String(), err)
		}
	}
	for _, bad := range []string{
		"ssh://owner@devbox", "owner@devbox/path", "owner@@devbox", "owner@dev@box", "owner devbox",
		"-oProxyCommand=touch", "owner@-devbox", "-owner@devbox", "devbox:0", "devbox:65536", "devbox:022",
		"devbox:", "@devbox", "[::1]", "::1", "devbox.", ".devbox", "dev\nbox", "dev%2fbox", "owner@",
		"o\"wner@devbox", "devbox?x", "devbox#x", "devbox+evil",
	} {
		c := base
		c.VSCodeSSHHost = bad
		if err := c.Validate(); err == nil {
			t.Errorf("%q validated", bad)
		}
	}
}
