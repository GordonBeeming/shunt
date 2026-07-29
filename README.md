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

Apple `container` is the only host runtime shunt needs. Docker runs inside each guest. Declare dependency images as unique tags in `prebakeImages`, and persistent Docker volumes in `dataVolumes`. `warm` resolves the latest digest for every configured tag into an immutable generation in a `0700` content-addressed cache directory, with one Docker-load export per image. Lifecycle commands compare that generation with the guest marker, load only missing or changed refs, then update the marker. A siding never pulls live: an undeclared or unavailable image stops startup. Re-run `shunt app add` after changing the root `.shunt.app.json`.
