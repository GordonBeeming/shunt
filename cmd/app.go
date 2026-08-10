package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/contract"
	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/runner"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

var (
	appAddEnsureControl = fsclone.EnsureControlRepo
	appAddPrepareCaddy  = func(ctx context.Context) (*caddy.Admin, error) {
		admin := caddy.NewAdmin()
		if err := admin.Ping(ctx); err != nil {
			return nil, err
		}
		return admin, nil
	}
	appAddDeleteRoute = func(ctx context.Context, admin *caddy.Admin, path string) error {
		return admin.Delete(ctx, path)
	}
	appAddEnsureFrontDoor = caddy.EnsureFrontDoor
	appAddSnapshotCaddy   = caddy.SnapshotRoutes
	appAddRestoreCaddy    = caddy.RestoreRoutes
	appAddPointCaddy      = siding.PointCaddy
	appAddSaveApp         = state.SaveApp
	appAddUpdateRegistry  = state.UpdateRegistry
	appAddRollbackState   = func(updating bool, existing state.App, published state.App) error {
		if updating {
			return state.SaveApp(existing)
		}
		err := os.Remove(filepath.Join(published.ConfigDir, "state-v2.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
)

// appAddRecoveryError reports the one split-publication case that is safe to
// retry: app state and Caddy are already visible, but the registry update did
// not publish. Its text deliberately omits internal state paths while Unwrap
// retains both causes for diagnostics and errors.Is/errors.As.
type appAddRecoveryError struct {
	appPublication      error
	registryPublication error
	command             string
}

func (e *appAddRecoveryError) Error() string {
	return fmt.Sprintf("app state and Caddy routes are visible, but registration is incomplete because state durability was unconfirmed and the channel registry update did not publish; run `%s` again from this repository to safely finish the idempotent registration", e.command)
}

func (e *appAddRecoveryError) Unwrap() []error {
	return []error{e.appPublication, e.registryPublication}
}

func newAppCmd() *cobra.Command {
	c := &cobra.Command{Use: "app", Short: "Manage registered apps"}
	c.AddCommand(newAppAddCmd())
	c.AddCommand(newAppSwitchCmd())
	return c
}

func newAppAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add",
		Short: "Register the app in the current repo (reads .shunt.app.json)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			loc, err := resolve.From(cwd)
			if err != nil {
				return err
			}
			ct, err := contract.Load(cwd)
			if err != nil {
				return err
			}

			// Contract discovery and any interactive runner prompt happen before the
			// project lock. The lock only covers the fresh-state and Caddy mutation.
			runnerKind, startCmd, workdir := ct.Runner, ct.Start, ct.Workdir
			if runnerKind == "" {
				det := runner.Detect(cwd)
				runnerKind = det.Kind
				if startCmd == "" {
					startCmd = det.Start
				}
				if workdir == "" {
					workdir = det.Workdir
				}
			}
			if runnerKind != runner.Aspire && startCmd == "" {
				startCmd, err = promptStartCommand(loc.Project)
				if err != nil {
					return err
				}
				runnerKind = runner.Custom
			}
			repoOrigin := gitOrigin(ctx, cwd)

			return siding.WithProjectOperation(ctx, loc.ConfigDir, func() error {
				// Re-running app add updates the registration from the contract
				// (e.g. new prebakeImages or front-door routes), preserving sidings.
				existing, existErr := state.LoadApp(loc.ConfigDir)
				if existErr != nil && !errors.Is(existErr, state.ErrNotFound) {
					return existErr
				}
				updating := existErr == nil
				if updating {
					if err := siding.EnsureNoRemovalInProgress(existing, "update app registration"); err != nil {
						return err
					}
					if err := validateBaselineVolumeChange(ctx, existing, ct.Volumes); err != nil {
						return err
					}
				}
				controlRepoPath := filepath.Join(loc.ConfigDir, ".control.git")
				originalRepoPath := cwd
				if updating {
					if existing.ControlRepoPath != "" {
						controlRepoPath = existing.ControlRepoPath
					}
					if existing.RepoPath != "" {
						originalRepoPath = existing.RepoPath
					}
					if repoOrigin == "" {
						repoOrigin = existing.RepoOrigin
					}
				}
				seedCommit, err := appAddEnsureControl(ctx, controlRepoPath, cwd, repoOrigin, "")
				if err != nil {
					return fmt.Errorf("initialize managed Git control repository: %w", err)
				}

				app := state.App{
					Version:         state.StateVersion,
					Name:            loc.Project,
					RepoPath:        originalRepoPath,
					RepoOrigin:      repoOrigin,
					ControlRepoPath: controlRepoPath,
					BaseCommit:      seedCommit,
					Runner:          runnerKind,
					Start:           startCmd,
					Stop:            ct.Stop,
					Workdir:         workdir,
					AppHostPath:     ct.AppHost,
					ConfigDir:       loc.ConfigDir,
					Env:             ct.Env,
					Mounts:          ct.Mounts,
					PrebakeImages:   ct.PrebakeImages,
					PrebakeBuilds:   ct.PrebakeBuilds,
					Volumes:         ct.Volumes,
					Memory:          ct.Memory,
					CPUs:            ct.CPUs,
					HealthPort:      ct.HealthPort,
					HealthPath:      ct.HealthPath,
					DisableCache:    ct.DisableCache,
					Sidings:         map[string]state.Siding{},
				}
				if updating {
					app.Sidings = existing.Sidings
					app.BaseSiding = existing.BaseSiding
					if existing.BaseCommit != "" {
						app.BaseCommit = existing.BaseCommit
					}
					app.Removal = existing.Removal
					app.LiveSiding = existing.LiveSiding
					if app.LiveSiding == state.HostTarget {
						app.LiveSiding = ""
					}
				}
				state.EnsureV2(&app)
				assigned := map[int]bool{}
				for _, r := range ct.FrontDoor {
					port := r.ListenPort
					if !ct.FixedPorts {
						port = 0
						if updating {
							port = existingRoutePort(existing, r.Key, r.Kind)
						}
						if port == 0 {
							port, err = freePort(assigned)
							if err != nil {
								return err
							}
						}
					}
					assigned[port] = true
					app.FrontDoor = append(app.FrontDoor, siding.RouteFromContract(loc.Project, r, port))
				}

				admin, err := appAddPrepareCaddy(ctx)
				if err != nil {
					return fmt.Errorf("caddy admin API not reachable — run `"+bin()+" init` first: %w", err)
				}
				affectedRoutes := append([]state.Route(nil), app.FrontDoor...)
				if updating {
					affectedRoutes = append(affectedRoutes, existing.FrontDoor...)
				}
				snapshot, err := appAddSnapshotCaddy(ctx, admin, app.Name, affectedRoutes)
				if err != nil {
					return err
				}
				rollbackCaddy := func(cause error) error {
					if rollbackErr := restoreAppAddCaddy(ctx, admin, snapshot); rollbackErr != nil {
						return errors.Join(cause, fmt.Errorf("restore Caddy routes after app registration failure: %w", rollbackErr))
					}
					return cause
				}
				if updating {
					keep := map[string]bool{}
					for _, r := range app.FrontDoor {
						keep[r.Kind+"/"+r.Key] = true
					}
					for _, r := range existing.FrontDoor {
						if keep[r.Kind+"/"+r.Key] {
							continue
						}
						if p, _, perr := caddy.ServerForRoute(loc.Project, r, false); perr != nil {
							return rollbackCaddy(perr)
						} else if snapshot.Contains(p) {
							if err := appAddDeleteRoute(ctx, admin, p); err != nil {
								return rollbackCaddy(fmt.Errorf("remove obsolete Caddy route %q: %w", r.Key, err))
							}
						}
					}
				}
				if err := appAddEnsureFrontDoor(ctx, admin, app); err != nil {
					return rollbackCaddy(err)
				}

				if updating && app.LiveSiding != "" {
					if sd, ok := app.Sidings[app.LiveSiding]; ok && len(sd.Bridges) > 0 {
						if err := appAddPointCaddy(ctx, app, &sd); err != nil {
							return rollbackCaddy(fmt.Errorf("restore live Caddy routes for %q: %w", app.LiveSiding, err))
						}
						app.Sidings[app.LiveSiding] = sd
					}
				}

				var durabilityErrs []error
				var appPublicationErr error
				if err := appAddSaveApp(app); err != nil {
					var committed *state.CommittedDurabilityError
					if !errors.As(err, &committed) {
						return rollbackCaddy(fmt.Errorf("publish app state: %w", err))
					}
					appPublicationErr = err
					durabilityErrs = append(durabilityErrs, fmt.Errorf("app state publication durability: %w", err))
				}
				_, registryErr := appAddUpdateRegistry(ctx, func(reg *state.Registry) error {
					reg.Projects[app.Name] = app.ConfigDir
					return nil
				})
				if registryErr != nil {
					var committed *state.CommittedDurabilityError
					if errors.As(registryErr, &committed) {
						durabilityErrs = append(durabilityErrs, fmt.Errorf("registry publication durability: %w", registryErr))
					} else if appPublicationErr != nil {
						return &appAddRecoveryError{
							appPublication:      appPublicationErr,
							registryPublication: registryErr,
							command:             bin() + " app add",
						}
					} else {
						rollbackErr := appAddRollbackState(updating, existing, app)
						caddyErr := restoreAppAddCaddy(ctx, admin, snapshot)
						joined := []error{fmt.Errorf("publish app registry: %w", registryErr)}
						if rollbackErr != nil {
							joined = append(joined, fmt.Errorf("roll back app state after registry publication failure: %w", rollbackErr))
						}
						if caddyErr != nil {
							joined = append(joined, fmt.Errorf("restore Caddy routes after registry publication failure: %w", caddyErr))
						}
						return errors.Join(joined...)
					}
				}

				verb := "registered"
				if updating {
					verb = "updated"
				}
				ports := "random free ports"
				if ct.FixedPorts {
					ports = "fixed ports"
				}
				fmt.Printf("%s %s %s (runner: %s, %d front-door routes, %s)\n", tick(), verb, app.Name, app.Runner, len(app.FrontDoor), ports)
				for _, r := range app.FrontDoor {
					fmt.Printf("  %-10s %-6s localhost:%d  ->  %s/%s\n", r.Key, r.Kind, r.ListenPort, r.Resource, r.Endpoint)
				}
				fmt.Println("next: `" + bin() + " new <name>` to create a siding")
				return errors.Join(durabilityErrs...)
			})
		},
	}
}

func restoreAppAddCaddy(ctx context.Context, admin *caddy.Admin, snapshot caddy.RouteSnapshot) error {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return appAddRestoreCaddy(restoreCtx, admin, snapshot)
}

func validateBaselineVolumeChange(ctx context.Context, existing state.App, requested []string) error {
	if sameVolumeNames(existing.Volumes, requested) || len(existing.Volumes) == 0 {
		return nil
	}
	baseline, err := databaseline.New(existing.ConfigDir, existing.Volumes)
	if err != nil {
		return fmt.Errorf("inspect existing data baseline: %w", err)
	}
	initialized, err := baseline.Initialized(ctx)
	if err != nil {
		return fmt.Errorf("inspect existing data baseline: %w", err)
	}
	if initialized {
		return errors.New("dataVolumes cannot change after a data baseline is initialized; keep the existing volume set or remove the baseline deliberately before re-registering the app")
	}
	return nil
}

func sameVolumeNames(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]struct{}, len(left))
	for _, name := range left {
		seen[name] = struct{}{}
	}
	for _, name := range right {
		if _, ok := seen[name]; !ok {
			return false
		}
	}
	return true
}

// freePort returns a free TCP port on the host not already in used.
func freePort(used map[int]bool) (int, error) {
	for i := 0; i < 200; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		if !used[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("could not find a free port")
}

// existingRoutePort returns the port already assigned to a route (by key+kind)
// so re-running app add keeps stable ports; 0 if not found.
func existingRoutePort(app state.App, key, kind string) int {
	for _, r := range app.FrontDoor {
		if r.Key == key && r.Kind == kind {
			return r.ListenPort
		}
	}
	return 0
}

// promptStartCommand asks (interactively) how to start an app shunt couldn't
// classify; errors in non-interactive mode so CI declares it in the contract.
func promptStartCommand(project string) (string, error) {
	if fi, _ := os.Stdout.Stat(); fi == nil || fi.Mode()&os.ModeCharDevice == 0 {
		return "", fmt.Errorf("could not detect how to start %q — set `runner` + `start` in .shunt.app.json", project)
	}
	fmt.Printf("shunt couldn't detect how to start %q.\nWhat command starts it (run from the repo root)?\n> ", project)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	cmd := strings.TrimSpace(line)
	if cmd == "" {
		return "", fmt.Errorf("no start command given — set `runner` + `start` in .shunt.app.json")
	}
	return cmd, nil
}
