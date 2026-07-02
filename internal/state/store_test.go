package state

import (
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
		{"HubX", "HubX", true},   // exact
		{"hubX", "HubX", true},   // cwd basename with different case (macOS)
		{"HUBX", "HubX", true},   // fold match
		{"Other", "", false},     // genuinely absent
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
