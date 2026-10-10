package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/krelinga/drydock/internal/config"
)

// checkVSCodeSSHHost exits 0 when the value is one `serve --vscode-ssh-host`
// accepts, and 1 with the reason on stderr when it is not. It is the
// installer's question, asked of the staged binary before anything is
// installed, so a value it would write into drydock.env never stops the
// service from starting, and the shell never carries a second copy of the
// rule: both run config.ParseSSHHost.
func checkVSCodeSSHHost(args []string, stderr io.Writer) int {
	var host string
	fs := flag.NewFlagSet("check-vscode-ssh-host", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&host, "vscode-ssh-host", "", "the [user@]host[:port] to check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "drydock check-vscode-ssh-host: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if host == "" {
		fmt.Fprintln(stderr, "drydock check-vscode-ssh-host: --vscode-ssh-host is required")
		return 2
	}
	if _, err := config.ParseSSHHost(host); err != nil {
		fmt.Fprintf(stderr, "drydock check-vscode-ssh-host: %v\n", err)
		return 1
	}
	return 0
}
