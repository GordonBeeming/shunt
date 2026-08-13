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

The bundled skill uses an explicit command placeholder. `shunt-dev skill install` renders that placeholder as the binary performing the install, including `shunt-nightly`; literal `.shunt-dev` migration text remains unchanged. To inspect the nightly-rendered copy locally, build the nightly channel and install its skill:

```bash
go build -ldflags "-X github.com/gordonbeeming/shunt/internal/config.Channel=nightly" -o ~/.local/bin/shunt-nightly .
shunt-nightly skill install --all
```

The nightly build is distributed through Homebrew for macOS 26 or newer on Apple silicon. Its first `init` needs Go, `xcaddy`, and the .NET SDK on `PATH`; Apple `container` provides the host runtime. The Aspire CLI remains conditional on an Aspire app. A project moving from dev to nightly must be registered again and start with a new siding and data baseline. Do not copy `.shunt-dev` state or migrate a dev siding.

## Git

Use the GitButler flow from the global rules (this repo lives on `gitbutler/workspace`; commit with `but`). Sidings themselves are plain-git worktrees — see the global git rule for the carve-out.

## Local pre-push integration check

Before pushing, run the hardware-dependent integration suite locally:

```bash
SHUNT_CONTAINER_INTEGRATION=1 go test -p 1 -tags integration ./... -count=1 -timeout 30m
```

This requires a running Apple container service on macOS 26+ Apple silicon. CI intentionally does not run this suite.

## Test fixtures

Two real Aspire repos are used to validate the infra and must never appear in shipped docs/README/help: `acme/SampleApp` (fast) and `acme/MyApp` (heavy). They exist only to prove shunt works end to end.
