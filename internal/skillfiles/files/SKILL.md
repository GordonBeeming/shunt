---
name: shunt
description: Run and edit an app in a shunt "siding": a small managed worktree that grows into an isolated Apple container guest only when needed. Use when starting substantial work in a shunt-managed repo, choosing or creating a worktree, switching isolated instances, warming the dependency-image cache, or testing changes without sharing an application environment. Triggers include "use shunt", "work in a siding", "spin up an instance", "switch instances", "isolated dev/track", or beginning substantial work in a repo that has a .shunt.app.json.
---

# shunt — isolated dev

shunt gives each experiment ("siding") its own managed Git worktree. A siding starts as source only; `up` materializes its data, output directory, and isolated Apple `container` guest when the application is actually needed. You edit the worktree on macOS and the guest runs it live behind stable local ports. There is no executable host target: in a registered app, application work belongs in a siding.

Apple `container` is the only host runtime prerequisite; Docker Desktop and OrbStack are not dependencies. Docker runs inside the guests. The contract's tag-only `prebakeImages` and typed `prebakeBuilds` declarations are authoritative: `warm` refreshes every registry tag and rebuilds every local image into immutable per-image cache generations under a `0700` content-addressed cache directory. Lifecycle commands assure the exact content-versioned base image locally plus the host dependency cache, load only missing or changed refs, then update the guest marker. A siding never pulls live; an undeclared or unavailable image fails startup. Publication collects unreachable cache data automatically; a cleanup failure is a warning and does not undo the published generation. `warm gc --dry-run` previews the same collector. `warm --from` bounds guest output and archive expansion to the configured cache budget.

This installed skill is scoped to **`{{shunt-command}}`**. Its commands, operational paths, and setup guidance match that channel. Full command list and the `.shunt.app.json` contract are in [references/commands.md](references/commands.md); the fast iteration loop, cache behavior, and data-baseline recovery are in [references/iterating.md](references/iterating.md).

## First-time setup

On Apple Container 1.2.2, Shunt enables writable `/proc/sys` only when recreating its privileged dockerd guest; ordinary container invocations keep the runtime's default read-only paths.

{{shunt-channel-onboarding}}

{{shunt-nightly-migration}}

For a host/base SDK refresh, upgrade or select a supported host SDK, run `{{shunt-command}} init --force`, then run plain `{{shunt-command}} reapply <siding>` and `{{shunt-command}} up <siding>` for each existing guest. Plain `reapply` preserves worktree, branch, and data; never use `--fresh-data` for an SDK-only refresh.

## When you start a substantial dev task

1. Run `bash scripts/status.sh` (or `{{shunt-command}} active --json`) from the repository root, any nested directory, or a siding worktree to check for durable Shunt state and list its sidings. `managed: true, active: false, registered: false` is a valid worktree-only project: select or create a siding normally, but run `app add` before the first guest. `active` retains its compatibility meaning of a registered app; use `managed` to detect any durable Shunt state.
2. **Before planning the work or running a mutating siding command, ask the user which siding should own the work, using the interactive question tool.** Present the existing names plus **a new siding**. Do not offer the registration checkout as an application work target: shunt has no executable host target.
   - *existing* → use the selected siding and work only in its `src` path.
   - *new* → ask for a name, then run `{{shunt-command}} new <name>`. In any Git repository, this creates only the persistent managed branch and worktree state—no app registration, registry entry, data, output directory, image load, guest, or application. Default `new` starts at the clean base siding's committed HEAD (or the saved zero-siding base commit). Use `--branch <ref>` only for an explicit alternate start point, or `--from <branch>` to continue an existing remote branch.
   - Registration is optional while the work stays in the worktree. Before the first guest is needed, add `.shunt.app.json` and run `{{shunt-command}} app add`; this enriches the same project state and preserves its sidings.
3. Build and test on the host, in the siding worktree, before `up` (`dotnet build`/`dotnet test`, `pnpm build`, and so on). Documentation, config, and other worktree-only changes may never need a guest. Read [the siding runs the app; tests run on the host](#the-siding-runs-the-app-tests-run-on-the-host) first: a host build against a siding checkout has a cost inside the guest.
4. When the app is needed, make sure `.shunt.app.json` has been registered with `{{shunt-command}} app add`, **ask the user before materializing the guest**, then run `{{shunt-command}} up <name> --no-bridge`. This is the step that grows the siding through data and guest phases and consumes runtime resources. Once healthy, plain `{{shunt-command}} up <name>` bridges it without moving the front door. **Confirm again before** `{{shunt-command}} switch <name>` (or use `up <name> --switch` when going live is already approved). The app always runs in the guest. Hot reload handles most edits; `{{shunt-command}} restart <name>` rebuilds the app without dropping the environment. Use `{{shunt-command}} park <name>` to release a non-live guest while keeping its worktree, data, and output. See [iterating](references/iterating.md).

## Rules

- **Never choose the work-owning siding or materialize its guest without the user's explicit choice via the question tool.** Creating a worktree-only siding is cheap; `up` is the resource boundary because it allocates data and a whole guest with dependency images.
- Code edits for a registered app go in the chosen siding's `src`, not the registration checkout. There is no native host application target.
- Treat .NET user secrets as **host-global, not worktree-local**. Every checkout and siding worktree with the same `<UserSecretsId>` resolves to the same host store under `~/.microsoft/usersecrets`, so never run `dotnet user-secrets set`, `remove`, or `clear` to customize a siding without the user's explicit approval — it changes the values seen by the registration checkout and every other siding too. Keep user-secrets mounts read-only (`"readOnly": true`; string-form mounts already default to read-only). That protects the store from commands inside the guest, but not from host-side `dotnet user-secrets` commands run in the worktree. If a siding genuinely needs different secret-backed values and the app has no isolated configuration path, stop and tell the user; shunt does not currently provide a per-siding user-secrets store. See [iterating](references/iterating.md).
- A siding `src` is a **git worktree on its own branch**, owned by Shunt's hidden independent control repository. Default `new` seeds from the clean source base's committed HEAD; the first siding becomes base, `{{shunt-command}} base set <siding>` changes it, and the saved commit survives when zero sidings remain. `--branch <ref>` explicitly overrides the start point, while `--from <branch>` fetches and continues an existing remote branch. **Commit + push with `{{shunt-command}} git commit …` / `{{shunt-command}} git push …`, not raw `git`** — they run host git in the right worktree (resolved from cwd or the live siding), sign with the repo's usual key, and push the siding branch, so you never hand-type the worktree path or fumble the push refspec. A fresh siding's branch has no upstream, so the **first** push must set one: `{{shunt-command}} git push -u origin <branch>` (args pass straight through to git); after that a bare `{{shunt-command}} git push` works. Read-only `git` (`status`/`log`/`diff`) in the worktree is fine. (Inside the guest git won't resolve the worktree — edit + commit on macOS.)
- Cleanup is permanent. `{{shunt-command}} cleanup` lists the current project's sidings, supports selecting several, and marks protected work. Committed work is preserved only when local refs prove reachability, strict rebased/cherry-picked patch equivalence, or one whole-branch squash match; ambiguous, partial, detached, missing, or over-limit histories stay protected. Recorded and actually checked-out branches are proved independently before their exact local refs are retired. If a selected siding is dirty or has unpreserved commits, stop at the confirmation prompt and ask the user before answering yes or rerunning with `-f`/`--force`. Never infer permission to discard changes from a general cleanup request. Force also allows removal of the live siding, so call that out and get explicit approval before using it; otherwise switch away first. Removing the source base with survivors also needs an explicit successor (`--next-base`) or the interactive prompt. Removing the final siding pins its committed HEAD and promotes a complete materialized volume set as the durable baseline; a worktree-only final siding preserves the existing baseline.
- Treat a reported active removal journal as authoritative. Source, lifecycle, data, sync/run/playwright, and trim mutations are fenced while removal resumes from its durable checkpoint; do not bypass the fence or manually delete partial resources. Shunt records exact branch OIDs and preservation witnesses, then creates deterministic `refs/shunt/recovery/...` refs before destructive stages. These tiny hidden refs remain permanently as durable work archives after successful removal; this is the deliberate tradeoff for crash-safe preservation. Downgrading Shunt while a removal journal is active is unsupported.
- Use `{{shunt-command}} space` for disk evidence. Text and JSON retain the literal unique-commit count and add conservative preservation evidence for both recorded and checked-out refs. Its project/siding scans are logical and clone-overlapping, the control repo and image cache are explicit managed rows, protected baseline rows are not cleanup candidates, and legacy/unknown entries remain unclassified until ownership is proven. Only the official Apple `container system df` observation may be described as reclaimable. Use `trim --dry-run` before `trim --yes`; trim rechecks the confirmed candidate identities and Git eligibility under the siding/removal lock.
- Never start the app locally (e.g. `aspire start`, `dotnet run`, `pnpm dev`). Use `up` / `restart`.
- Declare registry dependency tags in `prebakeImages` and local image builds in `prebakeBuilds`; normal lifecycle commands load only missing or changed refs, update the marker, and refuse application startup until every declared tag loads and inspects successfully. Use `warm` to refresh tags and rebuild local declarations. Re-run `{{shunt-command}} app add` from the registration checkout after changing the shared `.shunt.app.json`. See [iterating](references/iterating.md).

## When shunt itself misbehaves

Report it instead of working around it. A workaround you keep to yourself leaves the bug in place for everyone who hits it next, and shunt problems are usually environment-level rather than something your project can fix.

First check whether an agent session named `shunt-admin` is running — in Claude Code, list the agents you can reach. That session maintains shunt and can change the CLI, the guest image, or this skill directly.

**If `shunt-admin` is running**, message it like you would file a support ticket, and say outright that you are blocked and want it to **message you back once it is fixed so you can continue**. Give it:

- the channel and binary (`{{shunt-command}}`) and its `version` output
- the project, the siding, and whether the guest was running at the time
- the exact command and the exact error text, quoted rather than paraphrased
- what you already checked in the guest (`{{shunt-command}} run <siding> …`), and what you ruled out
- whether it reproduces on a fresh siding or only that one

Vague reports cost a round trip, and the person reading it cannot see your terminal. While you wait, carry on with anything the bug does not block, and do not silently change your approach to dodge it.

**If `shunt-admin` is not running**, ask the user with your question tool and let them choose between two options:

1. They start the `shunt-admin` agent, and you then send the ticket above.
2. You log it as a bug at https://github.com/GordonBeeming/shunt/issues instead.

That choice is the user's. Don't start the agent yourself and don't open an issue without being asked to.

## Data baseline workflow

`dataVolumes` starts with an empty source. To make a siding's complete volume set the source for later work, run `{{shunt-command}} data promote <siding>` after it quiesces the app and every volume consumer. Future first materializations (`up`) and `{{shunt-command}} reapply <siding> --fresh-data` rebuild from the promoted generation; worktree-only `new` does not touch data, already-materialized siding copies remain unchanged, and plain `reapply` preserves the siding copy. If the promotion is bad, run `{{shunt-command}} data rollback` before rebuilding affected sidings. The detailed recovery flow is in [iterating](references/iterating.md).

## The siding runs the app; tests run on the host

A siding exists to run the **app** in isolation. Test suites are not part of that split. Run them on the
host, the same as you would without shunt. That applies to the whole suite, integration tests included.

**The guest refuses image pulls, by design, and that is what breaks a Testcontainers suite.** A fixture
whose image is already present can still start; one that has to pull dies before its first test:

```
Docker API responded with status code=Forbidden,
response={"message":"shunt offline policy rejects Docker image pulls"}
```

The signature is a whole test project failing in milliseconds. Across a repository with many integration
projects that reads as thousands of failures and total breakage. It is neither. Every message is
identical, and that uniformity is the tell.

Three things make it easy to misdiagnose, and all three point at "run it on the host" rather than at a fix
inside the guest:

- **`docker ps` and `docker version` in the guest look healthy.** The daemon runs and the app's own
  containers are up. Only *pulls* are refused, so a check that stops at "is Docker working" concludes the
  wrong thing. Read the error text; it names the policy.
- **Prebaked images do not make the suite runnable.** `prebakeImages` and `prebakeBuilds` cover what the
  *app* needs. A suite pulling its own database image is reaching for something the cache was never meant
  to hold.
- **`TESTCONTAINERS_RYUK_DISABLED=true` gets some suites further, which is a trap.** It skips the reaper
  sidecar, so a suite whose database image happens to be prebaked then passes and the rest still fail. A
  partly-green run invites the conclusion that the remaining reds are real. They are not.

A host `dotnet` command in a siding worktree overwrites the bind-mounted `obj/` with host paths, so the
guest's next build fails with `NETSDK1064` and the running app can die with `Exec format error`. That is
the cost of a host test run against a siding checkout, and the repair is a restore and rebuild inside the
guest afterwards.

**A killed run leaks its containers, and the reaper does not save you.** Testcontainers starts a
`ryuk` sidecar to clean up after the run, but ryuk is a child of that run. Cancel a suite, or let it
time out, and ryuk dies with it holding nothing to reap against, so the SQL Server, Azurite and
smtp4dev it was meant to remove stay up indefinitely. A clean exit reaps correctly; a kill does not.

That matters on a shared machine, because the memory is not returned and the next person's suite
fails to start with a timeout that looks like their own code. Check with `docker ps` before
concluding a failure is yours, and reclaim with `docker container prune`.

Read a container census as valid only at the instant it is taken. Runs finish underneath you, so a
list gathered a few minutes ago will name containers that have since exited and miss ones that
started. Measure and act in the same breath, and never kill a container without confirming it has no
live parent process.

## Browser automation & recording

Guests bake in headless Chromium, `playwright-cli`, and the `playwright` Node library, so a browser session can run **inside** a siding instead of on the host. `{{shunt-command}} playwright [siding] [args...]` execs `playwright-cli <args>` in the guest, stdio passed through — every arg goes straight to `playwright-cli` (the command has no flags of its own beyond `-h`). Siding resolution matches `run`: the one your cwd is inside, else a leading arg naming a siding, else the live siding. The image's global playwright-cli config also pins the browser (`chromium`, sandbox off), so a bare `playwright-cli open <url>` just works — no `--browser` flag needed. See [references/commands.md](references/commands.md) for the full command surface.

- **Drive the browser where the app runs.** This is browser automation against the running app, not the test suite: unit and integration suites still run on the host, per [the siding runs the app; tests run on the host](#the-siding-runs-the-app-tests-run-on-the-host). Only one siding is ever live on the shared front door, so a macOS-side browser can only reach that one app; in-guest, each siding's own `https://localhost:<port>` is the app's real address (not a host-forwarded port), so N materialized sidings can be browser-tested and recorded in parallel. Use in-guest sessions for QA evidence and a macOS-side browser for a narrated, polished demo of the siding currently live at the front door.
- **Sessions persist.** The guest is long-lived, so the browser stays open between `{{shunt-command}} playwright` calls, the same stateful behaviour a host Playwright session has.
- **Recordings.** Auto-named output (snapshots, traces, `video-start`/`video-stop` without `--filename`) lands under `/out/playwright` in the guest, which is `<siding>/out/playwright` on the host: the standing per-siding output directory, outside the worktree and never committed. An explicit `--filename` resolves relative to the guest's cwd (the worktree) instead, so pass an explicit `/out/...` path if you want a named file to land there too. That redirect is set once, when the browser session **opens**, and sticks for the session's whole life. Open through `{{shunt-command}} playwright` — a session opened via a manual `{{shunt-command}} run <siding> playwright-cli open …` never gets it, so its auto-named output (video included) stays in the worktree's `.playwright-cli/` for as long as that session runs, even once you switch back to `{{shunt-command}} playwright` for later calls on it.
- **TLS just works — through playwright-cli's own config, not NSS.** The dev cert is a self-signed leaf, so Chromium's verifier won't accept it as a trust anchor no matter what the guest's NSS store says (NSS + the system CA bundle cover .NET `HttpClient` and other non-Chromium consumers, not Chromium). Instead the image bakes `--allow-insecure-localhost` into playwright-cli's global config, applied to every invocation — including a manual `{{shunt-command}} run <siding> playwright-cli …` — scoped to literal `localhost`/`127.0.0.1` origins, which is all a guest app ever serves. A script that imports the `playwright` Node library directly bypasses that config, so it needs `args: ['--allow-insecure-localhost']` in its own launch options. Either way, don't reach for `ignoreHTTPSErrors`.
