# Iterating in a siding — the fast loop

## Why the first run is slow (and how to fix it)

A siding's guest starts with an empty Docker store. So the first `up` rebuilds any custom dependency images (a SQL+FTS image can be ~6 GB) and re-pulls everything from scratch — that's the ~20–30 min. A native run only feels fast because the host's Docker already has those images cached from an earlier run; a genuinely clean native run is just as slow.

The fix is `shunt warm` — it treats the **host** Docker daemon as the canonical cache:

```bash
# declare the dependency images once in .shunt.app.json (prebakeImages), then:
shunt-dev warm                            # ensure they're on the host (pull only what's missing), save to the project cache
shunt-dev new exp1 && shunt-dev up exp1   # the guest docker-loads the cache → no rebuild, no pull
```

`warm` writes `<repos>/.shunt-dev/<project>/base/images.tar`; every `up` `docker load`s it instead of rebuilding/pulling. When the host already has the images (e.g. you've run the app natively), `warm` touches the network zero times — so a cold siding works even offline. Re-run `warm` when the app's dependencies change. (`--from <siding>` captures from a running guest instead, useful before `prebakeImages` are declared.)

## Going live — build + test on the host first, then bridge deliberately

A siding's `src` is an ordinary checkout on the host, so build it and run the tests **there** before touching the guest — `dotnet build` / `dotnet test`, `pnpm build`, whatever the project uses. That's the fast feedback loop; `up` is a full guest rebuild, so don't spend it on code that doesn't compile yet.

Once it builds and the tests pass, bring it online without stealing the front door, then go live deliberately:

1. **Ask the user first, then `shunt up <name> --no-bridge`** — starts the app in the guest but leaves the host alone: no socat bridges and no Caddy, so nothing is taken from whatever's currently live. The command prints the guest's own Aspire dashboard URL; open it to confirm the app actually comes up ("would it work").
2. **`shunt up <name>`** — bridges the host ports but leaves the stable front door where it is, so it does **not** interrupt whatever's currently live. Safe to run once step 1 looks healthy; the siding now shows as `up` in `ls`.
3. **Confirm with the user, then `shunt switch <name>`** — *this* repoints the front door at the siding, so it's the step that can interrupt another session/agent using those ports. Going live is only ever an explicit `switch`. (`up <name> --switch` folds steps 2–3 into one when you already want it live.)

So the default for a substantial change is: build + test on the host → ask → `up --no-bridge` → `up` → confirm → `switch`. The front door only moves on an explicit `switch`.

## Three tiers — never drop the whole environment to see a change

The guest, its Docker daemon, and the dependency containers (SQL etc.) are separate from the app process. Touch only what you need:

1. **Edit → hot reload.** `up` runs the app with hot reload where the runner supports it (e.g. .NET `dotnet watch`), so most code edits reload live (it's mounted from the host). Watch is best-effort — it won't catch everything.
2. **`shunt restart <name>` → full rebuild, env preserved.** When watch misses a change or you want a clean build: this kills only the app process and re-runs it, leaving the guest, dockerd, dependency containers, and their data up. No pull, no data loss.
3. **`shunt up` → cold start.** Only `rm` + `new` is a real teardown (fresh worktree + data).

So the loop is: `up` once, then edit-and-hot-reload, and reach for `restart` when watch falls short. You should rarely pay the cold start again on a warmed project.

## Changed config? Re-apply it — a running siding won't pick it up on its own

Editing `.shunt.app.json` or running `shunt config` does **not** reach a siding that already exists — guest settings are baked in when the guest is created, and the front door is derived at `app add` time. After any config change, re-apply it before expecting the siding to behave differently:

- **Guest settings** — `memory`, `cpus`, `mounts`, `env`: `shunt-dev reapply [siding]` recreates just the container from the app's **saved** settings, keeping the worktree, branch, and data clones; then `shunt-dev up [siding]`. If you changed these in `.shunt.app.json` (rather than via `shunt config`), run `shunt-dev app add` first so the saved settings pick up the edit — `reapply` reads saved state, only `app add` re-reads the contract.
- **Front-door ports/routes** — the contract's `frontDoor` (adding, removing, or renaming a route): re-run `shunt-dev app add`, **not** `reapply`. `reapply` never reads `frontDoor`, so a route you just added won't appear until `app add` re-derives the front door; then `up` (or `switch`) bridges the new route to the guest.

This is a common miss: a siding built before the edit keeps running the **old** config, so the app misbehaves or dies right after start — e.g. `up`/`aspire start` launches and the AppHost then exits with no error because a mount or env var it needs isn't there. If a siding goes wrong right after a config edit, `reapply` + `up` before you deep-dive the app itself.

## Reading `active --json` to decide the next move

- `appRunning == false` → `shunt-dev up <name>`
- `appRunning == true` but `live == false` → `shunt-dev switch <name>`
- both `true` → it's already serving; nothing to do

`scripts/status.sh` wraps this and prints the recommended next command.

## Pulling `main` into a siding — `shunt sync`

A siding starts off the repo HEAD on its own `shunt/<name>` branch. When it's drifted
behind, pull the latest default branch into it:

- `shunt-dev sync` — `fetch origin` then **merge** `origin/main` (auto-detected from
  `origin/HEAD`) into the current siding's branch: a 2-parent merge that keeps the
  siding's history intact (the safe default). `--rebase` rebases onto it instead
  (rewrites history); `--all` syncs every siding; `--from <branch>` targets a branch
  other than main.
- **On conflicts** it stops and lists the conflicted files. Resolve them in the
  worktree through the full `shunt-dev git` pass-through, then finish the merge:

  ```bash
  # edit the conflicted files, then:
  shunt-dev git add <files>
  shunt-dev git commit                 # completes the merge (or: shunt-dev git merge --abort)
  shunt-dev git push
  ```

  With `--rebase` instead: `shunt-dev git rebase --continue` (or `--abort`), then
  `shunt-dev git push --force-with-lease` (a rebase rewrote the branch).
- `--all` keeps going past a conflicted siding and prints a summary — resolve each in
  turn. Prefer the default merge; reach for `--rebase` only when you specifically want
  linear history and know the branch isn't shared.
