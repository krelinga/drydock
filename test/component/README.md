# test/component — real binaries, nothing mocked

Today: the Caddyfile conformance test (testing §3.2), mutation-checked against `deploy/Caddyfile`
and `deploy/preview.caddy`:

- no path routes across the two sockets;
- the preview wildcard matches one label;
- routing is by `Host` across SNI (`strict_sni_host` stays off: HTTP/2 coalescing across preview
  slugs would turn into `421`s, PF §9);
- the preview cookie and query pass through;
- no access log holds a token;
- nothing on `:2019` (Caddy's admin API is on a `0600` Unix socket).

It needs `caddy` on `PATH`; without it the test skips unless `DRYDOCK_REQUIRE_CADDY` is set.
