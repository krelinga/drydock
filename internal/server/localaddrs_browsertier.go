//go:build browsertier

package server

import "net/netip"

// The browser tier's build (test/browser/harness.ts builds with
// -tags browsertier), and never a release's: deploy/package.sh passes no tags.
//
// That tier has no Docker. Its stand-in docker reports a preview's container
// at this host's own non-loopback address, where the tier's Vite listens,
// because no other address can reach a process here without a network
// namespace the tier cannot make. A real build refuses that address — it is
// the host's — so this build lists no local address, and only that: loopback,
// gateways and non-bridge networks are refused as in production, and the
// refusal itself is tested in internal/container and the container tier.
var previewLocalAddrs = func() ([]netip.Addr, error) { return nil, nil }

const browserTierBuild = true
