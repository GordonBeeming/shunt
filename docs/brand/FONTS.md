# Fonts — all free & open-source

| Role | Font | License | Weights used |
|---|---|---|---|
| Wordmark / display | **Space Grotesk** | SIL Open Font License 1.1 | 600 |
| UI / body / headlines | **Inter** | SIL OFL 1.1 | 400 / 600 / 800 |
| Code / ports / terminal | **JetBrains Mono** | SIL OFL 1.1 | 400 / 500 / 700 |

All three are free for commercial use, redistributable, and self-hostable. Bundle the font
files in your repo and keep each font's `OFL.txt` beside them.

## Get them
- Space Grotesk — https://fonts.google.com/specimen/Space+Grotesk
- Inter — https://fonts.google.com/specimen/Inter
- JetBrains Mono — https://fonts.google.com/specimen/JetBrains+Mono

## Quick web load (CDN)
```html
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@600&family=Inter:wght@400;600;800&family=JetBrains+Mono:wght@400;500;700&display=swap">
```

## CSS tokens
```css
:root{
  --shunt-cyan:#46CBFF; --shunt-blue:#0063B2;
  --shunt-live:#6EE79A; --shunt-live-light:#1B7F3B;
  --shunt-ink:#1A1A1A;  --shunt-panel:#0c1015;
  --font-display:'Space Grotesk',sans-serif;
  --font-ui:'Inter',system-ui,sans-serif;
  --font-mono:'JetBrains Mono',ui-monospace,monospace;
}
```
