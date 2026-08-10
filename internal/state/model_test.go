package state

import "testing"

func TestRemovalStageValues(t *testing.T) {
	tests := []struct {
		stage RemovalStage
		want  string
	}{
		{RemovalStarted, "started"},
		{RemovalBasePinned, "base-pinned"},
		{RemovalBaselinePromoted, "baseline-promoted"},
		{RemovalGuestRemoved, "guest-removed"},
		{RemovalWorktreeRemoved, "worktree-removed"},
		{RemovalFilesRemoved, "files-removed"},
		{RemovalOperationForgotten, "operation-forgotten"},
	}
	for _, test := range tests {
		if got := string(test.stage); got != test.want {
			t.Errorf("removal stage = %q, want %q", got, test.want)
		}
	}
}
