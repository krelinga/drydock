# Drydock brand mark — design note

**Status: draft v2, three candidates. Nothing is chosen yet.**

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

So the mark is a **section through a drained basin** with a **shipping container** resting clear of the
floor on **two keel blocks**. The container is doing double duty, and deliberately: it is the vessel in
the dock, and it is the dev container. The gap under it is the whole point of a dry dock, and it is the
single detail that stops the mark reading as a generic "box in brackets" — so nothing may crowd it, and
every candidate is measured on whether it keeps that gap open.

Rejected, with reasons worth keeping:

- **A whale, or anything Docker-adjacent.** Drydock shells out to the `devcontainer` CLI and treats
  Docker as an implementation detail it never scrapes. Borrowing Docker's animal would claim a
  relationship the design explicitly refuses.
- **An anchor, a wheel, a porthole.** Nautical decoration with no mapping to anything in the system.
  An anchor is for holding a vessel in place at sea; it is the opposite of a dry dock.
- **A `D` monogram.** Survives 16 px fine, says nothing, and there are a great many `D`s.
- **A waterline.** Every version read as "harbour" rather than "dry dock". The drained basin is the
  distinguishing feature, and drawing water in it destroys the only idea the mark carries.

## What changed in v2

Draft v1 drew the basin as **stepped walls** — one altar step per side, all corners square. That was
rejected: too many right angles. Worth recording what the steps were doing, so the replacement is
judged against it rather than just preferred for being new. The steps read as *masonry* — a cut,
built, load-bearing structure — and they were the detail that made the enclosure say "dock" instead of
"bowl", "bin", or "basket". Any curved basin gives that up and leans harder on the keel blocks to carry
the dock reading, since nothing else in the mark is specific to a dock.

All three v2 candidates therefore keep the container and keel blocks byte for byte and change only the
basin, and **none of them contains a right angle**.

## Candidates

All three files are in `icons/`. Each is a single self-adapting SVG: a `<style>` block defines the light
palette on `svg` and overrides it under `prefers-color-scheme: dark`, and every element also carries its
light value as a presentation attribute, so a renderer that strips `<style>` still gets the light mark
rather than a black silhouette.

| File | Basin | Reads as |
| --- | --- | --- |
| `drydock-mark-battered.svg` | Straight walls battered inward to a flat floor; the wall/floor join is 109° | Excavated and built. Closest in feel to the stepped version without a square corner in it |
| `drydock-mark-cradle.svg` | One continuous curve, vertical at the lips, flat-tangent under the keel | Calm, and the most obviously "held". Softest of the three |
| `drydock-mark-flared.svg` | One curve whose walls lean 8° open at the lip | Open to receive. More motion; the lips read as a mouth rather than a wall |

The battered variant is the only one that keeps a genuinely flat floor, which matters more than it
looks: a flat floor is what keel blocks stand on, and it holds the air gap under the container at a
constant 2.1 units all the way across. The curved pair taper to 1.89 at the edge of the container's
underside — close enough to read the same, but they get there by being tuned rather than by the form
wanting it.

## Construction

- **32-unit `viewBox`**, 2 units of clear space on every side. Everything is placed on halves of a unit
  so a 16 px render lands on pixel edges rather than straddling them.
- **Stroke 2 units**, round caps and joins — 1 px at a 16 px render.
- **Every variant's container occupies the same 10 × 10 footprint**, outer edges at x 11 and 21, with a
  1.6-unit corner radius. **Keel blocks** are 2 × 2.2 rects at x 12.6 and 17.4, overlapping the floor
  stroke so no hairline seam opens up between block and floor at fractional zoom.
- **Basin paths**, each exactly mirror-symmetric about x = 16 by construction:

  | Variant | Path |
  | --- | --- |
  | Battered | `M3.1 8 8.6 24H23.4L28.9 8` |
  | Cradle | `M3.5 8C3.5 19 5.5 24 16 24s12.5-5 12.5-16` |
  | Flared | `M3 8C4.6 19 5.5 24 16 24s11.4-5 13-16` |

  Both curved paths use the `s` shorthand for the second half, which reflects the previous control
  point — so the right wall is the mirror of the left for free, and editing one lip cannot desynchronise
  the other.

Two numbers were found by measurement rather than by eye, and both are easy to break by nudging a
control point:

- **The curved basins' horizontal control arm sits at x 5.5.** At the first-draft value of 8.5 the
  curve rose into the keel-block zone: side clearance to the container fell to 0.99 units and the air
  gap under its underside to 1.71. Pulling the arm to 5.5 flattens the floor under the keel and
  restores 1.46 of side clearance with a 1.89–2.10 gap.
- **The lip x-positions are set by the round cap, not the path.** A round cap extends a full unit past
  the endpoint in every direction, so the leftmost ink is roughly one unit left of the lip. The flared
  variant starts at x 3.0 rather than 3.5 precisely because its lip leans 8° outward, which spends the
  margin the clear-space band would otherwise keep.

Earlier drafts of the outlined container also had to shrink their *path* rect to 8.4 units to hold the
10 × 10 footprint once the stroke was counted; the first attempt did not, left 0.5 units of clearance,
and the container visibly touched the wall below 24 px.

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

1. **Which basin.** Battered, cradle, or flared.
2. **What the vessel should be.** v1 also offered the container with a `>_` prompt on its face — the
   *session* rather than the *workspace*. That question is unresolved and orthogonal to the basin; the
   prompt payload will be redrawn on whichever basin wins rather than kept alive on a rejected one.
3. **Whether the door seam earns its place.** It is the one interior detail, it is the first thing to
   vanish as the icon shrinks, and the mark is fine without it.
4. **Whether the accent should be the blue at all.** It is the API colour in the architecture diagram,
   and the mark currently spends it on the container.

## Once a basin and a payload are chosen

1. Redraw the **badge** (accent rounded square, mark knocked out) and the **one-colour `currentColor`
   form** on the winning geometry. Both existed in v1 and were dropped when the stepped basin was, since
   both embedded it.
2. Cut the light/dark SVG pair under `icons/` as `NN-name-light.svg` / `NN-name-dark.svg`, matching the
   diagram convention, for embedding in Markdown through `<picture>`. The self-adapting single file
   stays — it is what the UI and the favicon link to.
3. Add the favicon sizes the UI serves: an SVG favicon plus a 180 px `apple-touch-icon` PNG, both out of
   the badge.
4. Check the mark against Caddy's TLS padlock and the Claude app's own icon in a tab strip — those are
   the two icons it will sit beside in practice.

`icon-preview.html` renders every candidate on both grounds down to 16 px, over its construction grid.
Re-render it after any geometry change; it is what caught both measured problems above.

Accessibility: every file carries `role="img"` with `<title>`/`<desc>` referenced from
`aria-labelledby`. Inline uses in the UI should set `aria-hidden="true"` when adjacent text already
names the product, so the mark is not announced twice.
