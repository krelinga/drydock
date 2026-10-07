package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/krelinga/drydock/internal/config"
)

// checkPreviewDomain exits 0 when the preview domain is cross-site with the UI
// host (a different registrable domain, PF §4), and 1 with the reason on
// stderr when it is not. It is the installer's question, asked of the staged
// binary before anything is installed, so the shell never reimplements the
// Public Suffix List and the installer and `serve` cannot disagree: both run
// config.CrossSite.
func checkPreviewDomain(args []string, stderr io.Writer) int {
	var uiHost, preview string
	fs := flag.NewFlagSet("check-preview-domain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&uiHost, "ui-host", "", "the UI's hostname")
	fs.StringVar(&preview, "preview-domain", "", "the preview domain to check against it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "drydock check-preview-domain: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if uiHost == "" || preview == "" {
		fmt.Fprintln(stderr, "drydock check-preview-domain: --ui-host and --preview-domain are both required")
		return 2
	}
	if err := config.CrossSite(uiHost, preview); err != nil {
		fmt.Fprintf(stderr, "drydock check-preview-domain: %v\n", err)
		return 1
	}
	return 0
}
