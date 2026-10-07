// Package docs holds checks on the operator documents that the code cannot
// make for itself.
//
// This file: every release a deploy document names exists, or is the release
// release-please will cut next. #49 pinned both documents to v0.3.1 because
// that was the next patch when it was written; release-please folded it into
// a minor release, v0.4.0, and v0.3.1 never existed — so a copied Ansible
// vars.yml failed at its download, and the runbook waited for a release that
// would never come. The rule here is release-please's own, applied to the
// commits it will read.
package docs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const root = "../.."

// The documents an operator copies versions out of.
var documents = []string{
	"README.md",
	"docs/deploy/first-deployment.md",
	"docs/deploy/first-deployment-ansible.md",
}

type version [3]int

func (v version) String() string { return fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2]) }

func parseVersion(s string) (version, error) {
	var v version
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("%q is not X.Y.Z", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, fmt.Errorf("%q is not X.Y.Z", s)
		}
		v[i] = n
	}
	return v, nil
}

var (
	subjectRE  = regexp.MustCompile(`^([a-z]+)(\([^)]*\))?(!)?: \S`)
	breakingRE = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE: `)
)

// nextVersion is the release release-please would cut from the version in
// its manifest and the commit messages merged since, under this repository's
// release-please-config.json ("bump-minor-pre-major": true, and
// "bump-patch-for-minor-pre-major" unset): a breaking change bumps the major
// version, or the minor one while the major is 0; a feat bumps the minor; a
// fix, perf, revert or deps bumps the patch. Nothing else is releasable, and
// with no releasable commit there is no next release (ok is false).
func nextVersion(current version, messages []string) (next version, ok bool) {
	const none, patch, minor, major = 0, 1, 2, 3
	bump := none
	for _, m := range messages {
		subject, _, _ := strings.Cut(m, "\n")
		sm := subjectRE.FindStringSubmatch(subject)
		if sm == nil {
			continue
		}
		b := none
		switch sm[1] {
		case "feat":
			b = minor
		case "fix", "perf", "revert", "deps":
			b = patch
		}
		if sm[3] == "!" || breakingRE.MatchString(m) {
			b = major
		}
		bump = max(bump, b)
	}
	if bump == major && current[0] == 0 {
		bump = minor
	}
	switch bump {
	case major:
		return version{current[0] + 1, 0, 0}, true
	case minor:
		return version{current[0], current[1] + 1, 0}, true
	case patch:
		return version{current[0], current[1], current[2] + 1}, true
	}
	return version{}, false
}

var mentionRE = regexp.MustCompile(`\bv(\d+)\.(\d+)\.(\d+)\b`)

// unknownReleases lists every vX.Y.Z in text that is neither released nor the
// pending release, as "line N: vX.Y.Z".
func unknownReleases(text string, released map[version]bool, pending *version) []string {
	var bad []string
	for i, line := range strings.Split(text, "\n") {
		for _, m := range mentionRE.FindAllString(line, -1) {
			v, err := parseVersion(m)
			if err != nil {
				bad = append(bad, fmt.Sprintf("line %d: %s: %v", i+1, m, err))
				continue
			}
			if released[v] || (pending != nil && v == *pending) {
				continue
			}
			bad = append(bad, fmt.Sprintf("line %d: %s", i+1, m))
		}
	}
	return bad
}

var changelogRE = regexp.MustCompile(`(?m)^## \[?(\d+\.\d+\.\d+)\]?`)

// releasedVersions reads CHANGELOG.md, which release-please writes in the
// release commit: a heading per release, so a version is in it exactly when
// that release was cut.
func releasedVersions(t *testing.T) map[version]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	released := map[version]bool{}
	for _, m := range changelogRE.FindAllStringSubmatch(string(b), -1) {
		v, err := parseVersion(m[1])
		if err != nil {
			t.Fatal(err)
		}
		released[v] = true
	}
	return released
}

func manifestVersion(t *testing.T) version {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".release-please-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	v, err := parseVersion(m["."])
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// unreleasedMessages are the commit messages release-please will read for the
// next release: those since the commit that last moved its manifest (the
// release commit), on main's first-parent line. On a pull request the
// checkout is GitHub's merge commit, whose first parent is main; the PR's own
// commits are not what lands — the squash commit, titled by the PR, is — so
// DRYDOCK_PR_TITLE stands in for them.
func unreleasedMessages(t *testing.T) []string {
	t.Helper()
	require := os.Getenv("DRYDOCK_REQUIRE_RELEASE_HISTORY") != ""
	skip := func(why string) {
		if require {
			t.Fatalf("%s, and DRYDOCK_REQUIRE_RELEASE_HISTORY is set (CI's checkout needs fetch-depth: 0)", why)
		}
		t.Skip(why)
	}
	if shallow, err := git("rev-parse", "--is-shallow-repository"); err != nil {
		skip("not a git checkout")
	} else if shallow == "true" {
		skip("a shallow clone: the commits since the last release are not here")
	}
	release, err := git("log", "-1", "--format=%H", "--", ".release-please-manifest.json")
	if err != nil || release == "" {
		t.Fatalf("no commit moved .release-please-manifest.json: %v", err)
	}
	head := "HEAD"
	title := os.Getenv("DRYDOCK_PR_TITLE")
	if title != "" {
		if parents, _ := git("rev-list", "--parents", "-n", "1", "HEAD"); len(strings.Fields(parents)) == 3 {
			head = "HEAD^1"
		}
	}
	log, err := git("log", "--first-parent", "--format=%B%x00", release+".."+head)
	if err != nil {
		t.Fatalf("git log %s..%s: %v", release, head, err)
	}
	var messages []string
	for _, m := range strings.Split(log, "\x00") {
		if m = strings.TrimSpace(m); m != "" {
			messages = append(messages, m)
		}
	}
	if title != "" {
		messages = append(messages, title)
	}
	return messages
}

// TestDeployDocsNameRealReleases: a document may name a release that exists,
// or the one this change will be released in — never one release-please will
// skip, and never one past it.
func TestDeployDocsNameRealReleases(t *testing.T) {
	released := releasedVersions(t)
	current := manifestVersion(t)
	if !released[current] {
		t.Fatalf("control: the manifest's version %s has no CHANGELOG.md heading; the release list is misread", current)
	}
	if !released[version{0, 4, 0}] || released[version{0, 3, 1}] {
		t.Fatalf("control: CHANGELOG.md is misread: v0.4.0 must be a release, and v0.3.1 must not")
	}
	messages := unreleasedMessages(t)
	pending, ok := nextVersion(current, messages)
	var pendingp *version
	if ok {
		pendingp = &pending
		t.Logf("the next release is %s, from %d unreleased commit(s)", pending, len(messages))
	} else {
		t.Logf("no releasable commit since %s: a document may name only released versions", current)
	}
	mentions := 0
	for _, doc := range documents {
		b, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		mentions += len(mentionRE.FindAllString(string(b), -1))
		for _, bad := range unknownReleases(string(b), released, pendingp) {
			next := "none: no feat: or fix: has merged since " + current.String()
			if ok {
				next = pending.String()
			}
			t.Errorf("%s %s is not a release, and not the next one (%s). Name a published release, or the one this change will ship in; write vX.Y.Z for an example.", doc, bad, next)
		}
	}
	if mentions == 0 {
		t.Error("control: the documents name no version at all; the scan is reading the wrong files")
	}
}

// TestUnknownReleases is the scan's positive control: v0.3.1, the pin #49
// shipped, is caught, beside a real release and the pending one passing.
func TestUnknownReleases(t *testing.T) {
	released := map[version]bool{{0, 3, 0}: true, {0, 4, 0}: true, {0, 4, 1}: true}
	pending := version{0, 4, 2}
	doc := "deploys **v0.4.1**\ndrydock_version: v0.3.1\nthis change ships in v0.4.2\nan example: vX.Y.Z\n"
	got := unknownReleases(doc, released, &pending)
	if len(got) != 1 || got[0] != "line 2: v0.3.1" {
		t.Errorf("unknownReleases = %q; want only line 2's v0.3.1", got)
	}
	if got := unknownReleases(doc, released, nil); len(got) != 2 {
		t.Errorf("with nothing pending, v0.4.2 must be refused too: %q", got)
	}
}

// TestNextVersion pins release-please's rule as this repository configures it.
func TestNextVersion(t *testing.T) {
	v041 := version{0, 4, 1}
	for _, c := range []struct {
		name     string
		current  version
		messages []string
		want     string
	}{
		{"a fix is a patch", v041, []string{"fix: a thing"}, "v0.4.2"},
		{"a feat is a minor, before 1.0 too", v041, []string{"fix: a", "feat(ui): b"}, "v0.5.0"},
		{"breaking is a minor before 1.0", v041, []string{"fix!: a"}, "v0.5.0"},
		{"a BREAKING CHANGE footer counts", v041, []string{"fix: a\n\nBREAKING CHANGE: b"}, "v0.5.0"},
		{"breaking is a major from 1.0", version{1, 2, 3}, []string{"feat!: a"}, "v2.0.0"},
		{"perf, revert and deps are patches", v041, []string{"perf: a", "docs: b"}, "v0.4.2"},
		{"docs, chore, ci, test and refactor release nothing", v041,
			[]string{"docs: a", "chore(main): b", "ci: c", "test: d", "refactor: e", "Merge branch x"}, "none"},
		{"no commits, no release", v041, nil, "none"},
		{"a subject must be conventional", v041, []string{"feat:missing space", "feature: x"}, "none"},
	} {
		got := "none"
		if v, ok := nextVersion(c.current, c.messages); ok {
			got = v.String()
		}
		if got != c.want {
			t.Errorf("%s: nextVersion(%s, %q) = %s; want %s", c.name, c.current, c.messages, got, c.want)
		}
	}
}
