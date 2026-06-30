# shunt command reference

The CLI is `shunt-dev` (dev channel). Release/beta builds are `shunt`/`shunt-beta` — same commands.

| Command | What it does |
|---|---|
| `shunt-dev active [--json]` | Is the current dir a shunt app? Lists sidings with `live`, `appRunning`, `guestRunning`, and the `src` edit path. Exits non-zero (plain mode) when it's not a shunt app. |
| `shunt-dev ls` | All apps + sidings across projects; `*` marks the live one. |
| `shunt-dev new <name>` | Create a siding — a git worktree off your current HEAD + an idle guest. **Fast**: no build, no Aspire. |
| `shunt-dev up <name>` | Build + start Aspire in the guest, then point the front door at it. Loads the project warm cache first (if any). First cold run is slow; see [iterating](iterating.md). |
| `shunt-dev warm [--from <siding>]` | Capture a running siding's built/pulled dependency images into a per-project cache so future sidings skip the rebuild/pull. Run `up` on one siding first, then `warm`. |
| `shunt-dev restart <name>` | Kill only the AppHost and rebuild it, keeping the guest + dockerd + dependency containers + data running. Use when `dotnet watch` misses a change. |
| `shunt-dev switch <name>` | Point the stable front-door ports at a running siding (live, no restart). |
| `shunt-dev kill <name>` | Stop a siding's guest, keeping its worktree + data to restart later. |
| `shunt-dev rm <name> [--force]` | Tear down a siding: remove the guest, its worktree + branch, and its data. `--force` if it's live. |
| `shunt-dev app add` | Register an Aspire repo with shunt (reads `.shunt.app.json`). One-time per repo. |
| `shunt-dev init` | One-time machine setup: builds Caddy + the base image, starts the proxy. |

## The `.shunt.app.json` contract (one per repo)

```json
{
  "apphost": "src/App.AppHost/App.AppHost.csproj",
  "frontDoor": [
    { "key": "web", "kind": "http",   "listenPort": 8080,  "resource": "<aspire-resource>", "endpoint": "http" },
    { "key": "db",  "kind": "layer4", "listenPort": 11433, "resource": "<sql-resource>",     "endpoint": "tcp" }
  ],
  "mounts": [
    { "host": "~/.microsoft/usersecrets", "guest": "/root/.microsoft/usersecrets", "readOnly": true }
  ]
}
```

- `frontDoor` maps Aspire resources/endpoints to stable local ports (the host:port you and Entra always hit). `kind` is `http` (web) or `layer4` (raw TCP, e.g. SQL).
- `mounts` carries per-project host paths into the guest — typically the dev's user-secrets so Aspire parameters resolve.
- shunt has no app-specific logic; this file is the only per-repo config.
