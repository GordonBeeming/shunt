<p align="center">
  <img src="docs/brand/social/shunt-readme-header-1280x320.png" alt="shunt" width="100%">
</p>

<p align="center">
  <em>Never lose your train of thought.</em><br>
  Run parallel experiments as isolated Apple container machines — a lightweight VM each —
  and switch which one is live onto stable local ports.
</p>

<p align="center">
  <img alt="works with any runner" src="https://img.shields.io/badge/works%20with-any%20runner-0063B2">
  <img alt="built for Apple silicon" src="https://img.shields.io/badge/built%20for-Apple%20silicon-46CBFF?labelColor=21262d">
  <img alt="requires macOS 27+" src="https://img.shields.io/badge/requires-macOS%2027%2B-30363d">
  <img alt="license FSL-1.1-MIT" src="https://img.shields.io/badge/license-FSL--1.1--MIT-30363d">
</p>

---

Each experiment — a **siding** — runs in its own isolated Apple `container` guest, and a stable Caddy front door lets you switch which siding is live on fixed local ports without disturbing the others. See **[DESIGN.md](DESIGN.md)** for the full model.

Apple `container` is the only host runtime shunt needs; Docker Desktop and OrbStack are neither installed nor expected. Docker runs inside each guest. Declare registry dependency tags in `prebakeImages`, host-built images in `prebakeBuilds`, and persistent Docker volumes in `dataVolumes`. `warm` refreshes every registry tag and rebuilds every declared local image into immutable, per-image cache generations. Normal lifecycle commands assure the host cache and the exact content-versioned native base image, load only missing or changed cached refs, and refuse to start until every declared image inspects correctly; a siding never pulls live. Shunt collects unreachable cache content automatically after publication, while `shunt warm gc --dry-run` and `shunt warm gc` provide inspection and explicit collection. A failed automatic collection is reported as a warning without undoing the published generation. `warm --from` bounds both the guest stream and archive expansion to the configured cache budget. Re-run `shunt app add` after changing the root `.shunt.app.json`.

When a siding's data is the state you want to keep, `shunt data promote <siding>` makes its complete declared-volume set the canonical baseline for future `new` and `reapply --fresh-data` operations. Existing siding copies stay unchanged, and `shunt data rollback` restores the immediately previous baseline.
