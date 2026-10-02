# Drydock brand mark — design note

**Status: draft v1, three candidates. Nothing is chosen yet.**

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

So the mark is a **section through a drained basin** — two stepped walls and a floor — with a
**shipping container** resting clear of the floor on **two keel blocks**. The container is doing double
duty, and deliberately: it is the vessel in the dock, and it is the dev container. The gap under it is
the whole point of a dry dock, and it is the single detail that stops the mark reading as a generic
"box in brackets".

Rejected, with reasons worth keeping:

- **A whale, or anything Docker-adjacent.** Drydock shells out to the `devcontainer` CLI and treats
  Docker as an implementation detail it never scrapes. Borrowing Docker's animal would claim a
  relationship the design explicitly refuses.
- **An anchor, a wheel, a porthole.** Nautical decoration with no mapping to anything in the system.
  An anchor is for holding a vessel in place at sea; it is the opposite of a dry dock.
- **A `D` monogram.** Survives 16 px fine, says nothing, and there are a great many `D`s.
- **A waterline.** Every version read as "harbour" rather than "dry dock". The drained basin is the
  distinguishing feature, and drawing water in it destroys the only idea the mark carries.

## Candidates

All four files are in `icons/`. Each is a single self-adapting SVG: a `<style>` block defines the light
palette on `svg` and overrides it under `prefers-color-scheme: dark`, and every element also carries its
light value as a presentation attribute, so a renderer that strips `<style>` still gets the light mark
rather than a black silhouette.

| File | What it is | Where it is for |
| --- | --- | --- |
| `drydock-mark-dock.svg` | Basin in ink, container as a solid accent block with a door seam | The default. Header mark, favicon, README |
| `drydock-mark-prompt.svg` | Same basin, container outlined in accent with a `>_` prompt on its face | If we want the mark to say "an agent is running in there" |
| `drydock-badge.svg` | Accent rounded square, basin and container knocked out of it | App icon, PWA, anywhere the mark needs its own ground |
| `drydock-mark-mono.svg` | One colour, `currentColor`, container outlined | Buttons, disabled states, print, one-bit favicons |

The real choice between the first two is what the vessel should be: the **workspace** (solid block —
calmer, survives 16 px with room to spare) or the **session** (prompt — says more, and the glyph is the
first thing to mush when the icon gets small). The badge is not an alternative to either; it is the
packaged form of whichever wins.

## Construction

- **32-unit `viewBox`**, 2 units of clear space on every side. Everything is placed on halves of a unit
  so a 16 px render lands on pixel edges rather than straddling them.
- **Basin profile**, shared by every variant, one path:
  `M3.5 8V15.5h5V24h15v-8.5h5V8`. One altar step per wall — real docks have several, and at 16 px more
  than one step per side closes up into a smudge.
- **Stroke 2 units**, round caps and joins — 1 px at a 16 px render.
- **Keel blocks** are 2 × 2.2 rects at x 12.6 and 17.4, overlapping the floor stroke by a tenth of a
  unit so no hairline seam appears between block and floor at fractional zoom.
- **Every variant's container occupies the same 10 × 10 footprint**, outer edges at x 11 and 21. That
  leaves 1.5 units between the container and the inner face of each basin wall. The outlined variants
  have to shrink their *path* rect to 8.4 units to keep that footprint once their stroke is counted;
  the first draft of the prompt variant did not, left 0.5 units of clearance, and the container visibly
  touched the wall below 24 px.
- The badge scales the mark by `0.76` about the centre and thickens its stroke to `2.6` to compensate,
  so the knockout weight matches the standalone mark.

## Palette

Taken from the design diagrams (`docs/design/*/diagrams/`) so the icon and the documentation are
visibly the same product. Nothing new was invented here.

| Role | Light | Dark |
| --- | --- | --- |
| Ink | `#0f172a` | `#e2e8f0` |
| Accent | `#1d4ed8` | `#60a5fa` |
| Ground | `#ffffff` | `#0b1220` |
| Badge ground | `#1d4ed8` | `#2563eb` |

The badge keeps a blue ground in both themes and knocks the mark out in near-white, because a favicon
sits on browser chrome whose colour Drydock does not control.

## Once a candidate is chosen

1. Cut the light/dark SVG pair under `icons/` as `NN-name-light.svg` / `NN-name-dark.svg`, matching the
   diagram convention, for embedding in Markdown through `<picture>`. The self-adapting single file
   stays — it is what the UI and the favicon link to.
2. Add the favicon sizes the UI serves. An SVG favicon plus a 180 px `apple-touch-icon` PNG covers
   everything; both come out of `drydock-badge.svg`.
3. Check the mark against Caddy's TLS padlock and the Claude app's own icon in a tab strip — those are
   the two icons it will sit beside in practice.

Accessibility: every file carries `role="img"` with `<title>`/`<desc>` referenced from
`aria-labelledby`. Inline uses in the UI should set `aria-hidden="true"` when adjacent text already
names the product, so the mark is not announced twice.
