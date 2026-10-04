// Spike 04 — can a headless Chromium be made to trust a locally-generated CA
// cleanly enough that `__Host-` cookie semantics are real?
//
// Usage: node spike.mjs <cert-dir> <port> <trust-mode>
//   trust-mode: nss | spki | none | ignore
//
// `none` and `ignore` are controls, not candidates. `none` proves the harness
// would notice an untrusted CA at all — without it, a passing run might mean
// Chromium is ignoring certificates for some other reason. `ignore` is the
// option the testing plan rules out (`ignoreHTTPSErrors`), measured so the
// ruling rests on an observation rather than an assumption.
import { chromium } from "@playwright/test";

const [certDir, portArg, trustMode = "nss"] = process.argv.slice(2);
const PORT = Number(portArg || 8443);
const UI_HOST = process.env.UI_HOST || "drydock.test";
const PREVIEW_HOST = `abc123.${process.env.PREVIEW_DOMAIN || "drydock-preview.test"}`;
const COOKIE = "__Host-drydock";

// The URLs carry no port: the origin must be the real default-port HTTPS origin
// or the site comparisons and `__Host-` rules are not the ones under test. The
// resolver rules below are what redirect the connection to the test listener.
const UI = `https://${UI_HOST}`;
const PREVIEW = `https://${PREVIEW_HOST}`;

const results = [];
const check = (name, pass, detail = "") =>
  results.push({ name, pass, detail });

const serverRecord = async (page) =>
  JSON.parse(
    await page.evaluate(
      async (u) => (await fetch(u, { credentials: "include" })).text(),
      `${UI}/_record`,
    ),
  );

const launch = async () => {
  const args = [
    // Wildcard resolution with a port, which /etc/hosts cannot express. The
    // port remap is what lets the test listener avoid binding 443 as root
    // while the page's origin stays port-less.
    `--host-resolver-rules=MAP ${UI_HOST} 127.0.0.1:${PORT},` +
      `MAP *.${process.env.PREVIEW_DOMAIN || "drydock-preview.test"} 127.0.0.1:${PORT}`,
  ];
  if (trustMode === "spki") {
    // The flag matches the SPKI of a certificate *in the presented chain*, so
    // the CA's own key is not enough — every leaf the listener serves has to be
    // listed. That is already the shape of its weakness as an approach: the
    // list has to be regenerated whenever a cert is, and it grows with the
    // number of hosts.
    const { execSync } = await import("node:child_process");
    const spkiOf = (name) =>
      execSync(
        `openssl x509 -in ${certDir}/${name}.crt -pubkey -noout | ` +
          `openssl pkey -pubin -outform der | openssl dgst -sha256 -binary | openssl enc -base64`,
        { shell: "/bin/bash" },
      )
        .toString()
        .trim();
    const list = ["ca", UI_HOST, process.env.PREVIEW_DOMAIN || "drydock-preview.test"]
      .map(spkiOf)
      .join(",");
    args.push(`--ignore-certificate-errors-spki-list=${list}`);
  }
  const browser = await chromium.launch({ args });
  const context = await browser.newContext(
    trustMode === "ignore" ? { ignoreHTTPSErrors: true } : {},
  );
  return { browser, context };
};

const { browser, context } = await launch();
const page = await context.newPage();

// ---- A. does the page load at all, over real HTTPS? -------------------------
let loadError = null;
try {
  const resp = await page.goto(`${UI}/`, { waitUntil: "load", timeout: 15000 });
  check("A1 UI origin loads over HTTPS", resp?.status() === 200, `status ${resp?.status()}`);
} catch (e) {
  loadError = String(e).split("\n")[0];
  check("A1 UI origin loads over HTTPS", false, loadError);
}

if (loadError) {
  // Nothing else can be measured; report and stop.
  console.log(JSON.stringify({ trustMode, results }, null, 2));
  await browser.close();
  process.exit(0);
}

// `__Host-` and `Secure` both require a trustworthy origin. If a trust hack
// leaves the origin non-secure, every cookie assertion below is meaningless —
// so this is asserted rather than assumed.
check(
  "A2 origin is a secure context",
  await page.evaluate(() => window.isSecureContext),
);

// ---- B. is a real __Host- cookie accepted and replayed? ---------------------
await page.evaluate(async (u) => {
  await fetch(u, { method: "POST", credentials: "include" });
}, `${UI}/signin`);

const cookies = await context.cookies(UI);
const host = cookies.find((c) => c.name === COOKIE);
check("B1 __Host- cookie accepted by the browser", !!host, host ? "" : "not stored");
check(
  "B2 stored with Secure, Path=/, HttpOnly",
  !!host && host.secure && host.path === "/" && host.httpOnly,
  host ? `secure=${host.secure} path=${host.path} httpOnly=${host.httpOnly}` : "",
);

await page.evaluate(async (u) => {
  await fetch(u, { credentials: "include" });
}, `${UI}/api/whoami`);
let rec = await serverRecord(page);
check(
  "B3 replayed on a same-origin request (seen at the server)",
  rec.some((r) => r.note === "ui GET /api/whoami" && r.cookieSent),
);

// ---- C. are illegal __Host- variants refused? -------------------------------
await page.evaluate(async (u) => {
  await fetch(u, { credentials: "include" });
}, `${UI}/variants`);
const names = (await context.cookies(UI)).map((c) => c.name);
check("C1 __Host-ok (Secure, Path=/) accepted", names.includes("__Host-ok"));
check("C2 __Host- with Path=/sub refused", !names.includes("__Host-badpath"));
check("C3 __Host- without Secure refused", !names.includes("__Host-nosecure"));
check("C4 __Host- with Domain= refused", !names.includes("__Host-domain"));

// ---- D/E/F. the cross-site boundary ----------------------------------------
await page.evaluate(async (u) => {
  await fetch(u, { method: "POST" });
}, `${UI}/_reset`);
await page.goto(`${PREVIEW}/`, { waitUntil: "load" });

check(
  "D0 preview origin is a different site",
  await page.evaluate(
    (ui) => new URL(ui).hostname.split(".").slice(-2).join(".") !==
      location.hostname.split(".").slice(-2).join("."),
    UI,
  ),
);

// A cross-site credentialed POST at a mutating route. `no-cors` is what an
// attacker page would actually use: the response is opaque, but the request is
// sent, so only the server can say whether a cookie rode along.
await page.evaluate(async (u) => {
  try {
    await fetch(u, { method: "POST", mode: "no-cors", credentials: "include" });
  } catch {}
}, `${UI}/api/mutate`);

// A cross-site credentialed GET of a data route.
await page.evaluate(async (u) => {
  try {
    await fetch(u, { mode: "no-cors", credentials: "include" });
  } catch {}
}, `${UI}/api/whoami`);

// A cross-site *top-level* navigation, by clicking a real link — not
// `page.goto`, which is browser-initiated and carries the cookie under Lax no
// matter what. This is the authorize hop, and the reason the cookie is Lax.
await page.evaluate((u) => {
  const a = document.createElement("a");
  a.href = u;
  a.textContent = "authorize";
  a.id = "go";
  document.body.append(a);
}, `${UI}/authorize`);
await Promise.all([page.waitForURL(`${UI}/authorize`), page.click("#go")]);

rec = await serverRecord(page);
const find = (note) => rec.find((r) => r.note === note);

const post = find("ui POST /api/mutate");
check(
  "D1 cross-site POST carries NO cookie",
  !!post && !post.cookieSent,
  post ? `sec-fetch-site=${post.secFetchSite} cookie=${post.rawCookie}` : "request never arrived",
);
check("D2 cross-site POST still reached the server", !!post,
  "the control: D1 would also pass if nothing was sent at all");

const get = find("ui GET /api/whoami");
check(
  "F1 cross-site GET carries NO cookie",
  !!get && !get.cookieSent,
  get ? `cookie=${get.rawCookie}` : "request never arrived",
);

const auth = find("ui GET /authorize (top-level cross-site)");
check(
  "E1 cross-site top-level navigation DOES carry the cookie (Lax)",
  !!auth && auth.cookieSent,
  auth ? `sec-fetch-site=${auth.secFetchSite}` : "navigation never arrived",
);

console.log(JSON.stringify({ trustMode, results }, null, 2));
await browser.close();
