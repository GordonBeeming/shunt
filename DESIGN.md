# shunt — design notes

> Divert your local ports onto a different experiment's track.

## The problem

I run parallel experiments across a whole codebase and want to flip between them without tearing anything down. The constraints that shape the design:

- **Aspire needs fixed internal ports.** Service-to-service comms are wired to specific host/port pairs, so I can't let a port juggler reassign them per experiment.
- **Entra is the binding constraint.** Every experiment shares one Entra app registration and one redirect URI (e.g. `localhost:5000`). The OAuth redirect has to land on a single stable host:port, always.
- **I want the same data per experiment** — a copy of a baseline DB, not a shared one, so a migration in one experiment can't corrupt another.
- **GitButler can't diverge the same file across experiments.** Its virtual branches live in one working tree, so two experiments rewriting the same file fight. I need separate working copies.
- **No worktrees.** They share the host network (port collisions) and I'd rather have independent clones anyway.

## The core idea

One stable front door, swap the upstream. A reverse proxy listens on the port Entra knows about and forwards to whichever experiment is currently "active." Switching experiments is just re-pointing the proxy and reloading, which takes under a second with no app restart and no Entra reconfig.

```
                  ┌─ exp-1 container (fixed ports, own clone, own DB copy)
  localhost:5000  │
  (Entra knows ───┤ proxy ──► exp-2 container   ◄── "active" = where the proxy points
   only this) ────┘  │
                     └─ exp-3 container (running, idle, ready to switch to)

  switch exp-2 → exp-3 = re-point proxy + reload. Entra none the wiser.
```

Each experiment runs inside its own Apple `container`, which gives it its own network namespace. That's what lets every experiment bind the *same* fixed ports without colliding — the ports only exist inside their own container.

## Decisions locked in

- **Runtime: Apple `container`.** Each container gets its own IP, so the proxy addresses each experiment distinctly while they all run identical internal ports.
- **Aspire shape: local processes/projects.** The AppHost runs the services as .NET processes, not Docker resources. So running the whole Aspire app inside one container is clean — no Docker-in-Docker to untangle.
- **Source: full clones, bind-mounted.** Each experiment is its own `git clone` in its own host folder, bind-mounted into the container. I edit on the Mac in VS Code; the container runs it. Separate folders mean two experiments can rewrite the same file with zero conflict.
- **DB: copy a baseline volume per experiment.** Seed `baseline-pgdata` once, then APFS-clone it (`cp -c`, near-instant copy-on-write) for each experiment and mount the copy. Identical start, diverges independently.
- **Proxy: Caddy.** Its Admin API repoints the upstream live, so a switch never restarts the proxy. One experiment sits in front at a time, which matches how I work. If I later want side-by-side at per-experiment hostnames, that's a Traefik-shaped change I can layer on, but it isn't the goal now.
- **Routes: a few, swapped as a set.** The front door holds more than one stable route: the frontend redirect URI, and the API origin that needs a stable host for CORS. A switch repoints all of them to the same experiment together, so the frontend and the API it calls always belong to the same track.

## Gotchas to remember

- **Hot reload across the VM boundary.** Apple `container` is a lightweight VM, so inotify events may not propagate host→guest. `dotnet watch` can miss changes. Fix: set `DOTNET_USE_POLLING_FILE_WATCHER=1` in the container — polls instead of relying on inotify.
- **Bake vs mount.** Bind-mount the experiment I'm actively editing (live, instant). Bake a finished experiment into an image only to park it: immutable, but you rebuild on every change, so it's not for active dev.

## The CLI surface

- `shunt spin <name> <branch>` — clone the repo, APFS-clone the DB volume, start the container with fixed ports, register it with the proxy.
- `shunt switch <name>` — re-point the proxy's stable port at this experiment and reload. The whole point.
- `shunt kill <name>` — tear the experiment down.

## Continuing this conversation

This design came out of a session in another folder. To pick it back up here with full history:

```bash
cd /Users/gordonbeeming/Developer/github/gordonbeeming/shunt
claude --resume 087e0cc6-37c5-4e30-aaae-be3b0242f856
```

The transcript was copied into this project's Claude history, so resume works from here.
