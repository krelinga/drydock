// Regenerate the PNG icons:
//
//   node docs/design/brand/render-icons.mjs '[
//     ["docs/design/brand/icons/apple-touch-icon-180.png", 180, "bleed", "steel"],
//     ["docs/design/brand/icons/favicon-32.png", 32, "badge", "steel"],
//     ["docs/design/brand/icons/favicon-16.png", 16, "badge", "steel"],
//     ["docs/design/brand/icons/github-app-200.png", 200, "bleed", "steel-lift"],
//     ["docs/design/brand/icons/github-app-dev-200.png", 200, "bleed", "steel-inverse"]
//   ]'
//
// Rasterise the Drydock mark straight from its geometry, so the PNG deliverables
// come from the same numbers as the SVGs rather than a second hand-drawn copy.
// If you change a path in icons/, change it here too and re-run — the two are
// not generated from one source, and nothing will warn you when they drift.
// Every shape in the mark is expressible as a signed distance, which also means
// the sample point can be transformed into mark space instead of scaling the
// geometry — the same trick the badge's stroke compensation describes.
import { deflateSync } from "node:zlib";
import { writeFileSync } from "node:fs";

/* ---------- signed distances, all in the 32-unit mark space ---------- */

const segDist = (px, py, ax, ay, bx, by) => {
  const vx = bx - ax, vy = by - ay, wx = px - ax, wy = py - ay;
  const t = Math.max(0, Math.min(1, (wx * vx + wy * vy) / (vx * vx + vy * vy)));
  return Math.hypot(wx - vx * t, wy - vy * t);
};

const roundRect = (px, py, x, y, w, h, r) => {
  const dx = Math.abs(px - (x + w / 2)) - (w / 2 - r);
  const dy = Math.abs(py - (y + h / 2)) - (h / 2 - r);
  return Math.hypot(Math.max(dx, 0), Math.max(dy, 0)) + Math.min(Math.max(dx, dy), 0) - r;
};

// the basin: three segments, round caps and joins
const BASIN = [[3.1, 8, 8.6, 24], [8.6, 24, 23.4, 24], [23.4, 24, 28.9, 8]];
const basin = (px, py) => Math.min(...BASIN.map((s) => segDist(px, py, ...s))) - 1;

// a stencilled D: stem, flat run, semicircular bowl, flat run back
const letter = (px, py, x0) => {
  const cx = x0 + 0.5, cy = 15.9, r = 2.2;
  const d = px - cx;
  const bowl = d >= 0
    ? Math.abs(Math.hypot(d, py - cy) - r)
    : Math.min(Math.hypot(px - cx, py - 13.7), Math.hypot(px - cx, py - 18.1));
  return Math.min(
    segDist(px, py, x0, 18.1, x0, 13.7),
    segDist(px, py, x0, 13.7, cx, 13.7),
    segDist(px, py, x0, 18.1, cx, 18.1),
    bowl) - 0.4;
};

const inMark = (px, py) =>
  basin(px, py) <= 0 ||
  roundRect(px, py, 12.6, 20.9, 2, 2.2, 0.4) <= 0 ||
  roundRect(px, py, 17.4, 20.9, 2, 2.2, 0.4) <= 0 ||
  roundRect(px, py, 11, 10.9, 10, 10, 1.6) <= 0;

const inLetters = (px, py) => letter(px, py, 12.5) <= 0 || letter(px, py, 16.8) <= 0;

/* ---------- PNG writer ---------- */

const CRC = (() => {
  const t = new Int32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c;
  }
  return (buf) => {
    let c = -1;
    for (const b of buf) c = t[(c ^ b) & 0xff] ^ (c >>> 8);
    return (c ^ -1) >>> 0;
  };
})();

const chunk = (type, data) => {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(CRC(body));
  return Buffer.concat([len, body, crc]);
};

function writePng(path, size, rgba) {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0);
  ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8;    // bit depth
  ihdr[9] = 6;    // RGBA
  const raw = Buffer.alloc(size * (size * 4 + 1));
  for (let y = 0; y < size; y++) {
    raw[y * (size * 4 + 1)] = 0;   // filter: none
    rgba.copy(raw, y * (size * 4 + 1) + 1, y * size * 4, (y + 1) * size * 4);
  }
  writeFileSync(path, Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", deflateSync(raw, { level: 9 })),
    chunk("IEND", Buffer.alloc(0)),
  ]));
}

/* ---------- compositing ---------- */

const rgb = (h) => [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16));
const SS = 6;   // supersampling per axis

// mode: "badge" (rounded square, transparent outside) | "bleed" (fills the square)
//       | "bare"  (the mark on an opaque page ground, no badge)
function render(size, mode, colors) {
  const out = Buffer.alloc(size * size * 4);
  const ground = rgb(colors.ground);
  const knock = rgb(colors.knock);
  const vessel = rgb(colors.vessel ?? colors.knock);
  const page = colors.page ? rgb(colors.page) : null;
  const scale = mode === "bare" ? 1 : 0.78;

  for (let py = 0; py < size; py++) {
    for (let px = 0; px < size; px++) {
      let r = 0, g = 0, b = 0, a = 0;
      for (let sy = 0; sy < SS; sy++) {
        for (let sx = 0; sx < SS; sx++) {
          // sample point in the 32-unit canvas
          const ux = ((px + (sx + 0.5) / SS) / size) * 32;
          const uy = ((py + (sy + 0.5) / SS) / size) * 32;

          let col = null;
          if (mode === "badge") {
            if (roundRect(ux, uy, 1, 1, 30, 30, 7.5) <= 0) col = ground;
          } else if (mode === "bleed") {
            col = ground;
          } else {
            col = page;
          }

          if (col) {
            // into mark space: undo the badge's scale about the centre
            const mx = (ux - 16) / scale + 16;
            const my = (uy - 16) / scale + 16;
            if (inMark(mx, my)) col = knock;
            if (roundRect(mx, my, 11, 10.9, 10, 10, 1.6) <= 0) col = vessel;
            if (inLetters(mx, my)) col = mode === "bare" ? rgb(colors.page) : ground;
            r += col[0]; g += col[1]; b += col[2]; a += 255;
          }
        }
      }
      const n = SS * SS, i = (py * size + px) * 4;
      out[i] = a ? Math.round(r / (a / 255)) : 0;
      out[i + 1] = a ? Math.round(g / (a / 255)) : 0;
      out[i + 2] = a ? Math.round(b / (a / 255)) : 0;
      out[i + 3] = Math.round(a / n);
    }
  }
  return out;
}

// "steel" is the badge's light value. A PNG cannot follow the page's theme, so
// one that GitHub shows on both a light and a dark page takes the lifted dark
// value instead, which holds an edge against both ("steel-lift"); the dev App's
// logo swaps ground and mark ("steel-inverse"). brand-design.md, "The GitHub
// App logo", has the numbers.
const PALETTES = {
  steel: { ground: "#475569", knock: "#f8fafc" },
  "steel-lift": { ground: "#64748b", knock: "#f8fafc" },
  "steel-inverse": { ground: "#f8fafc", knock: "#475569" },
  bare: { page: "#ffffff", ground: "#ffffff", knock: "#0f172a", vessel: "#1d4ed8" },
};
const MODES = ["badge", "bleed", "bare"];

for (const [path, size, mode, colors] of JSON.parse(process.argv[2])) {
  if (!MODES.includes(mode)) throw new Error(`unknown mode: ${mode}`);
  if (!PALETTES[colors]) throw new Error(`unknown colors: ${colors}`);
  writePng(path, size, render(size, mode, PALETTES[colors]));
}

console.log("rendered", JSON.parse(process.argv[2]).map((j) => j[0]).join(" "));
