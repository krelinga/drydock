# Drydock brand mark — design note

**Status: draft v3. Basin settled (battered). Vessel undecided — three candidates.**

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

| File | Vessel | Letters read from | Cost |
| --- | --- | --- | --- |
| `drydock-mark-dd-twin.svg` | Two solid `D` blocks, one keel block each | ~24 px | The vessel stops looking like a container |
| `drydock-mark-dd-twin-outline.svg` | The same two `D`s, outlined | ~20 px | Reads a little like a diagram of the mark |
| `drydock-mark-dd-face.svg` | The container, `DD` stencilled on its face | ~48 px | Invisible below header size |
| `drydock-mark-battered.svg` | Plain container with a door seam | — | The v2 baseline, kept for comparison |

**Twin** is the recommendation. Two vessels in one dock is *true of the product* — a dock holds more
than one workspace — so the letters are earned by the subject rather than pasted onto it, and they sit
in the silhouette, which is the only thing that survives shrinking. **Stencil** is the one option where
the letters do something the subject genuinely does (containers carry their owner's mark stencilled on
the side) and it risks nothing at small sizes, but it is a header-size detail and nothing else.

The baseline is still the cleanest of the four. The letters buy a tie to the name that only shows above
24 px, and the twin options buy it by giving up the container silhouette. That trade is the decision.

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

## Palette

Taken from the design diagrams (`docs/design/*/diagrams/`) so the icon and the documentation are
visibly the same product. Nothing new was invented here.

| Role | Light | Dark |
| --- | --- | --- |
| Ink | `#0f172a` | `#e2e8f0` |
| Accent | `#1d4ed8` | `#60a5fa` |
| Ground | `#ffffff` | `#0b1220` |
| Badge ground | `#1d4ed8` | `#2563eb` |

The badge form keeps a blue ground in both themes and knocks the mark out in near-white, because a
favicon sits on browser chrome whose colour Drydock does not control.

## Still open

1. **Whether the DD is worth its cost at all.** The plain battered mark is still the cleanest of the
   four.
2. **If yes: twin or stencil.** Letters as the subject, or letters as markings on the subject.
3. **A wordmark instead.** The letters always read beside the mark and never inside it. A lockup — the
   battered mark plus "Drydock" set alongside — ties the name down completely and leaves the icon clean
   for the favicon. The UI header needs one regardless, so this is not a consolation prize.
4. **Whether the accent should be the blue at all.** It is the API colour in the architecture diagram,
   and the mark spends it on the vessel.
5. **The `>_` prompt payload** from v1 — the *session* rather than the *workspace* — is still
   unresolved and now competes with the letters for the same surface.

## Once a vessel is chosen

1. Redraw the **badge** (accent rounded square, mark knocked out) and the **one-colour `currentColor`
   form** on the winning geometry. Both existed in v1 and were dropped when the stepped basin was.
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
