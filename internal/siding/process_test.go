package siding

import (
	"context"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestNonAspireStartTracksProcessGroup(t *testing.T) {
	script := nonAspireStartScript(state.App{Workdir: "web", Start: "npm start"})
	for _, want := range []string{"/bin/kill -0 -- \"-$old\"", "already running", "setsid", "/workspace/web", "exec npm start", "mktemp /run/.shunt-app.pid", "mv -f \"$tmp\" /run/shunt-app.pid"} {
		if !strings.Contains(script, want) {
			t.Errorf("script %q missing %q", script, want)
		}
	}
}

func TestNonAspireStartQuotesOuterShellExpansions(t *testing.T) {
	script := nonAspireStartScript(state.App{Workdir: "web $HOME `whoami` 'quoted'", Start: "printf '%s' \"$INNER\""})
	if strings.Contains(script, `-lc "`) {
		t.Fatalf("inner command is double quoted and exposed to the outer shell: %q", script)
	}
	for _, want := range []string{`'"'"'`, `$HOME`, "`whoami`", `$INNER`} {
		if !strings.Contains(script, want) {
			t.Errorf("script %q missing %q", script, want)
		}
	}
}

func TestNonAspireStopTerminatesThenKillsProcessGroup(t *testing.T) {
	script := nonAspireStopScript()
	term, kill := strings.Index(script, "/bin/kill -TERM -- \"-$pid\""), strings.Index(script, "/bin/kill -KILL -- \"-$pid\"")
	if term < 0 || kill < 0 || term >= kill {
		t.Fatalf("stop script does not TERM/wait/KILL in order: %q", script)
	}
	if !strings.Contains(script, "still running after SIGKILL") {
		t.Fatalf("stop script does not verify process-group exit: %q", script)
	}
}

func TestLoadWarmNoRefsAndNoArchiveIsNoOp(t *testing.T) {
	loaded, err := LoadWarm(context.Background(), state.App{ConfigDir: t.TempDir()}, state.Siding{})
	if err != nil || loaded {
		t.Fatalf("LoadWarm() = %v, %v; want false, nil", loaded, err)
	}
}
