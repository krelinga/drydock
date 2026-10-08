package container

import "bytes"

// tail keeps the last max bytes written to it, starting at a line boundary:
// for a log whose end is what matters. `devcontainer up` streams the whole
// image build and every Feature's install to stderr, and the lines that say
// why it failed — the Feature's preflight (postCreateCommand) and probe
// (postStartCommand) — come last, so a cap that kept the head would keep the
// build's middle and lose them. Nothing reads the head of `up`'s stderr.
type tail struct {
	b   bytes.Buffer
	max int
}

func newTail(max int) *tail { return &tail{max: max} }

func (t *tail) Write(p []byte) (int, error) {
	t.b.Write(p)
	if t.b.Len() > 2*t.max {
		t.trim()
	}
	return len(p), nil // never short: the child must not see a write error
}

func (t *tail) trim() {
	if t.b.Len() <= t.max {
		return
	}
	data := t.b.Bytes()
	keep := data[len(data)-t.max:]
	// Start at a whole line, unless the kept tail is one unbroken line.
	if i := bytes.IndexByte(keep, '\n'); i >= 0 && i+1 < len(keep) {
		keep = keep[i+1:]
	}
	kept := append([]byte(nil), keep...)
	t.b.Reset()
	t.b.Write(kept)
}

// Bytes is the kept tail.
func (t *tail) Bytes() []byte {
	t.trim()
	return t.b.Bytes()
}
