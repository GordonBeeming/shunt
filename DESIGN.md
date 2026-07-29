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

Each siding runs in its own Apple `container` guest, which gives it its own network namespace and IP. That's what lets every siding bind the *same* fixed internal ports without colliding — the ports only exist inside their own guest — while the front door still addresses each guest distinctly by IP.

## How a siding actually runs

Each guest runs its own Docker daemon beside the app process and its dependency containers. The runner controls how the app process starts and how shunt learns that routes are ready. Aspire is one runner; a .NET project, Node app, or custom command can use the same guest lifecycle.

The catch the feasibility spike turned up: Aspire/DCP binds the resource service and every endpoint to **loopback inside the guest**, so none of it is reachable at the guest IP directly. shunt bridges what it needs with an in-guest `socat` (`TCP-LISTEN:<port>,bind=<guest-ip>` → `127.0.0.1:<port>`), then points Caddy at `<guest-ip>:<port>`.

## Pointing the front door

Two ways, picked per app:

- **Fixed ports, host == guest (the common case).** The contract declares each route's port and the guest binds that same port, so shunt bridges it eagerly: socat comes up the moment the guest starts, before the app has even bound, and the front door serves each route as its service comes alive. No discovery step, no waiting on a slow dependency.
- **Runtime discovery (the Aspire runner, when ports aren't declared).** shunt connects to Aspire's gRPC resource service, enumerates the live endpoints and their resolved ports, and bridges those. Other runners declare the guest port in the contract.

Either way a **switch repoints every route as a set** — frontend, API, and DB move to the same siding together — and if any one route fails to repoint, the already-switched ones roll back, so the front door is never half on one siding and half on another.

## Data per siding

Every declared siding volume is host-backed. Before the first promotion, the canonical source is empty, so a new siding gets an empty volume set. `data promote` makes a quiesced siding's complete set the next canonical generation; new sidings and `reapply --fresh-data` rebuild from that generation. Baseline-backed sidings use `cp -c`, so writes stay local and removing the siding frees its copy.

`data promote` quiesces the application and every Docker container using the declared volumes, syncs the full set, durably commits it with file and directory flushes, then restores the siding's previous idle, running, and live-route state. `data rollback` durably swaps the current and immediately previous baseline. A post-commit cleanup failure is a warning, not a retryable publication failure. Plain `reapply` preserves a siding's data. If a promoted generation is bad, roll it back, then use `reapply --fresh-data` to rebuild sidings from the recovered generation.

## Dependency image cache

The host cache is built directly from registries or declared local builds, so it does not need a host Docker daemon, Docker Desktop, or OrbStack. Its `0700` content-addressed directory holds immutable generations, each with a Docker-load export per image. `prebakeImages` is the unique, tag-only registry list; `prebakeBuilds` declares host-built images with their context, Dockerfile, platform, and build arguments. `warm` refreshes every registry tag and rebuilds every local declaration before publishing. Normal lifecycle commands assure a usable host generation, compare it with the guest marker, load only missing or changed refs, update the marker, then inspect every declared tag before the application can start. Sidings never pull from a registry: an undeclared or unavailable image fails startup. Publication automatically collects unreachable content; if that follow-up cleanup fails, the immutable generation stays committed and the CLI reports a warning. Current, previous, and actively leased generations are protected; explicit `warm gc` uses the same collector and warns rather than deleting protected data above its configurable 100 GiB default budget. Captures from a guest are streamed and expanded only up to that budget, with bounded manifests, members, descriptors, layers, and tags.

## Runners

The runner is a seam: it decides the start command, the guest env, and how shunt learns a route is ready. Built in: `aspire` (the AppHost plus the gRPC discovery above), `dotnet` (a bare `.csproj` → `dotnet run`), `node` (a `package.json` dev/start script), and `custom` (an arbitrary command). `app add` detects the common cases and asks when it can't.

## Channels

A build-time `Channel` (`release` / `beta` / `dev`) drives everything channel-scoped: the binary name, the global dir, the Caddy admin and dashboard ports, the LaunchAgent labels, the container-name prefix, and a front-door port offset. So the three install and run side by side without fighting over one proxy or one port.

## The dashboard

An always-on local web page — its own port per channel, served over the dev cert's HTTPS — that lists every app's front-door routes with live up/down status and switch/restart buttons. A switch is the instant Caddy rebind; a restart runs the configured stop+start in the guest to bring a down route back. It's the stable place to land, unlike Aspire's own dashboard, which lives at a guest IP that changes per siding and dies when the app stops.

## The CLI surface

- `shunt init` — build Caddy, install the always-on Caddy + dashboard LaunchAgents, start the runtime.
- `shunt app add` — register the repo's `.shunt.app.json` contract: front-door routes, runner, data volumes, guest size. Re-run it after changing the root contract.
- `shunt new <name>` — a siding: a git worktree on its own branch, copy-on-write data cloned from the selected baseline generation (or an empty initial source), and an idle guest. Before creating anything, it assures the exact content-versioned base image locally with Apple `container`; it never asks a registry for a missing Shunt base tag. Lifecycle commands compare the selected image-cache generation with the guest marker, load only missing or changed refs, then update the marker. It does not start the application. Ordinary repos start from the current HEAD. If HEAD is a GitButler internal workspace ref, shunt starts from the configured default branch on `origin` instead. `--branch` explicitly chooses another start point; `--from` continues an existing remote branch.
- `shunt up <name>` — build + start the app in the guest, then bridge it without moving the front door. `--no-bridge` starts it in the guest only so you can confirm it works before going live; `--switch` bridges and points the front door in one command.
- `shunt switch <name>` — repoint the front door at a siding (the instant rebind).
- `shunt restart <name>` — stop + start the app in the guest with the environment preserved.
- `shunt status` — health of the machinery plus front-door drift (a restarted guest changes IP); `--fix` re-points it.
- `shunt ls` / `active` — list sidings / report whether the current directory is a shunt app (both take `--json`).
- `shunt run <name> <cmd>` — run a command inside the guest from the app's workdir.
- `shunt warm` — refresh every configured dependency-image tag from its registry.
- `shunt data promote [siding]` / `data rollback` — replace the complete baseline from a quiesced siding, or swap back one generation.
- `shunt dashboard`, `kill`, `rm`, `reapply`, `config` — round out the rest.

## Gotchas worth remembering

- **Worktrees work now.** The first sketch ruled them out because they share the host network. Inside a per-siding container that no longer applies: each worktree runs in its own guest, so the fixed ports don't collide, and a worktree is cheaper than a full clone and signs with the repo's usual key. Worktrees still share the repo's Git metadata, though. In a GitButler-backed repo, shunt avoids attaching a new siding to GitButler's mutable workspace commit by starting it from `origin`'s configured default branch.
- **Hot reload across the VM boundary.** Apple `container` is a lightweight VM, so host→guest inotify events don't always propagate; `DOTNET_USE_POLLING_FILE_WATCHER=1` makes `dotnet watch` poll instead of waiting on them.
- **The dev cert has to be trusted *inside* the guest.** Linux has no per-user trust store, so shunt exports the generated ASP.NET dev cert into the guest's `ca-certificates` bundle. Skip that and service-to-service HTTPS health checks fail with "the SSL connection could not be established," and everything that waits on them hangs.
- **A guest's IP changes when it restarts.** A live siding that restarts drifts until the next switch; `shunt status` flags it and `--fix` re-points.
