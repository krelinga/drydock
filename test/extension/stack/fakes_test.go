package stack

// The two fake executables (TestMain runs one when DRYDOCK_STACK_FAKE names
// it). Each appends its argv to <state>/<name>-argv.log, one JSON array per
// line, which is what the stack's own test reads back.
//
// docker keeps containers as JSON files in <state>/containers, made by the
// fake devcontainer's `up` and answered to `ps` and `inspect` exactly as far
// as Drydock asks: by label and status filters, with docker's own inspect
// shape (Id, Name, State, Config.Labels, Mounts). The shared credential
// volume is remembered as created; nothing ever logs in, so the identity
// helpers find no credential.
//
// devcontainer answers `read-configuration` from the recorded corpus
// (test/fixtures/devcontainer), the docker-in-docker recording for a clone
// carrying the dind marker — with an initializeCommand added while
// <state>/dind-variant says `initialize` — and the plain recording otherwise;
// `up` makes a running container and prints the one JSON object the real CLI
// prints; `exec` answers the git remote (from the clone itself) and claude's
// version.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/krelinga/drydock/internal/classify"
)

func logArgv(state, name string, args []string) {
	b, _ := json.Marshal(args)
	f, err := os.OpenFile(filepath.Join(state, name+"-argv.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// fakeContainer is one container, as docker inspect reports it.
type fakeContainer struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	SizeRw     int64 `json:"SizeRw"`
	SizeRootFs int64 `json:"SizeRootFs"`
}

func containersDir(state string) string { return filepath.Join(state, "containers") }

func loadContainers(state string) []fakeContainer {
	entries, _ := os.ReadDir(containersDir(state))
	var out []fakeContainer
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(containersDir(state), e.Name()))
		if err != nil {
			continue
		}
		var c fakeContainer
		if json.Unmarshal(b, &c) == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func saveContainer(state string, c fakeContainer) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(containersDir(state), c.ID+".json"), b, 0o644)
}

func hexID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// matches is docker's --filter for the two kinds Drydock uses.
func matches(c fakeContainer, filters []string) bool {
	for _, f := range filters {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "label":
			lk, lv, hasV := strings.Cut(v, "=")
			got, ok := c.Config.Labels[lk]
			if !ok || (hasV && got != lv) {
				return false
			}
		case "status":
			if c.State.Status != v {
				return false
			}
		case "id":
			if !strings.HasPrefix(c.ID, v) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func fakeDocker(state string, args []string) int {
	logArgv(state, "docker", args)
	if len(args) == 0 {
		return 64
	}
	switch args[0] {
	case "ps":
		all := false
		var filters []string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--all", "-a":
				all = true
			case "--filter", "-f":
				if i+1 < len(args) {
					filters = append(filters, args[i+1])
					i++
				}
			}
		}
		for _, c := range loadContainers(state) {
			if (all || c.State.Running) && matches(c, filters) {
				fmt.Println(c.ID)
			}
		}
		return 0
	case "inspect":
		var ids []string
		typ := ""
		for i := 1; i < len(args); i++ {
			switch a := args[i]; {
			case a == "--type":
				typ = args[i+1]
				i++
			case a == "--format" || a == "-f":
				i++
			case strings.HasPrefix(a, "-"):
			default:
				ids = append(ids, a)
			}
		}
		if typ != "" && typ != "container" {
			return 0
		}
		byID := map[string]fakeContainer{}
		for _, c := range loadContainers(state) {
			byID[c.ID] = c
		}
		out := []fakeContainer{}
		for _, id := range ids {
			c, ok := byID[id]
			if !ok {
				fmt.Fprintf(os.Stderr, "Error: No such container: %s\n", id)
				return 1
			}
			out = append(out, c)
		}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
		return 0
	case "stop", "kill", "start", "rm", "pause", "unpause":
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				continue
			}
			for _, c := range loadContainers(state) {
				if c.ID != a {
					continue
				}
				switch args[0] {
				case "rm":
					os.Remove(filepath.Join(containersDir(state), c.ID+".json"))
					continue
				case "start", "unpause":
					c.State.Status, c.State.Running = "running", true
				default:
					c.State.Status, c.State.Running = "exited", false
				}
				saveContainer(state, c)
			}
		}
		return 0
	case "volume":
		return fakeVolume(state, args[1:])
	case "image":
		if len(args) > 1 && args[1] == "inspect" {
			fmt.Println("sha256:" + strings.Repeat("b", 64))
		}
		return 0
	case "run":
		joined := " " + strings.Join(args, " ") + " "
		switch {
		case strings.Contains(joined, ".credentials.json"):
			return 3 // no one has logged in: no credential file
		case strings.Contains(joined, " auth status "):
			b, err := os.ReadFile(filepath.Join(repoRoot(), "test", "fixtures", "authstatus", "absent.json"))
			if err == nil {
				os.Stdout.Write(b)
			}
			return 1
		}
		return 0
	}
	return 0
}

func fakeVolume(state string, args []string) int {
	if len(args) == 0 {
		return 64
	}
	last := args[len(args)-1]
	made := filepath.Join(state, "volume")
	switch args[0] {
	case "ls":
		if b, err := os.ReadFile(made); err == nil {
			fmt.Println(strings.TrimSpace(string(b)))
		}
	case "create":
		label := ""
		for i := 1; i+1 < len(args); i++ {
			if args[i] == "--label" {
				label, _, _ = strings.Cut(args[i+1], "=")
			}
		}
		os.WriteFile(made, []byte(last+"\n"), 0o644)
		os.WriteFile(made+".label", []byte(label), 0o644)
		fmt.Println(last)
	case "inspect":
		label, _ := os.ReadFile(made + ".label")
		if len(args) > 1 && args[1] == "--format" {
			fmt.Printf("{%q:\"true\"}\n", string(label))
		} else {
			fmt.Printf("[{\"Name\":%q,\"Driver\":\"local\",\"Labels\":{%q:\"true\"}}]\n", last, string(label))
		}
	}
	return 0
}

// repoRoot is the module root, from this package's directory (go test runs
// a package's test binary in its own directory).
func repoRoot() string {
	if r := os.Getenv("DRYDOCK_STACK_REPO"); r != "" {
		return r
	}
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "..", "..")
}

func flag(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func fakeDevcontainer(state string, args []string) int {
	logArgv(state, "devcontainer", args)
	if len(args) == 0 {
		return 64
	}
	folder := flag(args, "--workspace-folder")
	switch args[0] {
	case "read-configuration":
		return readConfiguration(state, folder)
	case "up":
		return up(state, folder, args)
	case "exec":
		return devExec(folder, args)
	}
	return 64
}

func readConfiguration(state, folder string) int {
	fixtures := filepath.Join(repoRoot(), "test", "fixtures", "devcontainer")
	name, placeholder := "read-configuration-merged-ok.json", "/srv/drydock/ws/FIXTURE/alpha"
	dind := false
	if _, err := os.Stat(filepath.Join(folder, dindMarker)); err == nil {
		name, placeholder, dind = "read-configuration-merged-dind.json", "/srv/drydock/ws/FIXTURE/dind", true
	}
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	b = bytes.ReplaceAll(b, []byte(placeholder), []byte(folder))
	if v, _ := os.ReadFile(filepath.Join(state, "dind-variant")); dind && strings.TrimSpace(string(v)) == "initialize" {
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, k := range []string{"configuration", "mergedConfiguration"} {
			if m, ok := doc[k].(map[string]any); ok {
				m["initializeCommand"] = "make host-setup"
			}
		}
		b, _ = json.Marshal(doc)
	}
	os.Stdout.Write(b)
	return 0
}

func up(state, folder string, args []string) int {
	labels := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--id-label" {
			k, v, _ := strings.Cut(args[i+1], "=")
			labels[k] = v
		}
	}
	// A rebuild replaces the container; a start reuses a stopped one.
	rebuild := false
	for _, a := range args {
		if a == "--remove-existing-container" {
			rebuild = true
		}
	}
	var c fakeContainer
	for _, old := range loadContainers(state) {
		same := true
		for k, v := range labels {
			if old.Config.Labels[k] != v {
				same = false
			}
		}
		if !same {
			continue
		}
		if rebuild {
			os.Remove(filepath.Join(containersDir(state), old.ID+".json"))
			continue
		}
		c = old
	}
	if c.ID == "" {
		c.ID = hexID(32)
		c.Name = "/stack_" + hexID(4)
		c.Config.Labels = labels
		c.Mounts = append(c.Mounts, struct {
			Type        string `json:"Type"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		}{"bind", folder, "/workspaces/" + filepath.Base(folder)})
	}
	c.State.Status, c.State.Running = "running", true
	if err := saveContainer(state, c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	out, _ := json.Marshal(map[string]any{
		"outcome": "success", "containerId": c.ID, "remoteUser": "root",
		"remoteWorkspaceFolder": "/workspaces/" + filepath.Base(folder),
	})
	fmt.Println(string(out))
	return 0
}

func devExec(folder string, args []string) int {
	joined := " " + strings.Join(args, " ") + " "
	switch {
	case strings.Contains(joined, " remote -v "):
		cmd := exec.Command("git", "-C", folder, "remote", "-v")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return 1
		}
		return 0
	case strings.Contains(joined, " claude --version "):
		fmt.Printf("%s (Claude Code)\n", classify.ClaudeCodeVersion)
		return 0
	}
	return 0
}
