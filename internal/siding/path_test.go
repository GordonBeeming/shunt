package siding

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestValidateNameRejectsHostileValues(t *testing.T) {
	valid := []string{"a", "feature-1", "bug_fix.2", "A9"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) error = %v", name, err)
		}
	}

	invalid := []string{"", ".", "..", "host", "/tmp/escape", "../escape", "a/b", `a\b`, "a/../b", "two words", "-leading", ".hidden"}
	for _, name := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateName(name); err == nil {
				t.Fatalf("ValidateName(%q) succeeded", name)
			}
		})
	}
}

func TestSidingBaseRequiresOneContainedChild(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "project")
	app := state.App{ConfigDir: configDir}
	base, err := SidingBase(app, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configDir, "alpha"); base != want {
		t.Fatalf("SidingBase() = %q, want %q", base, want)
	}
	if _, err := SidingBase(state.App{ConfigDir: "relative"}, "alpha"); err == nil {
		t.Fatal("SidingBase accepted a relative config directory")
	}
}

func TestSpinRejectsHostileNameBeforeSideEffects(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "project")
	app := state.App{ConfigDir: configDir}
	if _, err := Spin(context.Background(), app, "..", "", ""); err == nil {
		t.Fatal("Spin accepted a traversal name")
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatalf("Spin created config state before validation: %v", err)
	}
}

func TestRemoveFilesCannotEscapeConfigDir(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "project")
	outside := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFiles(state.App{ConfigDir: configDir}, "../keep.txt"); err == nil {
		t.Fatal("RemoveFiles accepted a traversal name")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file was touched: %v", err)
	}
}
