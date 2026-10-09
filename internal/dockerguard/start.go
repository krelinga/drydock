package dockerguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
)

// SettingStartUnread is a `docker start` whose containers' settings the
// guard could not read, or read only in part: refused, as a command it
// cannot check.
const SettingStartUnread = "start_unread"

// fullID is a container id as Drydock and the CLI pass it to start.
var fullID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// defaultMaskedPaths and defaultReadonlyPaths are the /proc and /sys paths
// Docker masks and makes read-only in every container that is not
// privileged (measured on Docker 29.8.2). `--security-opt
// systempaths=unconfined` empties both and leaves no trace in SecurityOpt
// (measured), so a started container must still carry every one of them —
// a later Docker that masks more passes; one that masks fewer, or a
// container made with systempaths=unconfined, needs that approved.
var (
	defaultMaskedPaths = []string{"/proc/acpi", "/proc/asound", "/proc/interrupts", "/proc/kcore",
		"/proc/keys", "/proc/latency_stats", "/proc/sched_debug", "/proc/scsi", "/proc/timer_list",
		"/proc/timer_stats", "/sys/devices/virtual/powercap", "/sys/firmware"}
	defaultReadonlyPaths = []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}
)

// hostConfigRule is how CheckStarted reads one HostConfig field.
type hostConfigRule int

const (
	// hcAny: the field restricts or describes the container only — a
	// resource limit, its DNS, its log driver — whatever its value.
	hcAny hostConfigRule = iota
	// hcZero: host access unless zero (null, false, 0, "", [] or {}),
	// and only runArgs can set it, so it needs runArgs approved.
	hcZero
	// hcChecked: read by a rule of its own below.
	hcChecked
)

// hostConfigFields is every field of HostConfig in Docker 29's API, each
// with its rule. A field not here — one a later Docker adds — is host access
// unless zero, like an option run does not know: an allowlist, not a list of
// what is known to be dangerous (review of #78, round 2: a denylist missed
// systempaths=unconfined and --device-cgroup-rule).
var hostConfigFields = map[string]hostConfigRule{
	// Read by their own rules.
	"Privileged": hcChecked, "CapAdd": hcChecked, "SecurityOpt": hcChecked, "Mounts": hcChecked,
	"DeviceRequests": hcChecked, "PortBindings": hcChecked, "PublishAllPorts": hcChecked,
	"MaskedPaths": hcChecked, "ReadonlyPaths": hcChecked, "NetworkMode": hcChecked,
	"PidMode": hcChecked, "IpcMode": hcChecked, "UTSMode": hcChecked, "UsernsMode": hcChecked,
	"CgroupnsMode": hcChecked, "Runtime": hcChecked, "RestartPolicy": hcChecked, "LogConfig": hcChecked,
	// What only runArgs (or the API) can set, each host access when set:
	// -v binds, links, another container's volumes, devices and device
	// cgroup rules (with the default CAP_MKNOD, `b *:* rwm` reads the
	// host's disks), a cgroup parent, sysctls, a volume driver, an id file
	// the CLI writes on the host, real-time scheduling, the OOM killer's
	// aim, annotations a runtime reads.
	"Binds": hcZero, "Links": hcZero, "VolumesFrom": hcZero, "Devices": hcZero,
	"DeviceCgroupRules": hcZero, "CgroupParent": hcZero, "Cgroup": hcZero, "Sysctls": hcZero,
	"VolumeDriver": hcZero, "ContainerIDFile": hcZero, "CpuRealtimePeriod": hcZero,
	"CpuRealtimeRuntime": hcZero, "OomKillDisable": hcZero, "OomScoreAdj": hcZero,
	"Isolation": hcZero, "Annotations": hcZero, "StorageOpt": hcZero,
	// The container's own: limits, which only take away, and settings that
	// stay inside it.
	"AutoRemove": hcAny, "Init": hcAny, "CapDrop": hcAny, "ConsoleSize": hcAny,
	"Dns": hcAny, "DnsOptions": hcAny, "DnsSearch": hcAny, "ExtraHosts": hcAny, "GroupAdd": hcAny,
	"ReadonlyRootfs": hcAny, "ShmSize": hcAny, "Tmpfs": hcAny, "Ulimits": hcAny,
	"BlkioWeight": hcAny, "BlkioWeightDevice": hcAny, "BlkioDeviceReadBps": hcAny,
	"BlkioDeviceWriteBps": hcAny, "BlkioDeviceReadIOps": hcAny, "BlkioDeviceWriteIOps": hcAny,
	"CpuCount": hcAny, "CpuPercent": hcAny, "CpuPeriod": hcAny, "CpuQuota": hcAny, "CpuShares": hcAny,
	"CpusetCpus": hcAny, "CpusetMems": hcAny, "NanoCpus": hcAny, "Memory": hcAny,
	"MemoryReservation": hcAny, "MemorySwap": hcAny, "MemorySwappiness": hcAny, "KernelMemory": hcAny,
	"KernelMemoryTCP": hcAny, "PidsLimit": hcAny, "IOMaximumBandwidth": hcAny, "IOMaximumIOps": hcAny,
}

// started is one container in `docker inspect`'s answer.
type started struct {
	ID         *string                     `json:"Id"`
	HostConfig *map[string]json.RawMessage `json:"HostConfig"`
	Config     *struct {
		Labels map[string]string
	} `json:"Config"`
}

type startedMount struct {
	Type, Source, Target string
	ReadOnly             bool
	Consistency          string
	BindOptions          map[string]json.RawMessage
	VolumeOptions        *struct {
		NoCopy       bool
		Labels       map[string]string
		Subpath      string
		DriverConfig *struct {
			Name    string
			Options map[string]string
		}
	}
	TmpfsOptions   json.RawMessage
	ImageOptions   json.RawMessage
	ClusterOptions json.RawMessage
}

// CheckStarted holds the containers a `docker start` of ids would start to
// the policy, from `docker inspect`'s JSON for them: what each was created
// with is what it gets again (design §6, "The docker guard"). Every
// HostConfig field is read, by hostConfigFields:
//
//   - Privileged needs privileged; each CapAdd its capAdd element (or
//     SYS_PTRACE); each SecurityOpt its securityOpt element (or
//     seccomp=unconfined); MaskedPaths and ReadonlyPaths every default path
//     unless the container is privileged or systempaths=unconfined is
//     approved; DeviceRequests hostRequirements.gpu.
//   - Each mount — type, source, target, read-only and bind propagation —
//     is Drydock's own, this container's alone, or approved, as on run; a
//     volume with driver options, or a bind option the CLI cannot write, is
//     neither.
//   - A host or another container's namespace (network, pid, ipc, uts,
//     user, cgroup), a runtime but runc, a restart policy, and every hcZero
//     field set, need runArgs approved at all; published ports appPort or
//     runArgs. HostConfig does not say which approved words made them, so
//     this half is coarser than run's.
//   - A field not in the table, set, is refused.
//
// And it fails closed on what it did not expect: anything but exactly one
// result for each id, each with its full id and a HostConfig, is
// start_unread.
func CheckStarted(p *Policy, ids []string, inspectJSON []byte, daemonLog func() (*LogConfig, error)) Decision {
	c := &checker{p: p}
	var (
		asked bool
		dl    *LogConfig
		dlErr error
	)
	// Asked at most once, and only for a container whose log configuration
	// is not otherwise allowed.
	c.daemonLog = func() (*LogConfig, error) {
		if !asked {
			asked = true
			if daemonLog == nil {
				dlErr = errors.New("no way to ask the daemon")
			} else {
				dl, dlErr = daemonLog()
			}
		}
		return dl, dlErr
	}
	if p == nil {
		c.refuse(SettingNoPolicy, "no policy for a command that starts a container")
		return c.decision("start")
	}
	unread := func(why string) Decision {
		c.refuse(SettingStartUnread, why)
		return c.decision("start")
	}
	var all []started
	if err := json.Unmarshal(inspectJSON, &all); err != nil {
		return unread("docker inspect's answer could not be read")
	}
	want := map[string]bool{}
	for _, id := range ids {
		if !fullID.MatchString(id) {
			return unread("a start of " + id + ", not a full container id")
		}
		want[id] = true
	}
	if len(all) != len(ids) || len(want) != len(ids) {
		return unread("docker inspect did not answer once for each container to start")
	}
	for _, s := range all {
		if s.ID == nil || !want[*s.ID] || s.HostConfig == nil || *s.HostConfig == nil {
			return unread("docker inspect's answer lacks a container's id or its HostConfig")
		}
		delete(want, *s.ID)
		c.hostConfig(*s.HostConfig)
		if s.Config != nil {
			for k, v := range s.Config.Labels {
				if p.LabelPrefix != "" && strings.HasPrefix(k, p.LabelPrefix+".") {
					if w, ok := p.IDLabels[k]; !ok || w != v {
						c.refuse(SettingRunArgs, "a container labelled "+k+" that is not one of its id-labels")
					}
				}
			}
		}
	}
	d := c.decision("start")
	sort.Strings(d.Why)
	return d
}

// zero: null, false, 0, "", [] or {}.
func zero(raw json.RawMessage) bool {
	t := string(bytes.TrimSpace(raw))
	switch t {
	case "", "null", "false", "0", `""`, "[]", "{}":
		return true
	}
	return false
}

func (c *checker) hostConfig(hc map[string]json.RawMessage) {
	p := c.p
	runArgs := p.has("runArgs")
	str := func(k string) string {
		var s string
		json.Unmarshal(hc[k], &s)
		return s
	}
	strs := func(k string) ([]string, bool) {
		if hc[k] == nil || string(bytes.TrimSpace(hc[k])) == "null" {
			return nil, true
		}
		l := []string{}
		err := json.Unmarshal(hc[k], &l)
		return l, err == nil
	}
	needRunArgs := func(k, why string) {
		if !runArgs {
			c.refuse(SettingRunArgs, "a container created with "+k+" "+why)
		}
	}
	for k, raw := range hc {
		rule, known := hostConfigFields[k]
		switch {
		case !known:
			if !zero(raw) {
				c.refuse(SettingRunArgs, "a container created with "+k+", which the guard does not know")
			}
		case rule == hcZero:
			if !zero(raw) {
				needRunArgs(k, "set")
			}
		}
	}
	var privileged bool
	if json.Unmarshal(hc["Privileged"], &privileged) != nil && hc["Privileged"] != nil {
		c.refuse(SettingStartUnread, "Privileged is not a boolean")
	}
	if privileged && !p.has("privileged") {
		c.refuse(SettingPrivileged, "a container created --privileged")
	}
	if caps, ok := strs("CapAdd"); !ok {
		c.refuse(SettingStartUnread, "CapAdd is not a list")
	} else {
		for _, cp := range caps {
			if n := normCap(cp); n != "SYS_PTRACE" && !p.caps()[n] {
				c.refuse(SettingCapAdd, "a container created with --cap-add "+cp)
			}
		}
	}
	if opts, ok := strs("SecurityOpt"); !ok {
		c.refuse(SettingStartUnread, "SecurityOpt is not a list")
	} else {
		for _, so := range opts {
			// An approved seccomp=<file> is stored as the profile's JSON,
			// which is not what was approved: refused, and the workspace
			// recreates the container (design §6).
			// label=disable is what docker adds to a privileged
			// container's SecurityOpt (measured): the privileged rule
			// above decides it.
			if so == "label=disable" && privileged {
				continue
			}
			if so != "seccomp=unconfined" && !p.listed("securityOpt")[so] {
				c.refuse(SettingSecurityOpt, "a container created with --security-opt "+short(so))
			}
		}
	}
	systempaths := p.listed("securityOpt")["systempaths=unconfined"]
	for k, defaults := range map[string][]string{"MaskedPaths": defaultMaskedPaths, "ReadonlyPaths": defaultReadonlyPaths} {
		l, ok := strs(k)
		switch {
		case !ok:
			c.refuse(SettingStartUnread, k+" is not a list")
		case privileged: // a privileged container masks nothing; the rule above decides it
		case systempaths:
		case !containsAll(l, defaults):
			c.refuse(SettingSecurityOpt, "a container created with "+k+" unmasked (systempaths=unconfined)")
		}
	}
	var reqs []json.RawMessage
	json.Unmarshal(hc["DeviceRequests"], &reqs)
	if len(reqs) > 0 && !p.has("hostRequirements.gpu") && !runArgs {
		c.refuse(SettingGPU, "a container created with GPUs")
	}
	var publishAll bool
	json.Unmarshal(hc["PublishAllPorts"], &publishAll)
	if (!zero(hc["PortBindings"]) || publishAll) && !p.has("appPort") && !runArgs {
		c.refuse(SettingAppPort, "a container created with published ports")
	}
	ns := func(k string, ok ...string) {
		v := str(k)
		for _, o := range ok {
			if v == o {
				return
			}
		}
		needRunArgs(k, v)
	}
	ns("NetworkMode", "", "default", "bridge", "none")
	ns("PidMode", "")
	ns("IpcMode", "", "private", "shareable", "none")
	ns("UTSMode", "")
	ns("UsernsMode", "")
	ns("CgroupnsMode", "", "private")
	ns("Runtime", "", "runc")
	// A log driver is the daemon's, run on the host: gelf and syslog send
	// the container's output to an address the host reaches (measured: gelf
	// to a UDP listener on the daemon host's loopback, from the default
	// bridge — review of #78, round 3), fluentd and syslog reach host unix
	// sockets, splunk posts from the host, awslogs and gcplogs use the
	// daemon's credentials. So a container's log configuration needs runArgs
	// (--log-driver, --log-opt) approved unless it is one no configuration
	// chose: a file driver with no options, or exactly what the daemon gives
	// a container whose argv names no log option — the operator's own
	// default, which Docker writes into every container it creates and which
	// the first create, checked by run's rules, already got (logprobe.go).
	var logCfg LogConfig
	if !zero(hc["LogConfig"]) && json.Unmarshal(hc["LogConfig"], &logCfg) != nil {
		c.refuse(SettingStartUnread, "LogConfig is not a log configuration")
	}
	fileDriver := (logCfg.Type == "" || logCfg.Type == "json-file" || logCfg.Type == "local") && len(logCfg.Config) == 0
	if !fileDriver && !runArgs {
		switch def, err := c.daemonLog(); {
		case err != nil:
			c.refuse(SettingRunArgs, "a container created with LogConfig "+logCfg.String()+
				", and the daemon's default could not be read: "+err.Error())
		case def == nil || !logCfg.equal(*def):
			why := "a container created with LogConfig " + logCfg.String() + ", not the daemon's default"
			if def != nil {
				why += " (" + def.String() + ")"
			}
			c.refuse(SettingRunArgs, why)
		}
	}
	var restart struct{ Name string }
	json.Unmarshal(hc["RestartPolicy"], &restart)
	if restart.Name != "" && restart.Name != "no" {
		needRunArgs("RestartPolicy", restart.Name)
	}
	var mounts []startedMount
	if !zero(hc["Mounts"]) && json.Unmarshal(hc["Mounts"], &mounts) != nil {
		c.refuse(SettingStartUnread, "Mounts is not a list of mounts")
	}
	for _, m := range mounts {
		if !c.startedMountAllowed(m) {
			c.refuse(SettingMounts, "a container created with a mount of "+m.Source+" at "+m.Target)
		}
	}
}

func short(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

func containsAll(have, want []string) bool {
	h := map[string]bool{}
	for _, s := range have {
		h[s] = true
	}
	for _, w := range want {
		if !h[w] {
			return false
		}
	}
	return true
}

// startedMountAllowed is mountAllowed for a mount read back from inspect:
// type, source, target, read-only and bind propagation, which is all
// HostConfig.Mounts keeps of what the CLI can ask — so an approved mount is
// compared on those too. A volume with driver options (the local driver's
// o=bind), or any bind or volume option but propagation, the CLI never
// writes, so it is never this container's alone nor an approved one.
func (c *checker) startedMountAllowed(m startedMount) bool {
	got := mount{"type": strings.ToLower(m.Type), "target": m.Target}
	if m.Source != "" {
		got["source"] = m.Source
	}
	if m.ReadOnly {
		got["readonly"] = "true"
	}
	for k, v := range m.BindOptions {
		if k == "Propagation" {
			var s string
			json.Unmarshal(v, &s)
			if s != "" {
				got["bind-propagation"] = s
			}
			continue
		}
		if !zero(v) {
			return false
		}
	}
	if vo := m.VolumeOptions; vo != nil {
		if vo.Subpath != "" || (vo.DriverConfig != nil && (vo.DriverConfig.Name != "" || len(vo.DriverConfig.Options) > 0)) {
			return false
		}
	}
	if !zero(m.ImageOptions) || !zero(m.ClusterOptions) {
		return false
	}
	if _, prop := got["bind-propagation"]; !prop {
		keys := []string{"type", "source", "target", "readonly"}
		var b strings.Builder
		for _, k := range keys {
			if v, ok := got[k]; ok {
				if b.Len() > 0 {
					b.WriteByte(',')
				}
				b.WriteString(k + "=" + v)
			}
		}
		if c.mountAllowed(b.String()) {
			return true
		}
	}
	core := got.core()
	for _, a := range append(c.p.approvedMounts(), c.ownMounts()...) {
		if a.core() == core && (a.get("type", "volume") != "bind" || c.p.staysPut(a["source"])) {
			return true
		}
	}
	return false
}

func (c *checker) ownMounts() []mount {
	var out []mount
	for _, o := range c.p.OwnMounts {
		if m, ok := parseMount(o); ok {
			out = append(out, m)
		}
	}
	return out
}

// core is the mount on type, source, target, read-only and bind propagation
// alone.
func (m mount) core() string {
	c := mount{"type": m.get("type", "volume"), "target": m["target"]}
	if m["source"] != "" {
		c["source"] = m["source"]
	}
	if m["readonly"] == "true" {
		c["readonly"] = "true"
	}
	if m["bind-propagation"] != "" {
		c["bind-propagation"] = m["bind-propagation"]
	}
	return c.key()
}
