package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSaveLoadAppRoundTrip(t *testing.T) {
	dir := t.TempDir()
	app := App{
		Name:        "myapp",
		RepoPath:    "/repo",
		AppHostPath: "src/App.csproj",
		ConfigDir:   dir,
		FrontDoor: []Route{
			{Key: "frontend", Kind: KindHTTP, ListenPort: 5000, Resource: "web", CaddyID: "app_myapp_http_frontend"},
		},
		Sidings: map[string]Siding{
			"exp1": {Name: "exp1", Container: "shuntdev_myapp_exp1", RSPort: 18890, Bridges: map[string]int{"frontend": 39001}},
		},
		LiveSiding: "exp1",
	}
	if err := SaveApp(app); err != nil {
		t.Fatal(err)
	}
	got, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "myapp" || got.LiveSiding != "exp1" || len(got.FrontDoor) != 1 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	s := got.Sidings["exp1"]
	if s.Bridges["frontend"] != 39001 || s.RSPort != 18890 {
		t.Errorf("siding roundtrip mismatch: %+v", s)
	}
}

func TestUpdateAppRollsBackCallbackFailure(t *testing.T) {
	dir := t.TempDir()
	app := App{ConfigDir: dir, Memory: "4g", Sidings: map[string]Siding{}}
	if err := SaveApp(app); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("injected update failure")
	_, err := UpdateApp(context.Background(), dir, func(current *App) error {
		current.Memory = "8g"
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("UpdateApp() error = %v", err)
	}
	loaded, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Memory != "4g" {
		t.Fatalf("failed update was persisted: %#v", loaded)
	}
}

func TestUpdateAppSurfacesPublicationFailure(t *testing.T) {
	dir := t.TempDir()
	app := App{ConfigDir: dir, Memory: "4g", Sidings: map[string]Siding{}}
	if err := SaveApp(app); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(dir, "state.json")
	if err := os.Remove(stateFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateFile, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateApp(context.Background(), dir, func(current *App) error {
		current.Memory = "8g"
		return nil
	}); err == nil {
		t.Fatal("UpdateApp did not surface a state publication failure")
	}
}

func TestWithLockSecuresExistingLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	lockPath := path + ".lock"
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := withLock(context.Background(), path, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock permissions = %o, want 600", got)
	}
}

func TestWithLockCancellationIdentifiesLockPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	locked, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	if err := syscall.Flock(int(locked.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(locked.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = withLock(ctx, path, func() error { return nil })
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), path+".lock") {
		t.Fatalf("withLock() error = %v", err)
	}
}

func TestLoadAppNotFound(t *testing.T) {
	if _, err := LoadApp(t.TempDir()); err == nil {
		t.Error("expected ErrNotFound for empty dir")
	}
}

func TestSaveAppRequiresConfigDir(t *testing.T) {
	if err := SaveApp(App{Name: "x"}); err == nil {
		t.Error("expected error when ConfigDir is empty")
	}
}

func TestRegistryFindProjectCaseInsensitive(t *testing.T) {
	reg := Registry{Projects: map[string]string{"HubX": "/cfg/HubX"}}
	cases := []struct {
		name          string
		wantCanonical string
		wantOK        bool
	}{
		{"HubX", "HubX", true}, // exact
		{"hubX", "HubX", true}, // cwd basename with different case (macOS)
		{"HUBX", "HubX", true}, // fold match
		{"Other", "", false},   // genuinely absent
	}
	for _, c := range cases {
		gotName, gotDir, ok := reg.FindProject(c.name)
		if ok != c.wantOK {
			t.Errorf("FindProject(%q) ok = %v, want %v", c.name, ok, c.wantOK)
		}
		if gotName != c.wantCanonical {
			t.Errorf("FindProject(%q) canonical = %q, want %q", c.name, gotName, c.wantCanonical)
		}
		if c.wantOK && gotDir != "/cfg/HubX" {
			t.Errorf("FindProject(%q) dir = %q, want /cfg/HubX", c.name, gotDir)
		}
	}
}
