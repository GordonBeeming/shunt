package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestActiveDiscoversWorktreeOnlyStateFromRepoNestedDirAndSiding(t *testing.T) {
	repo, configDir, _ := newShellOnlyCommandRepo(t)
	withWorkingDirectory(t, repo)
	cmd := newNewCmd()
	cmd.SetArgs([]string{"shell"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	nestedRepoDir := filepath.Join(repo, "one", "two")
	if err := os.MkdirAll(nestedRepoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	sidingSrc := filepath.Join(configDir, "shell", "src")

	tests := []struct {
		name      string
		cwd       string
		cwdSiding string
	}{
		{name: "repository root", cwd: repo},
		{name: "nested repository directory", cwd: nestedRepoDir},
		{name: "siding worktree", cwd: sidingSrc, cwdSiding: "shell"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := activeResultForDir(context.Background(), tt.cwd)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Managed || got.Active || got.Registered {
				t.Fatalf("managed/active/registered = %t/%t/%t, want true/false/false: %#v", got.Managed, got.Active, got.Registered, got)
			}
			if got.Project != app.Name || got.ConfigDir != configDir || got.RepoPath != repo || got.Siding != tt.cwdSiding {
				t.Fatalf("resolved project = %#v", got)
			}
			if len(got.Sidings) != 1 || got.Sidings[0].Name != "shell" || got.Sidings[0].Src != sidingSrc {
				t.Fatalf("discovered sidings = %#v", got.Sidings)
			}
		})
	}
}

func TestActivePreservesRegisteredProjectDiscovery(t *testing.T) {
	repo, configDir, _ := newShellOnlyCommandRepo(t)
	withWorkingDirectory(t, repo)
	cmd := newNewCmd()
	cmd.SetArgs([]string{"registered"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	app, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveRegistry(state.Registry{Projects: map[string]string{app.Name: configDir}}); err != nil {
		t.Fatal(err)
	}

	nestedRepoDir := filepath.Join(repo, "nested")
	if err := os.MkdirAll(nestedRepoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		cwd       string
		cwdSiding string
	}{
		{name: "nested repository directory", cwd: nestedRepoDir},
		{name: "canonical siding worktree", cwd: filepath.Join(configDir, "registered", "src"), cwdSiding: "registered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := activeResultForDir(context.Background(), tt.cwd)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Managed || !got.Active || !got.Registered || got.Project != app.Name || got.ConfigDir != configDir || got.Siding != tt.cwdSiding {
				t.Fatalf("registered active result = %#v", got)
			}
		})
	}
}

func TestActiveDoesNotUseRegisteredProjectWithSameBasename(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := sourceStateTempDir(t)
	registeredRepo := filepath.Join(root, "registered-parent", "shared")
	registeredCommit := initializeCommandRepoAt(t, registeredRepo)
	shellOnlyRepo := filepath.Join(root, "shell-parent", "shared")
	shellOnlyCommit := initializeCommandRepoAt(t, shellOnlyRepo)

	registeredConfigDir, err := config.ProjectConfigDir(registeredRepo)
	if err != nil {
		t.Fatal(err)
	}
	withWorkingDirectory(t, registeredRepo)
	registeredNew := newNewCmd()
	registeredNew.SetArgs([]string{"registered-siding", "--branch", registeredCommit})
	if err := registeredNew.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveRegistry(state.Registry{Projects: map[string]string{"shared": registeredConfigDir}}); err != nil {
		t.Fatal(err)
	}

	shellOnlyConfigDir, err := config.ProjectConfigDir(shellOnlyRepo)
	if err != nil {
		t.Fatal(err)
	}
	withWorkingDirectory(t, shellOnlyRepo)
	shellOnlyNew := newNewCmd()
	shellOnlyNew.SetArgs([]string{"shell-only-siding", "--branch", shellOnlyCommit})
	if err := shellOnlyNew.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := activeResultForDir(context.Background(), shellOnlyRepo)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Managed || got.Active || got.Registered {
		t.Fatalf("managed/active/registered = %t/%t/%t, want true/false/false: %#v", got.Managed, got.Active, got.Registered, got)
	}
	if got.Project != "shared" || got.ConfigDir != shellOnlyConfigDir || got.RepoPath != shellOnlyRepo {
		t.Fatalf("same-basename shell-only project resolved as %#v", got)
	}
	if len(got.Sidings) != 1 || got.Sidings[0].Name != "shell-only-siding" {
		t.Fatalf("same-basename shell-only sidings = %#v", got.Sidings)
	}
}

func TestActiveJSONKeepsLegacyActiveFalseForWorktreeOnlyState(t *testing.T) {
	repo, _, _ := newShellOnlyCommandRepo(t)
	withWorkingDirectory(t, repo)
	cmd := newNewCmd()
	cmd.SetArgs([]string{"shell"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := activeResultForDir(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var current struct {
		Active     bool `json:"active"`
		Managed    bool `json:"managed"`
		Registered bool `json:"registered"`
	}
	if err := json.Unmarshal(payload, &current); err != nil {
		t.Fatal(err)
	}
	if current.Active || !current.Managed || current.Registered {
		t.Fatalf("serialized compatibility fields = %#v, want active=false managed=true registered=false; JSON: %s", current, payload)
	}

	// A pre-managed-field consumer only observes active and must continue to
	// treat worktree-only state as not yet registered for guest operations.
	var legacy struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(payload, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Active {
		t.Fatalf("legacy active-only consumer sees worktree-only state as registered: %s", payload)
	}
}
