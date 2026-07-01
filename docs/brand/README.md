# shunt — brand assets

Primary mark: **the switch** (a rail points glyph that also reads as a git branch).
Wordmark: **Space Grotesk** 600. Tagline: **"Never lose your train of thought."**

## Folder
- `logo/`  — scalable SVG marks (mark only, no wordmark). Use these anywhere.
  - `shunt-mark.svg`        switch mark for **dark** backgrounds (cyan + live-green)
  - `shunt-mark-light.svg`  switch mark for **light** backgrounds (blue + green)
  - `shunt-mark-white.svg`  1-colour white (on photos / brand blue)
  - `shunt-mark-black.svg`  1-colour black
  - `alt-monogram-s.svg`, `alt-sidings.svg` — the two runner-up directions, kept for reference
- `icon/` — app icon + favicon
  - `app-icon.svg` + PNG renders `app-icon-{512,256,128,64,32,16}.png`  (rounded dark tile)
  - `favicon.svg` + PNG renders `favicon-{48,32,16}.png`
- `social/` — raster pieces that use the Space Grotesk wordmark (PNG, because GitHub won't load a webfont inside an SVG)
  - `shunt-social-1280x640.png`        GitHub repo social preview
  - `shunt-readme-header-1280x320.png` header image for the README
  - `shunt-lockup-dark.png`, `shunt-lockup-light.png`  horizontal logo lockups

## Getting these into your repo
These assets live in `docs/brand/`. To reuse them elsewhere, copy the folder into that repo (e.g. `docs/brand/` or `.github/brand/`) and adjust the paths below to match.
1. **Social preview:** GitHub → repo **Settings → General → Social preview → Upload an image** → `docs/brand/social/shunt-social-1280x640.png`.
2. **README header:** add to the top of `README.md` (image path is repo-relative):
   ```md
   <p align="center"><img src="docs/brand/social/shunt-readme-header-1280x320.png" alt="shunt" width="100%"></p>
   ```
   (see `repo-readme-snippet.md` for a ready block with badges)
3. **Favicon** (for the dashboard / docs site): copy the icons to your site's web root first, then reference them (the `href`s below are root-relative — the files ship under `docs/brand/icon/`):
   ```html
   <link rel="icon" href="/favicon.svg" type="image/svg+xml">
   <link rel="icon" href="/favicon-32.png" sizes="32x32">
   <link rel="apple-touch-icon" href="/app-icon-256.png">
   ```

## Palette
```
shunt palette
  cyan (dark brand)   #46CBFF
  blue (light brand)  #0063B2
  live green          #6EE79A  (dark) / #1B7F3B (light)  — LIVE state ONLY
  ink                 #1A1A1A
  panel dark          #0c1015 / #0c1116 / #141414
  surface light       #F8F9FA
```
> Rule: **green is reserved for the LIVE state only** — never decorative.

See `FONTS.md` for the typefaces (all free / open-source).
