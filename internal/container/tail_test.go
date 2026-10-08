package container

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestUpKeepsTheTailOfAVerboseBuild: an `up` whose stderr runs past the cap
// returns its end — the Feature's last line intact, at a line boundary — not
// its beginning. The control is a short stderr, returned whole.
func TestUpKeepsTheTailOfAVerboseBuild(t *testing.T) {
	const last = "drydock: this container was started without a working broker socket; GitHub access will fail."
	run, _ := fakes(t, map[string]string{
		"devcontainer": "head -c 3000000 /dev/zero | tr '\\0' 'x' | fold -w 100 >&2\necho 'FIRST' >&2; echo '" + last + "' >&2\n" +
			"echo '{\"outcome\":\"error\",\"message\":\"m\",\"description\":\"d\"}'; exit 1",
	})
	_, stderr, err := guarded(t, run, "drydock.test").Up(context.Background(), upSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(stderr) > UpStderrTail || len(stderr) < UpStderrTail-200 {
		t.Errorf("kept %d bytes, want just under %d", len(stderr), UpStderrTail)
	}
	if !bytes.HasSuffix(bytes.TrimRight(stderr, "\n"), []byte(last)) {
		t.Errorf("the last line was lost: ends %q", stderr[len(stderr)-120:])
	}
	if first := string(stderr[:strings.IndexByte(string(stderr), '\n')]); first != strings.Repeat("x", 100) {
		t.Errorf("the kept tail starts mid-line: %q", first[:20])
	}

	tl := newTail(64)
	tl.Write([]byte("short\n"))
	if string(tl.Bytes()) != "short\n" {
		t.Errorf("control: %q", tl.Bytes())
	}
}
