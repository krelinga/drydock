package container

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/krelinga/drydock/internal/dockerguard"
)

// The host-access subset (design §6, "What a configuration may ask of the
// host"; §13.4).
//
// The clone is bind-mounted read-write into its container, so the
// devcontainer.json a start or rebuild reads is one the container can have
// written. `devcontainer up` acts on it with Drydock's uid on the host: it
// runs initializeCommand there, and passes runArgs, mounts, privileged and
// the rest to `docker run` on the host's daemon. So before every `up`,
// Drydock computes the part of the resolved configuration that reaches
// outside the container — the host-access subset — and runs it only when the
// operator has approved exactly that subset for the repository (provision's
// step 3). An empty subset needs no approval.
//
// The subset is defined by an allowlist. A field is outside it because it is
// known to stay in the container; a field this file does not name is in it —
// so a field a later CLI adds needs an approval until someone has read what
// it does. It is read from two things read-configuration prints: the
// configuration itself, and the merged configuration, which adds what the
// configuration's Features and its image's metadata declare. A Feature can
// ask for privileged, capAdd, securityOpt and mounts just as the repository
// can (measured: docker-in-docker sets privileged), and the CLI applies them.

// Sources of a host setting.
const (
	SourceRepository = "repository"       // the configuration itself
	SourceFeature    = "feature_or_image" // added by a Feature's or the image's metadata
)

// HostSetting is one entry of the host-access subset: a field, where it came
// from, and the host-affecting part of its value, as canonical JSON (object
// keys sorted, no insignificant space, the clone's path written as
// ${localWorkspaceFolder} so the same configuration hashes the same in every
// workspace of the repository).
type HostSetting struct {
	Field  string          `json:"field"`
	Source string          `json:"source"`
	Value  json.RawMessage `json:"value"`
}

// HostAccess is a configuration's host-access subset, sorted by field and
// source, and its hash. Hash is "" when the subset is empty.
type HostAccess struct {
	Settings []HostSetting
	Hash     string
}

// Empty reports whether the configuration asks for nothing outside the
// container.
func (h HostAccess) Empty() bool { return len(h.Settings) == 0 }

// HashSettings is the hash of a subset: "sha256:" and the hex SHA-256 of the
// settings' JSON array, in their canonical order. It is what an approval is
// keyed by, so it covers every byte of every entry — field name included —
// and nothing else: no workspace id, no path but the canonical one.
func HashSettings(s []HostSetting) string {
	if len(s) == 0 {
		return ""
	}
	sorted := append([]HostSetting(nil), s...)
	sortSettings(sorted)
	b, _ := json.Marshal(sorted)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sortSettings(s []HostSetting) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Field != s[j].Field {
			return s[i].Field < s[j].Field
		}
		return s[i].Source < s[j].Source
	})
}

// ErrPathEscapes is a Dockerfile, build context or bind-mount source that the
// configuration names inside the clone but that resolves, through a symbolic
// link, outside it. It is not a setting to approve: an approval is of the
// path as written, the container can move the link, and docker follows it —
// so the docker guard refuses such a path even when approved, and step 3
// refuses it first rather than ask for an approval that could never run.
var ErrPathEscapes = errors.New("container: a path the configuration names inside the clone leads outside it")

// ErrConfigFileOutside is a configuration file that is a symbolic link, not a
// regular file, or not inside the clone. It is not a setting to approve: the
// paths it names would not resolve where they appear to.
var ErrConfigFileOutside = errors.New("container: the dev container configuration file is not a regular file inside the clone")

// free are the configuration's own fields that stay inside the container
// whatever their value, or are read only by an editor.
var free = set(
	"$schema", "name", "image", "features",
	// The legacy spellings of build.dockerfile and build.context, whose
	// paths pathSettings checks.
	"dockerFile", "context",
	"overrideFeatureInstallOrder",
	"forwardPorts", "portsAttributes", "otherPortsAttributes",
	"containerEnv", "remoteEnv", "remoteUser", "containerUser", "updateRemoteUserUID", "userEnvProbe",
	"overrideCommand", "shutdownAction", "init", "customizations", "extensions", "settings", "secrets",
	// Lifecycle commands other than initializeCommand run in the container.
	"onCreateCommand", "updateContentCommand", "postCreateCommand", "postStartCommand", "postAttachCommand",
	"waitFor",
	// A path inside the container; where the clone is mounted is
	// workspaceMount's, which is in the subset.
	"workspaceFolder",
	// Added by the CLI: the file it read.
	"configFilePath",
)

// mergedOnly are the fields the merged configuration adds (CLI 0.89.0's
// merge): the plural lifecycle lists and the entrypoints, all run in the
// container. The rest of what it carries is a field of the configuration,
// read under the same rule.
var mergedOnly = set(
	"entrypoints", "onCreateCommands", "updateContentCommands", "postCreateCommands",
	"postStartCommands", "postAttachCommands",
)

// The debugger pair. The official Go, Rust and C++ Features and images
// declare exactly these, so asking an approval for them would ask it of most
// repositories in those languages, and teach the operator to approve without
// reading. Neither gives the container a path to the host — SYS_PTRACE
// reaches processes in the container's own PID namespace, and lifting the
// seccomp filter widens the kernel surface of a container that is still
// unprivileged — so they are outside the subset, and nothing else in either
// list is.
var (
	capAllowed         = set("SYS_PTRACE", "CAP_SYS_PTRACE")
	securityOptAllowed = set("seccomp=unconfined")
)

// HostAccessOf computes a configuration's host-access subset.
//
// clone is the workspace's clone on the host, written as
// ${localWorkspaceFolder} wherever it appears in a value. checkPaths, for the
// repository's own configuration, also requires the configuration file to be
// a regular file inside the clone (ErrConfigFileOutside otherwise), and puts
// a Dockerfile or build context that resolves outside it, symbolic links
// followed, into the subset: the host's daemon reads them. Drydock's own
// configuration, beside the clone, is read without it.
//
// A result read without the merged configuration is an error.
func HostAccessOf(c Configuration, clone string, checkPaths bool) (HostAccess, error) {
	if c.Own == nil || c.Merged == nil {
		return HostAccess{}, errors.New("container: read-configuration gave no merged configuration to check")
	}
	canon := canonicalizer(clone)
	var out []HostSetting
	own := subset(c.Own, nil, canon)
	for _, s := range own {
		s.Source = SourceRepository
		out = append(out, s)
	}
	if checkPaths {
		ps, err := pathSettings(c, clone, canon)
		if err != nil {
			return HostAccess{}, err
		}
		out = append(out, ps...)
	}
	// What the merged configuration adds beyond the configuration's own.
	ownBy := map[string]HostSetting{}
	for _, s := range own {
		ownBy[s.Field] = s
	}
	for _, m := range subset(c.Merged, mergedOnly, canon) {
		o, had := ownBy[m.Field]
		if !had {
			if _, inOwn := c.Own[topField(m.Field)]; inOwn && !listField[topField(m.Field)] && topField(m.Field) != "privileged" && topField(m.Field) != "hostRequirements" {
				continue // the configuration's own field, carried into the merge
			}
			m.Source = SourceFeature
			out = append(out, m)
			continue
		}
		if extra := listDifference(m.Value, o.Value); extra != nil {
			out = append(out, HostSetting{Field: m.Field, Source: SourceFeature, Value: extra})
		}
	}
	sortSettings(out)
	return HostAccess{Settings: out, Hash: HashSettings(out)}, nil
}

// listField are the fields the merge concatenates: what a Feature adds is
// the merged list less the configuration's own.
var listField = set("capAdd", "securityOpt", "mounts")

func topField(f string) string {
	if i := strings.IndexByte(f, '.'); i > 0 {
		return f[:i]
	}
	return f
}

// subset is one configuration object's host settings, Source unset.
func subset(cfg map[string]json.RawMessage, extra map[string]bool, canon func(json.RawMessage) json.RawMessage) []HostSetting {
	var out []HostSetting
	add := func(field string, v json.RawMessage) {
		out = append(out, HostSetting{Field: field, Value: canon(v)})
	}
	for k, raw := range cfg {
		if free[k] || extra[k] || empty(raw) {
			continue
		}
		switch k {
		case "privileged":
			if !isFalse(raw) {
				add(k, raw)
			}
		case "capAdd":
			if v, ok := notAllowed(raw, capAllowed, strings.ToUpper); ok {
				if v != nil {
					add(k, v)
				}
			} else {
				add(k, raw)
			}
		case "securityOpt":
			if v, ok := notAllowed(raw, securityOptAllowed, nil); ok {
				if v != nil {
					add(k, v)
				}
			} else {
				add(k, raw)
			}
		case "mounts":
			add2 := badMounts(raw)
			if add2 != nil {
				add(k, add2)
			}
		case "hostRequirements":
			var h map[string]json.RawMessage
			switch {
			case json.Unmarshal(raw, &h) != nil:
				add(k, raw)
			case h["gpu"] != nil && !isFalse(h["gpu"]):
				add("hostRequirements.gpu", h["gpu"])
			}
		case "build":
			var b map[string]json.RawMessage
			if json.Unmarshal(raw, &b) != nil {
				add(k, raw)
				continue
			}
			for bk, v := range b {
				switch bk {
				case "cacheFrom":
					// A registry cache names an image; any other — a
					// type=local cache reads a host directory — is in, read
					// as buildx reads it (dockerguard.CacheFromIsRegistry),
					// the parser the docker guard holds up to.
					if bad := nonRegistryCaches(v); bad != nil {
						add("build.cacheFrom", bad)
					}
				case "dockerfile", "context", "target", "args":
				default:
					// options are extra `docker build` flags, which can
					// hand the build host files (--build-context,
					// --secret) or the host network.
					if !empty(v) {
						add("build."+bk, v)
					}
				}
			}
		default:
			// initializeCommand, runArgs, appPort, workspaceMount,
			// dockerComposeFile, service, runServices — and any field not
			// named here.
			add(k, raw)
		}
	}
	return out
}

// nonRegistryCaches is build.cacheFrom's entries that are not registry
// caches, as a JSON list, or nil. The CLI takes a string or a list of them.
func nonRegistryCaches(raw json.RawMessage) json.RawMessage {
	var list []string
	var one string
	switch {
	case json.Unmarshal(raw, &one) == nil:
		list = []string{one}
	case json.Unmarshal(raw, &list) == nil:
	default:
		return raw
	}
	var bad []string
	for _, c := range list {
		if !dockerguard.CacheFromIsRegistry(c) {
			bad = append(bad, c)
		}
	}
	if bad == nil {
		return nil
	}
	b, _ := json.Marshal(bad)
	return b
}

// notAllowed is a string list's entries outside allowed (after norm), as a
// JSON list, or nil when there are none; ok is false when raw is not a list
// of strings at all.
func notAllowed(raw json.RawMessage, allowed map[string]bool, norm func(string) string) (json.RawMessage, bool) {
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return nil, false
	}
	var bad []string
	for _, s := range list {
		n := s
		if norm != nil {
			n = norm(s)
		}
		if !allowed[n] {
			bad = append(bad, s)
		}
	}
	if bad == nil {
		return nil, true
	}
	b, _ := json.Marshal(bad)
	return b, true
}

// badMounts is the mounts that are not this container's alone, as a JSON
// list, or nil. A mount is the container's alone when it is a volume whose
// name carries ${devcontainerId} — which the CLI fills from this workspace's
// own id-labels, so it cannot name another workspace's volume or the shared
// credential volume — an anonymous volume, or a tmpfs. Never a bind, and no
// option that turns a volume into one (volume-opt, volume-driver). Both of the
// CLI's forms: a `docker --mount` string and an object.
func badMounts(raw json.RawMessage) json.RawMessage {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return raw
	}
	var bad []json.RawMessage
	for _, m := range list {
		if !mountEntryOK(m) {
			bad = append(bad, m)
		}
	}
	if bad == nil {
		return nil
	}
	b, _ := json.Marshal(bad)
	return b
}

func mountEntryOK(m json.RawMessage) bool {
	fields := map[string]string{}
	var s string
	var o map[string]json.RawMessage
	switch {
	case json.Unmarshal(m, &s) == nil:
		if strings.ContainsAny(s, "\"'\n") {
			return false // docker reads --mount as CSV; no quoting games
		}
		for _, kv := range strings.Split(s, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				if kv == "readonly" || kv == "ro" {
					continue
				}
				return false
			}
			if _, dup := fields[k]; dup {
				return false
			}
			fields[k] = v
		}
	case json.Unmarshal(m, &o) == nil:
		for k, v := range o {
			var str string
			if json.Unmarshal(v, &str) != nil {
				return false
			}
			fields[k] = str
		}
	default:
		return false
	}
	return mountOK(fields)
}

var volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func mountOK(f map[string]string) bool {
	source, target := "", ""
	for k, v := range f {
		switch k {
		case "type":
		case "source", "src":
			source = v
		case "target", "destination", "dst":
			target = v
		case "readonly", "ro", "consistency":
		case "tmpfs-size", "tmpfs-mode":
			if f["type"] != "tmpfs" {
				return false
			}
		default:
			return false
		}
	}
	if !strings.HasPrefix(target, "/") {
		return false
	}
	switch f["type"] {
	case "tmpfs":
		return source == ""
	case "volume":
		if source == "" {
			return true // anonymous: this container's alone
		}
		return strings.Contains(source, "${devcontainerId}") &&
			volumeName.MatchString(strings.ReplaceAll(source, "${devcontainerId}", "id"))
	}
	return false
}

// pathSettings: the configuration file is a regular file inside the clone,
// or ErrConfigFileOutside; and a Dockerfile or build context — either
// spelling — that resolves outside it after symbolic links is a host
// setting, valued as the configuration wrote it. A context of "../.." would
// send the workspace directory, or more, to the host's daemon.
func pathSettings(c Configuration, root string, canon func(json.RawMessage) json.RawMessage) ([]HostSetting, error) {
	clone, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, ErrConfigFileOutside
	}
	inside := func(p string) bool {
		r, err := filepath.EvalSymlinks(p)
		return err == nil && strings.HasPrefix(r+"/", clone+"/")
	}
	if fi, err := os.Lstat(c.ConfigFile); err != nil || !fi.Mode().IsRegular() || !inside(c.ConfigFile) {
		return nil, ErrConfigFileOutside
	}
	dir := filepath.Dir(c.ConfigFile)
	at := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	str := func(raw json.RawMessage) (string, bool) {
		var s string
		return s, raw != nil && json.Unmarshal(raw, &s) == nil
	}
	var b map[string]json.RawMessage
	json.Unmarshal(c.Own["build"], &b)
	dockerfile, hasDF := str(b["dockerfile"])
	if s, ok := str(c.Own["dockerFile"]); ok {
		dockerfile, hasDF = s, true
	}
	context, hasCtx := str(b["context"])
	if s, ok := str(c.Own["context"]); ok {
		context, hasCtx = s, true
	}
	// Lexically inside, but resolving outside: a link the container made.
	escapes := func(p string) bool {
		c := filepath.Clean(p)
		if _, err := os.Lstat(c); err != nil {
			return false // nothing there to follow; a path that does not exist builds nothing
		}
		return (c == root || strings.HasPrefix(c, filepath.Clean(root)+"/")) && !inside(c)
	}
	if (hasDF && escapes(at(dockerfile))) || (hasCtx && escapes(at(context))) {
		return nil, ErrPathEscapes
	}
	for _, cfg := range []map[string]json.RawMessage{c.Own, c.Merged} {
		for _, src := range bindSources(cfg) {
			if escapes(src) {
				return nil, ErrPathEscapes
			}
		}
	}
	var out []HostSetting
	val := func(s string) json.RawMessage { v, _ := json.Marshal(s); return canon(v) }
	if hasDF && !inside(at(dockerfile)) {
		out = append(out, HostSetting{Field: "build.dockerfile", Source: SourceRepository, Value: val(dockerfile)})
	}
	if hasCtx && !inside(at(context)) {
		out = append(out, HostSetting{Field: "build.context", Source: SourceRepository, Value: val(context)})
	}
	return out, nil
}

// bindSources are the sources of a configuration's bind mounts — mounts and
// workspaceMount, in either of the CLI's forms — as absolute paths.
func bindSources(cfg map[string]json.RawMessage) []string {
	var entries []json.RawMessage
	var list []json.RawMessage
	if json.Unmarshal(cfg["mounts"], &list) == nil {
		entries = append(entries, list...)
	}
	if cfg["workspaceMount"] != nil {
		entries = append(entries, cfg["workspaceMount"])
	}
	var out []string
	for _, e := range entries {
		fields := map[string]string{}
		var str string
		var obj map[string]json.RawMessage
		switch {
		case json.Unmarshal(e, &str) == nil:
			for _, kv := range strings.Split(str, ",") {
				k, v, _ := strings.Cut(kv, "=")
				fields[strings.ToLower(k)] = v
			}
		case json.Unmarshal(e, &obj) == nil:
			for k, v := range obj {
				var s string
				json.Unmarshal(v, &s)
				fields[strings.ToLower(k)] = s
			}
		}
		src := fields["source"]
		if src == "" {
			src = fields["src"]
		}
		if strings.EqualFold(fields["type"], "bind") && filepath.IsAbs(src) {
			out = append(out, src)
		}
	}
	return out
}

// canonicalizer returns the canonical form of a value: decoded and encoded
// again by encoding/json (object keys sorted, numbers as written), with the clone's path
// replaced by ${localWorkspaceFolder} in every string.
func canonicalizer(clone string) func(json.RawMessage) json.RawMessage {
	return func(raw json.RawMessage) json.RawMessage {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			b, _ := json.Marshal(string(raw))
			return b
		}
		// json.Marshal's form, HTML escapes included, because that is the
		// form a stored subset comes back in: encoding/json re-compacts a
		// RawMessage it marshals, so any other spelling would differ from
		// itself after a round trip through the database.
		b, _ := json.Marshal(replaceStrings(v, clone))
		return b
	}
}

func replaceStrings(v any, clone string) any {
	switch x := v.(type) {
	case string:
		if clone != "" {
			return strings.ReplaceAll(x, clone, "${localWorkspaceFolder}")
		}
		return x
	case []any:
		for i := range x {
			x[i] = replaceStrings(x[i], clone)
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = replaceStrings(x[k], clone)
		}
		return x
	}
	return v
}

// listDifference is merged's entries not in own, both JSON lists, as a JSON
// list; or merged itself when either is not a list and they differ; or nil.
func listDifference(merged, own json.RawMessage) json.RawMessage {
	var m, o []json.RawMessage
	if json.Unmarshal(merged, &m) != nil || json.Unmarshal(own, &o) != nil {
		if bytes.Equal(merged, own) {
			return nil
		}
		return merged
	}
	have := map[string]int{}
	for _, e := range o {
		have[string(e)]++
	}
	var extra []json.RawMessage
	for _, e := range m {
		if have[string(e)] > 0 {
			have[string(e)]--
			continue
		}
		extra = append(extra, e)
	}
	if extra == nil {
		return nil
	}
	b, _ := json.Marshal(extra)
	return b
}

func isFalse(raw json.RawMessage) bool {
	var b bool
	return json.Unmarshal(raw, &b) == nil && !b
}

// empty is a value that asks for nothing: null, false, "", [] or {}.
func empty(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", "false", `""`, "[]", "{}":
		return true
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch x := v.(type) {
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// itemLists are the list fields whose elements each stand alone — one
// capability, one security option, one mount — so a list that lost an
// element asks for less, and Covered compares them element by element.
// runArgs is not one: its elements are argv, where meaning is in the
// sequence ("-v", "/a:/b" is one option; "--mount", "/a:/b" another), so a
// shorter or reordered runArgs could recombine approved words into something
// never approved. It must equal the approved value.
var itemLists = set("capAdd", "securityOpt", "mounts", "build.cacheFrom")

// Covered reports whether every entry of current is within approved — the
// rule that lets a run proceed without asking (design §6): less host access
// than was approved never needs sign-off, and anything new does. An entry is
// within approved when approved has an entry of the same field and source
// and either the same canonical value or, for an itemLists field, a list
// holding every element of the current one (compared as canonical JSON). An
// empty current subset is covered by anything.
func Covered(approved, current []HostSetting) bool {
	by := map[string]HostSetting{}
	for _, s := range approved {
		by[s.Field+"\x00"+s.Source] = s
	}
	for _, s := range current {
		a, ok := by[s.Field+"\x00"+s.Source]
		if !ok {
			return false
		}
		if bytes.Equal(a.Value, s.Value) {
			continue
		}
		if !itemLists[s.Field] || !elementsWithin(s.Value, a.Value) {
			return false
		}
	}
	return true
}

// elementsWithin: both are JSON lists, and every element of cur is an
// element of approved, byte for byte in canonical form.
func elementsWithin(cur, approved json.RawMessage) bool {
	var c, a []json.RawMessage
	if json.Unmarshal(cur, &c) != nil || json.Unmarshal(approved, &a) != nil {
		return false
	}
	have := map[string]bool{}
	for _, e := range a {
		have[string(e)] = true
	}
	for _, e := range c {
		if !have[string(e)] {
			return false
		}
	}
	return true
}

// SettingChange is a setting in both the approved subset and the current
// one, with a different value.
type SettingChange struct {
	Field  string          `json:"field"`
	Source string          `json:"source"`
	From   json.RawMessage `json:"from"`
	To     json.RawMessage `json:"to"`
}

// DiffSettings compares the current subset with the one last approved, by
// field and source: what is new, what changed value, and what is gone. Each
// result is non-nil, so it encodes as [] rather than null.
func DiffSettings(approved, current []HostSetting) (added []HostSetting, changed []SettingChange, removed []HostSetting) {
	added, changed, removed = []HostSetting{}, []SettingChange{}, []HostSetting{}
	key := func(s HostSetting) string { return s.Field + "\x00" + s.Source }
	was := map[string]HostSetting{}
	for _, s := range approved {
		was[key(s)] = s
	}
	now := map[string]bool{}
	for _, s := range current {
		now[key(s)] = true
		old, ok := was[key(s)]
		switch {
		case !ok:
			added = append(added, s)
		case !bytes.Equal(old.Value, s.Value):
			changed = append(changed, SettingChange{Field: s.Field, Source: s.Source, From: old.Value, To: s.Value})
		}
	}
	for _, s := range approved {
		if !now[key(s)] {
			removed = append(removed, s)
		}
	}
	return added, changed, removed
}

// FieldNames is the settings' field names for a sentence the operator reads,
// each once, in order. The container chose them, so one that is not a plain
// identifier is not quoted there; the event carries it exactly.
func FieldNames(s []HostSetting) []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range s {
		n := h.Field
		if !plainField.MatchString(n) {
			n = "a field with an unusual name"
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

var plainField = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,39}(\.[A-Za-z][A-Za-z0-9_]{0,39})?$`)

func set(s ...string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}
