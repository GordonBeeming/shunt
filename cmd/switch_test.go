package cmd

import (
	"bufio"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestPickSidingByNumberNamesTheSidingsWhenStdinIsEmpty(t *testing.T) {
	app := state.App{
		LiveSiding: "beta",
		Sidings: map[string]state.Siding{
			"alpha": {Name: "alpha"},
			"beta":  {Name: "beta"},
		},
	}
	names := []string{"alpha", "beta"}
	_, err := pickSidingByNumber(app, names, map[string]string{}, bufio.NewReader(strings.NewReader("")))
	if err == nil {
		t.Fatal("expected an error when stdin is empty")
	}
	if strings.Contains(err.Error(), "read selection") {
		t.Fatalf("still reports the raw EOF: %v", err)
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), bin()+" "+name) {
			t.Fatalf("hint is missing %q:\n%v", name, err)
		}
	}
}

func TestPickSidingByNumberStillAcceptsAPipedSelection(t *testing.T) {
	app := state.App{Sidings: map[string]state.Siding{"alpha": {}, "beta": {}}}
	got, err := pickSidingByNumber(app, []string{"alpha", "beta"}, map[string]string{}, bufio.NewReader(strings.NewReader("2\n")))
	if err != nil {
		t.Fatalf("piped selection failed: %v", err)
	}
	if got != "beta" {
		t.Fatalf("selected %q, want beta", got)
	}
}
