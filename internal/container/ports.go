package container

import (
	"encoding/json"
	"strconv"
	"strings"
)

// DeclaredPort is one port a dev container configuration names, with the
// label its portsAttributes gives it ("" when none).
type DeclaredPort struct {
	Port  int
	Label string
}

// DeclaredPorts reads the ports a resolved configuration declares (PF §8.2,
// §13 step 4): its own and its merged forwardPorts, then appPort, each port
// once, in that order, labelled from portsAttributes.
//
// What counts, and what does not:
//
//   - forwardPorts: a number, a string of digits, or "localhost:N" — the
//     container's own port. "service:N" names another Compose service's
//     port, which is not this container's to preview, and is skipped.
//   - appPort: a number, or a docker -p string ("3000", "8080:3000",
//     "127.0.0.1:8080:3000", "3000/tcp") whose last field is the container
//     port; /udp is skipped. appPort is host access and needs an approval to
//     run (design §6); declaring it here lists the container port, nothing
//     more.
//   - portsAttributes: a label for a key that is exactly a port number;
//     ranges and patterns label nothing.
//
// Anything malformed is skipped rather than refused: a declaration is only a
// row in the panel, never an exposure, and the configuration is the
// container's to write.
func DeclaredPorts(c Configuration) []DeclaredPort {
	var out []DeclaredPort
	seen := map[int]bool{}
	add := func(p int) {
		if p >= 1 && p <= 65535 && !seen[p] {
			seen[p] = true
			out = append(out, DeclaredPort{Port: p})
		}
	}
	for _, cfg := range []map[string]json.RawMessage{c.Own, c.Merged} {
		for _, raw := range list(cfg["forwardPorts"]) {
			add(forwardPort(raw))
		}
	}
	for _, cfg := range []map[string]json.RawMessage{c.Own, c.Merged} {
		for _, raw := range list(cfg["appPort"]) {
			add(appPort(raw))
		}
	}
	labels := map[int]string{}
	for _, cfg := range []map[string]json.RawMessage{c.Merged, c.Own} { // the repository's own wins
		var attrs map[string]struct {
			Label string `json:"label"`
		}
		if json.Unmarshal(cfg["portsAttributes"], &attrs) != nil {
			continue
		}
		for k, a := range attrs {
			if p, err := strconv.Atoi(k); err == nil && strconv.Itoa(p) == k && a.Label != "" {
				labels[p] = a.Label
			}
		}
	}
	for i := range out {
		out[i].Label = labels[out[i].Port]
	}
	return out
}

// list is a JSON array's elements, or the one value that is not an array.
func list(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	return []json.RawMessage{raw}
}

func number(raw json.RawMessage) (int, bool) {
	var n json.Number
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if d.Decode(&n) != nil {
		return 0, false
	}
	v, err := strconv.Atoi(n.String())
	return v, err == nil
}

func digits(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil || strconv.Itoa(v) != s {
		return 0
	}
	return v
}

func forwardPort(raw json.RawMessage) int {
	if n, ok := number(raw); ok {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0
	}
	if host, port, ok := strings.Cut(s, ":"); ok {
		if host != "localhost" {
			return 0
		}
		s = port
	}
	return digits(s)
}

func appPort(raw json.RawMessage) int {
	if n, ok := number(raw); ok {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0
	}
	if p, proto, ok := strings.Cut(s, "/"); ok {
		if proto != "tcp" {
			return 0
		}
		s = p
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	return digits(s)
}
