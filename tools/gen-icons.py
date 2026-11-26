#!/usr/bin/env python3
"""Render the bevin app-icon set from the favicon sun motif.

Outputs (into web/static):
  icon-192.png          favicon.svg as-is (rounded-square mark), 192px
  icon-512.png          favicon.svg as-is, 512px
  icon-maskable-512.png full-bleed cream square, sun + waves scaled into the
                        80% maskable safe zone (no ring, no rounded corners), 512px
  apple-touch-icon.png  the same full-bleed variant at 180px (iOS masks it;
                        it must be opaque — no transparency)
"""
import cairosvg

STATIC = "web/static"

FAVICON = f"""<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" role="img" aria-label="bevin">
  <defs>
    <radialGradient id="favSun" cx="40%" cy="36%" r="68%">
      <stop offset="0%" stop-color="#FFF3C9"/>
      <stop offset="55%" stop-color="#F8CE55"/>
      <stop offset="100%" stop-color="#ECB429"/>
    </radialGradient>
    <clipPath id="favFrame"><rect x="2" y="2" width="60" height="60" rx="16"/></clipPath>
  </defs>
  <rect x="2" y="2" width="60" height="60" rx="16" fill="#FFF3D2"/>
  <g clip-path="url(#favFrame)">
    <circle cx="31" cy="23" r="12" fill="url(#favSun)"/>
    <g fill="none" stroke="#DFA23A" stroke-linecap="round">
      <path d="M 15 41 Q 22 38.5 29 41 T 45 41" stroke-width="3" opacity="0.45"/>
      <path d="M 12 47 Q 20 44 28 47 T 48 47" stroke-width="3.5" opacity="0.7"/>
      <path d="M 10 53 Q 17 49.5 26 53 T 50 53" stroke-width="4" opacity="0.95"/>
    </g>
  </g>
  <rect x="3" y="3" width="58" height="58" rx="15" fill="none" stroke="#F2C23C" stroke-width="2"/>
</svg>
"""

# Full-bleed variant: opaque square background, the sun + waves scaled 0.75x
# about the center so every pixel of content (STROKE + round caps included —
# the wave endpoints reach 32.4 vb units from center) sits inside the
# maskable safe circle (80% of the icon = 25.6 vb units). No ring (it would
# be clipped at the mask edge).
FULLBLEED = f"""<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" role="img" aria-label="bevin">
  <defs>
    <radialGradient id="pwaSun" cx="40%" cy="36%" r="68%">
      <stop offset="0%" stop-color="#FFF3C9"/>
      <stop offset="55%" stop-color="#F8CE55"/>
      <stop offset="100%" stop-color="#ECB429"/>
    </radialGradient>
  </defs>
  <rect x="0" y="0" width="64" height="64" fill="#FFF3D2"/>
  <g transform="translate(32 32) scale(0.75) translate(-32 -32)">
    <circle cx="31" cy="23" r="12" fill="url(#pwaSun)"/>
    <g fill="none" stroke="#DFA23A" stroke-linecap="round">
      <path d="M 15 41 Q 22 38.5 29 41 T 45 41" stroke-width="3" opacity="0.45"/>
      <path d="M 12 47 Q 20 44 28 47 T 48 47" stroke-width="3.5" opacity="0.7"/>
      <path d="M 10 53 Q 17 49.5 26 53 T 50 53" stroke-width="4" opacity="0.95"/>
    </g>
  </g>
</svg>
"""

jobs = [
    ("icon-192.png", FAVICON, 192),
    ("icon-512.png", FAVICON, 512),
    ("icon-maskable-512.png", FULLBLEED, 512),
    ("apple-touch-icon.png", FULLBLEED, 180),
]

for name, svg, size in jobs:
    cairosvg.svg2png(bytestring=svg.encode(), write_to=f"{STATIC}/{name}",
                     output_width=size, output_height=size)
    print(f"wrote {STATIC}/{name} ({size}x{size})")
