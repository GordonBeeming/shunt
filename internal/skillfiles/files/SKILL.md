---
name: shunt
description: Run and edit an app inside an isolated shunt "siding" (a container instance with stable local ports) instead of the main repo. Use when starting a larger dev task in a shunt-managed repo, when the user wants to work in or switch between isolated instances, spin up a new track/siding, warm the dependency-image cache, or test changes without touching their main working copy. Triggers include "use shunt", "work in a siding", "spin up an instance", "switch instances", "isolated dev/track", or beginning substantial work in a repo that has a .shunt.app.json.
---

# shunt — isolated dev

shunt runs each experiment ("siding") of an app in its own isolated Apple `container` guest, with a stable set of local ports out front. You edit the siding's own git worktree on the host and the guest runs it live. The point: keep the user's main working copy and this conversation untouched while a bigger change runs in an isolated instance.

CLI: **`shunt-dev`**. Full command list and the `.shunt.app.json` contract are in [references/commands.md](references/commands.md); the fast iteration loop (warm cache, hot reload, restart) is in [references/iterating.md](references/iterating.md).

## When you start a substantial dev task

1. Run `bash scripts/status.sh` (or `shunt-dev active --json`) from the repo to see whether it's a shunt app and list any existing sidings.
2. **Ask the user how to work — always, using your agent's question/choice tool (e.g. AskUserQuestion); never assume.** It's a multi-level choice:
   - **Level 1** — present three options:
     - **Use the main repo** — work in the main working copy, no siding.
     - **Use an existing siding** — an isolated instance that already exists.
     - **Create a new siding** — a fresh isolated instance.
   - **Level 2** — based on the answer:
     - *existing* → ask **which siding** (list the names from the status output).
     - *new* → ask for a **siding name**, then `shunt-dev new <name>` (fast — a worktree off HEAD + idle guest, no build).
     - *main repo* → nothing more; proceed in the main repo.
   - If it isn't a shunt app and there's no `.shunt.app.json`, skip the prompt and just work in the main repo (offer `shunt-dev app add` only if a contract exists).
3. If they picked a siding, make every code change under that siding's `src` path. Never edit the main repo for that task — the guest mounts `src` and runs whatever's on disk.
4. **Build + test on the host before any `up`.** The siding's `src` is a normal checkout — compile it and run the tests there (`dotnet build`/`dotnet test`, `pnpm build`, etc.); `up` is a full guest rebuild, so don't spend it on code that doesn't compile yet. When it's green, go live in stages: **ask the user**, then `shunt-dev up <name> --no-bridge` (starts it in the guest with no host bridges/front door — confirm it comes up on the guest's Aspire dashboard without disturbing whatever's currently live); then, once it looks healthy, **confirm** and run a plain `shunt-dev up <name>` to bridge + go live. The full app always runs *in the guest*, never started on the host. After edits, hot reload covers most changes and `shunt-dev restart <name>` does a full rebuild without dropping the env. See [iterating](references/iterating.md).

## Rules

- Code edits go in the siding's `src`, not the main repo — that's the whole point.
- A siding `src` is a **git worktree off your current HEAD** on a `shunt/<name>` branch. **Commit + push with `shunt-dev git commit …` / `shunt-dev git push …`, not raw `git`** — they run host git in the right worktree (resolved from cwd or the live siding), sign with the repo's usual key, and push the `shunt/<name>` branch, so you never hand-type the worktree path or fumble the push refspec. GitButler manages the *main* repo, not sidings; read-only `git` (`status`/`log`/`diff`) in the worktree is fine. (Inside the guest git won't resolve the worktree — edit + commit on the host.)
- Never start the app locally (e.g. `aspire start`, `dotnet run`, `pnpm dev`). Use `up` / `restart`.
- First `up` on a fresh project is slow; `shunt-dev warm` after the first one makes later sidings fast. See [iterating](references/iterating.md).
