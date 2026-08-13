# shunt — design notes

> Divert your local ports onto a different experiment's track.

## The problem

I run parallel experiments across a whole codebase and want to flip between them locally without tearing anything down. A few constraints shape the design:

- **Something external is pinned to a stable `host:port`.** An OAuth redirect URI (one auth registration, one `localhost:5000`), a CORS origin, a bookmarked URL, a script a teammate runs. Whatever's live has to answer on that exact port, every time — I can't have a port juggler reassigning it per experiment.
- **The app wants fixed internal ports too.** Service-to-service communication can depend on fixed ports, so each experiment needs the *same* internal ports without colliding with its siblings.
- **Same data per experiment, diverging independently.** A copy of a baseline DB, not a shared one, so a migration in one experiment can't corrupt another.
- **Two experiments can't share one working copy.** GitButler's virtual branches live in a single working tree, so two experiments rewriting the same file fight each other. Each needs its own checkout.

## The core idea

One stable front door, swap the upstream. A reverse proxy (Caddy) listens on the ports the outside world knows about and forwards to whichever experiment — a **siding** — is currently live. Switching is just re-pointing the proxy: under a second, no app restart, nothing external reconfigured.

```
                  ┌─ siding "exp-1"  (fixed ports, own worktree, own data copy)
  localhost:5000  │
  (the outside ───┤ Caddy ──► siding "exp-2"   ◄── live = where the front door points
   world knows ───┘  │
   only this) ───────└─ siding "exp-3"  (running, idle, ready to switch to)

  switch exp-2 → exp-3 = re-point Caddy. Nothing external is any the wiser.
```

Each materialized siding runs in its own Apple `container` guest, which gives it its own network namespace and IP. That's what lets every siding bind the *same* fixed internal ports without colliding — the ports only exist inside their own guest — while the front door still addresses each guest distinctly by IP. The original checkout is registration input and a legacy source reference, not an executable target: Shunt never starts, stops, or switches to a native “host” app.

## Source ownership and the base siding

`app add` reads `.shunt.app.json`, registers the app, and creates a hidden independent bare repository at `<configDir>/.control.git`. It is self-contained—no alternates or borrowed object store—and owns all new siding worktrees. A protected base ref pins the committed source seed even when no worktrees remain.

Source and data are deliberately separate:

- The **source base** is a siding whose committed HEAD seeds default `new` worktrees. The first siding becomes base automatically; `base set` changes it. Selecting a dirty siding is allowed because only its HEAD is pinned, but default `new` fails until the base worktree is clean. Explicit `--branch` and `--from` remain deliberate overrides.
- The **data baseline** is an immutable generation of the complete declared volume set. Changing the source base does not silently change data; `data promote` is the explicit data operation.

When the current base is removed and other sidings survive, cleanup prompts for a successor; non-interactive use requires `--next-base`. When the last siding is removed, Shunt first pins its committed HEAD. If its complete volume set was materialized, Shunt publishes a durable APFS copy-on-write baseline before deleting it. A worktree-only final siding has no data to promote, so the existing baseline is preserved. Removal is journalled and resumes without duplicating a committed generation.

## A siding starts small

`new` creates only a branch and worktree. It does not start Apple `container`, clone data, create `out`, load images, or allocate a guest. This makes Shunt suitable as the default worktree path even for documentation and config-only changes.

`up` grows the siding through durable phases:

1. `worktree` — source only.
2. `data` — clone the selected data-baseline generation and create the standing `out` directory.
3. `guest` — create the Apple guest, load cached dependencies, and start/bridge the app.
4. `parked` — worktree, data, and output remain after the recreatable guest is removed; the next `up` recreates it.

Persisted phase and observed runtime are separate. A `guest` phase may be observed as running, stopped, or missing, while an unavailable service is `runtime-unavailable`; inspection failure is never silently rewritten to stopped. Guest-only commands (`switch`, `restart`, `run`, `playwright`, and `kill`) tell the user to run `up` when the siding is worktree-only, data-only, or parked.

## Durable state and operation fencing

Legacy projects load from `state.json` through a deterministic compatibility projection. The first state publication resulting from a mutation writes version 2 atomically to `state-v2.json`. Once that file exists it is the sole authority, so a concurrently installed older binary cannot roll the project back by rewriting `state.json`. Unknown future versions fail closed on load and save.

The projection preserves each legacy siding's original worktree owner and treats its pre-phase guest as materialized. A sole existing siding becomes the unambiguous source base. If several legacy sidings exist without a selected base, an interactive source operation asks for one and non-interactive use must run `base set <siding>`; Shunt does not guess from age, runtime, or disk size.

State publication writes and syncs a temporary file, atomically renames it, then syncs the parent directory. A failure after the rename is reported as committed with durability unconfirmed; callers must not compensate visible state or blindly retry non-idempotent effects.

Each siding operation holds a shared project lock plus its siding lock, while destructive removal holds the project exclusively. Removal also persists a stage journal covering source pinning, optional final data promotion, guest removal, worktree removal, file removal, and baseline-operation retirement. Mutating source, lifecycle, data, Git-sync/guest-command, and trim paths reload state after acquiring their lock and reject an active removal journal. A crash can therefore resume from the last published checkpoint without another command racing the resources being removed or duplicating a committed baseline generation.

## How a siding actually runs

Each guest runs its own Docker daemon beside the app process and its dependency containers. The runner controls how the app process starts and how shunt learns that routes are ready. Aspire is one runner; a .NET project, Node app, or custom command can use the same guest lifecycle.

The catch the feasibility spike turned up: Aspire/DCP binds the resource service and every endpoint to **loopback inside the guest**, so none of it is reachable at the guest IP directly. shunt bridges what it needs with an in-guest `socat` (`TCP-LISTEN:<port>,bind=<guest-ip>` → `127.0.0.1:<port>`), then points Caddy at `<guest-ip>:<port>`.

## Pointing the front door

Two ways, picked per app:

- **Fixed ports, host == guest (the common case).** The contract declares each route's port and the guest binds that same port, so shunt bridges it eagerly: socat comes up the moment the guest starts, before the app has even bound, and the front door serves each route as its service comes alive. No discovery step, no waiting on a slow dependency.
- **Runtime discovery (the Aspire runner, when ports aren't declared).** shunt connects to Aspire's gRPC resource service, enumerates the live endpoints and their resolved ports, and bridges those. Other runners declare the guest port in the contract.

Either way a **switch repoints every route as a set** — frontend, API, and DB move to the same siding together — and if any one route fails to repoint, the already-switched ones roll back, so the front door is never half on one siding and half on another.

## Data per siding

Every declared siding volume is host-backed. Before the first promotion, the canonical source is empty, so the first `up` gets an empty volume set. `data promote` makes a quiesced siding's complete set the next canonical generation; future materializations and `reapply --fresh-data` rebuild from that generation. Baseline-backed sidings use `cp -c`, so writes stay local and removing the siding frees its copy.

`data promote` quiesces the application and every Docker container using the declared volumes, syncs the full set, durably commits it with file and directory flushes, then restores the siding's previous idle, running, and live-route state. `data rollback` durably swaps the current and immediately previous baseline. A post-commit cleanup failure is a warning, not a retryable publication failure. Plain `reapply` preserves a siding's data. If a promoted generation is bad, roll it back, then use `reapply --fresh-data` to rebuild sidings from the recovered generation.

## Dependency image cache

The host cache is built directly from registries or declared local builds, so it does not need a host Docker daemon, Docker Desktop, or OrbStack. Its `0700` content-addressed directory holds immutable generations, each with a Docker-load export per image. `prebakeImages` is the unique, tag-only registry list; `prebakeBuilds` declares host-built images with their context, Dockerfile, platform, and build arguments. `warm` refreshes every registry tag and rebuilds every local declaration before publishing. Normal lifecycle commands assure a usable host generation, compare it with the guest marker, load only missing or changed refs, update the marker, then inspect every declared tag before the application can start. Sidings never pull from a registry: an undeclared or unavailable image fails startup. Publication automatically collects unreachable content; if that follow-up cleanup fails, the immutable generation stays committed and the CLI reports a warning. Current, previous, and actively leased generations are protected; explicit `warm gc` uses the same collector and warns rather than deleting protected data above its configurable 100 GiB default budget. Captures from a guest are streamed and expanded only up to that budget, with bounded manifests, members, descriptors, layers, and tags.

## Runners

The runner is a seam: it decides the start command, the guest env, and how shunt learns a route is ready. Built in: `aspire` (the AppHost plus the gRPC discovery above), `dotnet` (a bare `.csproj` → `dotnet run`), `node` (a `package.json` dev/start script), and `custom` (an arbitrary command). `app add` detects the common cases and asks when it can't.

## Channels

A build-time `Channel` (`release` / `beta` / `nightly` / `dev`) drives everything channel-scoped: the binary name, the global dir, the Caddy admin and dashboard ports, the LaunchAgent labels, the container-name prefix, and a front-door port offset. The four channels install and run side by side without fighting over one proxy or one port. The supported nightly distribution target is macOS 26 or newer on Apple silicon (arm64), installed as `shunt-nightly` through Homebrew.

Channel state is deliberately separate. A nightly install does not discover or adopt `.shunt-dev` sidings, control repositories, or data baselines. Register an existing project again with the nightly binary, create a new siding, and prepare its data independently.

## The dashboard

An always-on local web page — its own port per channel, served over the dev cert's HTTPS — lists every app's routes and sidings without a host row. It shows the base siding, persisted worktree/data/guest/parked phase, and independently observed runtime/serving state. Start uses the same lazy `Up` path as the CLI; Park confirms before removing a non-live guest. Switch and Stop are offered only for materialized guests. The page labels runtime inspection failures honestly instead of turning them into “stopped”.

The dashboard's mutation API accepts only POSTs with a loopback `Host`, exact same-origin `Origin`, per-process CSRF token, JSON content type, and a bounded single JSON object. It validates the project/siding against fresh state before acting, serializes overlapping long-running actions per siding, caps progress history, and expires terminal statuses. Those checks keep a localhost convenience UI from becoming a DNS-rebinding or unbounded-memory control surface.

## The CLI surface

- `shunt init` — build Caddy, install the always-on Caddy + dashboard LaunchAgents, start the runtime.
- `shunt app add` — register the repo contract, initialize/verify the independent control repo, and pin the zero-siding source seed. Re-run it after changing the registration contract.
- `shunt base` / `base set <siding>` — show or designate the source base. The committed HEAD is pinned; a dirty designated base blocks default `new` until clean.
- `shunt new <name>` — create only a managed worktree and branch. The first siding becomes base. Default source is the clean base HEAD (or the saved zero-siding commit); `--branch` chooses a start ref and `--from` continues an existing branch.
- `shunt up <name>` — materialize data, output, and guest as needed; assure cached images; build/start the app; then bridge without moving the front door. `--no-bridge` keeps routes untouched and `--switch` deliberately goes live.
- `shunt switch <name>` — repoint the front door at a siding (the instant rebind).
- `shunt restart <name>` — stop + start the app in the guest with the environment preserved.
- `shunt kill <name>` — stop the guest but retain it, together with code and data.
- `shunt park <name>` — remove a non-live guest while retaining worktree, data, and output; `up` recreates it.
- `shunt rm <name> [--next-base <siding>]` / `cleanup [--next-base <siding>]` — safely remove one or several sidings, hand off the source base, and journal final source/data preservation.
- `shunt space [--all] [--json]` — separate physical filesystem capacity, logical clone-overlapping scans, protected baselines, Git evidence, managed control/cache stores, unclassified legacy/unknown entries, and official Apple container reclaimability. It never starts an unavailable runtime.
- `shunt trim <name> [--dry-run] [--yes]` — preview or delete only exact allow-listed, ignored, untracked generated directories under siding/removal locks; revalidate the preview and Git evidence around same-filesystem quarantine; report logical candidates separately from the physical free-space change.
- `shunt status` — health of the machinery plus front-door drift (a restarted guest changes IP); `--fix` re-points it.
- `shunt ls` / `active` — list sidings / report whether the current directory is a shunt app (both take `--json`).
- `shunt run <name> <cmd>` — run a command inside the guest from the app's workdir.
- `shunt warm` — refresh every configured dependency-image tag from its registry.
- `shunt data promote [siding]` / `data rollback` — replace the complete baseline from a quiesced siding, or swap back one generation.
- `shunt version`: print the build channel, `version=<build-version>`, and its resolved identity.
- `shunt dashboard`, `reapply`, `config` — round out the rest.

## Gotchas worth remembering

- **Worktrees work now.** The first sketch ruled them out because they share the host network. Inside a per-siding guest that no longer applies: fixed ports do not collide, a worktree is cheaper than a full clone, and host Git still signs with the usual key. Managed worktrees share only Shunt's independent control repository, not GitButler's mutable workspace metadata.
- **Logical size is not reclaimable size.** APFS copy-on-write generations can make a directory scan count shared extents repeatedly. `space` labels those values logical/overlapping and accepts reclaimability only from Apple `container system df`. `trim` reports the actual filesystem free-space delta after its narrowly-scoped removal.
- **Known ownership is not inferred from a familiar path.** `space` labels the independent control repo and image cache as managed, protects baseline generations, and leaves legacy or otherwise unknown on-disk entries unclassified until their ownership and reclaimability are proven.
- **Hot reload across the VM boundary.** Apple `container` is a lightweight VM, so host→guest inotify events don't always propagate; `DOTNET_USE_POLLING_FILE_WATCHER=1` makes `dotnet watch` poll instead of waiting on them.
- **The dev cert has to be trusted *inside* the guest.** Linux has no per-user trust store, so shunt exports the generated ASP.NET dev cert into the guest's `ca-certificates` bundle. Skip that and service-to-service HTTPS health checks fail with "the SSL connection could not be established," and everything that waits on them hangs.
- **A guest's IP changes when it restarts.** A live siding that restarts drifts until the next switch; `shunt status` flags it and `--fix` re-points.
