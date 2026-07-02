---
name: shunt
description: Run and edit an app inside an isolated shunt "siding" (a container instance with stable local ports) instead of the main repo. Use when starting a larger dev task in a shunt-managed repo, when the user wants to work in or switch between isolated instances, spin up a new track/siding, warm the dependency-image cache, or test changes without touching their main working copy. Triggers include "use shunt", "work in a siding", "spin up an instance", "switch instances", "isolated dev/track", or beginning substantial work in a repo that has a .shunt.app.json.
---

# shunt — isolated dev

shunt runs each experiment ("siding") of an app in its own isolated Apple `container` guest, with a stable set of local ports out front. You edit the siding's own git worktree on the host and the guest runs it live. The point: keep the user's main working copy and this conversation untouched while a bigger change runs in an isolated instance.

CLI: **`shunt-dev`**. Full command list and the `.shunt.app.json` contract are in [references/commands.md](references/commands.md); the fast iteration loop (warm cache, hot reload, restart) is in [references/iterating.md](references/iterating.md).

## When you start a substantial dev task

1. Run `bash scripts/status.sh` (or `shunt-dev active --json`) from the repo to see whether it's a shunt app and list any existing sidings.
2. Once step 1's read-only check confirms it's a shunt app, **before you plan the work or run any siding command (`new`/`up`/`switch`/`restart`/…), ask the user where this runs — using the interactive question tool (AskUserQuestion). This is a standalone question you ask out loud, NOT a line buried in a plan, and NEVER a choice you make for them.** (Step 1's `status.sh`/`active --json` is fine to run first — it's read-only recon, and you need its output to list the existing sidings below.) A siding is a full container guest (its own RAM/CPU/disk + a copy of the app's dependency images), so **creating or starting one costs real machine resources** — you must not spin one up on your own initiative. Ask first:
   - **Level 1 — present all three options, every time.** Do not drop one because the existing sidings "don't look like they're for this task" — that just means the user may want the main repo or a brand-new siding; it's their call, not yours:
     - **The main repo (host)** — work in the main working copy. No siding, no extra machine load. (Often the right default when the user is already working on the host.)
     - **An existing siding** — reuse an isolated instance that already exists (no new guest).
     - **A new siding** — a fresh isolated instance (spins up a new guest — costs resources).
   - **Level 2 — based on the answer:**
     - *existing* → ask **which siding** (list the names from the status output).
     - *new* → ask for a **siding name**, then `shunt-dev new <name>` (fast — a worktree off HEAD + idle guest, no build).
     - *main repo* → nothing more; proceed in the main repo.
   - If it isn't a shunt app and there's no `.shunt.app.json`, skip the prompt and just work in the main repo (offer `shunt-dev app add` only if a contract exists).
3. If they picked a siding, make every code change under that siding's `src` path. Never edit the main repo for that task — the guest mounts `src` and runs whatever's on disk.
4. **Build + test on the host before any `up`.** The siding's `src` is a normal checkout — compile it and run the tests there (`dotnet build`/`dotnet test`, `pnpm build`, etc.); `up` is a full guest rebuild, so don't spend it on code that doesn't compile yet. When it's green, go live in stages: **ask the user**, then `shunt-dev up <name> --no-bridge` (starts it in the guest with no host bridges/front door — confirm it comes up on the guest's Aspire dashboard without disturbing whatever's currently live); then, once it looks healthy, run a plain `shunt-dev up <name>` to bridge it — this leaves the front door untouched, so it can't interrupt whatever's currently live. Going live is a separate, deliberate step: **confirm with the user**, then `shunt-dev switch <name>` (or `up <name> --switch` to bridge + go live in one shot). The full app always runs *in the guest*, never started on the host. After edits, hot reload covers most changes and `shunt-dev restart <name>` does a full rebuild without dropping the env. See [iterating](references/iterating.md).

## Rules

- **Never create or start a siding without the user's explicit choice via the question tool.** Sidings cost real machine resources (a whole container + its dependency images); "the existing ones don't fit my task" is a reason to *ask*, not to spin up a new one. When in doubt, the main repo (host) is the low-cost default — offer it.
- Code edits go in the siding's `src`, not the main repo — that's the whole point.
- A siding `src` is a **git worktree off your current HEAD** on a `shunt/<name>` branch. **Commit + push with `shunt-dev git commit …` / `shunt-dev git push …`, not raw `git`** — they run host git in the right worktree (resolved from cwd or the live siding), sign with the repo's usual key, and push the `shunt/<name>` branch, so you never hand-type the worktree path or fumble the push refspec. A fresh siding's branch has no upstream, so the **first** push must set one: `shunt-dev git push -u origin <branch>` (args pass straight through to git); after that a bare `shunt-dev git push` works. GitButler manages the *main* repo, not sidings; read-only `git` (`status`/`log`/`diff`) in the worktree is fine. (Inside the guest git won't resolve the worktree — edit + commit on the host.)
- Never start the app locally (e.g. `aspire start`, `dotnet run`, `pnpm dev`). Use `up` / `restart`.
- First `up` on a fresh project is slow; `shunt-dev warm` after the first one makes later sidings fast. See [iterating](references/iterating.md).
