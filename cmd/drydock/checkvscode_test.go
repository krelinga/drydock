package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestCheckVSCodeSSHHost: the installer's question exits 0 only for a value
// serve accepts, 1 for one it refuses (naming why), and 2 for a malformed
// call, so a typo in the installer cannot read as a pass.
func TestCheckVSCodeSSHHost(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--vscode-ssh-host", "owner@devbox.lan"}, 0},
		{[]string{"--vscode-ssh-host", "devbox"}, 0},
		{[]string{"--vscode-ssh-host", "owner@devbox:2222"}, 0},
		{[]string{"--vscode-ssh-host", "ssh://owner@devbox"}, 1},
		{[]string{"--vscode-ssh-host", "owner@devbox/x"}, 1},
		{[]string{"--vscode-ssh-host", "-oProxyCommand=x"}, 1},
		{[]string{}, 2},
		{[]string{"--vscode-ssh-host", "devbox", "extra"}, 2},
	} {
		var stderr bytes.Buffer
		if got := checkVSCodeSSHHost(tc.args, &stderr); got != tc.want {
			t.Errorf("%q: exit %d, want %d (stderr %q)", tc.args, got, tc.want, stderr.String())
		}
		if tc.want == 1 && !strings.Contains(stderr.String(), "must be") {
			t.Errorf("%q: stderr %q does not say why", tc.args, stderr.String())
		}
		if tc.want == 0 && stderr.Len() != 0 {
			t.Errorf("%q: a pass wrote %q", tc.args, stderr.String())
		}
	}
}
