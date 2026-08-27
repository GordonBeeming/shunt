package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/state"
)

// stubAppSwitchDependencies replaces every Caddy-touching seam in
// cmd/appswitch.go with a no-op, so tests exercise the command's own state
// changes without reaching a real Caddy admin API (none is running in CI, and
// a live one on this machine must never be touched by a unit test).
func stubAppSwitchDependencies(t *testing.T) func() {
	t.Helper()
	originalPrepare, originalDelete, originalPut := appSwitchPrepareCaddy, appSwitchDeleteRoute, appSwitchPutRoute
	originalDeleteIfExists := appSwitchDeleteRouteIfExists
	originalRemove, originalSwitchTo := appSwitchRemoveFrontDoor, appSwitchSwitchTo
	appSwitchPrepareCaddy = func(context.Context) (*caddy.Admin, error) { return nil, nil }
	appSwitchDeleteRoute = func(context.Context, *caddy.Admin, string) error { return nil }
	appSwitchPutRoute = func(context.Context, *caddy.Admin, string, []byte) error { return nil }
	appSwitchDeleteRouteIfExists = func(context.Context, *caddy.Admin, string) error { return nil }
	appSwitchRemoveFrontDoor = func(context.Context, *caddy.Admin, state.App) error { return nil }
	appSwitchSwitchTo = func(context.Context, *state.App, string) error { return nil }
	return func() {
		appSwitchPrepareCaddy, appSwitchDeleteRoute, appSwitchPutRoute = originalPrepare, originalDelete, originalPut
		appSwitchDeleteRouteIfExists = originalDeleteIfExists
		appSwitchRemoveFrontDoor, appSwitchSwitchTo = originalRemove, originalSwitchTo
	}
}

// appSwitchStateFixture points state and the channel registry at a fresh temp
// HOME (like appAddFixture) without requiring a real repo on disk — every test
// here drives state.App records directly rather than through `app add`.
func appSwitchStateFixture(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
}

func registerApp(t *testing.T, app state.App) {
	t.Helper()
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	if _, err := state.UpdateRegistry(context.Background(), func(reg *state.Registry) error {
		reg.Projects[app.Name] = app.ConfigDir
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func captureStdout(t *testing.T, run func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := run()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

func TestAppSwitchReleaseFreesRoutesAndMarksReleased(t *testing.T) {
	appSwitchStateFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	configDir := t.TempDir()
	app := state.App{
		Version:   state.StateVersion,
		Name:      "Alpha",
		ConfigDir: configDir,
		FrontDoor: []state.Route{
			{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Alpha_http_web"},
			{Key: "api", Kind: state.KindHTTP, ListenPort: 4101, CaddyID: "app_Alpha_http_api"},
		},
	}
	registerApp(t, app)

	var removedFor state.App
	appSwitchRemoveFrontDoor = func(_ context.Context, _ *caddy.Admin, a state.App) error {
		removedFor = a
		return nil
	}

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{"Alpha", "--release"})
	out, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
	if err != nil {
		t.Fatalf("release: %v (output=%s)", err, out)
	}
	if removedFor.Name != "Alpha" || len(removedFor.FrontDoor) != 2 {
		t.Fatalf("RemoveFrontDoor was not called with the full route set: %#v", removedFor)
	}
	saved, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.FrontDoorReleased {
		t.Fatal("FrontDoorReleased was not set")
	}
	for _, want := range []string{"released Alpha front door", "freed :4100 :4101", "reclaim with", "app switch Alpha"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestAppSwitchReleaseAlreadyReleasedIsANoOp(t *testing.T) {
	appSwitchStateFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	configDir := t.TempDir()
	app := state.App{
		Version:           state.StateVersion,
		Name:              "Alpha",
		ConfigDir:         configDir,
		FrontDoor:         []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Alpha_http_web"}},
		FrontDoorReleased: true,
	}
	registerApp(t, app)

	called := false
	appSwitchRemoveFrontDoor = func(context.Context, *caddy.Admin, state.App) error { called = true; return nil }

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{"Alpha", "--release"})
	out, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
	if err != nil {
		t.Fatalf("release: %v (output=%s)", err, out)
	}
	if called {
		t.Fatal("RemoveFrontDoor was called for an already-released app")
	}
	if !strings.Contains(out, "already released") {
		t.Fatalf("output = %q, want an already-released notice", out)
	}
}

func TestAppSwitchClaimClearsFrontDoorReleasedOnTarget(t *testing.T) {
	appSwitchStateFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	configDir := t.TempDir()
	app := state.App{
		Version:           state.StateVersion,
		Name:              "Alpha",
		ConfigDir:         configDir,
		FrontDoor:         []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Alpha_http_web"}},
		FrontDoorReleased: true,
	}
	registerApp(t, app)

	var claimed []string
	appSwitchPutRoute = func(_ context.Context, _ *caddy.Admin, path string, _ []byte) error {
		claimed = append(claimed, path)
		return nil
	}
	switchToCalled := false
	appSwitchSwitchTo = func(context.Context, *state.App, string) error { switchToCalled = true; return nil }

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{"Alpha"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed routes = %v, want 1", claimed)
	}
	saved, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if saved.FrontDoorReleased {
		t.Fatal("FrontDoorReleased was not cleared")
	}
	if switchToCalled {
		t.Fatal("switchTo was called for a target with no live siding")
	}
}

func TestAppSwitchClaimPointsAtLiveSiding(t *testing.T) {
	appSwitchStateFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	configDir := t.TempDir()
	app := state.App{
		Version:    state.StateVersion,
		Name:       "Alpha",
		ConfigDir:  configDir,
		FrontDoor:  []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Alpha_http_web"}},
		LiveSiding: "one",
		Sidings:    map[string]state.Siding{"one": {Name: "one"}},
	}
	registerApp(t, app)

	var switchedTo string
	appSwitchSwitchTo = func(_ context.Context, _ *state.App, target string) error {
		switchedTo = target
		return nil
	}

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{"Alpha"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if switchedTo != "one" {
		t.Fatalf("switchTo target = %q, want %q", switchedTo, "one")
	}
}

func TestAppSwitchClaimParksConflictingAppAndMarksItReleased(t *testing.T) {
	appSwitchStateFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	targetDir, otherDir := t.TempDir(), t.TempDir()
	target := state.App{
		Version:   state.StateVersion,
		Name:      "Alpha",
		ConfigDir: targetDir,
		FrontDoor: []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Alpha_http_web"}},
	}
	other := state.App{
		Version:   state.StateVersion,
		Name:      "Vite",
		ConfigDir: otherDir,
		FrontDoor: []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Vite_http_web"}},
	}
	registerApp(t, target)
	registerApp(t, other)

	var parkDeleted []string
	appSwitchDeleteRouteIfExists = func(_ context.Context, _ *caddy.Admin, path string) error {
		parkDeleted = append(parkDeleted, path)
		return nil
	}

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{"Alpha"})
	out, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
	if err != nil {
		t.Fatalf("claim: %v (output=%s)", err, out)
	}
	if len(parkDeleted) != 1 || !strings.Contains(parkDeleted[0], "srv_Vite_web") {
		t.Fatalf("park-loop deletes = %v, want exactly Vite's server", parkDeleted)
	}
	if !strings.Contains(out, "parked Vite/web") {
		t.Fatalf("output missing park notice: %s", out)
	}
	savedOther, err := state.LoadApp(otherDir)
	if err != nil {
		t.Fatal(err)
	}
	if !savedOther.FrontDoorReleased {
		t.Fatal("fully parked app's FrontDoorReleased was not set")
	}
}

func TestAppSwitchClaimLeavesFlagFalseOnAPartialPark(t *testing.T) {
	appSwitchStateFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	targetDir, otherDir := t.TempDir(), t.TempDir()
	target := state.App{
		Version:   state.StateVersion,
		Name:      "Alpha",
		ConfigDir: targetDir,
		FrontDoor: []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Alpha_http_web"}},
	}
	// Vite holds 2 ports; only :4100 conflicts with Alpha. Its :4101 keeps
	// serving after the switch, so it must not be reported as released.
	other := state.App{
		Version:   state.StateVersion,
		Name:      "Vite",
		ConfigDir: otherDir,
		FrontDoor: []state.Route{
			{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Vite_http_web"},
			{Key: "api", Kind: state.KindHTTP, ListenPort: 4101, CaddyID: "app_Vite_http_api"},
		},
	}
	registerApp(t, target)
	registerApp(t, other)

	var parkDeleted []string
	appSwitchDeleteRouteIfExists = func(_ context.Context, _ *caddy.Admin, path string) error {
		parkDeleted = append(parkDeleted, path)
		return nil
	}

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{"Alpha"})
	out, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
	if err != nil {
		t.Fatalf("claim: %v (output=%s)", err, out)
	}
	if len(parkDeleted) != 1 || !strings.Contains(parkDeleted[0], "srv_Vite_web") {
		t.Fatalf("park-loop deletes = %v, want only Vite's conflicting :4100 server", parkDeleted)
	}
	if strings.Contains(out, "parked Vite/api") {
		t.Fatalf("the non-conflicting route was parked: %s", out)
	}
	savedOther, err := state.LoadApp(otherDir)
	if err != nil {
		t.Fatal(err)
	}
	if savedOther.FrontDoorReleased {
		t.Fatal("a partially parked app must not be reported as released — its :4101 route still serves")
	}
}

func TestAppSwitchClaimFailsWhenParkingDeleteFails(t *testing.T) {
	appSwitchStateFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	targetDir, otherDir := t.TempDir(), t.TempDir()
	target := state.App{
		Version:   state.StateVersion,
		Name:      "Alpha",
		ConfigDir: targetDir,
		FrontDoor: []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Alpha_http_web"}},
	}
	other := state.App{
		Version:   state.StateVersion,
		Name:      "Vite",
		ConfigDir: otherDir,
		FrontDoor: []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Vite_http_web"}},
	}
	registerApp(t, target)
	registerApp(t, other)

	sentinel := errors.New("caddy returned 500")
	appSwitchDeleteRouteIfExists = func(context.Context, *caddy.Admin, string) error { return sentinel }

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{"Alpha"})
	out, err := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if strings.Contains(out, "parked Vite/web") {
		t.Fatalf("a failed delete was reported as a successful park: %s", out)
	}
	savedOther, err := state.LoadApp(otherDir)
	if err != nil {
		t.Fatal(err)
	}
	if savedOther.FrontDoorReleased {
		t.Fatal("a failed delete must not record the app as released")
	}
}

func TestAppSwitchClaimWarnsAndContinuesOnAnUnreadableRegistryEntry(t *testing.T) {
	appSwitchStateFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	targetDir := t.TempDir()
	registerApp(t, state.App{
		Version:   state.StateVersion,
		Name:      "Alpha",
		ConfigDir: targetDir,
		FrontDoor: []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_Alpha_http_web"}},
	})
	// Register "Broken" pointing at a config dir with no state file at all —
	// the same shape as a corrupted or half-written registry entry.
	if _, err := state.UpdateRegistry(context.Background(), func(reg *state.Registry) error {
		reg.Projects["Broken"] = t.TempDir()
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{"Alpha"})
	var stderr string
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	out, runErr := captureStdout(t, func() error { return cmd.ExecuteContext(context.Background()) })
	w.Close()
	os.Stderr = oldStderr
	errOut, _ := io.ReadAll(r)
	stderr = string(errOut)
	if runErr != nil {
		t.Fatalf("claim: %v (stdout=%s stderr=%s)", runErr, out, stderr)
	}
	if !strings.Contains(stderr, "Broken") {
		t.Fatalf("stderr missing a warning naming the unreadable app: %q", stderr)
	}
}

func TestAppSwitchArgsRequireAnAppNameWithoutRelease(t *testing.T) {
	appSwitchStateFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{})
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error for a bare `app switch` with no app name and no --release")
	}
}

func TestAppSwitchReleaseWithNoArgumentResolvesFromCwd(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	app := state.App{
		Version:   state.StateVersion,
		Name:      "sample",
		ConfigDir: configDir,
		FrontDoor: []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 3000, CaddyID: "app_sample_http_web"}},
	}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	withWorkingDirectory(t, repo)

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{"--release"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.FrontDoorReleased {
		t.Fatal("FrontDoorReleased was not set from the cwd-resolved app")
	}
}

func TestAppSwitchClaimFailsFastWhenCaddyIsUnreachable(t *testing.T) {
	appSwitchStateFixture(t)
	restore := stubAppSwitchDependencies(t)
	defer restore()

	sentinel := errors.New("caddy not reachable")
	appSwitchPrepareCaddy = func(context.Context) (*caddy.Admin, error) { return nil, sentinel }

	configDir := t.TempDir()
	registerApp(t, state.App{Version: state.StateVersion, Name: "Alpha", ConfigDir: configDir})

	cmd := newAppSwitchCmd()
	cmd.SetArgs([]string{"Alpha"})
	if err := cmd.ExecuteContext(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}
