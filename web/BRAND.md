# alcyona — Brand

A warm, calm, personal-assistant identity. The word *alcyona* evokes the
halcyon (kingfisher), the calm sea, and the Pleiades star Alcyone — so the
brand reads as **a soft, golden morning over still water**.

## Feel
Warm · inviting · calm · friendly. Never cold/corporate, never loud.

## Logo
- **Mark** — a soft sun (radial gradient, warm center → golden edge) over a
  single gentle wave. `web/static/logo-mark.svg` (no background — for the
  header, on any surface) and `web/static/favicon.svg` (the mark in a warm
  cream rounded square — for the browser tab / app icon).
- **Wordmark** — `alcyona` in **Nunito 800**, lowercase, tight tracking
  (`letter-spacing: -0.02em`), in `--text`. Rendered as HTML text next to the
  mark (not baked into the SVG), so it themes with the page.
- The mark stays legible down to ~16px (favicon). Don't recolor the sun; use
  the two gradient stops and the wave stroke `#DFA23A` as given.

## Color
Soft warm yellow is the brand color. It's a *fill* color (buttons, highlights,
logo) — for yellow as *text* on light surfaces use the darker `--brand-strong`
for contrast. Neutrals are warm (cream/taupe/espresso), not cool gray.

| Token | Light | Dark | Use |
|---|---|---|---|
| `--brand` | `#F2C23C` | `#F2C23C` | hero fills (buttons, logo, highlights) |
| `--brand-hover` | `#E7B62E` | `#FFD66B` | brand fill hover |
| `--brand-strong` | `#A8791C` | `#FFD66B` | brand as **text** (AA contrast per theme) |
| `--brand-soft` | `#FBE7A8` | `#3A3320` | soft yellow surface (user bubbles, focus ring) |
| `--brand-softer` | `#FFF3D1` | `#332C1C` | very soft yellow surface |
| `--brand-contrast` | `#33291C` | `#2A220F` | text/icon on brand fills |
| `--accent` | `#D97B4E` | `#F09A6E` | secondary warm terracotta (sparing) |
| `--bg` | `#FDF8EE` | `#1D1912` | page background (warm ivory / espresso) |
| `--surface` | `#FFFFFF` | `#26211A` | cards, inputs |
| `--surface-2` | `#FBF3E2` | `#2F2920` | panels, hover, muted fills |
| `--text` | `#33291C` | `#F4EDE0` | primary text (warm espresso / cream) |
| `--text-muted` | `#8A7B64` | `#AC9F88` | secondary text |
| `--border` | `#EBDFC7` | `#3A3327` | hairlines, borders |
| `--ok` / `--warn` / `--danger` | `#6E9E52` / `#D9971F` / `#C4552E` | `#8FC46A` / `#F0B840` / `#E8703F` | status |

## Typography
- **Family:** Nunito (variable, self-hosted at `web/static/fonts/nunito-variable.woff2`,
  weights 200–1000). Rounded terminals = the warm, friendly feel. No external
  font CDN — the app is fully self-contained/offline.
- Headings `Nunito 800`, `-0.01em` tracking. Emphasis `700`, body `400/500`.
- Body line-height `1.6` for a relaxed, calm read.
- Monospace: `--font-mono` (system stack) for code.

## Theming
CSS custom properties in `web/static/brand.css`:
- `:root` = **light**; `[data-theme="dark"]` = **dark**.
- Default follows the OS (`prefers-color-scheme`); a toggle sets
  `data-theme` on `<html>` and persists `localStorage["alcyona-theme"]`.
- Set the theme **before first paint** (small inline `<head>` script) to avoid
  a flash.

## Components (already styled in `brand.css`)
`.brand`, `.btn` / `.btn-primary` / `.btn-ghost`, inputs + focus ring,
`.card`, `.msg`/`.msg-user`/`.msg-assistant` (chat bubbles), `.chip`, `.swatch`,
`.theme-toggle`. Reference the tokens, not raw hex.

## Assets
```
web/static/logo-mark.svg      # mark (header)
web/static/favicon.svg        # mark in rounded square (tab / app icon)
web/static/brand.css          # tokens + light/dark + font + components
web/static/fonts/nunito-variable.woff2   # self-hosted font
web/static/index.html         # brand preview page (the reference)
```
All served from the embedded FS at the site root (e.g. `/brand.css`,
`/fonts/nunito-variable.woff2`, `/logo-mark.svg`).
