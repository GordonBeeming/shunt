# shunt — agent instructions

shunt is a Go CLI that runs parallel .NET Aspire experiments ("sidings") in isolated Apple `container` guests, with a stable Caddy front door you switch between live. See `DESIGN.md` and the v1/v1.1 plan for the model.

## Keep the skill in sync with the CLI (required)

The agent skill is bundled in this repo at `internal/skillfiles/files/` (`SKILL.md` + `references/` + `scripts/`) and `go:embed`ded into the binary. `shunt skill install` deploys it to installed agents (Claude Code, Codex, OpenCode). The repo copy is the single source of truth — never hand-edit the deployed copies under `~/.claude`, `~/.codex`, etc.

**Any change to CLI commands, flags/args, or the `.shunt.app.json` contract MUST update the skill in the same change.** Touch whatever's affected: the command table and contract example in `references/commands.md`, the workflow notes in `references/iterating.md`, the flow in `SKILL.md`. A CLI change without a matching skill update is incomplete — treat it like updating tests after changing behaviour.

After updating, rebuild and redeploy so local agents pick it up immediately — this is part of the local change loop, like reinstalling the binary:

```bash
go build -ldflags "-X github.com/gordonbeeming/shunt/internal/config.Channel=dev" -o ~/.local/bin/shunt-dev .
shunt-dev skill install --all
```

## Git

Use the GitButler flow from the global rules (this repo lives on `gitbutler/workspace`; commit with `but`). Sidings themselves are plain-git worktrees — see the global git rule for the carve-out.

## Test fixtures — client code, never named here

Real client Aspire repos are used **locally** to validate the infra end to end (a light one for fast loops, a heavy one for load). Their names, org names, paths, resource names, and ports are **client-confidential and must NEVER appear anywhere in this repo** — not in code, comments, tests, commit messages, PR titles/bodies, review replies, docs, README, or help. Keep every example generic (`myapp`, `MyApp`, `api`, `db`, `web`). If a real client name shows up in a diff, commit, or message, treat it as a leak and scrub it before anything is pushed.
