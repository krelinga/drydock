# Drydock brand mark — design note

**Status: draft v4. Mark settled — battered basin, container, `DD` stencilled on its face. Badge
ground undecided — three candidates.**

This note covers one asset: the Drydock icon. It exists because Phase 1 ships a LAN-facing web UI that
needs a favicon and a header mark, and because `claude.ai/code?environment=<id>` sends people back and
forth between the Claude app and Drydock — the tab has to be identifiable at 16 px.

## The idea the mark encodes

A dry dock is a basin you float a ship into and then **drain**. The water leaves, the hull settles onto
keel blocks, and work that is impossible afloat becomes ordinary: you can walk under the thing and
change it. The dock is not the ship and it does not sail — it is the prepared, isolated, drained place
where the ship is worked on.

That is exactly what Drydock is to a repo. A workspace is a clone on host disk, a container brought up
around it, and an agent inside with scoped credentials — a prepared place where work on the repo
happens, separate from the repo itself. The container survives nothing; the clone survives rebuild and
delete (§1 of the overall design). The dock persists, the vessel comes and goes.

So the mark is a **section through a drained basin** with a **vessel resting clear of the floor on keel
blocks**. The gap under the vessel is the whole point of a dry dock, and it is the single detail that
stops the mark reading as a generic "box in brackets". It is fixed at 2.1 units in every variant and
nothing is allowed to spend it.

Rejected, with reasons worth keeping:

- **A whale, or anything Docker-adjacent.** Drydock shells out to the `devcontainer` CLI and treats
  Docker as an implementation detail it never scrapes. Borrowing Docker's animal would claim a
  relationship the design explicitly refuses.
- **An anchor, a wheel, a porthole.** Nautical decoration with no mapping to anything in the system.
  An anchor is for holding a vessel in place at sea; it is the opposite of a dry dock.
- **A waterline.** Every version read as "harbour" rather than "dry dock". The drained basin is the
  distinguishing feature, and drawing water in it destroys the only idea the mark carries.

## The basin is settled

| Draft | Basin | Outcome |
| --- | --- | --- |
| v1 | Stepped walls, one altar step per side | Rejected: too many right angles |
| v2 | Battered / cradle / flared | **Battered chosen** |

**Battered** is straight walls battered inward to a flat floor, joining it at 109° — no right angle in
the mark. Path, shared by every candidate and no longer up for discussion:

```
M3.1 8 8.6 24H23.4L28.9 8
```

Worth recording what the v1 steps were doing, because the current mark no longer has it: they read as
*masonry* — cut, built, load-bearing — and they were the detail that made the enclosure say *dock*
rather than bowl or bin. The battered walls keep most of that (battered walls are what real dock walls
do) without a square corner. The curved alternatives gave it up entirely, which is part of why they
lost; they are in git history if that reading is ever wanted back.

## The "DD" question

The brief for v3: work a **DD** into the mark to tie it to the name. There is a hard physical limit
worth stating before the options. **At 16 px the whole mark is 16 pixels tall and the vessel is about
five of them.** No letterform survives that. So the question is not whether the icon can say DD — it is
*where the letters live, and what size they start reading at.*

A `D` reads as a `D` at a width-to-height ratio of roughly 0.6–0.8. Below that it reads as a leaf or a
lens. That single number decides where letters can go:

| Placement | Letter box | w : h | Verdict |
| --- | --- | --- | --- |
| Basin wall, bowl on the full wall | 5.4 × 16 | 0.34 | Reads as a leaf |
| Basin wall, bowl on the top half | 4.5 × 8 | 0.56 | A `D` with a leg — closer to a thorn |
| Basin wall, flatter batter for room | 3.2 × 8 | 0.40 | Worse, and it narrows the whole mark |
| **Vessel as letters** | 5.55 × 8 | **0.69** | Reads as a `D` |
| **Stencilled on the vessel's face** | 3.5 × 5.2 | **0.67** | Reads as a `D` |

### Why the walls can't be the letters

Letters in the basin walls are the only version that would survive to 16 px, because the walls *are*
the silhouette. It was the first thing tried. It fails on two structural counts, neither a matter of
taste:

1. **The proportions are wrong.** The wall is 16 units tall and only about 5 units of width are
   available for a bowl between the wall and the vessel — 0.34. Attaching the bowl to the top half only
   reaches 0.56 and leaves the wall continuing below as a leg, which turns the `D` into a thorn.
   Flattening the batter to free width makes the ratio worse *and* narrows the mark.
2. **One of them is always mirrored.** A symmetric basin needs its two walls mirrored, and a mirrored
   `D` is not a `D`. Making both face the same way means an asymmetric dock with one wall shelved
   inward and the other bulging outward, which stops reading as a basin at all.

`icon-preview.html` zone D renders the attempt so this is checkable rather than asserted.

## Candidates

All in `icons/`, each a single self-adapting SVG: a `<style>` block defines the light palette on `svg`
and overrides it under `prefers-color-scheme: dark`, and every element also carries its light value as
a presentation attribute, so a renderer that strips `<style>` still gets the light mark rather than a
black silhouette.

| File | What it is |
| --- | --- |
| `drydock-mark-dd-face.svg` | **The mark.** Battered basin, container, `DD` stencilled on its face |
| `drydock-mark-battered.svg` | The same mark without letters — a door seam instead. Kept for comparison |
| `drydock-badge-teal.svg` | The mark knocked out of a teal rounded square |
| `drydock-badge-blue.svg` | The same, accent blue |
| `drydock-badge-steel.svg` | The same, steel grey |

**Stencil won.** The letters do something the subject genuinely does — containers carry their owner's
mark stencilled on the side — and the silhouette is untouched, so nothing is risked at small sizes. The
price, paid knowingly, is that the letters are a header-size detail: by 32 px they are a texture and by
20 px they are gone.

Two rejected alternatives are in git history: the vessel drawn *as* two `D` blocks (letters in the
silhouette, reading from ~24 px, but the vessel stops looking like a container), and the same pair
outlined.

## Construction

- **32-unit `viewBox`**, 2 units of clear space on every side. Everything is placed on halves of a unit
  so a 16 px render lands on pixel edges rather than straddling them.
- **Stroke 2 units** on the basin, round caps and joins — 1 px at a 16 px render.
- **The letter `D`** is a stem, a flat run, then a semicircular bowl whose radius is exactly half the
  letter height. Making the bowl a true half-circle means the letter cannot go lopsided when it is
  resized. Solid versions carry their counter as a second subpath with `fill-rule="evenodd"`.
- **Twin, solid:** letters 5.55 × 8 at x 10 and 16.45; bowl radius 4, counter radius 2.2; gap 0.9;
  1.33 clear of the basin stroke.
- **Twin, outlined:** the same outer boxes once the 1.5-unit stroke is counted — which is why the path
  rect is 3.93 wide rather than 5.43. The stroke has to be paid for out of the letter or the two
  letters collide; the first attempt at the outlined pair overlapped by 0.1 units for exactly this
  reason.
- **Stencil:** letters 3.5 × 5.2 behind a 0.8-unit stroke, 1.1 clear of the container's edge, centred
  on the container's own centre at y 15.9.
- **Keel blocks** move for the twin variants: one block centred under each letter (x 11.8 and 18.2
  solid, 11.7 and 18.3 outlined) rather than two under one container. The stencil keeps the original
  12.6 and 17.4.
- **Air gap** is 2.1 units in every variant: every vessel's underside sits at y 20.9 and the floor
  stroke's top edge at y 23.
- **Symmetry** is checked, not eyeballed: every letter pair and block pair sums to 32 across the mark.

Two earlier measured near-misses, kept because both are easy to reintroduce:

- A round cap reaches a full unit past its endpoint in every direction, so **lip positions are set by
  the cap, not the path**. This is why the battered lips sit at x 3.1 rather than 3.5.
- The v2 curved basins needed their horizontal control arm at x 5.5. At the first-draft 8.5 the curve
  rose into the keel-block zone, cutting side clearance to 0.99 and the air gap to 1.71 — the curve was
  quietly closing the gap the whole mark depends on.

## The badge ground

A badge brings its own ground, which is the whole reason to have one: it stops depending on a page
whose colour Drydock does not control. But the badge still *sits* on that page, and if its ground is
close in value to the surface behind it, the rounded square dissolves and you are left with a floating
mark and no badge.

So the ground has to hold an edge against **four** surfaces, not two: a white page, a light tab strip,
a near-black page, and a dark tab strip. Measuring that kills the obvious answer — **a dark navy
badge.** Drydock's own ink scores **1.05** against a dark page, which is to say it is the same colour.

| Ground | White page | Light strip | Dark page | Dark strip | Worst | Verdict |
| --- | --- | --- | --- | --- | --- | --- |
| Dock teal `#0f766e` → `#0d9488` | 5.47 | 5.00 | 5.00 | 4.31 | **4.31** | Best of the three |
| Accent blue `#1d4ed8` → `#2563eb` | 6.70 | 6.12 | 3.62 | 3.12 | **3.12** | Passes |
| Steel `#475569` → `#64748b` | 7.58 | 6.92 | 3.93 | 3.39 | **3.39** | Passes |
| Violet `#6d28d9` | 7.10 | 6.49 | 2.64 | 2.27 | 2.27 | Thin, and spoken for |
| Graphite `#334155` | 10.35 | 9.45 | 1.81 | 1.56 | 1.56 | No edge on dark |
| Slate 800 `#1e293b` | 14.63 | 13.35 | 1.28 | 1.10 | 1.10 | Invisible on dark |
| Ink navy `#0f172a` | 17.85 | 16.30 | 1.05 | 1.11 | 1.05 | Invisible on dark |

The grounds that work on *both* are mid-value and saturated: dark enough to hold white out of them,
light enough to separate from a near-black page. Each gets a small luminance lift in dark mode so the
dark side is not just the light value, worse.

**Teal is the recommendation**, for a reason beyond the numbers. In the diagrams `#1d4ed8` already
*means* API traffic and violet means preview origin — a blue badge makes the brand colour and a
semantic colour the same colour. Teal keeps them separate and leaves the accent free to go on meaning
what it means. It is marine without being an anchor. The cost is a new hue in the palette, and teal
will date faster than the blue. Steel is the honest third option (a dock *is* a grey structure) but a
grey badge in a tab strip reads as a page that has not finished loading.

**The badge is single-knockout, and that was forced by measurement, not taste.** Keeping the accent
container and knocking the letters out in the badge ground gives a contrast of **1.00** on the blue
badge — the letters and the ground are then literally the same colour — and 1.82 even with a lifted
blue. So on a badge the container has to be the near-white and the letters the ground. One consequence:
the basin, keel blocks and container are all the same near-white, and because the blocks touch both the
container and the floor they merge into one mass. The air gap survives as three holes of ground colour
under the vessel.

If that flattening matters, exactly one two-tone survives: drop the **basin** to `#93c5fd` and keep the
vessel at full knockout. Every paler tint (blue-200, slate-300, teal-200) lands within 1.4 of white,
too close to separate from the vessel; `#93c5fd` sits at 1.72 against the vessel and 3.0–4.2 against the
grounds. It must be a fixed value rather than a themed one, or the hierarchy inverts between themes.

## Palette

Taken from the design diagrams (`docs/design/*/diagrams/`) so the icon and the documentation are
visibly the same product; the badge ground is the one addition and the reasoning is above.

| Role | Light | Dark |
| --- | --- | --- |
| Ink | `#0f172a` | `#e2e8f0` |
| Accent | `#1d4ed8` | `#60a5fa` |
| Ground | `#ffffff` | `#0b1220` |
| Badge ground | `#0f766e` | `#0d9488` |
| Badge knockout | `#f8fafc` | `#f8fafc` |
| Basin tint, if two-tone | `#93c5fd` | `#93c5fd` |

The knockout is a hair off pure white so it does not vibrate against a saturated ground.

## Badge construction

- **Badge:** `rect x=1 y=1 w=30 h=30 rx=7.5` — one unit of bleed inside the 32-unit box on every side,
  so the badge has room to be its own shape rather than running to the edge. 25% radius; 20% reads as a
  tile and holds up best at 16 px, 30% starts eating the mark's clear space at the corners. iOS masks
  the apple-touch icon to its own shape regardless, so this radius governs the favicon and direct SVG
  use only.
- **Mark inside:** scaled `0.78` about the centre. The mark's ink is 27.8 × 18 units, so width is the
  binding constraint; 0.78 puts its edges at x 5.16 and 26.84 — 4.16 units of padding, about 14%, in
  line with how platforms pad app icons.
- **Stroke compensation:** every stroke is divided by the scale so the knockout keeps the weight it has
  at full size — the basin's 2 units become `2.56`, the stencil's 0.8 becomes `1.03`. Skipping this is
  the usual way a scaled-down mark comes out visibly thinner than its standalone version.

## Still open

1. **Which badge ground.** Teal on the numbers and on keeping the accent free; blue to avoid
   introducing a hue; steel if the mark should not be branded at all.
2. **Flat or two-tone.** Flat is one colour and no decisions. Two-tone restores the dock-versus-vessel
   hierarchy the single knockout flattens.
3. **Corner radius.** 25% unless the squarer tile is wanted.
4. **A wordmark.** The letters always read beside the mark and never inside it. A lockup — the mark plus
   "Drydock" set alongside — ties the name down completely and leaves the icon clean for the favicon.
   The UI header needs one regardless, so this is not a consolation prize.
5. **The `>_` prompt payload** from v1 — the *session* rather than the *workspace* — is still
   unresolved and competes with the letters for the same surface.

## Once a ground is chosen

1. Redraw the **one-colour `currentColor` form** on the final geometry, for buttons, disabled states,
   print and one-bit favicons. It existed in v1 and was dropped when the stepped basin was.
2. Cut the light/dark SVG pair under `icons/` as `NN-name-light.svg` / `NN-name-dark.svg`, matching the
   diagram convention, for embedding in Markdown through `<picture>`. The self-adapting single file
   stays — it is what the UI and the favicon link to.
3. Add the favicon sizes the UI serves: an SVG favicon plus a 180 px `apple-touch-icon` PNG, both out of
   the badge.
4. Check the mark against Caddy's TLS padlock and the Claude app's own icon in a tab strip — those are
   the two icons it will sit beside in practice.

`icon-preview.html` renders every candidate on both grounds down to 16 px and renders the rejected
walls-as-letters attempt beside its numbers. Re-render it after any geometry change; it is what has
caught every measured problem so far.

Accessibility: every file carries `role="img"` with `<title>`/`<desc>` referenced from
`aria-labelledby`. Inline uses in the UI should set `aria-hidden="true"` when adjacent text already
names the product, so the mark is not announced twice.
