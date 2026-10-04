package secrets

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Codes for a refused write. They are the error envelope's `code` (api
// re-exports them), so they are stable: the UI turns each into one sentence.
const (
	CodeNameInvalid    = "secret_name_invalid"
	CodeNameReserved   = "secret_name_reserved"
	CodeValueEmpty     = "secret_value_empty"
	CodeValueControl   = "secret_value_control_character"
	CodeValueTooLong   = "secret_value_too_long"
	CodeReachRequired  = "secret_reach_required"
	CodeUnknownRepo    = "unknown_repository"
	CodeDescriptionBad = "secret_description_invalid"
)

// Invalid is a refused write: a code for the UI, a sentence for a human, and
// a detail that never quotes the value.
type Invalid struct {
	Code, Message, Detail string
}

func (e *Invalid) Error() string { return "secrets: " + e.Message + " " + e.Detail }

// MaxNameLen and MaxValueLen bound a secret. A value is delivered into the
// environment of every command a workspace runs, and Linux refuses a single
// environment string over 128 KiB (MAX_ARG_STRLEN); 32 KiB keeps well clear
// while holding any token or URL. A PEM, which is multi-line anyway, goes in
// base64.
const (
	MaxNameLen        = 128
	MaxValueLen       = 32 << 10
	MaxReachLen       = 2000
	MaxDescriptionLen = 4000
)

var namePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// reservedExact and reservedPrefix are §10.1's reserved names, as built.
//
// A secret's name becomes an environment variable in every command shell the
// workspace runs and — through the supervisor's exec (§10.3) — in Claude
// Code itself. A name on this list would break the system quietly: it builds,
// starts, and then fails somewhere that does not look like a secret. The
// groups, and why each one is here:
//
//   - Remote Control. ANTHROPIC_BASE_URL, DISABLE_TELEMETRY, DO_NOT_TRACK,
//     CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC and DISABLE_GROWTHBOOK each
//     disable it (§2.1) while everything else still works.
//   - Claude Code's own configuration: every CLAUDE_*, ANTHROPIC_* and
//     DISABLE_* name. CLAUDE_CONFIG_DIR moves the shared credential volume
//     (§7.1), CLAUDE_ENV_FILE replaces this very delivery mechanism,
//     CLAUDE_CODE_OAUTH_TOKEN and ANTHROPIC_API_KEY replace the login §7
//     establishes with one that cannot run Remote Control, and
//     DISABLE_AUTOUPDATER is what the version pin rests on (§11).
//   - GitHub credentials (§9.2). GH_* (GH_TOKEN, GH_HOST, GH_CONFIG_DIR…),
//     GITHUB_TOKEN and GITHUB_ENTERPRISE_TOKEN would shadow the gh shim's
//     expiring, repository-scoped token with a static one; GIT_* includes
//     GIT_CONFIG_COUNT/KEY/VALUE, which can replace the credential helper and
//     core.hooksPath (the drydock/ branch guard), and GIT_ASKPASS.
//   - Drydock's own: DRYDOCK_*. DRYDOCK_BROKER_SOCK points the clients at
//     another socket, and DRYDOCK_GITHUB_HOST would hand the token to another
//     host.
//   - The process: PATH and the loader's LD_* (LD_PRELOAD, LD_LIBRARY_PATH,
//     LD_AUDIT) decide which code every command runs; HOME, SHELL, USER,
//     LOGNAME and PWD are what everything else is resolved against.
//   - The shell the prelude runs in: BASH_* (BASH_ENV runs a file before
//     every non-interactive bash), ENV, IFS, CDPATH, PS4, PROMPT_COMMAND,
//     SHELLOPTS, BASHOPTS and GLOBIGNORE change how every later command
//     parses; UID, EUID and PPID are read-only in bash, so `export UID=…` is
//     an error inside the prelude itself, on every command; and `_` is the
//     shell's own.
//   - Claude Code's runtime, which is Node: NODE_OPTIONS loads code into it,
//     and NODE_EXTRA_CA_CERTS and NODE_TLS_REJECT_UNAUTHORIZED change whom it
//     trusts.
//   - Trust and routing for the credentials Drydock delivers: HTTP_PROXY,
//     HTTPS_PROXY, ALL_PROXY, NO_PROXY, SSL_CERT_FILE, SSL_CERT_DIR and
//     CURL_CA_BUNDLE would send the broker's GitHub tokens and the session's
//     traffic through, or trust, whatever the value names.
//
// Lowercase variants (http_proxy) cannot be secret names at all: the name
// pattern is uppercase.
var reservedExact = map[string]string{
	"DO_NOT_TRACK":                 "it disables Remote Control (design §2.1)",
	"GITHUB_TOKEN":                 "it would shadow the gh shim's per-call, repository-scoped token (§9.2)",
	"GITHUB_ENTERPRISE_TOKEN":      "it would shadow the gh shim's per-call, repository-scoped token (§9.2)",
	"PATH":                         "it decides which program every command runs",
	"HOME":                         "everything in the container resolves its configuration against it",
	"SHELL":                        "everything in the container resolves its configuration against it",
	"USER":                         "everything in the container resolves its configuration against it",
	"LOGNAME":                      "everything in the container resolves its configuration against it",
	"PWD":                          "everything in the container resolves its configuration against it",
	"ENV":                          "it changes how every later shell command is parsed",
	"IFS":                          "it changes how every later shell command is parsed",
	"CDPATH":                       "it changes how every later shell command is parsed",
	"PS4":                          "it changes how every later shell command is parsed",
	"PROMPT_COMMAND":               "it changes how every later shell command is parsed",
	"SHELLOPTS":                    "it changes how every later shell command is parsed",
	"BASHOPTS":                     "it changes how every later shell command is parsed",
	"GLOBIGNORE":                   "it changes how every later shell command is parsed",
	"UID":                          "bash makes it read-only, so exporting it fails the prelude on every command",
	"EUID":                         "bash makes it read-only, so exporting it fails the prelude on every command",
	"PPID":                         "bash makes it read-only, so exporting it fails the prelude on every command",
	"_":                            "the shell sets it itself",
	"NODE_OPTIONS":                 "it loads code into Claude Code, which is a Node program",
	"NODE_EXTRA_CA_CERTS":          "it changes whom Claude Code trusts",
	"NODE_TLS_REJECT_UNAUTHORIZED": "it changes whom Claude Code trusts",
	"HTTP_PROXY":                   "it would route the workspace's GitHub tokens and session traffic through another host",
	"HTTPS_PROXY":                  "it would route the workspace's GitHub tokens and session traffic through another host",
	"ALL_PROXY":                    "it would route the workspace's GitHub tokens and session traffic through another host",
	"NO_PROXY":                     "it would route the workspace's GitHub tokens and session traffic through another host",
	"SSL_CERT_FILE":                "it changes which certificates the workspace trusts with its GitHub tokens",
	"SSL_CERT_DIR":                 "it changes which certificates the workspace trusts with its GitHub tokens",
	"CURL_CA_BUNDLE":               "it changes which certificates the workspace trusts with its GitHub tokens",
}

var reservedPrefix = []struct{ prefix, why string }{
	{"ANTHROPIC_", "Claude Code reads ANTHROPIC_* itself; ANTHROPIC_BASE_URL disables Remote Control (§2.1) and ANTHROPIC_API_KEY replaces its login (§7)"},
	{"CLAUDE_", "Claude Code reads CLAUDE_* itself: the config directory, this delivery mechanism, its login (§7, §10.3)"},
	{"DISABLE_", "Claude Code reads DISABLE_* itself; DISABLE_TELEMETRY and DISABLE_GROWTHBOOK disable Remote Control (§2.1), DISABLE_AUTOUPDATER holds the version pin (§11)"},
	{"GH_", "gh reads GH_* itself; GH_TOKEN would shadow the shim's repository-scoped token (§9.2)"},
	{"GIT_", "git reads GIT_* itself; GIT_CONFIG_* can replace the credential helper and the branch guard (§9.2, §11)"},
	{"DRYDOCK_", "Drydock's own clients read DRYDOCK_* to find the broker (§9)"},
	{"LD_", "the dynamic loader reads LD_* and loads code into every process"},
	{"BASH_", "bash reads BASH_* itself; BASH_ENV runs a file before every command"},
}

// Reserved reports whether name is reserved, and why.
func Reserved(name string) (string, bool) {
	if why, ok := reservedExact[name]; ok {
		return why, true
	}
	for _, p := range reservedPrefix {
		if strings.HasPrefix(name, p.prefix) {
			return p.why, true
		}
	}
	return "", false
}

// ReservedNames lists the exact names and the prefixes (with a trailing *),
// for the design doc and the tests.
func ReservedNames() []string {
	var out []string
	for n := range reservedExact {
		out = append(out, n)
	}
	for _, p := range reservedPrefix {
		out = append(out, p.prefix+"*")
	}
	return out
}

// ValidateName is §10.1's name rule plus the reserved list.
func ValidateName(name string) error {
	if len(name) == 0 || len(name) > MaxNameLen || !namePattern.MatchString(name) {
		return &Invalid{Code: CodeNameInvalid,
			Message: "A secret's name is its environment variable name.",
			Detail:  fmt.Sprintf("Use capital letters, digits and underscores, not starting with a digit, at most %d characters.", MaxNameLen)}
	}
	if why, ok := Reserved(name); ok {
		return &Invalid{Code: CodeNameReserved,
			Message: name + " is reserved.",
			Detail:  "Drydock refuses it because " + why + "."}
	}
	return nil
}

// ValidateValue is §10.1's value rule. Any control character is refused —
// newline, carriage return and NUL above all, because a newline inside a
// value forges a second `NAME value` line in the GET-SECRETS answer and
// delivers a secret the reserved list never saw (§10.3). The detail names the
// character, never the value.
func ValidateValue(v string) error {
	if v == "" {
		return &Invalid{Code: CodeValueEmpty, Message: "A secret needs a value.",
			Detail: "An empty value cannot be told apart from an unset variable."}
	}
	if len(v) > MaxValueLen {
		return &Invalid{Code: CodeValueTooLong, Message: "That value is too long.",
			Detail: fmt.Sprintf("A value is at most %d bytes. Encode a large or multi-line credential, such as a PEM, as base64.", MaxValueLen)}
	}
	if !utf8.ValidString(v) {
		return &Invalid{Code: CodeValueControl, Message: "A secret's value must be text.",
			Detail: "The value is not valid UTF-8."}
	}
	for i, r := range v {
		if unicode.IsControl(r) {
			return &Invalid{Code: CodeValueControl,
				Message: "A secret's value must be a single line with no control characters.",
				Detail:  fmt.Sprintf("It contains %s at byte %d. A multi-line credential, such as a PEM, goes in as base64.", charName(r), i)}
		}
	}
	return nil
}

func charName(r rune) string {
	switch r {
	case '\n':
		return "a newline (U+000A)"
	case '\r':
		return "a carriage return (U+000D)"
	case 0:
		return "a NUL (U+0000)"
	case '\t':
		return "a tab (U+0009)"
	}
	return fmt.Sprintf("the control character U+%04X", r)
}

// ValidateReach is the required sentence (§10.1, §10.4). The schema's CHECK
// refuses a blank one too; both, because the database is the last line and
// this is the one that has an error message.
func ValidateReach(reach string) error {
	if strings.TrimSpace(reach) == "" {
		return &Invalid{Code: CodeReachRequired, Message: "Say what someone could do with this secret.",
			Detail: "The reach field is required: it is the decision to grant, written down."}
	}
	if len(reach) > MaxReachLen {
		return &Invalid{Code: CodeReachRequired, Message: "The reach is too long.",
			Detail: fmt.Sprintf("At most %d bytes.", MaxReachLen)}
	}
	return nil
}

func validateDescription(d string) error {
	if len(d) > MaxDescriptionLen || !utf8.ValidString(d) {
		return &Invalid{Code: CodeDescriptionBad, Message: "The description is too long or is not text.",
			Detail: fmt.Sprintf("At most %d bytes of UTF-8.", MaxDescriptionLen)}
	}
	return nil
}
