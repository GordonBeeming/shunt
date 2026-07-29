package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestConfirmDataChangeForceSkipsPrompt(t *testing.T) {
	var out bytes.Buffer
	if err := confirmDataChange(true, "replace baseline?", bufio.NewReader(strings.NewReader("")), &out); err != nil {
		t.Fatalf("confirmDataChange() error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("force output = %q, want no prompt", out.String())
	}
}

func TestResolveDataPromoteSourceOrder(t *testing.T) {
	app := state.App{LiveSiding: "live", Sidings: map[string]state.Siding{"live": {}, "cwd": {}}}
	pickerCalls := 0
	pick := func(context.Context, state.App) (string, error) { pickerCalls++; return "picked", nil }
	tests := []struct {
		name      string
		args      []string
		loc       resolve.Location
		live      string
		want      string
		wantPicks int
	}{
		{"explicit", []string{"arg"}, resolve.Location{Siding: "cwd"}, "live", "arg", 0},
		{"cwd", nil, resolve.Location{Siding: "cwd"}, "live", "cwd", 0},
		{"live", nil, resolve.Location{}, "live", "live", 0},
		{"host falls back", nil, resolve.Location{}, state.HostTarget, "picked", 1},
		{"missing live falls back", nil, resolve.Location{}, "missing", "picked", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pickerCalls = 0
			app.LiveSiding = tc.live
			got, err := resolveDataPromoteSource(context.Background(), app, tc.loc, tc.args, pick)
			if err != nil || got != tc.want || pickerCalls != tc.wantPicks {
				t.Fatalf("got %q, %v, picks=%d", got, err, pickerCalls)
			}
		})
	}
}

func TestDataCommandShape(t *testing.T) {
	command := newDataCmd()
	if command.Use != "data" || len(command.Commands()) != 2 {
		t.Fatalf("data command = %#v", command)
	}
}

func TestDataPromoteHelpExplainsFutureBaseline(t *testing.T) {
	command := newDataPromoteCmd()
	if !strings.Contains(command.Short, "baseline") {
		t.Fatalf("data promote help = %q", command.Short)
	}
	prompt := dataPromotePrompt("source", []string{"database", "files"})
	if !strings.Contains(prompt, "future new sidings") || !strings.Contains(prompt, "existing siding copies") {
		t.Fatalf("promotion wording lost lifecycle scope: %q", prompt)
	}
}

func TestFormatDataPromoteErrorDoesNotClaimFailedPromotionCommitted(t *testing.T) {
	restoreFailure := &databaseline.RestoreError{
		Committed: false,
		Err:       errors.New("restore failed"),
	}
	err := formatDataPromoteError(databaseline.Result{}, restoreFailure)
	if !strings.HasPrefix(err.Error(), "data baseline was not committed") {
		t.Fatalf("formatDataPromoteError() = %q", err)
	}
}

func TestFormatDataPromoteErrorReportsCommittedRestoreFailure(t *testing.T) {
	restoreFailure := &databaseline.RestoreError{
		Committed: true,
		Err:       errors.New("restore failed"),
	}
	err := formatDataPromoteError(databaseline.Result{Committed: true}, restoreFailure)
	if !strings.Contains(err.Error(), "baseline committed") {
		t.Fatalf("formatDataPromoteError() = %q", err)
	}
}

func TestFormatDataPromoteErrorReportsDurabilityUncertaintyWithoutRetry(t *testing.T) {
	durabilityFailure := &databaseline.CommittedDurabilityError{
		Operation: "baseline manifest",
		Err:       errors.New("directory sync failed"),
	}
	err := formatDataPromoteError(databaseline.Result{Committed: true}, durabilityFailure)
	if !strings.Contains(err.Error(), "visible but durability is unconfirmed") || !strings.Contains(err.Error(), "do not retry") {
		t.Fatalf("formatDataPromoteError() = %q", err)
	}
	if _, ok := committedDataWarning("data baseline", databaseline.Result{Committed: true}, durabilityFailure); ok {
		t.Fatal("durability uncertainty was treated as a successful cleanup warning")
	}
}

func TestCommittedDataWarningUsesSuccessPathOnlyAfterCommit(t *testing.T) {
	err := &databaseline.CommittedCleanupError{Operation: "promote", RecoveryPaths: []string{"recover"}, Err: errors.New("cleanup failed")}
	warning, ok := committedDataWarning("data baseline", databaseline.Result{Committed: true, RecoveryPaths: []string{"recover"}}, err)
	if !ok || !strings.Contains(warning, "warning: data baseline committed") || !strings.Contains(warning, "recover") {
		t.Fatalf("committedDataWarning() = %q, %v", warning, ok)
	}
	if _, ok := committedDataWarning("data baseline", databaseline.Result{}, err); ok {
		t.Fatal("uncommitted result was treated as a committed warning")
	}
}

func TestRootRegistersDataCommand(t *testing.T) {
	root := newRootCmd()
	for _, command := range root.Commands() {
		if command.Name() == "data" {
			return
		}
	}
	t.Fatal("root command does not register data")
}
