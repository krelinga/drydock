// Package claudeimage builds the one image Drydock runs Claude Code in
// itself, outside any workspace: the short-lived container the identity watch
// runs `claude auth status --json` in (design §7.3), and the scratch container
// the login handshake will run `claude auth login` in (§7.2).
//
// It is built locally rather than pulled because no published image carries
// Claude Code at the version Drydock pins, and Drydock already builds images
// on this host — every workspace is a `devcontainer up`. Both halves are
// pinned: the base by digest (configuration, validated like the cleanup
// helper's), Claude Code by exact version (classify.ClaudeCodeVersion, the
// version every recorded fixture and spike measured). The build fails unless
// the installed binary reports exactly that version, so a moved npm dist-tag
// or a registry surprise is a failed build and never a different Claude.
//
// The tag is derived from the Dockerfile's own text, so a changed base or
// version is a different tag and an old image is never mistaken for the new
// one; the image is then run by its ID, which is a content digest.
package claudeimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/krelinga/drydock/internal/subproc"
)

var (
	basePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[0-9a-f]{64}$`)
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	idPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Dockerfile is the whole recipe. Both inputs are validated before they are
// written into it, so neither can add a line.
//
// DISABLE_AUTOUPDATER is set for the reason the Feature sets it (§11): the
// version is the thing being pinned. The `--version` check is exact-line, so
// "2.1.2890" does not pass for "2.1.289".
func Dockerfile(base, version string) (string, error) {
	if !basePattern.MatchString(base) {
		return "", fmt.Errorf("claudeimage: base %q is not pinned by digest", base)
	}
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("claudeimage: %q is not an exact Claude Code version", version)
	}
	return "FROM " + base + "\n" +
		"ENV DISABLE_AUTOUPDATER=1\n" +
		"RUN npm install --global --no-fund --no-audit --no-update-notifier @anthropic-ai/claude-code@" + version + " \\\n" +
		" && claude --version | grep -qx '" + strings.ReplaceAll(version, ".", `\.`) + " (Claude Code)' \\\n" +
		" && npm cache clean --force\n", nil
}

// Builder ensures the image exists and reports its ID.
type Builder struct {
	Run     subproc.Runner
	Base    string
	Version string

	mu sync.Mutex // one build at a time; a second caller waits and finds it
}

// Tag is the local tag the image is built under: the version, and a hash of
// the recipe so a changed base is a changed tag.
func (b *Builder) Tag() (string, error) {
	df, err := Dockerfile(b.Base, b.Version)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(df))
	return "drydock-claude:" + b.Version + "-" + hex.EncodeToString(sum[:6]), nil
}

// Ensure returns the image's ID, building it first if this host does not
// have it. The build needs the network once (npm); after that every use is
// local.
func (b *Builder) Ensure(ctx context.Context) (string, error) {
	df, err := Dockerfile(b.Base, b.Version)
	if err != nil {
		return "", err
	}
	tag, _ := b.Tag()
	b.mu.Lock()
	defer b.mu.Unlock()

	var out, stderr bytes.Buffer
	res := b.Run.Run(ctx, subproc.Cmd{Name: "docker",
		Args:   []string{"image", "inspect", "--format", "{{.Id}}", tag},
		Stdout: &capped{buf: &out, max: 4 << 10}, Stderr: &capped{buf: &stderr, max: 4 << 10}})
	if res.Err != nil {
		return "", fmt.Errorf("claudeimage: docker image inspect: %w", res.Err)
	}
	if id := strings.TrimSpace(out.String()); res.ExitCode == 0 && idPattern.MatchString(id) {
		return id, nil
	}

	// `docker build -` with the recipe on stdin and no context: there is
	// nothing to COPY, so nothing from this host can reach the image.
	out.Reset()
	stderr.Reset()
	res = b.Run.Run(ctx, subproc.Cmd{Name: "docker",
		Args:  BuildArgs(tag),
		Stdin: strings.NewReader(df), Stdout: &capped{buf: &out, max: 4 << 10}, Stderr: &capped{buf: &stderr, max: 64 << 10}})
	if res.Err != nil {
		return "", fmt.Errorf("claudeimage: docker build: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("claudeimage: docker build exited %d: %s", res.ExitCode, lastLine(stderr.String()))
	}
	id := lastLine(out.String())
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("claudeimage: docker build printed no image id")
	}
	return id, nil
}

// BuildArgs is the build's argv, exported so a test can assert it.
func BuildArgs(tag string) []string {
	return []string{"build", "--quiet", "--tag", tag, "-"}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// capped keeps the first max bytes and drops the rest, so a chatty build
// cannot grow Drydock's memory without bound.
type capped struct {
	buf *bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}
