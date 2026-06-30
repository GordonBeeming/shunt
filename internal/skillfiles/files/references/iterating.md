# Iterating in a siding — the fast loop

## Why the first run is slow (and how to fix it)

A siding's guest starts with an empty Docker store. So the first `up` rebuilds any custom dependency images (a SQL+FTS image can be ~6 GB) and re-pulls everything from scratch — that's the ~20–30 min. A native `aspire start` only feels fast because the host's Docker already has those images cached from an earlier run; a genuinely clean native run is just as slow.

The fix is `shunt warm`:

```bash
shunt-dev up <one-siding>     # pay the cold tax once
shunt-dev warm                # capture that siding's built/pulled images into a project cache
shunt-dev new exp2 && shunt-dev up exp2   # loads the cache → skips the rebuild + pulls
```

`warm` writes `<repos>/.shunt-dev/<project>/base/images.tar`; every later `up` `docker load`s it instead of rebuilding. Re-run `warm` when the app's dependencies change.

## Three tiers — never drop the whole environment to see a change

The guest, its Docker daemon, and the dependency containers (SQL etc.) are separate from the AppHost process. Touch only what you need:

1. **Edit → hot reload.** `up` runs the AppHost under `dotnet watch`, so most code edits reload live (it's mounted from the host). Watch is best-effort — it won't catch everything.
2. **`shunt restart <name>` → full rebuild, env preserved.** When watch misses a change or you want a clean build: this kills only the AppHost and re-runs it, leaving the guest, dockerd, dependency containers, and their data up. No pull, no data loss.
3. **`shunt up` → cold start.** Only `rm` + `new` is a real teardown (fresh worktree + data).

So the loop is: `up` once, then edit-and-hot-reload, and reach for `restart` when watch falls short. You should rarely pay the cold start again on a warmed project.

## Reading `active --json` to decide the next move

- `appRunning == false` → `shunt-dev up <name>`
- `appRunning == true` but `live == false` → `shunt-dev switch <name>`
- both `true` → it's already serving; nothing to do

`scripts/status.sh` wraps this and prints the recommended next command.
