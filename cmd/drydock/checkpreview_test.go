package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestCheckPreviewDomain: the installer's question exits 0 only for a
// cross-site pair, 1 for a same-site one (naming why), and 2 for a malformed
// call, so a typo in the installer cannot read as a pass. The control is a
// genuinely different domain.
func TestCheckPreviewDomain(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--ui-host", "drydock.example.com", "--preview-domain", "drydock-preview.net"}, 0},
		{[]string{"--ui-host", "drydock.example.com", "--preview-domain", "preview.example.com"}, 1},
		{[]string{"--ui-host", "drydock.example.com", "--preview-domain", "example.com"}, 1},
		{[]string{"--ui-host", "drydock.example.com"}, 2},
		{[]string{"--preview-domain", "drydock-preview.net"}, 2},
		{[]string{"--ui-host", "drydock.example.com", "--preview-domain", "drydock-preview.net", "extra"}, 2},
	} {
		var stderr bytes.Buffer
		if got := checkPreviewDomain(tc.args, &stderr); got != tc.want {
			t.Errorf("%q: exit %d, want %d (stderr %q)", tc.args, got, tc.want, stderr.String())
		}
		if tc.want == 1 && !strings.Contains(stderr.String(), "registrable domain") {
			t.Errorf("%q: stderr %q does not say why", tc.args, stderr.String())
		}
		if tc.want == 0 && stderr.Len() != 0 {
			t.Errorf("%q: a pass wrote %q", tc.args, stderr.String())
		}
	}
}
