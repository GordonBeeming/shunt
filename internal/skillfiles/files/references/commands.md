# shunt command reference

The CLI is `shunt-dev` (dev channel). Release/beta builds are `shunt`/`shunt-beta` —— same commands.

| Command | What it does |
|---|---|
| `shunt-dev active [--json]` | Is the current dir a shunt app? Lists sidings with `live`, `appRunning`, `guestRunning`, and the `src` edit path. Exits non-zero (plain mode) when it's not a shunt app. |
| `shunt-dev ls [-a]` | Sidings for the **current project**; `*` marks the live one. `-a`/`--all` lists every project on the host. |
| `shunt-dev new <name>` | Create a siding —— a git worktree off your current HEAD + an idle guest. **Fast**: no build, nothing started. |
| `shunt-dev up <name>` | Build + start the app in the guest, then point the front door at it. Loads the project warm cache first (if any). First cold run is slow; see [iterating](iterating.md). |
| `shunt-dev warm [--from <siding>]` | Build the project's dependency-image cache from the **host** Docker daemon (the canonical cache): ensure the contract's `prebakeImages` are on the host (pulls only what's missing —— the one shared network call), then save them for sidings to load. Offline-friendly when the host already has them. `--from <siding>` captures from a running guest's Docker store instead. |
| `shunt-dev restart <name>` | Kill only the app process and rebuild it, keeping the guest + dockerd + dependency containers + data running. Use when hot reload misses a change. |
| `shunt-dev logs <name> [-n N]` | Print the siding's app log (build + startup + crash output) from inside the guest. Use to diagnose a failed `up`. |
| `shunt-dev git commit [args]` | Commit in the siding's worktree on the **host** (git doesn't resolve inside the guest). Args pass straight through to `git -C <worktree> commit`; signs with your usual key. Siding taken from cwd or the live one. |
| `shunt-dev git push [args]` | Push the siding's branch (your configured prefix) from its worktree on the host. Args pass straight through to `git -C <worktree> push`. |
| `shunt-dev run <siding> [command...]` | Run a command inside the siding's guest, from the app's workdir (/workspace or the contract `workdir`), stdio passed through. No command drops into a shell. For migrations, one-off dotnet/npm commands, etc. The guest-side complement to `shunt git`. |
| `shunt-dev switch [name]` | Point the stable front-door ports at a running siding (live, no restart). No name —— arrow-key picker. **—- Always confirm with the user before switching** —— it repoints the shared front door away from the current live siding and can interrupt another agent/session working against it. |
| `shunt-dev dashboard [--install]` | The shunt web UI on this channel's port (dev `2220`), always-on via a LaunchAgent (`--install` to set it up; `init` does it too). Browse every app's front-door ports with live up/down status —— browse every app's front-door ports with live up/down status, and switch/restart sidings with a click. Non-release channels show a corner ribbon. Switch/restart in the UI confirm first.|
| `shunt-dev up`/`restart`/`logs`/`kill`/`rm` `[name]` | All accept no name and fall back to the arrow-key picker (like `switch`). |
| `shunt-dev kill <name>` | Stop a siding's guest, keeping its worktree + data to restart later. |
| `shunt-dev rm <name> [--force]` | Tear down a siding: remove the guest, its worktree + branch, and its data. `--force` if it's live. |
| `shunt-dev app add` | Register the repo with shunt (reads `.shunt.app.json`). Front-door ports are random+free by default (no collisions); `fixedPorts:true` pins them. Re-run to apply contract changes. |
| `shunt-dev app switch <app>` | Make `<app>` active on its (fixed) front-door ports, parking any app that conflicts —— without stopping the parked app's siding. For apps that share ports (e.g. Vite on the same port). |
| `shunt-dev init` | One-time machine setup: builds Caddy + the base image, starts the proxy. |
| `shunt-dev cert install` | Export the host's dotnet dev cert and load it into Caddy (front door serves HTTPS with the cert you already trust —— no extra CA). |
| `shunt-dev config <branchPrefix|memory|cpus> [value]` | Get/set user-config defaults: `branchPrefix` (e.g. `gb/shunt/`), `memory` (per-guest RAM, default `4g`), `cpus` (default `4`). A repo's `.shunt.app.json` `memory`/`cpus` overrides per app. Stored per channel in `<global-dir>/config.json`. |

## The `.shunt.app.json` contract (one per repo)

```json
{
  "runner": "aspire",                      // aspire | dotnet | node | custom (omit = auto-detect at app add)
  "apphost": "src/App.AppHost/App.AppHost.csproj",  // aspire only
  "start": "pnpm dev",                     // non-aspire: command to start the app
  "stop": "aspire stop",                   // optional clean-stop command (shunt force-kills as a fallback)
  "workdir": "src/web",                    // non-aspire: dir to run start in (relative)
  "frontDoor": [
    // host == guest: listenPort/guestPort = the app's real port (its launchSettings port for aspire)
    { "key": "web", "kind": "http",   "listenPort": 7011, "guestPort": 7011, "resource": "<aspire-resource>", "endpoint": "https", "tls": true },
    { "key": "db",  "kind": "layer4", "listenPort": 2100, "guestPort": 2100, "resource": "<sql-resource>",     "endpoint": "tcp" }
  ],
  "mounts": [
    { "host": "~/.microsoft/usersecrets", "guest": "/root/.microsoft/usersecrets", "readOnly": true }
  ],
  "prebakeImages": [
    "mcr.microsoft.com/mssql/server:2022-latest",
    "mcr.microsoft.com/azure-storage/azurite:3.35.0"
  ],
  "fixedPorts": true
}
```

- `runner` is how the app starts. **aspire** keeps gRPC discovery; **dotnet**/**node**/**custom** use `start` + a per-route `guestPort` (the in-guest port the app binds —— no discovery, shunt waits for it to listen). `app add` auto-detects (AppHost——aspire, package.json——node, .csproj——dotnet) and asks if it can't.
- `frontDoor` maps each service to a stable local port (the host:port you and Entra always hit). `kind` is `http` (web) or `layer4` (raw TCP, e.g. SQL). **HTTP routes serve HTTPS** at the front door using the **.NET dev cert** (run `shunt cert install` once so it's trusted); the proxy reaches the app over TLS with skip-verify. `layer4` routes stay raw TCP.
- **Ports are host == guest.** Set each route's `listenPort` (and `guestPort`) to the app's *actual* port —— for Aspire that's the project's launchSettings port (e.g. `https://localhost:7011` —— `7011`), not a made-up number. shunt runs the Aspire AppHost **with** its launch profile so the projects bind those exact ports, then bridges each to the same number on the guest IP, so the front door is `localhost:7011 —— guest:7011`. No random bridge ports.
- `mounts` carries per-project host paths into the guest (typically user-secrets so the app's config resolves). Each entry is either a plain path string — `"~/.microsoft/usersecrets"` auto-maps the host home to the guest home (`/root/…`) read-only, `"/abs"` maps to the same path — or the full `{ "host", "guest", "readOnly" }` object for read-write or a different guest path.
- `fixedPorts: true` pins the front door to the exact `listenPort` values (no channel offset) —— required for host==guest when the app's config + Entra redirect URIs point at specific ports. Default (omit it) lets the channel offset apply so channels coexist (ports won't match the app then).
- `prebakeImages` lists the dependency container images the app brings up. `shunt warm` keeps these in the host Docker cache and copies them into each siding, so guests never pull from the network. Re-run `app add` after editing the contract to apply changes.
- `dataVolumes` lists the Docker named volumes (the `WithDataVolume` names from the AppHost) to seed each siding with the host's test data. shunt extracts each host volume to an APFS baseline **once** (the only slow step —— a big SQL volume is copied a single time), then `cp -c` copy-on-write clones it per siding (instant, shares blocks until written) and points a guest Docker volume at the clone —— so every siding starts with your real data, cheaply, and `rm` frees the clone. Omit for a clean per-siding DB.
- `memory`/`cpus` cap each siding guest (e.g. `"memory": "12g"`) —— heavy stacks (SQL + several services) need headroom; omit to use the `shunt config` default (`4g`/`4`). Changing them needs `shunt reapply <siding>` (caps are fixed at guest creation).
- shunt has no app-specific logic; this file is the only per-repo config.
