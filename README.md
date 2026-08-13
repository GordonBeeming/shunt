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
  <img alt="requires macOS 26+" src="https://img.shields.io/badge/requires-macOS%2026%2B-30363d">
  <img alt="license FSL-1.1-MIT" src="https://img.shields.io/badge/license-FSL--1.1--MIT-30363d">
</p>

---

Each experiment is a **siding**: first a small Git worktree, then—only when you ask—its own data, output directory, Apple `container` guest, Docker daemon, and application. A stable Caddy front door switches fixed local ports between running sidings without rebuilding them. There is no executable “host” target; application work happens in sidings.

## Nightly distribution

The planned public nightly target is macOS 26 or newer on Apple silicon (arm64). Once the public nightly release and Homebrew tap are available, install it with:

```bash
brew update
brew install gordonbeeming/tap/shunt-nightly
```

Later, upgrade with `brew update && brew upgrade gordonbeeming/tap/shunt-nightly`.

The first `init` needs the .NET SDK and `xcaddy`. Homebrew's `go@1.25` formula is keg-only, so select its binary and put both it and its GOPATH bin directory on your `PATH` before running `init`:

```bash
GO_BIN="$(brew --prefix go@1.25)/bin/go"
GOPATH_BIN="$("$GO_BIN" env GOPATH)/bin"
"$GO_BIN" install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.6
export PATH="$(brew --prefix go@1.25)/bin:$GOPATH_BIN:$PATH"
```

The nightly package gate accepts canonical darwin/arm64 Go 1.25.13 or a later patch on the 1.25 line. Shunt uses `xcaddy` to build its Caddy binary; Apple `container` is the host runtime, and Docker runs inside each guest. The Aspire CLI is required only for .NET Aspire apps.

Initialise the nightly channel once, then install its agent skill:

```bash
shunt-nightly init
shunt-nightly skill install --all
shunt-nightly version
```

`version` reports the channel and build version, for example `channel=nightly ... version=0.0.0-nightly.123`. `init` exports and loads the development certificate into Caddy. Trust it in macOS with `dotnet dev-certs https --trust`; run `shunt-nightly cert install` only after trust, rotation, or regeneration needs Caddy to reload it.

Nightly has its own binary, ports, launch agents, project state, and data baselines. If a project already uses `shunt-dev`, register it again from the original checkout with `shunt-nightly app add`, then create a fresh nightly siding. Do not copy `.shunt-dev`, reuse a dev siding, or migrate its data. The first nightly `up` starts with the channel's own empty data baseline; promote data only when that is deliberate.

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
| `worktree` | Branch and checkout only | Edit/test, or `shunt-nightly up <name>` |
| `data` | Worktree, data clones, and `out` | Retry `shunt-nightly up <name>` |
| `guest` | Worktree, data, output, and Apple guest | `up`, `switch`, `restart`, `kill`, or `park` |
| `parked` | Worktree, data, and output; no guest | `shunt-nightly up <name>` |

Runtime observation is separate from this persisted phase. A guest can be `running`, `stopped`, `missing`, or `runtime-unavailable`; an unavailable Apple runtime is never guessed to mean “stopped”.

## Quick start

```bash
# Once per repository: read .shunt.app.json, create the independent control repo,
# and pin the committed source seed. This does not make the checkout an app host.
shunt-nightly app add

# Cheap by default: creates only a worktree and branch.
shunt-nightly new my-change
cd "$(shunt-nightly cd my-change)"

# Edit, build, and test directly in the worktree. Grow it only when needed.
shunt-nightly up my-change --no-bridge   # create data/out/guest and start privately
shunt-nightly up my-change               # bridge it; front door stays where it is
shunt-nightly switch my-change           # deliberately make it live

# Release guest disk when you are done running it, without losing work or data.
shunt-nightly park my-change
shunt-nightly up my-change               # recreates the guest later
```

The first siding becomes the source base automatically. `shunt-nightly base` shows it and `shunt-nightly base set <siding>` changes it. New sidings seed from the base siding’s committed HEAD. A dirty base may be selected, but default `shunt-nightly new` remains blocked until its uncommitted and untracked files are resolved; use an explicit `--branch` or `--from` only when that is genuinely the intended source. The hidden `.control.git` keeps the pinned base commit even when no sidings remain, so a zero-siding project can create its next siding without depending on an old checkout.

## Disk space without clone-inflated guesses

```bash
shunt-nightly space                # current project
shunt-nightly space --all          # every registered project
shunt-nightly space --json         # explicit physical/logical/observation semantics
shunt-nightly trim my-change --dry-run
shunt-nightly trim my-change --yes # non-interactive confirmation
```

`space` separates filesystem capacity from logical source, generated, output, data, and protected-baseline scans. It also names Shunt-managed stores such as `.control.git` and the image cache, while old layout remnants and other unknown entries are shown as **unclassified** with ownership and reclaimability left explicitly unverified. APFS clone totals overlap and share blocks, so Shunt never calls those numbers reclaimable. Reclaimable Apple container storage comes only from the official `container system df --format json` result; if the service is unavailable, Shunt reports it as unobserved and does not start it.

`trim` removes only exact allow-listed generated directories that Git confirms are ignored and untracked, never source, `.git`, data, output, or symlink targets. After confirmation it locks the siding, reloads the removal journal, and requires a fresh scan to match the preview. Eligible directories are moved to same-filesystem quarantine as one set, checked against Git again, and restored if validation fails before deletion. The result keeps the logical candidate total separate from the observed filesystem free-space delta.

## Data and dependency images

Declared `dataVolumes` are cloned from an immutable project baseline when `up` first materializes a siding. `shunt-nightly data promote <siding>` quiesces the complete volume set and publishes it for future materializations and `reapply --fresh-data`; existing siding copies stay unchanged. `shunt-nightly data rollback` restores the immediately previous baseline.

Apple `container` is the only host runtime Shunt needs; Docker Desktop and OrbStack are neither installed nor expected. Docker runs inside each guest. Declare registry dependencies in `prebakeImages` and host-built images in `prebakeBuilds`. `shunt-nightly warm` publishes immutable cache generations, and normal lifecycle commands load only missing or changed cached images. Sidings never pull live. `shunt-nightly warm gc --dry-run` previews unreachable cache content before collection.

## Durable state and safe removal

Existing projects are read from legacy `state.json` with a deterministic compatibility projection. The next state-changing operation atomically publishes `state-v2.json`; from then on that file is authoritative and later writes by an older binary to `state.json` are ignored. A state file from a newer unsupported version is rejected rather than guessed or overwritten.

Siding removal is a project-exclusive, resumable journal. It checkpoints source pinning, optional final-baseline publication, guest/worktree/file removal, and operation retirement. Source, lifecycle, data, Git-sync/guest-command, and trim mutations reload state under the appropriate project/siding lock and refuse to cross an active removal journal. If state is visible after its atomic rename but the final directory durability check fails, Shunt reports that publication as committed-but-unconfirmed and does not compensate or blindly repeat non-idempotent work.

The dashboard is hostless in its app model and local-only in its control surface. Mutation requests require a loopback `Host`, the exact dashboard origin, a per-process CSRF token, JSON content type, and a bounded single request body; siding names are checked again against current state. Progress history and completed statuses are bounded so an always-on dashboard cannot grow them forever.

## .NET user secrets are host-global

Every checkout and siding that contains the same `<UserSecretsId>` resolves to the same store under `~/.microsoft/usersecrets`. Running `dotnet user-secrets set`, `remove`, or `clear` from one siding changes the values seen by the registration checkout and every other siding; the first visible symptom may be a SQL login or API authentication failure much later.

Keep user-secrets mounts read-only (`"readOnly": true`; string-form mounts are read-only by default). That prevents guest processes from writing the host store, but it cannot protect against host-side `dotnet user-secrets` commands run in a worktree. Shunt does not currently provide per-siding user-secrets stores, so use the contract `env` map only for non-sensitive values that are intentionally shared, and never put real credentials in `.shunt.app.json`.

See **[DESIGN.md](DESIGN.md)** for the full model and lifecycle invariants.
