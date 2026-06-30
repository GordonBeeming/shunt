---
name: shunt
description: Run and edit an app inside an isolated shunt "siding" (a container instance with stable local ports) instead of the main repo. Use when starting a larger dev task in a shunt-managed repo, when the user wants to work in or switch between isolated instances, spin up a new track/siding, warm the dependency-image cache, or test changes without touching their main working copy. Triggers include "use shunt", "work in a siding", "spin up an instance", "switch instances", "isolated dev/track", or beginning substantial work in a repo that has a .shunt.app.json.
---

# shunt — isolated dev

shunt runs each experiment ("siding") of an app in its own isolated Apple `container` guest, with a stable set of local ports out front. You edit the siding's own git worktree on the host and the guest runs it live. The point: keep the user's main working copy and this conversation untouched while a bigger change runs in an isolated instance.

CLI: **`shunt-dev`**. Full command list and the `.shunt.app.json` contract are in [references/commands.md](references/commands.md); the fast iteration loop (warm cache, hot reload, restart) is in [references/iterating.md](references/iterating.md).

## When you start a substantial dev task

1. Run `bash scripts/status.sh` (or `shunt-dev active --json`) from the repo.
   - Not a shunt app → work normally in the main repo, or offer `shunt-dev app add` if a `.shunt.app.json` exists.
   - It is a shunt app → do **not** start editing the main repo. Go to step 2.
2. Pick the siding — **ask the user**. Use an existing one from the status output, or create a fresh one: `shunt-dev new <name>` (fast — a worktree off HEAD + idle guest, no build).
3. Make every code change under that siding's `src` path. Never edit the main repo for this task — the guest mounts that `src` and runs whatever's on disk.
4. Run it via shunt, never started locally (the app runs in the guest). The status script prints the right next command — usually `shunt-dev up <name>`; after edits, hot reload covers most changes and `shunt-dev restart <name>` does a full rebuild without dropping the env. See [iterating](references/iterating.md).

## Rules

- Code edits go in the siding's `src`, not the main repo — that's the whole point.
- A siding `src` is a **git worktree off your current HEAD** on a `shunt/<name>` branch, so plain `git` is fine here and signs with the repo's usual key. GitButler manages the *main* repo, not sidings. (Inside the guest git won't resolve the worktree — edit + commit on the host.)
- Never start the app locally (e.g. `aspire start`, `dotnet run`, `pnpm dev`). Use `up` / `restart`.
- First `up` on a fresh project is slow; `shunt-dev warm` after the first one makes later sidings fast. See [iterating](references/iterating.md).
