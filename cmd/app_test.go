package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestValidateBaselineVolumeChangeRejectsInitializedBaseline(t *testing.T) {
	dir := t.TempDir()
	baseline, err := databaseline.New(dir, []string{"database"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := baseline.InitializeEmpty(context.Background()); err != nil {
		t.Fatal(err)
	}
	existing := state.App{ConfigDir: dir, Volumes: []string{"database"}}
	err = validateBaselineVolumeChange(context.Background(), existing, []string{"database", "files"})
	if err == nil || !strings.Contains(err.Error(), "cannot change") {
		t.Fatalf("validateBaselineVolumeChange() error = %v", err)
	}
}

func TestValidateBaselineVolumeChangeAllowsUninitializedBaselineAndReordering(t *testing.T) {
	existing := state.App{ConfigDir: t.TempDir(), Volumes: []string{"database", "files"}}
	if err := validateBaselineVolumeChange(context.Background(), existing, []string{"files", "database"}); err != nil {
		t.Fatalf("reordered volume set error = %v", err)
	}
	if err := validateBaselineVolumeChange(context.Background(), existing, []string{"database", "cache"}); err != nil {
		t.Fatalf("uninitialized volume change error = %v", err)
	}
}
