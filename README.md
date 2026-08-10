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

Each experiment is a **siding**: first a small Git worktree, then—only when you ask—its own data, output directory, Apple `container` guest, Docker daemon, and application. A stable Caddy front door switches fixed local ports between running sidings without rebuilding them. There is no executable “host” target; application work happens in sidings.

## Lifecycle

```text
app add
  │  register contract + create hidden independent .control.git + pin a source commit
  ▼
new <name> ─────────────── worktree
  │                        code + branch only; first siding becomes the source base
  │ up <name>
  ▼
data ───────────────────── baseline-backed APFS copies + out/
  │
  ▼
guest ── up ──► running ── switch ──► live on the stable front door
  │                    │
  │ kill               └── restart: rebuild the app, keep guest + dependencies + data
  │ (guest retained)
  │
  └── park ─────────────── parked: worktree + data + out remain, guest removed
          │
          └── up ───────── data/out are reused, the guest is recreated, and the app starts

cleanup / rm
  ├── base removed with survivors: choose --next-base (or answer the prompt)
  └── last siding removed: pin its commit; promote a complete materialized volume set
      as the durable APFS COW baseline; keep the old baseline for worktree-only sidings
```

| Persisted phase | What exists | Useful next command |
|---|---|---|
| `worktree` | Branch and checkout only | Edit/test, or `shunt up <name>` |
| `data` | Worktree, data clones, and `out` | Retry `shunt up <name>` |
| `guest` | Worktree, data, output, and Apple guest | `up`, `switch`, `restart`, `kill`, or `park` |
| `parked` | Worktree, data, and output; no guest | `shunt up <name>` |

Runtime observation is separate from this persisted phase. A guest can be `running`, `stopped`, `missing`, or `runtime-unavailable`; an unavailable Apple runtime is never guessed to mean “stopped”.

## Quick start

```bash
# Once per repository: read .shunt.app.json, create the independent control repo,
# and pin the committed source seed. This does not make the checkout an app host.
shunt app add

# Cheap by default: creates only a worktree and branch.
shunt new my-change
cd "$(shunt cd my-change)"

# Edit, build, and test directly in the worktree. Grow it only when needed.
shunt up my-change --no-bridge   # create data/out/guest and start privately
shunt up my-change               # bridge it; front door stays where it is
shunt switch my-change           # deliberately make it live

# Release guest disk when you are done running it, without losing work or data.
shunt park my-change
shunt up my-change               # recreates the guest later
```

The first siding becomes the source base automatically. `shunt base` shows it and `shunt base set <siding>` changes it. New sidings seed from the base siding’s committed HEAD. A dirty base may be selected, but default `shunt new` remains blocked until its uncommitted and untracked files are resolved; use an explicit `--branch` or `--from` only when that is genuinely the intended source. The hidden `.control.git` keeps the pinned base commit even when no worktrees remain, so a zero-siding project can create its next siding without depending on an old checkout.

## Disk space without clone-inflated guesses

```bash
shunt space                # current project
shunt space --all          # every registered project
shunt space --json         # explicit physical/logical/observation semantics
shunt trim my-change --dry-run
shunt trim my-change --yes # non-interactive confirmation
```

`space` separates filesystem capacity from logical source, generated, output, data, and protected-baseline scans. It also names Shunt-managed stores such as `.control.git` and the image cache, while old layout remnants and other unknown entries are shown as **unclassified** with ownership and reclaimability left explicitly unverified. APFS clone totals overlap and share blocks, so Shunt never calls those numbers reclaimable. Reclaimable Apple container storage comes only from the official `container system df --format json` result; if the service is unavailable, Shunt reports it as unobserved and does not start it.

`trim` removes only exact allow-listed generated directories that Git confirms are ignored and untracked, never source, `.git`, data, output, or symlink targets. After confirmation it locks the siding, reloads the removal journal, and requires a fresh scan to match the preview. Eligible directories are moved to same-filesystem quarantine as one set, checked against Git again, and restored if validation fails before deletion. The result keeps the logical candidate total separate from the observed filesystem free-space delta.

## Data and dependency images

Declared `dataVolumes` are cloned from an immutable project baseline when `up` first materializes a siding. `shunt data promote <siding>` quiesces the complete volume set and publishes it for future materializations and `reapply --fresh-data`; existing siding copies stay unchanged. `shunt data rollback` restores the immediately previous baseline.

Apple `container` is the only host runtime Shunt needs; Docker Desktop and OrbStack are neither installed nor expected. Docker runs inside each guest. Declare registry dependencies in `prebakeImages` and host-built images in `prebakeBuilds`. `shunt warm` publishes immutable cache generations, and normal lifecycle commands load only missing or changed cached images. Sidings never pull live. `shunt warm gc --dry-run` previews unreachable cache content before collection.

## Durable state and safe removal

Existing projects are read from legacy `state.json` with a deterministic compatibility projection. The next state-changing operation atomically publishes `state-v2.json`; from then on that file is authoritative and later writes by an older binary to `state.json` are ignored. A state file from a newer unsupported version is rejected rather than guessed or overwritten.

Siding removal is a project-exclusive, resumable journal. It checkpoints source pinning, optional final-baseline publication, guest/worktree/file removal, and operation retirement. Source, lifecycle, data, Git-sync/guest-command, and trim mutations reload state under the appropriate project/siding lock and refuse to cross an active removal journal. If state is visible after its atomic rename but the final directory durability check fails, Shunt reports that publication as committed-but-unconfirmed and does not compensate or blindly repeat non-idempotent work.

The dashboard is hostless in its app model and local-only in its control surface. Mutation requests require a loopback `Host`, the exact dashboard origin, a per-process CSRF token, JSON content type, and a bounded single request body; siding names are checked again against current state. Progress history and completed statuses are bounded so an always-on dashboard cannot grow them forever.

## .NET user secrets are host-global

Every checkout and siding that contains the same `<UserSecretsId>` resolves to the same store under `~/.microsoft/usersecrets`. Running `dotnet user-secrets set`, `remove`, or `clear` from one siding changes the values seen by the registration checkout and every other siding; the first visible symptom may be a SQL login or API authentication failure much later.

Keep user-secrets mounts read-only (`"readOnly": true`; string-form mounts are read-only by default). That prevents guest processes from writing the host store, but it cannot protect against host-side `dotnet user-secrets` commands run in a worktree. Shunt does not currently provide per-siding user-secrets stores, so use the contract `env` map only for non-sensitive values that are intentionally shared, and never put real credentials in `.shunt.app.json`.

See **[DESIGN.md](DESIGN.md)** for the full model and lifecycle invariants.
