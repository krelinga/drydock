// Package dockerguard checks the docker commands the devcontainer CLI runs
// against the host access the operator approved (design §6, "What a
// configuration may ask of the host", and "The docker guard").
//
// Step 3 decides from what `read-configuration` says the configuration asks
// for. That is computed from registries, and `up` computes it again: a
// Feature or an image behind a tag its author controls can serve harmless
// metadata to the check and `privileged` to `up`, and the check never sees
// it (measured on CLI 0.89.0 with a local registry: read-configuration
// fetches the image's metadata without pulling, and up pulls the moved tag
// and runs `docker run --privileged`). So `up` is given `--docker-path`
// pointing at this guard, and every docker command the CLI runs passes
// through Check first. A command that creates a container or builds an image
// is held to the repository's approval in the form docker reads it — argv —
// and refused, before docker is ever run, if it asks for anything the
// approval does not cover. Everything the CLI runs that creates nothing
// (inspect, ps, exec, start, …) is passed through untouched.
//
// It has one effect on the host besides running docker: a build context that
// does not exist yet, written clean and below the run's own TMPDIR under an
// existing directory that resolves inside it, is made there as a directory
// before it is checked, because the CLI starts the Features build without
// waiting for its own mkdir of it (stagedContext). Nothing else is ever made,
// and nothing outside that TMPDIR.
//
// It is an allowlist, like the subset it enforces: a docker option it does
// not know is host-affecting until someone has read what it does, and so is
// a docker command it does not know.
//
// # Rules and details
//
// Check(policy, argv) decides one docker command; Main is the process.
// cmd/drydock (and the test binaries' TestMain) run Main when run as docker
// (IsGuard); it reads policy.json from the directory of the path it was run as
// — absolute, or it runs nothing — and either refuses (one
// `drydock-docker-guard: refused: …` stderr line, refused.json beside it, exit
// 77, docker never run) or execves real-docker with argv untouched but for WithLabels, so stdin,
// stdout, stderr, signals and the exit status are docker's own.
//
// **Fail closed:** with no readable policy every command that creates a
// container or builds an image is refused (no_policy). run/create options are
// parsed against a table of the ones CLI 0.89.0 writes (measured,
// test/fixtures/devcontainer/docker-argv-*.jsonl), and an option it does not
// know is refused as runArgs, since its arity is what is not known; an
// approved runArgs or build.options is removed first as the contiguous run it
// was approved as.
//
// --mount passes as Drydock's own, the clone's bind, a volume named with
// DevcontainerID(idLabels) (the CLI's base-32 SHA-256, tested against the
// recorded docker-in-docker volume) and no other 52-digit id, an anonymous
// volume or a tmpfs, or an approved mounts/workspaceMount element compared
// field by field — an approved bind whose source is in the clone only while it
// still resolves inside it (staysPut: the container could have made it a link
// to /). --privileged, --cap-add (but SYS_PTRACE), --security-opt (but
// seccomp=unconfined), -p (as the CLI renders appPort), --gpus need their
// field approved; -e/--build-arg without a value, a label under the prefix
// that is not an id-label, and one of the policy's Labels with another value
// are refused. Those Labels — the dev container spec's
// devcontainer.local_folder and devcontainer.config_file, which VS Code's
// Reopen in Container finds a folder's container by — are the one thing the
// guard adds to argv (WithLabels, before the check), to every run and create:
// labels, not id-labels, so the CLI's container matching and
// ${devcontainerId} are untouched. A start of a container whose local_folder
// is not the clone, or whose config_file is neither path the CLI looks at in
// it, is refused (startableLabel: the label was fixed at create, and the
// repository may have moved its configuration since); one without them,
// made before, starts.
//
// Builds: -f/context inside the clone or TMPDIR (symlinks followed) or
// approved, --build-context only the CLI's own in its TMPDIR, --cache-from
// only a registry cache or an approved element, **read by CacheFromIsRegistry
// as buildx reads it** (CSV, keys lowercased, last type wins, no = is a
// reference, quotes refused) — never a substring: TYPE=local,src=/x is a local
// import to buildx. Every option whose value the guard reads goes through a
// parser equivalent to docker's or buildx's, or an exact comparison.
//
// compose needs dockerComposeFile approved but for read-only commands; exec
// --privileged needs privileged. **start is not passed**: Main reads the
// containers with docker inspect and CheckStarted holds **every** HostConfig
// field to the policy by hostConfigFields, an allowlist of Docker 29's fields
// (an unknown one, set, is refused): precise for privileged, capabilities,
// security options, MaskedPaths/ReadonlyPaths (where systempaths=unconfined
// shows), mounts with bind propagation, and GPUs; namespaces,
// DeviceCgroupRules, CgroupParent, Sysctls, a log configuration (the daemon
// runs the driver on the host: gelf reached the host's loopback), and the
// rest only runArgs sets, and published ports, need that field approved at
// all. A log configuration passes without runArgs when it is a file driver
// with no options or **equals the daemon's own default** — which Docker
// writes into every container it creates with no log option, so a host whose
// daemon.json sets journald or a max-size had every second start refused.
// The default is read from the daemon, not daemon.json (DaemonLogConfig,
// logprobe.go): a container created from the policy's pinned ProbeImage with
// no log option and --network none, labelled <prefix>.log-probe, never
// started, inspected and removed — asked only when a start needs it.
// Anything but one result per full
// 64-hex id, each with a HostConfig, is start_unread. Measured inspect output
// is the fixtures docker-inspect-{image,dind,hostile}.json; a global option or
// an unknown docker command is refused.
//
// Labels off the prefix pass, deliberately: a denylist of host services'
// prefixes would read as a guarantee (design §6). Source-blind: docker's argv
// cannot say whether a --privileged was the repository's or a Feature's.
package dockerguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Setting is one approved host setting: a devcontainer.json field and its
// canonical value, as container.HostSetting carries it — the clone's path
// written ${localWorkspaceFolder}, and ${devcontainerId} as written. The
// guard is source-blind: docker's argv does not say whether --privileged came
// from the repository or a Feature, so an approval of either covers it.
type Setting struct {
	Field string          `json:"field"`
	Value json.RawMessage `json:"value"`
}

// Policy is what one `up` may ask docker for: Drydock's own part of the
// argv, and the repository's approval. Drydock writes it beside the guard
// for the length of the run (WritePolicy).
type Policy struct {
	Version int `json:"version"`
	// Clone is the workspace's clone on the host — the --workspace-folder
	// given to the CLI, whose bind mount is Drydock's own, and inside which
	// a build context may lie.
	Clone string `json:"clone"`
	// TempDir is the TMPDIR the CLI runs with, the workspace's own: where it
	// writes the Dockerfiles and the Feature build context it builds with.
	TempDir string `json:"temp_dir"`
	// ConfigDir is the directory of the configuration file, which a relative
	// build context is resolved against.
	ConfigDir string `json:"config_dir"`
	// LabelPrefix is config.LabelPrefix. A label under it is Drydock's, and
	// only the id-labels Drydock passed may carry it: reconciliation adopts
	// and deletes by label.
	LabelPrefix string            `json:"label_prefix"`
	IDLabels    map[string]string `json:"id_labels"`
	// Labels are labels the guard itself adds to every container this up
	// creates (WithLabels) and that are not id-labels: the dev container
	// spec's devcontainer.local_folder and devcontainer.config_file, which
	// VS Code finds a folder's container by. Not id-labels, so the CLI's
	// container matching and ${devcontainerId} are what they were. A run
	// carrying one with another value, or a start of a container that does,
	// is refused.
	Labels map[string]string `json:"labels,omitempty"`
	// OwnMounts are the --mount values Drydock passes to up: the broker
	// directory and the credential volume.
	OwnMounts []string `json:"own_mounts"`
	// Approved is the repository's current approval (§6), empty when none
	// has been given.
	Approved []Setting `json:"approved"`
	// ProbeImage is config.CleanupImage, pinned by digest: the image of the
	// container the guard creates, never starts, and removes to learn the
	// daemon's default log configuration (DaemonLogConfig). Empty or not
	// pinned, there is no probe: a start whose log configuration needs one
	// is refused, naming "no probe image pinned by digest in the policy".
	ProbeImage string `json:"probe_image,omitempty"`
}

// PolicyVersion is the Policy shape this guard reads. Any other is refused
// as unreadable.
const PolicyVersion = 1

// Settings the guard names when it refuses. Each is a devcontainer.json
// field, so the operator's sentence names what to look for, apart from the
// three that name no field.
const (
	SettingPrivileged  = "privileged"
	SettingCapAdd      = "capAdd"
	SettingSecurityOpt = "securityOpt"
	SettingMounts      = "mounts"
	SettingAppPort     = "appPort"
	SettingGPU         = "hostRequirements.gpu"
	SettingRunArgs     = "runArgs"
	SettingBuildOpts   = "build.options"
	SettingBuildCtx    = "build.context"
	SettingDockerfile  = "build.dockerfile"
	SettingCacheFrom   = "build.cacheFrom"
	SettingCompose     = "dockerComposeFile"
	// SettingCommand is a docker command, or a docker option before the
	// command, that the guard does not know.
	SettingCommand = "docker_command"
	// SettingExecOption is a `docker exec` option the guard does not know.
	SettingExecOption = "exec_option"
	// SettingNoPolicy is a command that creates a container or builds an
	// image, run when the guard has no policy to hold it to: fail closed.
	SettingNoPolicy = "no_policy"
)

// Decision is Check's verdict on one docker command.
type Decision struct {
	Refused bool
	// Command is which kind of command was checked: run, build, exec,
	// compose, pass (passed through unchecked) or unknown. A closed set.
	Command string
	// Settings are what was refused, from the constants above, sorted.
	Settings []string
	// Why says which options, for the service log. It quotes argv, which a
	// configuration chose, so it never reaches the event log.
	Why []string
	// Start is the containers a `docker start` names: passed only once
	// their settings, read with docker inspect, are held to the policy
	// (CheckStarted). Main does that read.
	Start []string
}

type checker struct {
	p        *Policy
	settings map[string]bool
	why      []string
	// daemonLog is the daemon's default log configuration, for a start
	// (CheckStarted).
	daemonLog func() (*LogConfig, error)
}

func (c *checker) refuse(setting, why string) {
	if c.settings == nil {
		c.settings = map[string]bool{}
	}
	c.settings[setting] = true
	c.why = append(c.why, why)
}

func (c *checker) decision(cmd string) Decision {
	d := Decision{Command: cmd, Why: c.why}
	for s := range c.settings {
		d.Settings = append(d.Settings, s)
	}
	sort.Strings(d.Settings)
	d.Refused = len(d.Settings) > 0
	return d
}

// passed are the docker commands the CLI runs that create nothing and give
// nothing to a container: measured on CLI 0.89.0 across up, read-configuration
// and exec, plus their read-only neighbours. A command not here and not
// checked below is refused.
var passed = set("version", "info", "ps", "inspect", "events", "stop", "rm",
	"kill", "wait", "logs", "pull", "top", "port", "images")

// Check decides one docker command. A nil policy refuses any command that
// creates a container or builds an image, and passes the rest.
func Check(p *Policy, args []string) Decision {
	c := &checker{p: p}
	if len(args) == 0 {
		return c.decision("pass") // `docker` alone prints its usage
	}
	if strings.HasPrefix(args[0], "-") {
		// Only the version probe the CLI runs (`docker -v`) and help: any
		// other global option (-H, --context, --config, -l) could point the
		// command somewhere the rest of this check does not describe.
		if len(args) == 1 && (args[0] == "-v" || args[0] == "--version" || args[0] == "-h" || args[0] == "--help") {
			return c.decision("pass")
		}
		c.refuse(SettingCommand, "a docker option before the command: "+args[0])
		return c.decision("unknown")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "run", "create":
		return c.run(rest)
	case "build":
		return c.build(rest)
	case "exec":
		return c.exec(rest)
	case "start":
		return c.start(rest)
	case "compose":
		return c.compose(rest)
	case "container":
		if len(rest) == 0 {
			return c.decision("pass")
		}
		switch rest[0] {
		case "run", "create":
			return c.run(rest[1:])
		case "exec":
			return c.exec(rest[1:])
		case "start":
			return c.start(rest[1:])
		case "inspect", "ls", "stop", "rm", "kill", "wait", "logs", "top", "port":
			return c.decision("pass")
		}
	case "image":
		if len(rest) == 0 {
			return c.decision("pass")
		}
		switch rest[0] {
		case "build":
			return c.build(rest[1:])
		case "inspect", "ls", "pull", "history":
			return c.decision("pass")
		}
	case "builder":
		if len(rest) > 0 && rest[0] == "build" {
			return c.build(rest[1:])
		}
	case "buildx":
		if len(rest) == 0 {
			return c.decision("pass")
		}
		switch rest[0] {
		case "build", "b":
			return c.build(rest[1:])
		case "version", "ls":
			return c.decision("pass")
		}
	default:
		if passed[sub] {
			return c.decision("pass")
		}
	}
	c.refuse(SettingCommand, "a docker command the guard does not know: "+strings.Join(args[:min(2, len(args))], " "))
	return c.decision("unknown")
}

// flagKind is what a docker option means here.
type flagKind int

const (
	kFree       flagKind = iota // in the container, or the CLI's own plumbing
	kEnv                        // -e/--build-arg: must carry its value, or docker copies the guard's environment
	kLabel                      // a container label
	kMount                      // --mount
	kPublish                    // -p
	kPrivileged                 // --privileged
	kCapAdd                     // --cap-add
	kSecOpt                     // --security-opt
	kGPUs                       // --gpus
	kFile                       // build -f
	kContext                    // build --build-context
	kCacheFrom                  // build --cache-from
)

type flagSpec struct {
	bool bool
	kind flagKind
}

// runFlags are the `docker run` options CLI 0.89.0 emits (measured: the
// recorded fixtures under test/fixtures/devcontainer/docker-argv-*), from
// Drydock's own flags and every devcontainer.json field the subset names.
// Anything else — --network, --pid, -v, --device, … — reaches argv only
// through runArgs, which is approved as a whole and removed before this
// parse; one that is left is refused.
var runFlags = map[string]flagSpec{
	"--sig-proxy": {bool: true}, "-d": {bool: true}, "--detach": {bool: true},
	"-i": {bool: true}, "--interactive": {bool: true}, "-t": {bool: true}, "--tty": {bool: true},
	"--rm": {bool: true}, "--init": {bool: true},
	"-a": {}, "--attach": {}, "-u": {}, "--user": {}, "-w": {}, "--workdir": {}, "--entrypoint": {},
	"-e": {kind: kEnv}, "--env": {kind: kEnv},
	"-l": {kind: kLabel}, "--label": {kind: kLabel},
	"--mount":        {kind: kMount},
	"-p":             {kind: kPublish},
	"--publish":      {kind: kPublish},
	"--privileged":   {bool: true, kind: kPrivileged},
	"--cap-add":      {kind: kCapAdd},
	"--security-opt": {kind: kSecOpt},
	"--gpus":         {kind: kGPUs},
}

// buildFlags are the `docker build` / `docker buildx build` options the CLI
// emits (measured), and their read-only neighbours.
var buildFlags = map[string]flagSpec{
	"--load": {bool: true}, "--no-cache": {bool: true}, "--pull": {bool: true},
	"-q": {bool: true}, "--quiet": {bool: true},
	"-t": {}, "--tag": {}, "--target": {}, "--platform": {}, "--progress": {}, "--label": {},
	"--build-arg":     {kind: kEnv},
	"-f":              {kind: kFile},
	"--file":          {kind: kFile},
	"--build-context": {kind: kContext},
	"--cache-from":    {kind: kCacheFrom},
}

// execFlags are the `docker exec` options the CLI emits (measured).
var execFlags = map[string]flagSpec{
	"-i": {bool: true}, "--interactive": {bool: true}, "-t": {bool: true}, "--tty": {bool: true},
	"-d": {bool: true}, "--detach": {bool: true},
	"-u": {}, "--user": {}, "-w": {}, "--workdir": {}, "--detach-keys": {},
	"-e": {kind: kEnv}, "--env": {kind: kEnv},
	"--privileged": {bool: true, kind: kPrivileged},
}

// parsed is one option: its name, and its value (for a boolean, "true" or
// "false").
type parsed struct {
	name, value string
	spec        flagSpec
}

// parseOptions reads options up to the first positional. It stops at an
// option it does not know — whether that one takes a value is exactly what is
// not known, so nothing after it can be read — and returns its name.
func parseOptions(args []string, table map[string]flagSpec) (opts []parsed, positional []string, unknown string) {
	opts, positional, unknown, _ = parseFrom(args, table)
	return opts, positional, unknown
}

// parseFrom is parseOptions, also returning what follows an unknown option.
func parseFrom(args []string, table map[string]flagSpec) (opts []parsed, positional []string, unknown string, after []string) {
	i := 0
	for ; i < len(args); i++ {
		x := args[i]
		if x == "-" || !strings.HasPrefix(x, "-") {
			break
		}
		name, val, hasVal := x, "", false
		switch {
		case strings.HasPrefix(x, "--"):
			if k, v, ok := strings.Cut(x, "="); ok {
				name, val, hasVal = k, v, true
			}
		case len(x) > 2:
			// -pVALUE or a cluster such as -it: neither is what the CLI
			// writes, and reading either wrongly is reading the command
			// wrongly.
			return opts, nil, x, args[i+1:]
		}
		spec, ok := table[name]
		if !ok {
			return opts, nil, name, args[i+1:]
		}
		if spec.bool {
			switch {
			case !hasVal:
				val = "true"
			case val != "true" && val != "false":
				return opts, nil, x, args[i+1:]
			}
		} else if !hasVal {
			if i+1 >= len(args) {
				return opts, nil, x, args[i+1:]
			}
			i++
			val = args[i]
		}
		opts = append(opts, parsed{name: name, value: val, spec: spec})
	}
	return opts, args[i:], "", nil
}

// run checks `docker run` and `docker create`.
func (c *checker) run(args []string) Decision {
	if c.p == nil {
		c.refuse(SettingNoPolicy, "no policy for a command that creates a container")
		return c.decision("run")
	}
	// runArgs is approved as a whole: the CLI writes it into argv verbatim
	// and in order (measured), so an approved value is removed as one
	// contiguous run of arguments and what remains is read option by option.
	for _, ra := range c.p.stringLists("runArgs") {
		args = removeRun(args, ra)
	}
	for len(args) > 0 {
		opts, _, unknown, after := parseFrom(args, runFlags)
		for _, o := range opts {
			c.option(o)
		}
		if unknown == "" {
			break
		}
		// Refused already. The rest is read on, from the next option, only
		// so the refusal names everything it can: which arguments belonged
		// to the unknown option cannot be known.
		c.refuse(SettingRunArgs, "an option the guard does not know: "+unknown)
		for len(after) > 0 && !strings.HasPrefix(after[0], "-") {
			after = after[1:]
		}
		args = after
	}
	return c.decision("run")
}

// option checks one parsed run or exec option.
func (c *checker) option(o parsed) {
	switch o.spec.kind {
	case kEnv:
		// "-e NAME" copies NAME from docker's own environment — the
		// guard's, which is Drydock's — into the container.
		if !strings.Contains(o.value, "=") {
			c.refuse(SettingRunArgs, o.name+" without a value copies Drydock's environment")
		}
	case kLabel:
		k, v, _ := strings.Cut(o.value, "=")
		if c.p.LabelPrefix != "" && strings.HasPrefix(k, c.p.LabelPrefix+".") {
			if want, ok := c.p.IDLabels[k]; !ok || want != v {
				c.refuse(SettingRunArgs, "a label under Drydock's prefix that is not one of its id-labels: "+k)
			}
		} else if want, ok := c.p.Labels[k]; ok && want != v {
			// One of the guard's own labels — the spec's
			// devcontainer.local_folder and config_file — with another value:
			// what VS Code would find another folder's container by, and
			// docker keeps the last -l.
			c.refuse(SettingRunArgs, "a label the guard sets, with a value other than Drydock's: "+k)
		}
	case kMount:
		if !c.mountAllowed(o.value) {
			c.refuse(SettingMounts, "--mount "+o.value)
		}
	case kPublish:
		if !c.p.published()[o.value] {
			c.refuse(SettingAppPort, "--publish "+o.value)
		}
	case kPrivileged:
		if o.value == "true" && !c.p.has("privileged") {
			c.refuse(SettingPrivileged, o.name)
		}
	case kCapAdd:
		n := normCap(o.value)
		if n != "SYS_PTRACE" && !c.p.caps()[n] {
			c.refuse(SettingCapAdd, "--cap-add "+o.value)
		}
	case kSecOpt:
		if o.value != "seccomp=unconfined" && !c.p.listed("securityOpt")[o.value] {
			c.refuse(SettingSecurityOpt, "--security-opt "+o.value)
		}
	case kGPUs:
		if !c.p.has("hostRequirements.gpu") {
			c.refuse(SettingGPU, "--gpus "+o.value)
		}
	}
}

// build checks `docker build` and `docker buildx build`.
func (c *checker) build(args []string) Decision {
	if c.p == nil {
		c.refuse(SettingNoPolicy, "no policy for a command that builds an image")
		return c.decision("build")
	}
	for _, bo := range c.p.stringLists("build.options") {
		args = removeRun(args, bo)
	}
	opts, positional, unknown := parseOptionsAll(args, buildFlags)
	for _, o := range opts {
		switch o.spec.kind {
		case kEnv:
			if !strings.Contains(o.value, "=") {
				c.refuse(SettingBuildOpts, o.name+" without a value copies Drydock's environment")
			}
		case kFile:
			if !c.p.insideOwn(o.value) && !c.p.approvedPath("build.dockerfile", o.value) {
				c.refuse(SettingDockerfile, "--file "+o.value)
			}
		case kContext:
			// The CLI's one named context: the Features it staged in its
			// TMPDIR. Any other hands the daemon a host directory.
			name, path, _ := strings.Cut(o.value, "=")
			if name != "dev_containers_feature_content_source" || !inside(path, c.p.TempDir) {
				c.refuse(SettingBuildOpts, "--build-context "+o.value)
			}
		case kCacheFrom:
			// A registry reference names an image; type=local reads a
			// directory on the host. Read as buildx reads it, or the
			// guard and buildx disagree about which it is.
			if !CacheFromIsRegistry(o.value) && !c.p.listed("build.cacheFrom")[o.value] {
				c.refuse(SettingCacheFrom, "--cache-from "+o.value)
			}
		}
	}
	if unknown != "" {
		c.refuse(SettingBuildOpts, "an option the guard does not know: "+unknown)
		return c.decision("build")
	}
	switch {
	case len(positional) != 1:
		c.refuse(SettingBuildCtx, "not exactly one build context")
	case !inside(positional[0], c.p.Clone) && !c.p.stagedContext(positional[0]) &&
		!c.p.approvedPath("build.context", positional[0]):
		c.refuse(SettingBuildCtx, "a build context outside the clone: "+positional[0])
	}
	return c.decision("build")
}

// parseOptionsAll is parseOptions for a command whose positional may come
// before its options, as `docker build`'s context can.
func parseOptionsAll(args []string, table map[string]flagSpec) (opts []parsed, positional []string, unknown string) {
	for len(args) > 0 {
		o, pos, u := parseOptions(args, table)
		opts = append(opts, o...)
		if u != "" {
			return opts, positional, u
		}
		if len(pos) == 0 {
			break
		}
		if pos[0] == "--" {
			return opts, positional, "--"
		}
		positional = append(positional, pos[0])
		args = pos[1:]
	}
	return opts, positional, ""
}

// exec checks `docker exec`. It creates nothing, but --privileged gives the
// process every capability in a container that has none.
func (c *checker) exec(args []string) Decision {
	opts, _, unknown := parseOptions(args, execFlags)
	for _, o := range opts {
		switch o.spec.kind {
		case kEnv:
			if !strings.Contains(o.value, "=") {
				c.refuse(SettingExecOption, o.name+" without a value copies Drydock's environment")
			}
		case kPrivileged:
			if o.value == "true" && (c.p == nil || !c.p.has("privileged")) {
				c.refuse(SettingPrivileged, "exec "+o.name)
			}
		}
	}
	if unknown != "" {
		c.refuse(SettingExecOption, "an exec option the guard does not know: "+unknown)
	}
	return c.decision("exec")
}

// startFlags are `docker start`'s options the guard knows.
var startFlags = map[string]flagSpec{
	"-a": {bool: true}, "--attach": {bool: true}, "-i": {bool: true}, "--interactive": {bool: true},
}

// start checks `docker start`. It creates nothing, but the container it
// starts keeps whatever host access it was created with — perhaps under a
// wider approval since narrowed, or by an up this guard never saw — so its
// settings are held to the current policy too (CheckStarted).
func (c *checker) start(args []string) Decision {
	if c.p == nil {
		c.refuse(SettingNoPolicy, "no policy for a command that starts a container")
		return c.decision("start")
	}
	_, ids, unknown := parseOptions(args, startFlags)
	if unknown != "" || len(ids) == 0 {
		c.refuse(SettingCommand, "a docker start the guard does not read: "+unknown)
		return c.decision("start")
	}
	d := c.decision("start")
	d.Start = ids
	return d
}

// composeGlobal are `docker compose`'s options before its command, as the
// CLI writes them (measured), with whether each takes a value.
var composeGlobal = map[string]bool{
	"-f": true, "--file": true, "-p": true, "--project-name": true, "--profile": true,
	"--project-directory": true,
}

// composeReadOnly are the compose commands that create nothing.
var composeReadOnly = set("version", "config", "ps", "ls", "images", "top", "port")

// compose checks `docker compose`. Drydock reads no Compose file (§6), so a
// Compose setup is approved as a whole, and without that approval only the
// commands that create nothing run.
func (c *checker) compose(args []string) Decision {
	if c.p != nil && c.p.has("dockerComposeFile") {
		return c.decision("compose")
	}
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		name, _, inline := strings.Cut(args[i], "=")
		takes, ok := composeGlobal[name]
		if !ok {
			c.refuse(SettingCompose, "a compose option the guard does not know: "+name)
			return c.decision("compose")
		}
		i++
		if takes && !inline {
			i++
		}
	}
	if i < len(args) && composeReadOnly[args[i]] {
		return c.decision("compose")
	}
	c.refuse(SettingCompose, "docker compose without an approved Compose setup")
	return c.decision("compose")
}

// CacheFromIsRegistry reports whether a --cache-from value is a registry
// cache, read as buildx (v0.33) reads it: a value with no "=" is a registry
// reference; any other is one CSV record of key=value fields, each key
// lowercased, the last "type" winning, and no type at all an error. Only
// type=registry names nothing on the host. A substring test is not this:
// buildx reads TYPE=local,src=/x, and type=registry,ref=x,type=local,src=/x,
// as local cache imports from /x (measured in the review of #78). A value
// carrying a quote or a line break is refused rather than parsed: quoting is
// where two CSV readers differ.
func CacheFromIsRegistry(v string) bool {
	if v == "" {
		return true // buildx skips it
	}
	if strings.ContainsAny(v, "\"\r\n") {
		return false
	}
	if !strings.Contains(v, "=") {
		return true
	}
	r := csv.NewReader(strings.NewReader(v))
	fields, err := r.Read()
	if err != nil {
		return false
	}
	typ := ""
	for _, f := range fields {
		k, val, ok := strings.Cut(f, "=")
		if !ok {
			return false // buildx: invalid value
		}
		if strings.ToLower(k) == "type" {
			typ = val
		}
	}
	return typ == "registry"
}

// removeRun removes the first occurrence of seq, as a contiguous run, from
// args.
func removeRun(args, seq []string) []string {
	if len(seq) == 0 || len(seq) > len(args) {
		return args
	}
	for i := 0; i+len(seq) <= len(args); i++ {
		match := true
		for j := range seq {
			if args[i+j] != seq[j] {
				match = false
				break
			}
		}
		if match {
			out := append([]string(nil), args[:i]...)
			return append(out, args[i+len(seq):]...)
		}
	}
	return args
}

func normCap(s string) string {
	return strings.TrimPrefix(strings.ToUpper(s), "CAP_")
}

// values is every approved value of a field, with the placeholders the
// canonical form keeps put back: ${localWorkspaceFolder} as the clone and
// ${devcontainerId} as this run's id.
func (p *Policy) values(field string) []json.RawMessage {
	var out []json.RawMessage
	for _, s := range p.Approved {
		if s.Field == field {
			out = append(out, s.Value)
		}
	}
	return out
}

func (p *Policy) substitute(s string) string {
	s = strings.ReplaceAll(s, "${localWorkspaceFolder}", p.Clone)
	return strings.ReplaceAll(s, "${devcontainerId}", DevcontainerID(p.IDLabels))
}

func (p *Policy) has(field string) bool {
	for _, v := range p.values(field) {
		if !bytes.Equal(bytes.TrimSpace(v), []byte("false")) {
			return true
		}
	}
	return false
}

// stringLists is each approved value of a list-of-strings field, substituted.
func (p *Policy) stringLists(field string) [][]string {
	var out [][]string
	for _, v := range p.values(field) {
		var l []string
		if json.Unmarshal(v, &l) != nil {
			continue
		}
		for i := range l {
			l[i] = p.substitute(l[i])
		}
		out = append(out, l)
	}
	return out
}

// listed is the union of a list field's approved elements.
func (p *Policy) listed(field string) map[string]bool {
	m := map[string]bool{}
	for _, l := range p.stringLists(field) {
		for _, s := range l {
			m[s] = true
		}
	}
	return m
}

func (p *Policy) caps() map[string]bool {
	m := map[string]bool{}
	for s := range p.listed("capAdd") {
		m[normCap(s)] = true
	}
	return m
}

// published is the -p values an approved appPort becomes, as CLI 0.89.0
// writes them: a number n as 127.0.0.1:n:n, a string as itself.
func (p *Policy) published() map[string]bool {
	m := map[string]bool{}
	add := func(raw json.RawMessage) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			return
		}
		switch x := v.(type) {
		case json.Number:
			m["127.0.0.1:"+x.String()+":"+x.String()] = true
		case string:
			m[x] = true
		}
	}
	for _, v := range p.values("appPort") {
		var list []json.RawMessage
		if json.Unmarshal(v, &list) == nil {
			for _, e := range list {
				add(e)
			}
			continue
		}
		add(v)
	}
	return m
}

// approvedPath reports whether path is an approved build.dockerfile or
// build.context, resolved as the CLI resolves it: relative to the
// configuration's directory.
func (p *Policy) approvedPath(field, path string) bool {
	for _, v := range p.values(field) {
		var s string
		if json.Unmarshal(v, &s) != nil {
			continue
		}
		s = p.substitute(s)
		if !filepath.IsAbs(s) {
			if p.ConfigDir == "" {
				continue
			}
			s = filepath.Join(p.ConfigDir, s)
		}
		if filepath.Clean(s) == filepath.Clean(path) && p.staysPut(s) {
			return true
		}
	}
	return false
}

// staysPut: an approved path the clone holds — which the container can
// replace with a symbolic link — still resolves inside the clone. The approval
// is of the path as written, and docker follows a link in a bind source or a
// build context (measured), so the same string can name another place on the
// host by the time up runs. The workspace's containers are stopped before
// step 3 and stay stopped through up, so nothing in a container can move the
// link between this check and docker's use of it. A path outside the clone is
// the host's, which no container writes.
func (p *Policy) staysPut(path string) bool {
	clean := filepath.Clean(path)
	if clean != p.Clone && !strings.HasPrefix(clean, p.Clone+"/") {
		return true
	}
	return inside(clean, p.Clone)
}

// insideOwn: path is in the clone or in the CLI's TMPDIR, symbolic links
// followed.
func (p *Policy) insideOwn(path string) bool {
	return inside(path, p.Clone) || inside(path, p.TempDir)
}

// stagedContext: path is a build context in the CLI's TMPDIR — making it
// first when it is not there yet. CLI 0.89.0 starts the Features build
// without waiting for its own mkdir of the empty context folder
// (`<TMPDIR>/devcontainercli-<user>/empty-folder`: `s.mkdirp(l)` is not
// awaited), and every up's TMPDIR is new, so the guard can run before the
// folder exists, and a path that does not resolve is inside nothing: the
// build was refused as a context outside the clone, now and then (CI, #90).
// So a missing path is made here, as the CLI is about to make it, only when
// it is already clean (no `.`, `..` or `//`), lexically below the TMPDIR,
// and its nearest existing ancestor resolves, symbolic links followed,
// inside the TMPDIR — and is then held to the same rule as any other.
//
// Clean, because docker reads the path as written and the kernel follows a
// link before a later `..`: `<tmp>/link/../x` with link leading out names a
// place beside link's target, not <tmp>/x (measured with buildx). The walk
// up below is lexical, so it holds only for a path whose lexical and
// resolved readings agree. The TMPDIR is beside the clone and in no
// container, but its content is not trusted: the CLI extracts each Feature's
// tarball into it, links and all.
//
// Nothing outside the TMPDIR is ever made, and a link in the TMPDIR leading
// out of it is not followed into making anything. The Lstat-must-be-missing
// check and the final inside are redundant with each other: an existing link
// out of the TMPDIR, given as the context itself, is refused by either alone
// (and a test fails only when both are removed). Beyond that, only something
// swapping a component between those steps — Drydock or the CLI, nothing in
// a container — could reach either.
func (p *Policy) stagedContext(path string) bool {
	if inside(path, p.TempDir) {
		return true
	}
	if p.TempDir == "" || !filepath.IsAbs(path) || !filepath.IsAbs(p.TempDir) {
		return false
	}
	root, clean := filepath.Clean(p.TempDir), filepath.Clean(path)
	if clean != path || !strings.HasPrefix(clean, root+"/") {
		return false
	}
	if _, err := os.Lstat(clean); !errors.Is(err, fs.ErrNotExist) {
		return false // there, and not inside: a link out, or unreadable
	}
	ancestor := filepath.Dir(clean)
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) || ancestor == root {
			return false
		}
		ancestor = filepath.Dir(ancestor)
	}
	if !inside(ancestor, p.TempDir) {
		return false
	}
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return false
	}
	return inside(clean, p.TempDir)
}

// inside reports whether path resolves, symbolic links followed, to root or
// below it. A path that does not resolve is not inside anything.
func inside(path, root string) bool {
	if root == "" || !filepath.IsAbs(path) {
		return false
	}
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	q, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return q == r || strings.HasPrefix(q, r+"/")
}

// DevcontainerID is the CLI's ${devcontainerId} for a set of id-labels,
// computed as CLI 0.89.0 computes it: the SHA-256 of the labels as a JSON
// object with sorted keys, written in base 32 and padded to 52 digits. A
// volume whose name carries it is this workspace's alone (§6).
func DevcontainerID(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // JSON.stringify does not escape <, > or &
	enc.Encode(labels)       // a map: keys sorted, as the CLI sorts them
	sum := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	s := new(big.Int).SetBytes(sum[:]).Text(32)
	for len(s) < 52 {
		s = "0" + s
	}
	return s
}

var volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// mountAllowed: a --mount value is Drydock's own (the clone, the broker
// directory, the credential volume), this container's alone (a volume named
// with its ${devcontainerId}, an anonymous volume, a tmpfs — the same rule as
// step 3's), or an approved mounts entry or workspaceMount.
func (c *checker) mountAllowed(v string) bool {
	m, ok := parseMount(v)
	if !ok {
		return false
	}
	key := m.key()
	for _, own := range c.p.OwnMounts {
		if o, ok := parseMount(own); ok && o.key() == key {
			return true
		}
	}
	typ := m.get("type", "volume")
	switch typ {
	case "bind":
		// The clone at the workspace folder, as the CLI mounts it.
		if m.only("type", "source", "target", "consistency") && m["source"] == c.p.Clone && strings.HasPrefix(m["target"], "/") {
			return true
		}
	case "volume":
		if m.only("type", "source", "target", "readonly", "consistency") && strings.HasPrefix(m["target"], "/") {
			src := m["source"]
			if src == "" || ownVolume(src, DevcontainerID(c.p.IDLabels)) {
				return true
			}
		}
	case "tmpfs":
		if m.only("type", "target", "tmpfs-size", "tmpfs-mode", "readonly") && strings.HasPrefix(m["target"], "/") {
			return true
		}
	}
	for _, a := range c.p.approvedMounts() {
		if a.key() == key && (a.get("type", "volume") != "bind" || c.p.staysPut(a["source"])) {
			return true
		}
	}
	return false
}

// otherID is a ${devcontainerId} as the CLI renders one: 52 base-32 digits.
var otherID = regexp.MustCompile(`[0-9a-v]{52}`)

// ownVolume: a volume name carrying this workspace's ${devcontainerId}, and
// no other workspace's — a name carrying two would be both workspaces'.
func ownVolume(src, id string) bool {
	if id == "" || !strings.Contains(src, id) || !volumeName.MatchString(src) {
		return false
	}
	return !otherID.MatchString(strings.ReplaceAll(src, id, "-"))
}

// mount is a --mount value's fields, with docker's aliases folded.
type mount map[string]string

var mountAlias = map[string]string{"src": "source", "dst": "target", "destination": "target", "ro": "readonly"}

// parseMount reads a --mount value as docker does — comma-separated
// key=value fields — refusing a quoted one (docker reads it as CSV, and
// quoting is how a field hides a comma).
func parseMount(v string) (mount, bool) {
	if v == "" || strings.ContainsAny(v, "\"'\n") {
		return nil, false
	}
	m := mount{}
	for _, f := range strings.Split(v, ",") {
		k, val, has := strings.Cut(f, "=")
		k = strings.ToLower(k)
		if a, ok := mountAlias[k]; ok {
			k = a
		}
		if !has {
			if k != "readonly" {
				return nil, false
			}
			val = "true"
		}
		if k == "readonly" && (val == "1" || val == "true") {
			val = "true"
		}
		if k == "type" {
			val = strings.ToLower(val) // as docker reads it
		}
		if _, dup := m[k]; dup {
			return nil, false
		}
		m[k] = val
	}
	return m, true
}

func (m mount) get(k, def string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return def
}

func (m mount) only(keys ...string) bool {
	allowed := set(keys...)
	for k := range m {
		if !allowed[k] {
			return false
		}
	}
	return true
}

// key is the mount in one canonical string, docker's default type filled in.
func (m mount) key() string {
	c := mount{}
	for k, v := range m {
		c[k] = v
	}
	if _, ok := c["type"]; !ok {
		c["type"] = "volume"
	}
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k + "=" + c[k])
	}
	return b.String()
}

// approvedMounts is every approved mounts element and workspaceMount, as the
// CLI turns each into a --mount value: a string as written (substituted), an
// object as type, source and target alone (CLI 0.89.0 drops its other keys).
func (p *Policy) approvedMounts() []mount {
	var out []mount
	addOne := func(raw json.RawMessage) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if m, ok := parseMount(p.substitute(s)); ok {
				out = append(out, m)
			}
			return
		}
		var o struct{ Type, Source, Target string }
		if json.Unmarshal(raw, &o) == nil {
			m := mount{"type": p.substitute(o.Type), "target": p.substitute(o.Target)}
			if o.Source != "" {
				m["source"] = p.substitute(o.Source)
			}
			out = append(out, m)
		}
	}
	for _, v := range p.values("mounts") {
		var list []json.RawMessage
		if json.Unmarshal(v, &list) == nil {
			for _, e := range list {
				addOne(e)
			}
		}
	}
	for _, v := range p.values("workspaceMount") {
		addOne(v)
	}
	return out
}

func set(s ...string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}
