# Iterating in a siding — the fast loop

## Why the first run is slow (and how to fix it)

A siding's guest starts with an empty Docker store. A `0700` content-addressed cache directory keeps immutable image generations, with one Docker-load export per image. It is daemon-free: Apple `container` is the host runtime, and Docker runs inside each guest.

`new` and guest recreation also assure Shunt's exact content-versioned base image locally before changing a siding. A stale `latest` tag is never accepted as a substitute, and a missing Shunt base tag is built from the binary's embedded assets instead of being pulled from a registry.

List registry dependency tags in `prebakeImages`, and declare app-owned images in `prebakeBuilds`:

```bash
# declare registry tags and local builds once in .shunt.app.json, then:
shunt-dev warm                            # refresh every configured tag from its registry
shunt-dev new exp1 && shunt-dev up exp1   # the guest docker-loads the cache → no rebuild, no pull
```

`warm` resolves the latest digest for every configured registry tag, rebuilds every local declaration, and publishes immutable generations in `<repos>/.shunt-dev/<project>/base/images/` with mode `0700`. Each configured image has its own Docker-load export. Lifecycle commands assure the host generation, compare it with the guest marker, Docker-load only missing or changed refs, then update the marker. Before an application starts, shunt inspects every declared tag. A siding never pulls live: an undeclared image, unavailable export, failed load, or failed inspection stops there. Digest-pinned refs aren't accepted because Docker load cannot recreate a runnable `repo@digest` alias. Publication automatically collects unreachable cache content; a cleanup failure warns without undoing the committed generation. Use `shunt-dev warm gc --dry-run` to preview the same collector or `shunt-dev warm gc` to run it explicitly; current, previous, and leased generations stay protected. `warm --from` limits both guest output and archive expansion to `SHUNT_CACHE_MAX_BYTES` (100 GiB by default).

## Data baseline workflow

`dataVolumes` declares the Docker named volumes that matter to the app. The initial source is empty, so sidings created before any promotion start with empty volume directories.

1. Create or use a siding, then prepare the data you want future sidings to start from.
2. Run `shunt-dev data promote <siding>` to quiesce the app and its volume consumers, then atomically install that siding's complete volume set as the next canonical generation.
3. Future `shunt-dev new <name>` calls use the promoted generation. Use `shunt-dev reapply <siding> --fresh-data` to rebuild an existing siding from it; already-created sidings remain unchanged, and plain `reapply` keeps that siding's current data.
4. Run `shunt-dev data rollback` to swap back to the immediately previous generation.
5. If a promotion proves bad, roll it back first, then rebuild affected sidings with `reapply --fresh-data`. A failed promotion leaves the current generation in place.

## Going live — build + test on the host first, then bridge deliberately

A siding's `src` is an ordinary checkout on the host, so build it and run the tests **there** before touching the guest — `dotnet build` / `dotnet test`, `pnpm build`, whatever the project uses. That's the fast feedback loop; `up` is a full guest rebuild, so don't spend it on code that doesn't compile yet.

Once it builds and the tests pass, bring it online without stealing the front door, then go live deliberately:

1. **Ask the user first, then `shunt up <name> --no-bridge`** — starts the app in the guest but leaves the host alone: no socat bridges and no Caddy, so nothing is taken from whatever's currently live. Check the runner's guest output (the Aspire dashboard for an Aspire app) to confirm the app actually comes up ("would it work").
2. **`shunt up <name>`** — bridges the host ports but leaves the stable front door where it is, so it does **not** interrupt whatever's currently live. Safe to run once step 1 looks healthy; the siding now shows as `up` in `ls`.
3. **Confirm with the user, then `shunt switch <name>`** — *this* repoints the front door at the siding, so it's the step that can interrupt another session/agent using those ports. Going live is only ever an explicit `switch`. (`up <name> --switch` folds steps 2–3 into one when you already want it live.)

So the default for a substantial change is: build + test on the host → ask → `up --no-bridge` → `up` → confirm → `switch`. The front door only moves on an explicit `switch`.

## Three tiers — never drop the whole environment to see a change

The guest, its Docker daemon, and the dependency containers (SQL etc.) are separate from the app process. Touch only what you need:

1. **Edit → hot reload.** `up` runs the app with hot reload where the runner supports it (e.g. .NET `dotnet watch`), so most code edits reload live (it's mounted from the host). Watch is best-effort — it won't catch everything.
2. **`shunt restart <name>` → full rebuild, env preserved.** When watch misses a change or you want a clean build: this kills only the app process and re-runs it, leaving the guest, dockerd, dependency containers, and their data up. No pull, no data loss.
3. **`shunt up` → cold start / self-heal.** `up` is also the recovery button: if the guest is missing or wedged it recreates it from saved settings (keeps code + data — no manual `reapply`), and if the app is up but not serving (health endpoint fails) it rebuilds it. Only `rm` + `new` is a real teardown (fresh worktree + data).

So the loop is: `up` once, then edit-and-hot-reload, and reach for `restart` when watch falls short. You should rarely pay the cold start again on a warmed project.

## Cleaning up sidings without losing work

Run `shunt-dev cleanup` from the registered repo when several sidings are finished. It lists every siding for that project, lets you select more than one, and marks worktrees with uncommitted or untracked files, or commits reachable only from that siding branch. After the selection, a protected siding gets a second confirmation before Shunt removes its guest, worktree, branch, and data.

For an AI coding client, that confirmation belongs to the user. Stop and name the dirty sidings, then ask whether those changes should be discarded. Only answer yes or rerun with `shunt-dev cleanup --force` after the user explicitly approves it. Force also permits cleanup of the live siding; without force, switch away first. `shunt-dev rm <name>` uses the same checks when only one siding needs removal.

## Changed config? Re-apply it — a running siding won't pick it up on its own

Editing the **root repo's** `.shunt.app.json` or running `shunt config` does **not** reach a siding that already exists — guest settings are baked in when the guest is created, and the app-level front door is derived at `app add` time. (One exception: a siding's *own* worktree `.shunt.app.json` front door — `up`/`switch` on that siding read it live; see the front-door bullet below.) After any config change, re-apply it before expecting the siding to behave differently:

- **Guest settings** — `memory`, `cpus`, `mounts`, `env`: `shunt-dev reapply [siding]` recreates just the container from the app's **saved** settings, keeping the worktree, branch, and data clones; then `shunt-dev up [siding]`. If you changed these in `.shunt.app.json` (rather than via `shunt config`), run `shunt-dev app add` first so the saved settings pick up the edit — `reapply` reads saved state, only `app add` re-reads the contract. If you skip `app add`, `reapply` does not error; it silently recreates the guest with the previously saved memory, CPU, mount, and environment values.
- If the first `up` after `reapply` fails because the app cannot bind a port, retry `up` once before digging deeper.
- **Front-door ports/routes** — the contract's `frontDoor` (adding, removing, or renaming a route): **which `.shunt.app.json` you edited matters.** A change in a **siding's** worktree copy (`<repos>/.shunt[-ch]/<project>/<siding>/src/.shunt.app.json`) is that siding's own front door — the guest runs the siding's code, so `up`/`switch` on that siding read it and bridge the new route with **no `app add`** needed. A change in the **root repo's** `.shunt.app.json` is the app-level default (shared by sidings that don't override a route) and needs `shunt-dev app add` to re-derive. Do **not** run `app add` from inside a siding — it repoints the app's `RepoPath` to the worktree and breaks `new`. A route with a declared `guestPort` bridges eagerly (host==guest, no discovery), so even a late `WaitFor`-gated resource gets bridged; if a front-door port isn't coming online, first check it's actually in the siding's contract (`grep <port> <siding-src>/.shunt.app.json`) — not only the root repo's — before chasing discovery/timing.

This is a common miss: a siding built before the edit keeps running the **old** config, so the app misbehaves or dies right after start because a mount or environment value it needs isn't there. If a siding goes wrong right after a config edit, run `app add`, then `reapply` + `up` before you deep-dive the app itself.

## Reading `active --json` to decide the next move

- `appRunning == false` → `shunt-dev up <name>`
- `appRunning == true` but `live == false` → `shunt-dev switch <name>`
- both `true` → it's already serving; nothing to do

`scripts/status.sh` wraps this and prints the recommended next command.

## Pulling the default branch into a siding — `shunt sync`

A siding has its own branch. In an ordinary repo, a default `shunt new` starts from the
current HEAD. If HEAD is a GitButler internal workspace ref, it starts from `origin`'s
configured default branch instead, avoiding GitButler's mutable workspace commit.
`--branch <ref>` explicitly chooses another starting point; `--from <branch>` fetches
and continues an existing remote branch. When the siding has drifted behind, pull the
latest default branch into it:

- `shunt-dev sync` — `fetch origin` then **merge** `origin/<default-branch>` (auto-detected from
  `origin/HEAD`) into the current siding's branch: a non-rewriting merge that keeps the
  siding's history intact (the safe default). `--rebase` rebases onto it instead
  (rewrites history); `--all` syncs every siding; `--from <branch>` targets a branch
  other than the default.
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
