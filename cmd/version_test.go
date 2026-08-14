package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/config"
)

func TestVersionTextIncludesBuildVersionAndNightlyIdentity(t *testing.T) {
	oldChannel, oldBuildVersion := config.Channel, config.BuildVersion
	t.Cleanup(func() {
		config.Channel = oldChannel
		config.BuildVersion = oldBuildVersion
	})
	config.Channel = "nightly"
	config.BuildVersion = "2026.08.12.1"

	got := versionText()
	for _, want := range []string{
		"channel=nightly",
		"binary=shunt-nightly",
		"adminPort=2319",
		"portOffset=+300",
		"containerPrefix=shuntnightly",
		"version=2026.08.12.1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("versionText() = %q, missing %q", got, want)
		}
	}
}

func TestInvokedExecutablePathPreservesStablePathFromPATH(t *testing.T) {
	root := t.TempDir()
	cellar := filepath.Join(root, "Cellar", "shunt-nightly", "1.0", "bin")
	stableDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(cellar, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stableDir, 0o755); err != nil {
		t.Fatal(err)
	}
	versioned := filepath.Join(cellar, "shunt-nightly")
	stable := filepath.Join(stableDir, "shunt-nightly")
	if err := os.WriteFile(versioned, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(versioned, stable); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	os.Args = []string{"shunt-nightly"}
	t.Setenv("PATH", stableDir)
	t.Cleanup(func() { os.Args = oldArgs })

	got, err := invokedExecutablePath()
	if err != nil {
		t.Fatal(err)
	}
	if got != stable {
		t.Fatalf("invokedExecutablePath() = %q, want stable path %q", got, stable)
	}
}

func TestInstallDashboardAgentUsesStableInvokedExecutablePath(t *testing.T) {
	root := t.TempDir()
	stableDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(stableDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(stableDir, "shunt-nightly")
	if err := os.WriteFile(stable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldArgs := os.Args
	os.Args = []string{"shunt-nightly"}
	t.Setenv("PATH", stableDir)
	t.Cleanup(func() { os.Args = oldArgs })

	var received string
	err := installDashboardAgent(context.Background(), func(_ context.Context, executable string) error {
		received = executable
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if received != stable {
		t.Fatalf("installer received %q, want stable path %q", received, stable)
	}
}
