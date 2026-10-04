// Command drydock is the whole server: one binary, three subcommands.
//
//	drydock serve   [flags]   run the front door on its two Unix sockets
//	drydock passwd  [flags]   set the operator password
//	drydock version           print the release this binary was built from
//
// There is deliberately nothing else. In particular there is no flag that binds a
// TCP port (design §13.5), and no way to set the password except from a shell
// on the host (§13.2) — which removes the "unauthenticated bootstrap endpoint
// left enabled" class of bug by not having the endpoint.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/server"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

// version is the release this binary was built from, stamped at release time
// with -ldflags "-X main.version=v1.2.3". A local build says "dev".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(os.Args[2:]))
	case "passwd":
		os.Exit(passwd(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	case "version", "--version":
		fmt.Println(version)
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "drydock: unknown command %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  drydock serve   [flags]   run the front door on its two Unix sockets
  drydock passwd  [flags]   set the operator password (ends every session)
  drydock version           print the release this binary was built from

Run either with -h for its flags.
`)
}

func serve(args []string) int {
	cfg := config.Default()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&cfg.UIOrigin, "ui-origin", "", "the exact origin the UI is served from, e.g. https://drydock.example.com (required)")
	fs.StringVar(&cfg.UIHost, "ui-host", "", "the hostname the UI answers to (required)")
	fs.StringVar(&cfg.PreviewDomain, "preview-domain", "", "the separate registrable domain previews are served from")
	fs.StringVar(&cfg.APISocket, "api-socket", cfg.APISocket, "Unix socket for the UI and API")
	fs.StringVar(&cfg.PreviewSocket, "preview-socket", cfg.PreviewSocket, "Unix socket for previews")
	fs.StringVar(&cfg.SocketGroup, "socket-group", cfg.SocketGroup, "group owning both sockets; Caddy must be its only other member")
	fs.StringVar(&cfg.DatabasePath, "db", cfg.DatabasePath, "SQLite database path")
	fs.StringVar(&cfg.WorkspaceRoot, "workspace-root", cfg.WorkspaceRoot, "where clones live")
	fs.StringVar(&cfg.BrokerDir, "broker-dir", cfg.BrokerDir, "directory for the per-workspace token broker sockets (made 0700)")
	fs.StringVar(&cfg.LabelPrefix, "label-prefix", cfg.LabelPrefix, "workspace container label prefix (never shared with another Drydock)")
	fs.Int64Var(&cfg.GitHubAppID, "github-app-id", 0, "the GitHub App's numeric App ID (not its Client ID)")
	fs.StringVar(&cfg.GitHubAppKey, "github-app-key", "", "path of the GitHub App's private key, mode 0400 (never the key itself)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv, err := server.New(ctx, cfg, sys.Production())
	if err != nil {
		fmt.Fprintf(os.Stderr, "drydock serve: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "drydock: serving on %s and %s\n", cfg.APISocket, cfg.PreviewSocket)
	if err := srv.Serve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "drydock serve: %v\n", err)
		return 1
	}
	return 0
}

// passwd sets the operator password. On a terminal it asks twice without
// echo; otherwise it reads one line from stdin, so it can be scripted at
// install time. The password is never printed, logged, or put in argv — a
// password flag would put it in `ps` and the shell history.
func passwd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	dbPath := config.Default().DatabasePath
	fs := flag.NewFlagSet("passwd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&dbPath, "db", dbPath, "SQLite database path")
	ifUnset := fs.Bool("if-unset", false, "do nothing, without prompting, if a password is already set (for installers)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	// Without the instance lock: this must work while the server runs.
	db, err := store.OpenAdmin(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "drydock passwd: %v\n", err)
		return 1
	}
	defer db.Close()
	svc := auth.New(db.DB, sys.Production())

	// Checked before prompting, so a re-run of the installer never asks for a
	// password — and never ends every session as a side effect of upgrading.
	if *ifUnset {
		set, err := svc.HasPassword(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "drydock passwd: %v\n", err)
			return 1
		}
		if set {
			fmt.Fprintln(stdout, "A password is already set; leaving it unchanged.")
			return 0
		}
	}

	pw, err := readPassword(stdin, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "drydock passwd: %v\n", err)
		return 1
	}
	if err := svc.SetPassword(ctx, pw); err != nil {
		fmt.Fprintf(stderr, "drydock passwd: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Password set. Every existing session has been signed out.")
	return 0
}

func readPassword(stdin io.Reader, prompt io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(prompt, "New password: ")
		a, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		fmt.Fprint(prompt, "Again: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("the two entries did not match")
		}
		return string(a), nil
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", errors.New("no password on stdin")
	}
	// Strip only the line ending: a password may legitimately contain spaces.
	return strings.TrimRight(line, "\r\n"), nil
}
