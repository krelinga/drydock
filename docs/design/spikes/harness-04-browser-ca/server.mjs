// A stand-in for Drydock's two listeners, just real enough to test cookie
// semantics: one HTTPS listener serving two different registrable domains by
// SNI, which is what Caddy does in production.
//
// Usage: node server.mjs <cert-dir> <port>
//
// It records what actually arrived on every request — in particular whether a
// cookie was attached — because the assertions that matter are server-side.
// "The browser did not send the cookie" cannot be established from inside the
// page: a cross-site fetch that is blocked by CORS and one that was sent
// without credentials look identical to the caller.
import { createServer } from "node:https";
import { readFileSync } from "node:fs";
import { createSecureContext } from "node:tls";

const [certDir, portArg] = process.argv.slice(2);
const PORT = Number(portArg || 8443);
const UI_HOST = process.env.UI_HOST || "drydock.test";
const PREVIEW_DOMAIN = process.env.PREVIEW_DOMAIN || "drydock-preview.test";

const ctx = (name) =>
  createSecureContext({
    key: readFileSync(`${certDir}/${name}.key`),
    cert: readFileSync(`${certDir}/${name}.crt`),
  });
const uiCtx = ctx(UI_HOST);
const previewCtx = ctx(PREVIEW_DOMAIN);

const COOKIE = "__Host-drydock";
const SESSION = "s3cr3t-session-value";

// What the server saw. The test reads this back over the same listener.
const seen = [];
const record = (req, note) =>
  seen.push({
    note,
    host: req.headers.host,
    method: req.method,
    path: req.url,
    origin: req.headers.origin ?? null,
    // The single fact most assertions turn on.
    cookieSent: (req.headers.cookie ?? "").includes(`${COOKIE}=${SESSION}`),
    rawCookie: req.headers.cookie ?? null,
    secFetchSite: req.headers["sec-fetch-site"] ?? null,
  });

const html = (body) =>
  `<!doctype html><meta charset=utf-8><title>spike</title>${body}`;

const send = (res, code, type, body, headers = {}) => {
  res.writeHead(code, { "content-type": type, ...headers });
  res.end(body);
};

// With WRONG_CERT=1 the UI host is served the *preview* certificate, whose SAN
// does not cover it. Every trust route still trusts the issuing CA, so this
// isolates one question: does the route notice a certificate that is validly
// signed and wrong for the host it is serving?
const WRONG_CERT = process.env.WRONG_CERT === "1";

const server = createServer(
  {
    SNICallback: (name, cb) =>
      cb(null, name === UI_HOST && !WRONG_CERT ? uiCtx : previewCtx),
    // Default context for a connection with no SNI.
    key: readFileSync(`${certDir}/${UI_HOST}.key`),
    cert: readFileSync(`${certDir}/${UI_HOST}.crt`),
  },
  (req, res) => {
    const host = (req.headers.host || "").split(":")[0];
    const url = new URL(req.url, `https://${host}`);
    const isPreview = host.endsWith(PREVIEW_DOMAIN);
    const p = url.pathname;

    // ---- the preview origin: a different registrable domain ----------------
    if (isPreview) {
      record(req, `preview ${p}`);
      if (p === "/") {
        // A page on a hostile-ish origin, from which the test drives
        // cross-site requests at the UI origin.
        return send(res, 200, "text/html", html(`<h1>preview ${host}</h1>`));
      }
      return send(res, 404, "text/plain", "not found");
    }

    // ---- the UI origin -----------------------------------------------------
    switch (p) {
      case "/":
        record(req, "ui /");
        return send(res, 200, "text/html", html("<h1>drydock UI</h1>"));

      case "/signin":
        // The real thing: Secure, Path=/, HttpOnly, SameSite=Lax. Lax rather
        // than Strict because the cross-site authorize redirect has to carry
        // it — which is itself one of the assertions below.
        record(req, "ui /signin");
        return send(res, 204, "text/plain", "", {
          "set-cookie": `${COOKIE}=${SESSION}; Secure; Path=/; HttpOnly; SameSite=Lax`,
        });

      case "/variants":
        // One response offering four __Host- cookies, exactly one of which is
        // legal. The browser is the thing under test: it must keep #1 and
        // refuse the other three.
        record(req, "ui /variants");
        return send(res, 204, "text/plain", "", {
          "set-cookie": [
            `__Host-ok=1; Secure; Path=/`,
            `__Host-badpath=1; Secure; Path=/sub`, // Path must be exactly /
            `__Host-nosecure=1; Path=/`, // Secure is required
            `__Host-domain=1; Secure; Path=/; Domain=${UI_HOST}`, // Domain forbidden
          ],
        });

      case "/api/whoami":
        record(req, "ui GET /api/whoami");
        return send(
          res,
          200,
          "application/json",
          JSON.stringify({ signedIn: (req.headers.cookie ?? "").includes(COOKIE) }),
        );

      case "/api/mutate":
        // The route a cross-site POST would attack. No CORS headers, ever.
        record(req, "ui POST /api/mutate");
        return send(res, 202, "application/json", "{}");

      case "/authorize":
        // The preview authorize hop: a cross-site *top-level* GET navigation.
        // SameSite=Lax is supposed to carry the cookie here and nowhere else
        // cross-site, which is the whole reason the cookie is not Strict.
        record(req, "ui GET /authorize (top-level cross-site)");
        return send(res, 200, "text/html", html("<h1>authorize</h1>"));

      case "/_record":
        return send(res, 200, "application/json", JSON.stringify(seen, null, 2));

      case "/_reset":
        seen.length = 0;
        return send(res, 204, "text/plain", "");

      default:
        return send(res, 404, "text/plain", "not found");
    }
  },
);

server.listen(PORT, "127.0.0.1", () =>
  console.log(`listening on 127.0.0.1:${PORT} for ${UI_HOST} and *.${PREVIEW_DOMAIN}`),
);
