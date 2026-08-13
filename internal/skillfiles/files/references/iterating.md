# Iterating in a siding — the fast loop

## Why the first run is slow (and how to fix it)

A siding's guest starts with an empty Docker store. A `0700` content-addressed cache directory keeps immutable image generations, with one Docker-load export per image. It is daemon-free: Apple `container` is the macOS runtime, and Docker runs inside each guest.

The shared first-time setup block in `SKILL.md` is the source of truth for every channel's host prerequisites. It includes the reviewed Go patch floors, `xcaddy`, the .NET SDK, Apple `container`, and the conditional Aspire CLI requirement.

The first `up` and guest recreation assure Shunt's exact content-versioned base image before materializing a siding. Worktree-only `new` does not touch the runtime. A stale `latest` tag is never accepted as a substitute, and a missing Shunt base tag is built from the binary's embedded assets instead of being pulled from a registry.

List registry dependency tags in `prebakeImages`, and declare app-owned images in `prebakeBuilds`:

```bash
# declare registry tags and local builds once in .shunt.app.json, then:
{{shunt-command}} warm                            # refresh every configured tag from its registry
{{shunt-command}} new exp1 && {{shunt-command}} up exp1   # the guest docker-loads the cache → no rebuild, no pull
```

`warm` resolves the latest digest for every configured registry tag, rebuilds every local declaration, and publishes immutable generations in `<repos>/{{shunt-channel-directory}}/<project>/base/images/` with mode `0700`. Each configured image has its own Docker-load export. Guest lifecycle commands assure the macOS generation, compare it with the guest marker, Docker-load only missing or changed refs, then update the marker. Before an application starts, shunt inspects every declared tag. A siding never pulls live: an undeclared image, unavailable export, failed load, or failed inspection stops there. Digest-pinned refs aren't accepted because Docker load cannot recreate a runnable `repo@digest` alias. Publication automatically collects unreachable cache content; a cleanup failure warns without undoing the committed generation. Use `{{shunt-command}} warm gc --dry-run` to preview the same collector or `{{shunt-command}} warm gc` to run it explicitly; current, previous, and leased generations stay protected. `warm --from` limits both guest output and archive expansion to `SHUNT_CACHE_MAX_BYTES` (100 GiB by default).

## Start small, then materialize only when needed

`{{shunt-command}} new <name>` creates a managed branch and worktree only. It works from any Git repository: when no Shunt state exists, `new` saves only the project identity, independent control repository, source commit, and siding state. It does not need `.shunt.app.json`, inspect a runner, or register the project. The first siding becomes the source base. Later default sidings seed from that base siding's clean committed HEAD; `{{shunt-command}} base set <siding>` changes it, and `{{shunt-command}} base` shows either the current siding or the pinned commit that remains when the project has zero sidings. Use `--branch` or `--from` only as deliberate source overrides.

Before a worktree-only project can use a guest, add `.shunt.app.json` and run `{{shunt-command}} app add`. Registration fills in the runtime settings and registry entry without replacing existing sidings. Then `{{shunt-command}} up <name>` grows the durable state from `worktree` to `data` to `guest`, creating `out`, cloning the selected data baseline, loading dependency images, and starting the app. `kill` stops but retains the guest. `park` removes a non-live guest while preserving the worktree, data, and output; the next `up` recreates it. Persisted phase is not runtime observation: the dashboard reports `running`, `stopped`, `missing`, or `runtime-unavailable` independently, and an unavailable Apple runtime is never guessed to mean stopped.

{{shunt-nightly-migration}}

Existing registrations load legacy `state.json` without rewriting it during read-only commands. Legacy worktrees keep their original Git owner and pre-phase sidings project as materialized guests. A sole siding becomes the source base; with several and no existing selection, an interactive source operation asks and non-interactive use must run `{{shunt-command}} base set <siding>`. The next state publication atomically renames `state-v2.json` into place; after that v2 is authoritative and later writes to the legacy file are ignored. A file from an unsupported future version is rejected rather than guessed or overwritten.

## Data baseline workflow

`dataVolumes` declares the Docker named volumes that matter to the app. The initial source is empty, so a siding first materialized before any promotion starts with empty volume directories.

1. Materialize a siding with `up`, then prepare the data you want future siding materializations to start from.
2. Run `{{shunt-command}} data promote <siding>` to quiesce the app and its volume consumers, then atomically install that siding's complete volume set as the next canonical generation.
3. Future first `{{shunt-command}} up <name>` materializations use the promoted generation. Worktree-only `new` still touches no data. Use `{{shunt-command}} reapply <siding> --fresh-data` to rebuild an existing materialized siding from the baseline; other siding copies remain unchanged, and plain `reapply` keeps that siding's current data.
4. Run `{{shunt-command}} data rollback` to swap back to the immediately previous generation.
5. If a promotion proves bad, roll it back first, then rebuild affected sidings with `reapply --fresh-data`. A failed promotion leaves the current generation in place.

## Going live — build + test in the worktree first, then bridge deliberately

A siding's `src` is an ordinary checkout on the host, so build it and run the tests **there** before touching the guest — `dotnet build` / `dotnet test`, `pnpm build`, whatever the project uses. That's the fast feedback loop; `up` is a full guest rebuild, so don't spend it on code that doesn't compile yet.

Once it builds and the tests pass, bring it online without stealing the front door, then go live deliberately:

1. **Ask the user first, then `{{shunt-command}} up <name> --no-bridge`** — starts the app in the guest but leaves the host alone: no socat bridges and no Caddy, so nothing is taken from whatever's currently live. Check the runner's guest output (the Aspire dashboard for an Aspire app) to confirm the app actually comes up ("would it work").
2. **`{{shunt-command}} up <name>`** — bridges the host ports but leaves the stable front door where it is, so it does **not** interrupt whatever's currently live. Safe to run once step 1 looks healthy; the siding now shows as `up` in `ls`.
3. **Confirm with the user, then `{{shunt-command}} switch <name>`** — *this* repoints the front door at the siding, so it's the step that can interrupt another session/agent using those ports. Going live is only ever an explicit `switch`. (`up <name> --switch` folds steps 2–3 into one when you already want it live.)

So the default for a substantial change is: build + test in the macOS worktree → ask → `up --no-bridge` → `up` → confirm → `switch`. The front door only moves on an explicit `switch`.

## Three tiers — never drop the whole environment to see a change

The guest, its Docker daemon, and the dependency containers (SQL etc.) are separate from the app process. Touch only what you need:

1. **Edit → hot reload.** `up` runs the app with hot reload where the runner supports it (e.g. .NET `dotnet watch`), so most code edits reload live (it's mounted from the host). Watch is best-effort — it won't catch everything.
2. **`{{shunt-command}} restart <name>` → full rebuild, env preserved.** When watch misses a change or you want a clean build: this kills only the app process and re-runs it, leaving the guest, dockerd, dependency containers, and their data up. No pull, no data loss.
3. **`{{shunt-command}} up` → materialize / cold start / self-heal.** From `worktree`, `data`, or `parked`, `up` creates only the missing layers. If a guest is missing or wedged it recreates it from saved settings while keeping code and data, and if the app is up but not serving it rebuilds it. `rm` is the permanent teardown.

So the loop is: `up` once, then edit-and-hot-reload, and reach for `restart` when watch falls short. You should rarely pay the cold start again on a warmed project.

## Cleaning up sidings without losing work

Run `{{shunt-command}} cleanup` from the registered repo when several sidings are finished. It lists every siding for that project, lets you select more than one, and marks worktrees with uncommitted or untracked files, or commits reachable only from that siding branch. After the selection, a protected siding gets a second confirmation before Shunt removes its guest, worktree, branch, and data.

For an AI coding client, that confirmation belongs to the user. Stop and name the dirty sidings, then ask whether those changes should be discarded. Only answer yes or rerun with `{{shunt-command}} cleanup --force` after the user explicitly approves it. Force also permits cleanup of the live siding; without force, switch away first. `{{shunt-command}} rm <name>` uses the same checks when only one siding needs removal.

The source base has an additional hand-off rule. If it is selected while another siding survives, interactive cleanup prompts for the successor and automation must pass `--next-base <siding>`. The old base is removed last. If the final siding is removed, Shunt first pins its committed HEAD so the next `new` works with zero sidings. A complete materialized volume set is promoted as the durable APFS copy-on-write baseline before deletion; a worktree-only final siding has no data to promote and leaves the existing baseline alone. The operation is journalled so a retry does not duplicate an already-committed generation. While that journal is active, source, guest/data lifecycle, Git-sync/guest-command, and trim mutations refuse to race the removal; let the original removal command resume it.

## Measure before reclaiming space

Use `{{shunt-command}} space` for the current project or `{{shunt-command}} space --all` across registrations. Directory scans are labelled logical and clone-overlapping; they are evidence about where content lives, not estimates of reclaimable bytes. The independent source-control store and image cache appear as managed rows, baseline generations remain protected rows, and legacy layout remnants or other unknown entries are reported as unclassified with ownership/reclaimability unverified. The only Apple-container reclaimability figure comes from the official `container system df --format json` output. If that runtime is unavailable, Shunt reports the observation gap and does not start it. `--json` preserves those semantics for tooling.

Use `{{shunt-command}} trim <siding> --dry-run` before removing generated worktree content. Trim accepts only its exact allow-list and only when Git proves each directory ignored and untracked; it refuses `.git`, source/data/output paths, and symlink targets. Interactive deletion asks for confirmation, while automation must pass `--yes`. After confirmation it reloads state under the siding/removal lock, requires a fresh scan to match the preview, quarantines the complete set on the same filesystem, and checks Git again. A pre-deletion validation failure restores the whole quarantined set. Output keeps the logical candidate total separate from the actual filesystem free-space delta.

The exact generated-directory names are `bin`, `obj`, `node_modules`, `.next`, `.nuxt`, `.vite`, `.turbo`, `dist`, `build`, and `coverage`. A familiar name alone is never enough: ignored and wholly untracked remain mandatory.

## Changed config? Re-apply it — a running siding won't pick it up on its own

Editing the **registration checkout's** `.shunt.app.json` or running `{{shunt-command}} config` does **not** reach a guest that already exists — guest settings are baked in when the guest is created, and the app-level front door is derived at `app add` time. (One exception: a siding's *own* worktree `.shunt.app.json` front door — `up`/`switch` on that siding read it live; see the front-door bullet below.) After any config change, re-apply it before expecting the siding to behave differently:

- **Guest settings** — `memory`, `cpus`, `mounts`, `env`: `{{shunt-command}} reapply [siding]` recreates just the container from the app's **saved** settings, keeping the worktree, branch, and data clones; then `{{shunt-command}} up [siding]`. If you changed these in `.shunt.app.json` (rather than via `{{shunt-command}} config`), run `{{shunt-command}} app add` first so the saved settings pick up the edit — `reapply` reads saved state, only `app add` re-reads the contract. If you skip `app add`, `reapply` does not error; it silently recreates the guest with the previously saved memory, CPU, mount, and environment values.
- If the first `up` after `reapply` fails because the app cannot bind a port, retry `up` once before digging deeper.
- **Front-door ports/routes** — the contract's `frontDoor` (adding, removing, or renaming a route): **which `.shunt.app.json` you edited matters.** A change in a **siding's** worktree copy (`<repos>/.shunt[-ch]/<project>/<siding>/src/.shunt.app.json`) is that siding's own front door — the guest runs the siding's code, so `up`/`switch` on that siding read it and bridge the new route with **no `app add`** needed. A change in the **registration checkout's** `.shunt.app.json` is the app-level default (shared by sidings that don't override a route) and needs `{{shunt-command}} app add` there to re-derive saved settings and verify the independent control repository. A route with a declared `guestPort` bridges eagerly (host==guest, no discovery), so even a late `WaitFor`-gated resource gets bridged; if a front-door port isn't coming online, first check it's actually in the siding's contract (`grep <port> <siding-src>/.shunt.app.json`) — not only the registration checkout — before chasing discovery/timing.

This is a common miss: a siding built before the edit keeps running the **old** config, so the app misbehaves or dies right after start because a mount or environment value it needs isn't there. If a siding goes wrong right after a config edit, run `app add`, then `reapply` + `up` before you deep-dive the app itself.

## .NET user secrets are shared across the registration checkout and every siding

.NET user secrets are keyed by the project's `<UserSecretsId>` and stored in the host profile, not beside the checkout. Git worktrees keep the same project file and therefore the same ID. A host-side command such as `dotnet user-secrets set` run from a siding's `src` changes the store used by the registration checkout and every other siding; a later SQL login or API authentication failure may be the first visible symptom.

Shunt does not clone that store per siding. Mount `~/.microsoft/usersecrets` read-only in `.shunt.app.json` (`"readOnly": true`; a string-form mount is read-only by default). This prevents processes **inside the guest** from changing the host files, but it cannot protect them from `dotnet user-secrets set`, `remove`, or `clear` run on the host in a siding worktree.

- Never change user secrets merely to give one siding different parameter values without the user's explicit approval.
- For a non-sensitive development value that should be the same for every siding, use the contract's `env` map (for example, an Aspire `Parameters__name` override), then run `{{shunt-command}} app add` from the registration checkout and `reapply` + `up` for existing guests.
- Do not put real credentials in a source-controlled `.shunt.app.json`. If a task needs different secret-backed values per siding and the app has no isolated injection mechanism, stop and surface the limitation; shunt does not currently provide per-siding user-secrets isolation.

## Reading `active --json` to decide the next move

- `managed == false` → create a worktree with `{{shunt-command}} new <name>`
- `managed == true` and `registered == false` → edit/test in a siding; add `.shunt.app.json` and run `{{shunt-command}} app add` before any guest operation
- `registered == true` and `appRunning == false` → `{{shunt-command}} up <name>`
- `registered == true`, `appRunning == true`, but `live == false` → `{{shunt-command}} switch <name>`
- `registered`, `appRunning`, and `live` all `true` → it's already serving; nothing to do

`active` retains its compatibility meaning of a registered app. New consumers should use `managed` to distinguish no state from worktree-only state, and `registered` before recommending guest commands.
Use `--json` for that decision: it exits successfully whenever discovery succeeds. Plain `active` retains its legacy exit-status contract and exits non-zero for worktree-only as well as unmanaged directories, even though it prints the discovered worktree details.

`scripts/status.sh` wraps this and prints the recommended next command.

## Pulling the default branch into a siding — `{{shunt-command}} sync`

A siding has its own branch in Shunt's independent control repository. Default
`{{shunt-command}} new` starts from the clean source base siding's committed HEAD, or the pinned
base commit when no sidings remain. `--branch <ref>` explicitly chooses another
starting point; `--from <branch>` fetches and continues an existing remote branch.
When the siding has drifted behind, pull the latest default branch into it:

- `{{shunt-command}} sync` — `fetch origin` then **merge** `origin/<default-branch>` (auto-detected from
  `origin/HEAD`) into the current siding's branch: a non-rewriting merge that keeps the
  siding's history intact (the safe default). `--rebase` rebases onto it instead
  (rewrites history); `--all` syncs every siding; `--from <branch>` targets a branch
  other than the default.
- **On conflicts** it stops and lists the conflicted files. Resolve them in the
  worktree through the full `{{shunt-command}} git` pass-through, then finish the merge:

  ```bash
  # edit the conflicted files, then:
  {{shunt-command}} git add <files>
  {{shunt-command}} git commit                 # completes the merge (or: {{shunt-command}} git merge --abort)
  {{shunt-command}} git push
  ```

  With `--rebase` instead: `{{shunt-command}} git rebase --continue` (or `--abort`), then
  `{{shunt-command}} git push --force-with-lease` (a rebase rewrote the branch).
- `--all` keeps going past a conflicted siding and prints a summary — resolve each in
  turn. Prefer the default merge; reach for `--rebase` only when you specifically want
  linear history and know the branch isn't shared.
