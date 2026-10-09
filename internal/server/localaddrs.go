//go:build !browsertier

package server

import "net/netip"

// previewLocalAddrs is the container manager's LocalAddrs: nil, so Address
// asks the kernel for this host's addresses at every resolution and never
// returns one of them as a container's (PF §10.6). Only the browser tier's
// build (localaddrs_browsertier.go) differs.
var previewLocalAddrs func() ([]netip.Addr, error)

const browserTierBuild = false
